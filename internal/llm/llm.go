// Package llm proxies chat requests to admin-configured LLM providers.
// Drydock never exposes provider API keys to the browser; the backend
// attaches card context (task description, step plan, recent messages) and
// forwards to the provider's chat API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// ChatRequest is what the backend sends when an agent needs an LLM turn.
type ChatRequest struct {
	CardID     string    `json:"card_id"`
	ProviderID string    `json:"provider_id"`
	Messages   []Message `json:"messages"`
	CardContext string   `json:"card_context,omitempty"`
}

// ChatResponse is what Drydock returns.
type ChatResponse struct {
	Response string `json:"response"`
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

// SystemPrompt builds the system prompt with card context for an agent that
// is working (or verifying) a task card.
func SystemPrompt(cardTitle, cardBody, stepPlan string) string {
	var sb strings.Builder
	sb.WriteString("You are an AI coding agent working a Drydock task card. ")
	sb.WriteString("Work the task to completion inside the card's container. ")
	sb.WriteString("If you are blocked or need a decision, ask a concise question and wait. ")
	sb.WriteString("Be practical and report what you did.")
	sb.WriteString("\n\nTask: " + cardTitle)
	if cardBody != "" {
		sb.WriteString("\n\nDetails:\n" + cardBody)
	}
	if stepPlan != "" {
		sb.WriteString("\n\nPlan:\n" + stepPlan)
	}
	return sb.String()
}

// Chat sends the conversation to the provider and returns its reply.
func Chat(ctx context.Context, providerType, endpoint, apiKey, model, systemPrompt string, messages []Message, screenshotB64 string) (string, error) {
	switch providerType {
	case "anthropic":
		return anthropicChat(ctx, endpoint, apiKey, model, systemPrompt, messages, screenshotB64)
	default: // "openai" and OpenAI-compatible
		return openAIChat(ctx, endpoint, apiKey, model, systemPrompt, messages, screenshotB64)
	}
}

func defaultEndpoint(providerType, endpoint string) string {
	if endpoint != "" {
		return strings.TrimSuffix(endpoint, "/")
	}
	if providerType == "anthropic" {
		return "https://api.anthropic.com"
	}
	return "https://api.openai.com/v1"
}

// openAIChat uses the OpenAI chat completions format (also works with
// OpenAI-compatible endpoints like Ollama, Together, etc.).
func openAIChat(ctx context.Context, endpoint, apiKey, model, systemPrompt string, messages []Message, screenshotB64 string) (string, error) {
	type contentPart struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	type chatMsg struct {
		Role    string        `json:"role"`
		Content []contentPart `json:"content"`
	}
	msgs := []chatMsg{{Role: "system", Content: []contentPart{{Type: "text", Text: systemPrompt}}}}
	for _, m := range messages {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		cm := chatMsg{Role: role, Content: []contentPart{{Type: "text", Text: m.Content}}}
		// Attach the screenshot to the last user message.
		if screenshotB64 != "" && role == "user" && m == messages[len(messages)-1] {
			cm.Content = append(cm.Content, contentPart{
				Type:     "image_url",
				ImageURL: &struct {
					URL string `json:"url"`
				}{URL: "data:image/png;base64," + screenshotB64},
			})
		}
		msgs = append(msgs, cm)
	}
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": msgs,
	})
	url := defaultEndpoint("openai", endpoint) + "/chat/completions"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm error %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("llm decode: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// anthropicChat uses the Anthropic messages format.
func anthropicChat(ctx context.Context, endpoint, apiKey, model, systemPrompt string, messages []Message, screenshotB64 string) (string, error) {
	type contentBlock struct {
		Type   string `json:"type"`
		Text   string `json:"text,omitempty"`
		Source *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source,omitempty"`
	}
	type apiMsg struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	var msgs []apiMsg
	for _, m := range messages {
		role := m.Role
		if role != "assistant" {
			role = "user"
		}
		am := apiMsg{Role: role, Content: []contentBlock{{Type: "text", Text: m.Content}}}
		if screenshotB64 != "" && role == "user" && m == messages[len(messages)-1] {
			am.Content = append(am.Content, contentBlock{
				Type: "image",
				Source: &struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				}{Type: "base64", MediaType: "image/png", Data: screenshotB64},
			})
		}
		msgs = append(msgs, am)
	}
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"system":     systemPrompt,
		"messages":   msgs,
		"max_tokens": 4096,
	})
	url := defaultEndpoint("anthropic", endpoint) + "/v1/messages"
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("llm error %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("llm decode: %w", err)
	}
	var sb strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
