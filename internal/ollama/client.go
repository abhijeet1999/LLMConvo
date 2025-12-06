package ollama

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	Model   string
	Client  *http.Client
}

type GenerateRequest struct {
	Model   string  `json:"model"`
	Prompt  string  `json:"prompt"`
	System  string  `json:"system,omitempty"`
	Stream  bool    `json:"stream"`
	Options Options `json:"options,omitempty"`
}

type Options struct {
	Temperature float64 `json:"temperature,omitempty"`
	MaxTokens   int     `json:"num_predict,omitempty"`
}

type GenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

func NewClient(baseURL, model string) *Client {
	return &Client{
		BaseURL: baseURL,
		Model:   model,
		Client: &http.Client{
			Timeout: 5 * time.Minute, // Increased timeout for model loading and generation
		},
	}
}

func (c *Client) Generate(prompt, systemPrompt string) (string, error) {
	return c.GenerateWithTokens(prompt, systemPrompt, 50)
}

func (c *Client) GenerateWithTokens(prompt, systemPrompt string, maxTokens int) (string, error) {
	const maxRetries = 3
	const initialTimeout = 30 * time.Second
	const maxTimeout = 5 * time.Minute // Increased from 2 minutes to handle longer contexts

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		// Increase timeout with each retry
		timeout := initialTimeout * time.Duration(1<<uint(attempt))
		if timeout > maxTimeout {
			timeout = maxTimeout
		}

		client := &http.Client{
			Timeout: timeout,
		}

		reqBody := GenerateRequest{
			Model:  c.Model,
			Prompt: prompt,
			System: systemPrompt,
			Stream: false,
			Options: Options{
				Temperature: 0.7,
				MaxTokens:   maxTokens,
			},
		}

		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			return "", fmt.Errorf("failed to marshal request: %w", err)
		}

		startTime := time.Now()
		resp, err := client.Post(
			fmt.Sprintf("%s/api/generate", c.BaseURL),
			"application/json",
			bytes.NewBuffer(jsonData),
		)
		elapsed := time.Since(startTime)

		if err != nil {
			lastErr = fmt.Errorf("attempt %d/%d failed after %v: %w", attempt+1, maxRetries, elapsed, err)
			if attempt < maxRetries-1 {
				// Exponential backoff: 1s, 2s, 4s
				backoff := time.Duration(1<<uint(attempt)) * time.Second
				time.Sleep(backoff)
				continue
			}
			return "", lastErr
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			lastErr = fmt.Errorf("API error: %d - %s", resp.StatusCode, string(body))
			if attempt < maxRetries-1 {
				backoff := time.Duration(1<<uint(attempt)) * time.Second
				time.Sleep(backoff)
				continue
			}
			return "", lastErr
		}

		var genResp GenerateResponse
		if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
			lastErr = fmt.Errorf("failed to decode response: %w", err)
			if attempt < maxRetries-1 {
				backoff := time.Duration(1<<uint(attempt)) * time.Second
				time.Sleep(backoff)
				continue
			}
			return "", lastErr
		}

		// Success - log if it took a while
		if elapsed > 60*time.Second {
			fmt.Printf("⚠️  Model response took %v (retried %d times)\n", elapsed, attempt)
		}

		return genResp.Response, nil
	}

	return "", lastErr
}

// ListModelsResponse represents the response from the /api/tags endpoint
type ListModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// CheckModelAvailability checks if a specific model is available by listing models.
func (c *Client) CheckModelAvailability(model string) bool {
	resp, err := c.Client.Get(fmt.Sprintf("%s/api/tags", c.BaseURL))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var listResp ListModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		return false
	}

	for _, m := range listResp.Models {
		if m.Name == model {
			return true
		}
	}
	return false
}
