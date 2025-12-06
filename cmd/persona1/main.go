package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"llmconvo/internal/kafka"
	"llmconvo/internal/models"
	"llmconvo/internal/ollama"
	"llmconvo/internal/personas"
)

const (
	TopicRequest  = "debate-request"
	TopicResponse = "persona1-response"
	OllamaURL     = "http://localhost:11434"
	DefaultModel  = "llama3.2:3b"
	PersonaName   = "persona1"
)

// Available models for persona1 (lightweight, fast) - prioritize smallest first
var AvailableModels = []string{
	"gemma:2b",    // Smallest, fastest (~1.7 GB) - download first, use for all
	"phi3:mini",   // Very fast, small (~2.2 GB)
	"llama3.2:3b", // Good balance (~2.0 GB)
}

type PersonaService struct {
	ollamaClient         *ollama.Client
	producer             *kafka.Producer
	consumer             *kafka.Consumer
	conversationConsumer *kafka.Consumer // Consumer for conversation-log to see opponent responses
	systemPrompt         string
	personaName          string
	conversation         []models.Message
	currentModel         string
	availableModels      []string
	ollamaURL            string
}

func getKafkaBrokers() []string {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		brokers = "localhost:19092" // Default for local development
	}
	return []string{brokers}
}

func NewPersonaService(ollamaURL, model, personaName, systemPrompt string) (*PersonaService, error) {
	ollamaClient := ollama.NewClient(ollamaURL, model)

	producer, err := kafka.NewProducer(getKafkaBrokers())
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	service := &PersonaService{
		ollamaClient:    ollamaClient,
		producer:        producer,
		systemPrompt:    systemPrompt,
		personaName:     personaName,
		conversation:    []models.Message{},
		currentModel:    model,
		availableModels: AvailableModels,
		ollamaURL:       ollamaURL,
	}

	consumer, err := kafka.NewConsumer(getKafkaBrokers(), service.handleMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}
	service.consumer = consumer

	// Also consume from conversation-log to see opponent's actual responses
	conversationConsumer, err := kafka.NewConsumer(getKafkaBrokers(), service.handleConversationMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to create conversation consumer: %w", err)
	}
	service.conversationConsumer = conversationConsumer

	return service, nil
}

func (p *PersonaService) handleMessage(data []byte) error {
	log.Printf("[%s] Received message: %s", p.personaName, string(data))
	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("[%s] Failed to unmarshal: %v", p.personaName, err)
		return fmt.Errorf("failed to unmarshal message: %w", err)
	}
	log.Printf("[%s] Parsed message type: %s, starter: %s", p.personaName, msg.Type, msg.Starter)

	// Handle topic message (start of debate)
	if msg.Type == models.MessageTypeTopic {
		p.conversation = []models.Message{}
		// If I'm the starter, make opening argument (round 1)
		if msg.Starter == p.personaName {
			log.Printf("[%s] I'm the starter! Generating opening argument...", p.personaName)
			// Small delay to ensure orchestrator is ready
			time.Sleep(500 * time.Millisecond)
			if err := p.generateResponse(msg, true); err != nil {
				log.Printf("[%s] Error generating response: %v", p.personaName, err)
				return err
			}
			log.Printf("[%s] Successfully generated opening argument", p.personaName)
			return nil
		}
		log.Printf("[%s] Not the starter, waiting for my turn", p.personaName)
		return nil
	}

	// Handle argument request - this is just a notification that it's my turn
	if msg.Type == models.MessageTypeArgument {
		if msg.Persona == p.personaName {
			// It's my turn to respond
			return p.generateResponse(msg, false)
		}
		// If it's opponent's turn, we'll get their actual response via conversation-log
	}

	return nil
}

// handleConversationMessage processes messages from conversation-log topic
// This is where we see the actual opponent responses with content
func (p *PersonaService) handleConversationMessage(data []byte) error {
	var msg models.Message
	if err := kafka.UnmarshalMessage(data, &msg); err != nil {
		return err
	}

	// Only store opponent's actual argument responses (with content)
	if msg.Type == models.MessageTypeArgument && msg.Persona != p.personaName && msg.Content != "" {
		// Store opponent's actual response for context
		p.conversation = append(p.conversation, msg)
		log.Printf("[%s] Stored opponent's response from Round %d: %s", p.personaName, msg.Round, msg.Content[:min(50, len(msg.Content))])
	}

	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (p *PersonaService) generateResponse(msg models.Message, isOpening bool) error {
	// Determine round number
	round := 1
	if !isOpening {
		round = msg.Round
	}

	// Send "thinking" status
	thinkingMsg := models.Message{
		Type:      models.MessageTypeThinking,
		Persona:   p.personaName,
		Round:     round,
		Topic:     msg.Topic,
		Timestamp: time.Now(),
		Model:     p.currentModel, // Include model info
	}
	if err := p.producer.SendMessage("conversation-log", thinkingMsg); err != nil {
		log.Printf("[%s] Failed to send thinking status: %v", p.personaName, err)
	}

	var prompt string

	maxTokens := 50
	if isOpening {
		prompt = fmt.Sprintf("Debate Topic: %s\n\nAs a Hindu Republican Capitalist, make your opening argument about this topic (1 line max, use abbrevs like 'govt' not 'government'). Apply your beliefs to this specific topic. Do NOT say 'I conclude' - that's only for Round 5. Just make your opening statement:", msg.Topic)
	} else {
		// Check if this is the final round (round 5)
		isFinalRound := msg.Round >= 5

		// Build conversation context - adaptive context size based on round
		context := fmt.Sprintf("Topic: %s\n\nRecent conversation:\n\n", msg.Topic)

		// Use minimal context (1 message) for fastest generation
		contextSize := 1

		// Get last N messages for context
		startIdx := len(p.conversation) - contextSize
		if startIdx < 0 {
			startIdx = 0
		}

		for i := startIdx; i < len(p.conversation); i++ {
			m := p.conversation[i]
			if m.Type == models.MessageTypeArgument {
				speaker := "Opponent"
				if m.Persona == p.personaName {
					speaker = "You"
				}
				context += fmt.Sprintf("Round %d - %s: %s\n\n", m.Round, speaker, m.Content)
			}
		}

		if isFinalRound {
			context += fmt.Sprintf("This is the FINAL ROUND (Round 5) about the topic '%s'. Provide your conclusion:\n", msg.Topic)
			context += "- Start with 'I conclude' (ONLY in this final round)\n"
			context += "- Mention what both sides may agree on regarding this topic\n"
			context += "- End with a strong closing point from your Hindu Republican Capitalist perspective\n"
			context += "- You can use up to 100 tokens for this conclusion\n"
			context += "- IMPORTANT: Only write your conclusion, do NOT repeat the conversation format or round numbers"
			maxTokens = 100 // Allow 100 tokens for conclusion
		} else {
			context += fmt.Sprintf("Now respond about the topic '%s' (1 line max, use abbrevs like 'govt' not 'government'). CRITICAL: You MUST directly address and counter your opponent's most recent argument above. Do NOT give generic responses like 'I respect diversity' or 'it's a matter of belief' - you are DEBATING! Challenge their points, provide counter-examples, or refute their logic. Then add your own NEW point. Stay in character as a Hindu Republican Capitalist. Do NOT say 'I conclude' - that's only for the final round. Only write your response, do NOT include round numbers or 'Opponent:' labels:", msg.Topic)
		}
		prompt = context
	}

	// Estimate token usage and warn if approaching limit
	estimatedTokens := estimateTokenUsage(prompt, p.systemPrompt, len(p.conversation))
	if estimatedTokens > 7900 {
		log.Printf("⚠️  WARNING: Approaching token limit! Estimated usage: %d/8000 tokens", estimatedTokens)
	}

	var response string
	var err error
	if maxTokens > 50 {
		response, err = p.ollamaClient.GenerateWithTokens(prompt, p.systemPrompt, maxTokens)
	} else {
		response, err = p.ollamaClient.Generate(prompt, p.systemPrompt)
	}
	if err != nil {
		// Try fallback models on any error (timeout, memory, connection, etc.)
		log.Printf("[%s] Error with model %s: %v. Trying fallback models...", p.personaName, p.currentModel, err)

		// Fallback models in order of preference (fastest/smallest first)
		fallbackModels := []string{"gemma:2b", "phi3:mini", "llama3.2:3b"}

		// Remove current model from fallback list if it's already there
		for i, model := range fallbackModels {
			if model == p.currentModel {
				fallbackModels = append(fallbackModels[:i], fallbackModels[i+1:]...)
				break
			}
		}

		// Try each fallback model
		for _, fallbackModel := range fallbackModels {
			log.Printf("[%s] Trying fallback model: %s", p.personaName, fallbackModel)
			fallbackClient := ollama.NewClient(p.ollamaURL, fallbackModel)

			if maxTokens > 50 {
				response, err = fallbackClient.GenerateWithTokens(prompt, p.systemPrompt, maxTokens)
			} else {
				response, err = fallbackClient.Generate(prompt, p.systemPrompt)
			}

			if err == nil {
				log.Printf("[%s] ✅ Successfully switched to model: %s", p.personaName, fallbackModel)
				p.currentModel = fallbackModel // Update current model for future requests
				break                          // Success, exit loop
			} else {
				log.Printf("[%s] ⚠️  Fallback model %s also failed: %v", p.personaName, fallbackModel, err)
			}
		}

		// If all models failed, return error
		if err != nil {
			log.Printf("[%s] ❌ All models failed. Last error: %v", p.personaName, err)
			return fmt.Errorf("failed to generate response after trying all models: %w", err)
		}
	}

	// Clean up response (remove extra whitespace and prompt artifacts)
	response = strings.TrimSpace(response)
	// Remove any prompt format artifacts (e.g., "Round X - Opponent:" or "Round X - You:")
	lines := strings.Split(response, "\n")
	var cleanedLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip lines that look like prompt format
		if strings.HasPrefix(line, "Round ") && (strings.Contains(line, "- Opponent:") || strings.Contains(line, "- You:")) {
			continue
		}
		if line != "" {
			cleanedLines = append(cleanedLines, line)
		}
	}
	response = strings.Join(cleanedLines, " ")
	response = strings.TrimSpace(response)

	// Create response message (round already determined above)

	responseMsg := models.Message{
		Type:      models.MessageTypeArgument,
		Round:     round,
		Persona:   p.personaName,
		Content:   response,
		Topic:     msg.Topic,
		Timestamp: time.Now(),
		Model:     p.currentModel, // Include the model being used
	}

	// Add to conversation
	p.conversation = append(p.conversation, responseMsg)

	// Send response
	if err := p.producer.SendMessage(TopicResponse, responseMsg); err != nil {
		return fmt.Errorf("failed to send response: %w", err)
	}

	log.Printf("[%s] Round %d: %s", p.personaName, round, response)
	return nil
}

func (p *PersonaService) Start() error {
	log.Printf("Starting %s service...", p.personaName)

	// Start conversation consumer in background to see opponent responses
	go func() {
		if err := p.conversationConsumer.Consume("conversation-log"); err != nil {
			log.Printf("[%s] Conversation consumer error: %v", p.personaName, err)
		}
	}()

	return p.consumer.Consume(TopicRequest)
}

func selectModel() string {
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = OllamaURL
	}
	client := ollama.NewClient(ollamaURL, "")

	// Check if model is specified via environment variable
	if model := os.Getenv("PERSONA1_MODEL"); model != "" {
		if client.CheckModelAvailability(model) {
			log.Printf("Using model from PERSONA1_MODEL: %s", model)
			return model
		}
		log.Printf("Warning: Model '%s' specified in PERSONA1_MODEL not found, trying alternatives.", model)
	}

	// Check if all 3 models are available - if so, use gemma:2b for persona1
	allModels := []string{"gemma:2b", "phi3:mini", "llama3.2:3b"}
	allAvailable := true
	for _, model := range allModels {
		if !client.CheckModelAvailability(model) {
			allAvailable = false
			break
		}
	}

	if allAvailable {
		// All models available - use gemma:2b for persona1
		log.Printf("All models available! Using gemma:2b for Persona1")
		return "gemma:2b"
	}

	// Not all models available - use first available from list
	for _, model := range AvailableModels {
		if client.CheckModelAvailability(model) {
			log.Printf("Using available model: %s", model)
			return model
		}
	}

	// Fallback to default
	log.Printf("Using default model: %s", DefaultModel)
	return DefaultModel
}

func main() {
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = OllamaURL
	}

	model := selectModel()
	log.Printf("Persona1 service starting with model: %s", model)

	service, err := NewPersonaService(ollamaURL, model, PersonaName, personas.Persona1SystemPrompt)
	if err != nil {
		log.Fatalf("Failed to create persona service: %v", err)
	}
	defer service.producer.Close()
	defer service.consumer.Close()

	if err := service.Start(); err != nil {
		log.Fatalf("Service error: %v", err)
	}
}

// estimateTokenUsage roughly estimates token count (1 token ≈ 4 characters)
func estimateTokenUsage(prompt, systemPrompt string, conversationLength int) int {
	// System prompt: ~200 tokens
	// Topic + prompt: ~50 tokens per 200 chars
	// Conversation context: ~150 tokens per round (last 3 rounds)
	// Response: ~50 tokens

	systemTokens := len(systemPrompt) / 4
	promptTokens := len(prompt) / 4
	contextTokens := conversationLength * 50 // Rough estimate
	responseTokens := 50

	total := systemTokens + promptTokens + contextTokens + responseTokens
	return total
}
