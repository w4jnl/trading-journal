// Package ai is the in-app analysis engine: it calls an OpenAI-compatible
// chat-completions endpoint to generate trading-journal analyses server-side (the
// inward counterpart to the MCP server, which lets external AIs do the same).
//
// One client speaks the OpenAI /chat/completions format, which covers a local MLX
// server, OpenAI, OpenRouter and other aggregators, LM Studio, vLLM, and similar -
// switchable purely by base URL + model + key in config. No model tool-calling is
// required: the caller (internal/handlers) gathers the trade/chart/strategy context
// and asks for the analysis text, then writes it.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Options configures a Client. main.go maps config.AI onto this so this package
// never imports internal/config (mirrors marketdata / proxy-auth options).
type Options struct {
	Provider    string
	BaseURL     string
	Model       string
	APIKey      string
	Timeout     time.Duration
	MaxTokens   int
	Temperature float64
	Headers     map[string]string
}

// Client turns a system+user prompt into a single completion.
type Client interface {
	Generate(ctx context.Context, system, user string) (string, error)
}

// New builds a Client from opts. v1 supports only the OpenAI-compatible provider.
func New(opts Options) (Client, error) {
	switch opts.Provider {
	case "", "openai":
		if opts.BaseURL == "" {
			return nil, fmt.Errorf("ai: base_url is required")
		}
		if opts.Model == "" {
			return nil, fmt.Errorf("ai: model is required")
		}
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		maxTokens := opts.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 1024
		}
		return &openAIClient{
			baseURL:     strings.TrimRight(opts.BaseURL, "/"),
			model:       opts.Model,
			apiKey:      opts.APIKey,
			maxTokens:   maxTokens,
			temperature: opts.Temperature,
			headers:     opts.Headers,
			http:        &http.Client{Timeout: timeout},
		}, nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q (only \"openai\" is supported)", opts.Provider)
	}
}

type openAIClient struct {
	baseURL     string
	model       string
	apiKey      string
	maxTokens   int
	temperature float64
	headers     map[string]string
	http        *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate POSTs a single-turn chat completion and returns the assistant content.
func (c *openAIClient) Generate(ctx context.Context, system, user string) (string, error) {
	payload, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens:   c.maxTokens,
		Temperature: c.temperature,
		Stream:      false,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ai request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ai: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("ai: decode response: %w", err)
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("ai: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("ai: empty response (no choices)")
	}
	return stripReasoning(cr.Choices[0].Message.Content), nil
}

// reasoningBlockRe matches the <think>…</think> / <thinking>…</thinking> blocks
// that reasoning models (DeepSeek-R1, Qwen3, QwQ, …) emit before their answer.
// Many OpenAI-compatible servers leave them in message.content, where they would
// otherwise render as visible prose in the journal.
var (
	reasoningBlockRe = regexp.MustCompile(`(?is)<think(?:ing)?>.*?</think(?:ing)?>`)
	strayThinkTagRe  = regexp.MustCompile(`(?i)</?think(?:ing)?>`)
)

// stripReasoning drops closed reasoning blocks and any stray lone think tags, then
// trims. A model that emits chain-of-thought with NO tags can't be cleaned here -
// disable "thinking" at the model/server instead (e.g. Qwen3 enable_thinking=false).
func stripReasoning(s string) string {
	s = reasoningBlockRe.ReplaceAllString(s, "")
	s = strayThinkTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
