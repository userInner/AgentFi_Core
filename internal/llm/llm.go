// Package llm provides an OpenAI-compatible LLM client with tool_call support.
// It implements the Chat Completions API and supports per-agent parameter
// overrides (model, temperature, max_tokens) with fallback to global defaults.
// Requirements: 4.1
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/agentfi/agentfi-go-backend/pkg/config"
)

// --- Public types ---

// Client defines the interface for communicating with an LLM.
type Client interface {
	// Chat sends a chat completion request. If req.Model is empty the global
	// default is used; likewise for Temperature (0) and MaxTokens (0).
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Message represents a single message in the conversation.
type Message struct {
	Role       string     `json:"role"`                  // "system" | "user" | "assistant" | "tool"
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`  // present when role=assistant
	ToolCallID string     `json:"tool_call_id,omitempty"` // present when role=tool
}

// ToolDefinition describes a tool the LLM may call.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatRequest is the input for a chat completion call.
type ChatRequest struct {
	Model       string           // Agent-level override; empty → global default
	Temperature float64          // 0 → global default
	MaxTokens   int              // 0 → global default
	Messages    []Message
	Tools       []ToolDefinition
}

// ChatResponse is the output of a chat completion call.
type ChatResponse struct {
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Usage     TokenUsage `json:"usage"`
}

// ToolCall represents a tool invocation requested by the LLM.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// TokenUsage tracks token consumption for a single request.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- OpenAI-compatible HTTP client ---

// OpenAIClient implements Client using the OpenAI Chat Completions API.
// It is compatible with any provider that exposes the same endpoint shape
// (OpenAI, Anthropic via proxy, local vLLM, etc.).
type OpenAIClient struct {
	apiURL     string
	apiKey     string
	httpClient *http.Client
	defaults   config.LLMConfig
}

// NewOpenAIClient creates a new OpenAI-compatible LLM client.
func NewOpenAIClient(cfg config.LLMConfig) *OpenAIClient {
	apiURL := cfg.APIURL
	if apiURL == "" {
		apiURL = "https://api.openai.com/v1"
	}
	return &OpenAIClient{
		apiURL: apiURL,
		apiKey: cfg.APIKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		defaults: cfg,
	}
}

// Chat sends a chat completion request to the OpenAI-compatible API.
// Agent-level overrides take precedence; zero values fall back to global config.
func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = c.defaults.Model
	}
	temp := req.Temperature
	if temp == 0 {
		temp = c.defaults.Temperature
	}
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = c.defaults.MaxTokens
	}

	apiReq := openAIRequest{
		Model:       model,
		Temperature: temp,
		MaxTokens:   maxTok,
		Messages:    toAPIMessages(req.Messages),
	}
	if len(req.Tools) > 0 {
		apiReq.Tools = toAPITools(req.Tools)
	}

	start := time.Now()
	slog.Debug("llm: chat request",
		slog.String("model", model),
		slog.Float64("temperature", temp),
		slog.Int("max_tokens", maxTok),
		slog.Int("messages", len(req.Messages)),
		slog.Int("tools", len(req.Tools)),
	)

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("llm: api error",
			slog.Int("status", resp.StatusCode),
			slog.String("body", truncate(string(respBody), 500)),
		)
		return nil, fmt.Errorf("llm: api returned status %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var apiResp openAIResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("llm: unmarshal response: %w", err)
	}

	result := fromAPIResponse(apiResp)

	elapsed := time.Since(start)
	slog.Info("llm: chat response",
		slog.String("model", model),
		slog.Int("duration_ms", int(elapsed.Milliseconds())),
		slog.Int("prompt_tokens", result.Usage.PromptTokens),
		slog.Int("completion_tokens", result.Usage.CompletionTokens),
		slog.Int("tool_calls", len(result.ToolCalls)),
	)

	return result, nil
}

// --- OpenAI API wire types ---

type openAIRequest struct {
	Model       string         `json:"model"`
	Temperature float64        `json:"temperature"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Messages    []apiMessage   `json:"messages"`
	Tools       []apiTool      `json:"tools,omitempty"`
}

type apiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type apiTool struct {
	Type     string          `json:"type"`
	Function apiFunctionDef  `json:"function"`
}

type apiFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type apiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function apiToolCallFunc `json:"function"`
}

type apiToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Message apiMessage `json:"message"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// --- Conversion helpers ---

func toAPIMessages(msgs []Message) []apiMessage {
	out := make([]apiMessage, len(msgs))
	for i, m := range msgs {
		am := apiMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			am.ToolCalls = append(am.ToolCalls, apiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: apiToolCallFunc{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			})
		}
		out[i] = am
	}
	return out
}

func toAPITools(tools []ToolDefinition) []apiTool {
	out := make([]apiTool, len(tools))
	for i, t := range tools {
		out[i] = apiTool{
			Type: "function",
			Function: apiFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}

func fromAPIResponse(resp openAIResponse) *ChatResponse {
	result := &ChatResponse{
		Usage: TokenUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}
	if len(resp.Choices) > 0 {
		msg := resp.Choices[0].Message
		result.Content = msg.Content
		for _, tc := range msg.ToolCalls {
			var args map[string]any
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if args == nil {
				args = map[string]any{}
			}
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
	}
	return result
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
