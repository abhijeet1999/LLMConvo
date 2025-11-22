package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"llmconvo/internal/kafka"
	"llmconvo/internal/models"
	"llmconvo/internal/ollama"
	"llmconvo/internal/personas"
)

const (
	TopicRequest  = "debate-request"
	TopicResponse = "judge-response"
	OllamaURL     = "http://localhost:11434" // Shared Ollama instance
	DefaultModel  = "llama3.2:3b"
)

// Available models for judge - prioritize smallest first
var AvailableModels = []string{
	"gemma:2b",    // Smallest, fastest (~1.7 GB) - download first, use for all
	"llama3.2:3b", // Default - good reasoning (~2.0 GB)
	"phi3:mini",   // Fast alternative (~2.2 GB)
}

type JudgeService struct {
	ollamaClient *ollama.Client
	producer     *kafka.Producer
	consumer     *kafka.Consumer
	systemPrompt string
	model        string // Store the selected model
}

type JudgeRequest struct {
	Message      models.Message   `json:"message"`
	Conversation []models.Message `json:"conversation"`
}

func NewJudgeService(ollamaURL, model string) (*JudgeService, error) {
	ollamaClient := ollama.NewClient(ollamaURL, model)

	producer, err := kafka.NewProducer([]string{"localhost:19092"})
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	service := &JudgeService{
		ollamaClient: ollamaClient,
		producer:     producer,
		systemPrompt: personas.JudgeSystemPrompt,
	}

	consumer, err := kafka.NewConsumer([]string{"localhost:19092"}, service.handleMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to create consumer: %w", err)
	}
	service.consumer = consumer

	return service, nil
}

func (j *JudgeService) handleMessage(data []byte) error {
	var judgeReq JudgeRequest
	if err := json.Unmarshal(data, &judgeReq); err != nil {
		// Try as regular message first
		var msg models.Message
		if err2 := json.Unmarshal(data, &msg); err2 != nil {
			return fmt.Errorf("failed to unmarshal message: %w", err)
		}

		// If not a judge request, ignore
		if msg.Type != models.MessageTypeJudge {
			return nil
		}

		// This shouldn't happen, but handle gracefully
		return nil
	}

	if judgeReq.Message.Type != models.MessageTypeJudge {
		return nil
	}

	return j.evaluateDebate(judgeReq.Message, judgeReq.Conversation)
}

func (j *JudgeService) evaluateDebate(msg models.Message, conversation []models.Message) error {
	// Send "judge thinking" status
	thinkingMsg := models.Message{
		Type:      models.MessageTypeThinking,
		Persona:   "judge",
		Topic:     msg.Topic,
		Timestamp: time.Now(),
	}
	if err := j.producer.SendMessage("conversation-log", thinkingMsg); err != nil {
		log.Printf("Failed to send judge thinking status: %v", err)
	}

	// Build conversation transcript
	var transcript strings.Builder
	transcript.WriteString(fmt.Sprintf("Debate Topic: %s\n\n", msg.Topic))
	transcript.WriteString("Conversation:\n\n")

	for _, m := range conversation {
		if m.Type == models.MessageTypeArgument {
			personaName := "Hindu Republican Capitalist"
			if m.Persona == "persona2" {
				personaName = "Atheist Democratic Socialist"
			}
			transcript.WriteString(fmt.Sprintf("Round %d - %s: %s\n\n", m.Round, personaName, m.Content))
		}
	}

	transcript.WriteString("\n\nNow evaluate this debate and choose a winner. Your response must:\n")
	transcript.WriteString("1. Start with EXACTLY ONE line: either 'WINNER: persona1' or 'WINNER: persona2' (choose only one)\n")
	transcript.WriteString("2. Follow with 3-5 sentences explaining your reasoning (do NOT repeat the winner declaration)\n")
	transcript.WriteString("3. Do NOT mention both personas as winners - choose only ONE winner\n")

	prompt := transcript.String()

	// Judge needs more tokens for evaluation (3-5 sentences)
	response, err := j.ollamaClient.GenerateWithTokens(prompt, j.systemPrompt, 200)
	if err != nil {
		return fmt.Errorf("failed to generate judgment: %w", err)
	}

	// Parse winner from response - find the FIRST occurrence
	responseUpper := strings.ToUpper(response)
	winner := "persona1" // Default
	reason := response

	// Find the first occurrence of WINNER declaration (check both formats)
	persona2Idx := strings.Index(responseUpper, "WINNER: PERSONA2")
	persona1Idx := strings.Index(responseUpper, "WINNER: PERSONA1")

	// Also check for persona names in the response
	hinduCapitalistIdx := strings.Index(responseUpper, "WINNER: HINDU REPUBLICAN CAPITALIST")
	atheistSocialistIdx := strings.Index(responseUpper, "WINNER: ATHEIST DEMOCRATIC SOCIALIST")

	// Determine winner based on first occurrence (prioritize persona1/persona2 format, then names)
	var winnerIdx = -1
	var winnerPersona = "persona1"

	// Check persona1/persona2 format first
	if persona1Idx != -1 {
		winnerIdx = persona1Idx
		winnerPersona = "persona1"
	}
	if persona2Idx != -1 && (winnerIdx == -1 || persona2Idx < winnerIdx) {
		winnerIdx = persona2Idx
		winnerPersona = "persona2"
	}

	// Check persona names format
	if hinduCapitalistIdx != -1 && (winnerIdx == -1 || hinduCapitalistIdx < winnerIdx) {
		winnerIdx = hinduCapitalistIdx
		winnerPersona = "persona1"
	}
	if atheistSocialistIdx != -1 && (winnerIdx == -1 || atheistSocialistIdx < winnerIdx) {
		winnerIdx = atheistSocialistIdx
		winnerPersona = "persona2"
	}

	winner = winnerPersona

	// Extract reasoning - remove all WINNER declarations from the text
	lines := strings.Split(response, "\n")
	var reasonLines []string
	for _, line := range lines {
		lineUpper := strings.ToUpper(strings.TrimSpace(line))
		// Skip lines that contain WINNER declarations (all formats)
		if !strings.Contains(lineUpper, "WINNER: PERSONA1") &&
			!strings.Contains(lineUpper, "WINNER: PERSONA2") &&
			!strings.Contains(lineUpper, "WINNER: HINDU REPUBLICAN CAPITALIST") &&
			!strings.Contains(lineUpper, "WINNER: ATHEIST DEMOCRATIC SOCIALIST") &&
			!strings.Contains(lineUpper, "**WINNER:") {
			reasonLines = append(reasonLines, line)
		}
	}

	// Join the reason lines, removing empty lines at the start
	reason = strings.TrimSpace(strings.Join(reasonLines, "\n"))

	// If we couldn't extract a clean reason, use the original but clean it up
	if reason == "" || strings.Contains(strings.ToUpper(reason), "WINNER:") {
		reason = response
		// Remove common WINNER patterns (all formats)
		reason = strings.ReplaceAll(reason, "WINNER: persona1", "")
		reason = strings.ReplaceAll(reason, "WINNER: persona2", "")
		reason = strings.ReplaceAll(reason, "WINNER: PERSONA1", "")
		reason = strings.ReplaceAll(reason, "WINNER: PERSONA2", "")
		reason = strings.ReplaceAll(reason, "WINNER: Hindu Republican Capitalist", "")
		reason = strings.ReplaceAll(reason, "WINNER: Atheist Democratic Socialist", "")
		reason = strings.ReplaceAll(reason, "WINNER: HINDU REPUBLICAN CAPITALIST", "")
		reason = strings.ReplaceAll(reason, "WINNER: ATHEIST DEMOCRATIC SOCIALIST", "")
		reason = strings.ReplaceAll(reason, "**WINNER:", "")
		reason = strings.ReplaceAll(reason, "**", "")
		// Remove any remaining "WINNER:" patterns
		reason = regexp.MustCompile(`(?i)WINNER:\s*[^\n]*`).ReplaceAllString(reason, "")
		reason = strings.TrimSpace(reason)
	}

	judgeMsg := models.Message{
		Type:      models.MessageTypeDebateEnd,
		Persona:   "judge",
		Content:   reason,
		Topic:     msg.Topic,
		Winner:    winner,
		Timestamp: msg.Timestamp,
		Model:     j.model, // Include the model being used
	}

	if err := j.producer.SendMessage(TopicResponse, judgeMsg); err != nil {
		return fmt.Errorf("failed to send judgment: %w", err)
	}

	log.Printf("Judge decision: Winner is %s", winner)
	log.Printf("Reasoning: %s", reason)

	return nil
}

func (j *JudgeService) Start() error {
	log.Println("Starting judge service...")
	return j.consumer.Consume(TopicRequest)
}

func selectModel() string {
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = OllamaURL
	}
	client := ollama.NewClient(ollamaURL, "")

	// Check if model is specified via environment variable
	if model := os.Getenv("JUDGE_MODEL"); model != "" {
		if client.CheckModelAvailability(model) {
			log.Printf("Using model from JUDGE_MODEL: %s", model)
			return model
		}
		log.Printf("Warning: Model '%s' specified in JUDGE_MODEL not found, trying alternatives.", model)
	}

	// Check if all 3 models are available - if so, use llama3.2:3b for judge
	allModels := []string{"gemma:2b", "phi3:mini", "llama3.2:3b"}
	allAvailable := true
	for _, model := range allModels {
		if !client.CheckModelAvailability(model) {
			allAvailable = false
			break
		}
	}

	if allAvailable {
		// All models available - use llama3.2:3b for judge (best reasoning)
		log.Printf("All models available! Using llama3.2:3b for Judge")
		return "llama3.2:3b"
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
	log.Printf("Judge service starting with model: %s", model)

	service, err := NewJudgeService(ollamaURL, model)
	if err != nil {
		log.Fatalf("Failed to create judge service: %v", err)
	}
	defer service.producer.Close()
	defer service.consumer.Close()

	if err := service.Start(); err != nil {
		log.Fatalf("Service error: %v", err)
	}
}
