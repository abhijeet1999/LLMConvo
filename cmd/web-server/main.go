package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"llmconvo/internal/kafka"
	"llmconvo/internal/models"

	"github.com/gorilla/websocket"
)

const (
	TopicConversation = "conversation-log"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins in development
	},
}

type WebServer struct {
	clients     map[*websocket.Conn]bool
	clientsMu   sync.RWMutex
	consumer    *kafka.Consumer
	debateState *models.DebateState
	stateMu     sync.RWMutex
	models      map[string]string // Track which model each service is using: "persona1" -> "gemma:2b"
	modelsMu    sync.RWMutex
}

func NewWebServer() (*WebServer, error) {
	ws := &WebServer{
		clients:     make(map[*websocket.Conn]bool),
		debateState: &models.DebateState{Messages: []models.Message{}},
		models:      make(map[string]string),
	}

	consumer, err := kafka.NewConsumer([]string{"localhost:19092"}, ws.handleMessage)
	if err != nil {
		return nil, err
	}
	ws.consumer = consumer

	return ws, nil
}

func (ws *WebServer) handleMessage(data []byte) error {
	var msg models.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to unmarshal message: %v", err)
		return err
	}

	log.Printf("Web server received message: type=%s, topic=%s, round=%d", msg.Type, msg.Topic, msg.Round)

	ws.stateMu.Lock()
	if msg.Type == models.MessageTypeTopic {
		ws.debateState.Topic = msg.Topic
		ws.debateState.Starter = msg.Starter
		ws.debateState.Messages = []models.Message{}
		ws.debateState.CurrentRound = 0
		ws.debateState.Winner = ""
		ws.debateState.JudgeReason = ""
	} else if msg.Type == models.MessageTypeArgument {
		ws.debateState.Messages = append(ws.debateState.Messages, msg)
		ws.debateState.CurrentRound = msg.Round
		// Track model being used by this persona
		if msg.Model != "" && (msg.Persona == "persona1" || msg.Persona == "persona2") {
			ws.modelsMu.Lock()
			ws.models[msg.Persona] = msg.Model
			ws.modelsMu.Unlock()
		}
	} else if msg.Type == models.MessageTypeDebateEnd {
		ws.debateState.Winner = msg.Winner
		ws.debateState.JudgeReason = msg.Content
		// Track judge model
		if msg.Model != "" {
			ws.modelsMu.Lock()
			ws.models["judge"] = msg.Model
			ws.modelsMu.Unlock()
		}
	} else if msg.Type == models.MessageTypeThinking {
		// Track model from thinking messages too (they include model info)
		if msg.Model != "" {
			ws.modelsMu.Lock()
			if msg.Persona == "judge" {
				ws.models["judge"] = msg.Model
			} else if msg.Persona == "persona1" || msg.Persona == "persona2" {
				ws.models[msg.Persona] = msg.Model
			}
			ws.modelsMu.Unlock()
		}
	} else if msg.Type == models.MessageTypeRestart {
		// Reset UI state on restart message
		ws.debateState = &models.DebateState{Messages: []models.Message{}}
		ws.modelsMu.Lock()
		ws.models = make(map[string]string)
		ws.modelsMu.Unlock()
	}
	ws.stateMu.Unlock()

	// Broadcast to all connected clients
	ws.broadcast(msg)

	return nil
}

func (ws *WebServer) broadcast(msg models.Message) {
	ws.clientsMu.RLock()
	clientCount := len(ws.clients)
	ws.clientsMu.RUnlock()

	if clientCount == 0 {
		log.Printf("No WebSocket clients connected, skipping broadcast")
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message: %v", err)
		return
	}

	ws.clientsMu.RLock()
	defer ws.clientsMu.RUnlock()

	for client := range ws.clients {
		if err := client.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("Failed to send message to client: %v", err)
			delete(ws.clients, client)
			client.Close()
		}
	}
	log.Printf("Broadcasted message type=%s to %d clients", msg.Type, len(ws.clients))
}

func (ws *WebServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	ws.clientsMu.Lock()
	ws.clients[conn] = true
	clientCount := len(ws.clients)
	ws.clientsMu.Unlock()

	log.Printf("WebSocket client connected. Total clients: %d", clientCount)

	// Send current state to new client
	ws.stateMu.RLock()
	stateData, err := json.Marshal(ws.debateState)
	ws.stateMu.RUnlock()

	if err != nil {
		log.Printf("Failed to marshal state: %v", err)
	} else {
		if err := conn.WriteMessage(websocket.TextMessage, stateData); err != nil {
			log.Printf("Failed to send initial state: %v", err)
		} else {
			log.Printf("Sent initial state to new client")
		}
	}

	// Keep connection alive
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket client disconnected: %v", err)
			ws.clientsMu.Lock()
			delete(ws.clients, conn)
			ws.clientsMu.Unlock()
			break
		}
	}
}

func (ws *WebServer) checkModelsReady() (bool, string) {
	ollamaURL := "http://localhost:11434"
	// Only need ONE model to be ready - all services will use the same model
	primaryModel := "gemma:2b" // Smallest, downloads first

	// Try to get model list from Ollama
	resp, err := http.Get(fmt.Sprintf("%s/api/tags", ollamaURL))
	if err != nil {
		return false, "Ollama not responding"
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, "Ollama API error"
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "Failed to read model list"
	}

	var modelsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return false, "Failed to parse model list"
	}

	// Check which models are available
	availableModels := make(map[string]bool)
	for _, model := range modelsResp.Models {
		availableModels[model.Name] = true
	}

	// Only need the primary model to be ready
	if !availableModels[primaryModel] {
		return false, fmt.Sprintf("Waiting for %s to download...", primaryModel)
	}

	return true, "Primary model ready (all services will use it)"
}

func (ws *WebServer) handleStartDebate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Topic string `json:"topic"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if req.Topic == "" {
		http.Error(w, "Topic is required", http.StatusBadRequest)
		return
	}

	// Health check: Verify models are loaded before starting debate
	ready, message := ws.checkModelsReady()
	if !ready {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "models_not_ready",
			"message": message,
		})
		return
	}

	// Send topic to orchestrator via Kafka
	producer, err := kafka.NewProducer([]string{"localhost:19092"})
	if err != nil {
		http.Error(w, "Failed to connect to message broker", http.StatusInternalServerError)
		return
	}
	defer producer.Close()

	topicMsg := models.Message{
		Type:      models.MessageTypeTopic,
		Topic:     req.Topic,
		Timestamp: time.Now(),
	}

	if err := producer.SendMessage("debate-request", topicMsg); err != nil {
		http.Error(w, "Failed to start debate", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "debate_started", "topic": req.Topic})
}

func (ws *WebServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Send restart signal to orchestrator via Kafka
	producer, err := kafka.NewProducer([]string{"localhost:19092"})
	if err != nil {
		http.Error(w, "Failed to connect to message broker", http.StatusInternalServerError)
		return
	}
	defer producer.Close()

	restartMsg := models.Message{
		Type:      models.MessageTypeRestart,
		Timestamp: time.Now(),
	}

	if err := producer.SendMessage("debate-request", restartMsg); err != nil {
		log.Printf("Failed to send restart message: %v", err)
		// Continue anyway
	}

	// Reset web server state
	ws.stateMu.Lock()
	ws.debateState = &models.DebateState{Messages: []models.Message{}}
	ws.stateMu.Unlock()

	// Clear Kafka topics
	topics := []string{
		"debate-request",
		"persona1-response",
		"persona2-response",
		"judge-response",
		"conversation-log",
	}

	if err := kafka.DeleteTopics([]string{"localhost:19092"}, topics); err != nil {
		log.Printf("Warning: Failed to delete some topics: %v", err)
		// Continue anyway - topics will be recreated on next use
	}

	// Broadcast reset message to clients
	resetMsg := models.Message{
		Type:      models.MessageTypeRestart,
		Timestamp: time.Now(),
	}
	ws.broadcast(resetMsg)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "restarted"})
}

func (ws *WebServer) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check Ollama for model availability - prioritize gemma:2b (downloads first)
	ollamaURL := "http://localhost:11434"
	primaryModel := "gemma:2b"                                         // Downloads first, all services use this
	requiredModels := []string{"gemma:2b", "phi3:mini", "llama3.2:3b"} // Others download in background

	status := map[string]interface{}{
		"models":      make(map[string]bool),
		"downloading": false,
		"ready":       true,
	}

	// Try to get model list from Ollama
	resp, err := http.Get(fmt.Sprintf("%s/api/tags", ollamaURL))
	if err != nil {
		// Ollama might not be ready yet
		status["downloading"] = true
		status["ready"] = false
		status["message"] = "Connecting to Ollama..."
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		status["downloading"] = true
		status["ready"] = false
		status["message"] = "Ollama is starting up..."
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		status["downloading"] = true
		status["ready"] = false
		status["message"] = "Checking models..."
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
		return
	}

	var modelsResp struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	if err := json.Unmarshal(body, &modelsResp); err != nil {
		status["downloading"] = true
		status["ready"] = false
		status["message"] = "Parsing model list..."
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
		return
	}

	// Check which models are available
	availableModels := make(map[string]bool)
	for _, model := range modelsResp.Models {
		// Model name might be "model:tag" format
		modelName := model.Name
		if strings.Contains(modelName, ":") {
			availableModels[modelName] = true
		}
	}

	modelsStatus := make(map[string]interface{})
	allReady := true
	downloading := false
	readyCount := 0

	for _, model := range requiredModels {
		if availableModels[model] {
			modelsStatus[model] = map[string]interface{}{
				"available": true,
				"status":    "Ready",
			}
			readyCount++
		} else {
			modelsStatus[model] = map[string]interface{}{
				"available": false,
				"status":    "Downloading...",
			}
			allReady = false
			downloading = true
		}
	}

	// Calculate progress percentage
	progress := int((float64(readyCount) / float64(len(requiredModels))) * 100)

	status["models"] = modelsStatus
	status["downloading"] = downloading
	status["ready"] = allReady
	status["progress"] = progress
	status["readyModels"] = readyCount
	status["totalModels"] = len(requiredModels)

	// Check if primary model (gemma:2b) is ready - that's all we need!
	primaryReady := availableModels[primaryModel]

	if primaryReady {
		// Primary model ready - system can start!
		status["ready"] = true
		status["downloading"] = downloading // Keep downloading status if other models are still downloading
		if downloading {
			missing := []string{}
			for model, modelInfo := range modelsStatus {
				if info, ok := modelInfo.(map[string]interface{}); ok {
					if !info["available"].(bool) {
						missing = append(missing, model)
					}
				}
			}
			status["message"] = fmt.Sprintf("✅ Primary model ready! System can start. (Background: downloading %s - %d%% complete)", strings.Join(missing, ", "), progress)
		} else {
			status["message"] = "✅ All models ready!"
		}
	} else if downloading {
		missing := []string{}
		for model, modelInfo := range modelsStatus {
			if info, ok := modelInfo.(map[string]interface{}); ok {
				if !info["available"].(bool) {
					missing = append(missing, model)
				}
			}
		}
		status["message"] = fmt.Sprintf("Downloading primary model: %s (%d%% complete - %d/%d ready)", primaryModel, progress, readyCount, len(requiredModels))
	} else {
		status["message"] = "All models ready!"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (ws *WebServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ws.modelsMu.RLock()
	// If we have models from messages, use those
	persona1Model := ws.models["persona1"]
	persona2Model := ws.models["persona2"]
	judgeModel := ws.models["judge"]
	ws.modelsMu.RUnlock()

	// If models are empty, query Ollama to determine which models services are using
	if persona1Model == "" || persona2Model == "" || judgeModel == "" {
		ollamaURL := "http://localhost:11434"
		resp, err := http.Get(fmt.Sprintf("%s/api/tags", ollamaURL))
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				var modelsResp struct {
					Models []struct {
						Name string `json:"name"`
					} `json:"models"`
				}
				if json.Unmarshal(body, &modelsResp) == nil {
					// Check which models are available
					availableModels := make(map[string]bool)
					for _, m := range modelsResp.Models {
						if strings.Contains(m.Name, ":") {
							availableModels[m.Name] = true
						}
					}

					// Check if all 3 models are available
					allModels := []string{"gemma:2b", "phi3:mini", "llama3.2:3b"}
					allAvailable := true
					for _, model := range allModels {
						if !availableModels[model] {
							allAvailable = false
							break
						}
					}

					// Assign models based on availability (same logic as services)
					if allAvailable {
						if persona1Model == "" {
							persona1Model = "gemma:2b"
						}
						if persona2Model == "" {
							persona2Model = "phi3:mini"
						}
						if judgeModel == "" {
							judgeModel = "llama3.2:3b"
						}
					} else {
						// Use first available model for all
						for _, model := range allModels {
							if availableModels[model] {
								if persona1Model == "" {
									persona1Model = model
								}
								if persona2Model == "" {
									persona2Model = model
								}
								if judgeModel == "" {
									judgeModel = model
								}
								break
							}
						}
					}
				}
			}
		}
	}

	response := map[string]string{
		"persona1": persona1Model,
		"persona2": persona2Model,
		"judge":    judgeModel,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (ws *WebServer) Start() error {
	http.HandleFunc("/ws", ws.handleWebSocket)
	http.HandleFunc("/api/start-debate", ws.handleStartDebate)
	http.HandleFunc("/api/restart", ws.handleRestart)
	http.HandleFunc("/api/model-status", ws.handleModelStatus)
	http.HandleFunc("/api/models", ws.handleModels)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/index.html")
	})

	go func() {
		// Consumer.Consume now handles reconnection internally
		if err := ws.consumer.Consume(TopicConversation); err != nil {
			log.Printf("Consumer error: %v", err)
		}
	}()

	log.Println("Web server starting on :8080")
	return http.ListenAndServe(":8080", nil)
}

func main() {
	server, err := NewWebServer()
	if err != nil {
		log.Fatalf("Failed to create web server: %v", err)
	}
	defer server.consumer.Close()

	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
