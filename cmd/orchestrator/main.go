package main

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"llmconvo/internal/kafka"
	"llmconvo/internal/models"
)

const (
	TopicRequest      = "debate-request"
	TopicPersona1Resp = "persona1-response"
	TopicPersona2Resp = "persona2-response"
	TopicJudgeResp    = "judge-response"
	TopicConversation = "conversation-log"
)

type Orchestrator struct {
	producer         *kafka.Producer
	state            *models.DebateState
	maxRounds        int
	currentRound     int
	consumersStarted bool
	mu               sync.Mutex
}

func getKafkaBrokers() []string {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		brokers = "localhost:19092" // Default for local development
	}
	return []string{brokers}
}

func NewOrchestrator() (*Orchestrator, error) {
	producer, err := kafka.NewProducer(getKafkaBrokers())
	if err != nil {
		return nil, fmt.Errorf("failed to create producer: %w", err)
	}

	o := &Orchestrator{
		producer:     producer,
		maxRounds:    5,
		currentRound: 0,
	}

	// Start consumers immediately at startup
	o.consumersStarted = true
	go o.consumeResponses()

	return o, nil
}

func (o *Orchestrator) StartDebate(topic string, maxRounds int) error {
	o.mu.Lock()
	// Don't start if there's already an active debate
	if o.state != nil {
		log.Printf("Debate already in progress, ignoring topic request: %s", topic)
		o.mu.Unlock()
		return nil
	}

	// Validate maxRounds
	if maxRounds <= 0 {
		maxRounds = 5 // Default to 5
	}
	if maxRounds != 5 && maxRounds != 10 {
		maxRounds = 5 // Only allow 5 or 10, default to 5
	}

	// Randomly select starter
	rand.Seed(time.Now().UnixNano())
	starter := "persona1"
	if rand.Intn(2) == 1 {
		starter = "persona2"
	}

	o.state = &models.DebateState{
		Topic:        topic,
		CurrentRound: 0,
		MaxRounds:    maxRounds,
		Starter:      starter,
		Messages:     []models.Message{},
	}
	o.mu.Unlock()

	log.Printf("Starting debate on topic: %s", topic)
	log.Printf("Starter: %s", starter)
	log.Printf("Max Rounds: %d", maxRounds)

	// Send topic message
	topicMsg := models.Message{
		Type:      models.MessageTypeTopic,
		Topic:     topic,
		Starter:   starter,
		MaxRounds: maxRounds,
		Timestamp: time.Now(),
	}

	if err := o.producer.SendMessage(TopicRequest, topicMsg); err != nil {
		return fmt.Errorf("failed to send topic: %w", err)
	}

	// Also log to conversation
	if err := o.producer.SendMessage(TopicConversation, topicMsg); err != nil {
		return fmt.Errorf("failed to log topic: %w", err)
	}

	// Send thinking status for starter
	thinkingMsg := models.Message{
		Type:      models.MessageTypeThinking,
		Round:     1,
		Persona:   starter,
		Topic:     topic,
		Timestamp: time.Now(),
	}
	if err := o.producer.SendMessage(TopicConversation, thinkingMsg); err != nil {
		log.Printf("Failed to send thinking status: %v", err)
	}

	return nil
}

func (o *Orchestrator) consumeResponses() {
	// Consumer for persona1 responses
	go func() {
		consumer, err := kafka.NewConsumer(getKafkaBrokers(), func(data []byte) error {
			var msg models.Message
			if err := kafka.UnmarshalMessage(data, &msg); err != nil {
				return err
			}
			return o.handleResponse("persona1", msg)
		})
		if err != nil {
			log.Printf("Failed to create persona1 consumer: %v", err)
			return
		}
		defer consumer.Close()

		if err := consumer.Consume(TopicPersona1Resp); err != nil {
			log.Printf("Persona1 consumer error: %v", err)
		}
	}()

	// Consumer for persona2 responses
	go func() {
		consumer, err := kafka.NewConsumer(getKafkaBrokers(), func(data []byte) error {
			var msg models.Message
			if err := kafka.UnmarshalMessage(data, &msg); err != nil {
				return err
			}
			return o.handleResponse("persona2", msg)
		})
		if err != nil {
			log.Printf("Failed to create persona2 consumer: %v", err)
			return
		}
		defer consumer.Close()

		if err := consumer.Consume(TopicPersona2Resp); err != nil {
			log.Printf("Persona2 consumer error: %v", err)
		}
	}()

	// Consumer for judge response
	go func() {
		consumer, err := kafka.NewConsumer(getKafkaBrokers(), func(data []byte) error {
			var msg models.Message
			if err := kafka.UnmarshalMessage(data, &msg); err != nil {
				return err
			}
			return o.handleJudgeResponse(msg)
		})
		if err != nil {
			log.Printf("Failed to create judge consumer: %v", err)
			return
		}
		defer consumer.Close()

		if err := consumer.Consume(TopicJudgeResp); err != nil {
			log.Printf("Judge consumer error: %v", err)
		}
	}()
}

func (o *Orchestrator) handleResponse(persona string, msg models.Message) error {
	o.mu.Lock()
	if o.state == nil {
		o.mu.Unlock()
		log.Printf("Ignoring response from %s: debate state is nil", persona)
		return nil // Debate was reset, ignore old messages
	}

	// Validate message matches current debate
	if msg.Topic != o.state.Topic {
		o.mu.Unlock()
		log.Printf("Ignoring response from %s: topic mismatch (got '%s', expected '%s')", persona, msg.Topic, o.state.Topic)
		return nil // Different topic, ignore
	}

	// Validate round number - must be next expected round
	// Allow some flexibility: accept current round or next round (handles delayed responses)
	expectedRound := o.state.CurrentRound + 1
	if msg.Round != expectedRound && msg.Round != o.state.CurrentRound {
		o.mu.Unlock()
		log.Printf("Ignoring response from %s: round mismatch (got %d, expected %d or %d)", persona, msg.Round, expectedRound, o.state.CurrentRound)
		return nil // Wrong round, ignore
	}
	// If message is for current round, it's a duplicate - ignore
	if msg.Round == o.state.CurrentRound {
		o.mu.Unlock()
		log.Printf("Ignoring duplicate response from %s for round %d", persona, msg.Round)
		return nil
	}

	// Validate persona - must be the expected speaker
	// For round 1, it's the starter. For subsequent rounds, alternate.
	var expectedPersona string
	if expectedRound == 1 {
		expectedPersona = o.state.Starter
	} else {
		// Get last message to determine who should speak next
		if len(o.state.Messages) > 0 {
			lastPersona := o.state.Messages[len(o.state.Messages)-1].Persona
			if lastPersona == "persona1" {
				expectedPersona = "persona2"
			} else {
				expectedPersona = "persona1"
			}
		} else {
			expectedPersona = o.state.Starter
		}
	}

	if msg.Persona != expectedPersona {
		o.mu.Unlock()
		log.Printf("Ignoring response: wrong persona (got %s, expected %s) for round %d", msg.Persona, expectedPersona, msg.Round)
		return nil // Wrong persona, ignore
	}

	// Check if we already have this round (duplicate check)
	for _, existingMsg := range o.state.Messages {
		if existingMsg.Round == msg.Round && existingMsg.Persona == msg.Persona {
			o.mu.Unlock()
			log.Printf("Ignoring duplicate response from %s for round %d", persona, msg.Round)
			return nil // Duplicate, ignore
		}
	}

	// All checks passed - process the response
	o.state.Messages = append(o.state.Messages, msg)
	o.state.CurrentRound = msg.Round
	state := o.state
	currentRound := msg.Round
	o.mu.Unlock()

	// Log to conversation
	if err := o.producer.SendMessage(TopicConversation, msg); err != nil {
		return err
	}

	log.Printf("✅ Round %d complete from %s. Total messages: %d", currentRound, persona, len(state.Messages))

	if currentRound >= state.MaxRounds {
		log.Println("Debate complete! Requesting judge evaluation...")
		return o.requestJudgeEvaluation()
	}

	// Determine next speaker (alternate)
	nextPersona := "persona2"
	if persona == "persona2" {
		nextPersona = "persona1"
	}

	// Send thinking status for next persona
	thinkingMsg := models.Message{
		Type:      models.MessageTypeThinking,
		Round:     currentRound + 1,
		Persona:   nextPersona,
		Topic:     state.Topic,
		Timestamp: time.Now(),
	}
	if err := o.producer.SendMessage(TopicConversation, thinkingMsg); err != nil {
		log.Printf("Failed to send thinking status: %v", err)
	}

	// Send next round request
	nextMsg := models.Message{
		Type:      models.MessageTypeArgument,
		Round:     currentRound + 1,
		Persona:   nextPersona,
		Topic:     state.Topic,
		Timestamp: time.Now(),
	}

	log.Printf("➡️  Requesting response from %s for round %d", nextPersona, nextMsg.Round)
	return o.producer.SendMessage(TopicRequest, nextMsg)
}

func (o *Orchestrator) requestJudgeEvaluation() error {
	judgeMsg := models.Message{
		Type:      models.MessageTypeJudge,
		Topic:     o.state.Topic,
		Timestamp: time.Now(),
	}

	// Include full conversation history
	judgeData := map[string]interface{}{
		"message":      judgeMsg,
		"conversation": o.state.Messages,
	}

	return o.producer.SendMessage(TopicRequest, judgeData)
}

func (o *Orchestrator) handleJudgeResponse(msg models.Message) error {
	o.mu.Lock()
	if o.state == nil {
		o.mu.Unlock()
		return nil // Debate was reset
	}
	// Use msg.Winner if available, otherwise fall back to msg.Persona
	winner := msg.Winner
	if winner == "" {
		winner = msg.Persona
	}
	o.state.Winner = winner
	o.state.JudgeReason = msg.Content
	state := o.state // Capture state for saving
	o.mu.Unlock()

	endMsg := models.Message{
		Type:      models.MessageTypeDebateEnd,
		Winner:    winner,
		Content:   msg.Content,
		Timestamp: time.Now(),
	}

	log.Printf("Debate ended! Winner: %s", winner)
	log.Printf("Judge's reasoning: %s", msg.Content)

	// Save conversation to file
	if err := o.saveConversation(state); err != nil {
		log.Printf("Failed to save conversation: %v", err)
		// Don't fail the debate end if saving fails
	}

	return o.producer.SendMessage(TopicConversation, endMsg)
}

// saveConversation saves the complete debate conversation to a file
func (o *Orchestrator) saveConversation(state *models.DebateState) error {
	// Sanitize topic for filename
	filename := sanitizeFilename(state.Topic)
	if filename == "" {
		filename = "untitled"
	}
	filename = fmt.Sprintf("topic-%s.txt", filename)

	// Create conversations directory if it doesn't exist
	conversationsDir := "conversations"
	if err := os.MkdirAll(conversationsDir, 0755); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
	}

	filepath := filepath.Join(conversationsDir, filename)

	// Build conversation text
	var sb strings.Builder
	sb.WriteString("=" + strings.Repeat("=", 78) + "\n")
	sb.WriteString(fmt.Sprintf("DEBATE TOPIC: %s\n", state.Topic))
	sb.WriteString("=" + strings.Repeat("=", 78) + "\n\n")

	// Starter information
	starterName := "Hindu Republican Capitalist"
	if state.Starter == "persona2" {
		starterName = "Atheist Democratic Socialist"
	}
	sb.WriteString(fmt.Sprintf("Started by: %s\n", starterName))
	sb.WriteString(fmt.Sprintf("Total Rounds: %d\n", state.CurrentRound))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString("\n" + strings.Repeat("-", 80) + "\n\n")

	// Write all messages
	for _, msg := range state.Messages {
		if msg.Type == models.MessageTypeArgument {
			personaName := "Hindu Republican Capitalist"
			if msg.Persona == "persona2" {
				personaName = "Atheist Democratic Socialist"
			}
			sb.WriteString(fmt.Sprintf("Round %d - %s:\n", msg.Round, personaName))
			sb.WriteString(fmt.Sprintf("%s\n\n", msg.Content))
		}
	}

	// Judge decision
	sb.WriteString(strings.Repeat("=", 80) + "\n")
	sb.WriteString("JUDGE'S DECISION\n")
	sb.WriteString(strings.Repeat("=", 80) + "\n\n")

	winnerName := "Hindu Republican Capitalist"
	if state.Winner == "persona2" {
		winnerName = "Atheist Democratic Socialist"
	}
	sb.WriteString(fmt.Sprintf("Winner: %s\n\n", winnerName))
	sb.WriteString("Reasoning:\n")
	sb.WriteString(fmt.Sprintf("%s\n\n", state.JudgeReason))

	sb.WriteString(strings.Repeat("=", 80) + "\n")
	sb.WriteString(fmt.Sprintf("End of Debate - %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(strings.Repeat("=", 80) + "\n")

	// Write to file
	if err := os.WriteFile(filepath, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("failed to write conversation file: %w", err)
	}

	log.Printf("Conversation saved to: %s", filepath)
	return nil
}

// sanitizeFilename creates a safe filename from the topic string
func sanitizeFilename(topic string) string {
	// Remove or replace invalid filename characters
	invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r"}
	result := topic
	for _, char := range invalidChars {
		result = strings.ReplaceAll(result, char, "-")
	}
	// Remove multiple consecutive dashes
	result = strings.ReplaceAll(result, "--", "-")
	result = strings.Trim(result, "- ")
	// Limit length
	if len(result) > 100 {
		result = result[:100]
	}
	// Convert to lowercase and replace spaces with dashes
	result = strings.ToLower(result)
	result = strings.ReplaceAll(result, " ", "-")
	return result
}

func main() {
	orchestrator, err := NewOrchestrator()
	if err != nil {
		log.Fatalf("Failed to create orchestrator: %v", err)
	}
	defer orchestrator.producer.Close()

	// Listen for topic messages from web server
	go func() {
		consumer, err := kafka.NewConsumer(getKafkaBrokers(), func(data []byte) error {
			var msg models.Message
			if err := kafka.UnmarshalMessage(data, &msg); err != nil {
				return err
			}

			// Handle restart message
			if msg.Type == models.MessageTypeRestart {
				log.Println("Received restart request - resetting debate state")
				orchestrator.mu.Lock()
				orchestrator.state = nil
				orchestrator.currentRound = 0
				orchestrator.mu.Unlock()
				return nil
			}

			// If it's a topic message, start a new debate (will replace any existing one)
			if msg.Type == models.MessageTypeTopic {
				// Ignore empty topics (from old/restart messages)
				if msg.Topic == "" {
					return nil
				}

				log.Printf("Received topic request: %s", msg.Topic)
				orchestrator.mu.Lock()
				hasActiveDebate := orchestrator.state != nil
				currentTopic := ""
				if hasActiveDebate {
					currentTopic = orchestrator.state.Topic
				}
				orchestrator.mu.Unlock()

				// If we have an active debate with the same topic, ignore duplicate
				if hasActiveDebate && currentTopic == msg.Topic {
					log.Printf("Ignoring duplicate topic request for active debate: %s", msg.Topic)
					return nil
				}

				// If we have an active debate with a different topic, reset first
				if hasActiveDebate {
					log.Println("Stopping ongoing debate and starting new one")
					orchestrator.mu.Lock()
					orchestrator.state = nil
					orchestrator.currentRound = 0
					orchestrator.mu.Unlock()
				}

				// Start the new debate
				maxRounds := msg.MaxRounds
				if maxRounds == 0 {
					maxRounds = 5 // Default to 5
				}
				return orchestrator.StartDebate(msg.Topic, maxRounds)
			}

			return nil
		})
		if err != nil {
			log.Printf("Failed to create topic consumer: %v", err)
			return
		}
		defer consumer.Close()

		if err := consumer.Consume(TopicRequest); err != nil {
			log.Printf("Topic consumer error: %v", err)
		}
	}()

	log.Println("Orchestrator ready. Waiting for topic from web interface...")

	// Keep running
	select {}
}
