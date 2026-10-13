// Package ai is the in-app analysis engine: it calls an OpenAI-compatible
// chat-completions endpoint to generate trading-journal analyses server-side (the
// inward counterpart to the MCP server, which lets external AIs do the same).
//
// Two dialects cover every provider: the OpenAI /chat/completions format (a local
// MLX server, OpenAI, Gemini, Mistral, OpenRouter, LM Studio, vLLM, ...) and
// Anthropic's /messages. The server-wide default comes from config; each user may
// bring their own provider key instead (see Preset). No model tool-calling is
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
	"sort"
	"strings"
	"time"
)

// Options configures a Client. main.go maps config.AI onto this so this package
// never imports internal/config (mirrors marketdata / proxy-auth options).
type Options struct {
	Provider    string // "openai" (OpenAI-compatible, the default) or "anthropic"
	BaseURL     string
	Model       string
	APIKey      string
	Timeout     time.Duration
	MaxTokens   int
	Temperature float64
	Headers     map[string]string
	// OmitTemperature leaves the sampling temperature to the provider: several
	// hosted reasoning models reject any value but their default.
	OmitTemperature bool
	// CompletionTokens sends the output budget as max_completion_tokens, the
	// only name OpenAI's reasoning models accept.
	CompletionTokens bool
}

// Client turns a system+user prompt into a single completion.
type Client interface {
	Generate(ctx context.Context, system, user string) (string, error)
}

// ModelLister is implemented by every Client New returns: it lists the model
// ids the key can use, to suggest in Settings.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// New builds a Client from opts.
func New(opts Options) (Client, error) {
	if opts.Model == "" {
		return nil, fmt.Errorf("ai: model is required")
	}
	return newClient(opts)
}

// ListModels lists the models opts' key can use (opts.Model may be empty).
func ListModels(ctx context.Context, opts Options) ([]string, error) {
	c, err := newClient(opts)
	if err != nil {
		return nil, err
	}
	return c.(ModelLister).ListModels(ctx)
}

func newClient(opts Options) (Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("ai: base_url is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	var temperature *float64
	if !opts.OmitTemperature {
		t := opts.Temperature
		temperature = &t
	}
	base := baseClient{
		baseURL:     strings.TrimRight(opts.BaseURL, "/"),
		model:       opts.Model,
		apiKey:      opts.APIKey,
		maxTokens:   maxTokens,
		temperature: temperature,
		headers:     opts.Headers,
		http:        &http.Client{Timeout: timeout},
	}
	switch opts.Provider {
	case "", "openai":
		return &openAIClient{baseClient: base, completionTokens: opts.CompletionTokens}, nil
	case "anthropic":
		return &anthropicClient{baseClient: base}, nil
	default:
		return nil, fmt.Errorf("ai: unknown provider %q (want \"openai\" or \"anthropic\")", opts.Provider)
	}
}

// baseClient holds what both dialects share.
type baseClient struct {
	baseURL     string
	model       string
	apiKey      string
	maxTokens   int
	temperature *float64 // nil = the provider's default
	headers     map[string]string
	http        *http.Client
}

// do sends req (the dialect has set its auth headers) and returns the body of
// a 200. Any other status becomes an error carrying the provider's own message.
func (c *baseClient) do(req *http.Request) ([]byte, error) {
	req.Header.Set("Accept", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		msg := providerError(body)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("ai: the provider rejected the API key (status %d): %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("ai: status %d: %s", resp.StatusCode, msg)
	}
	return body, nil
}

// providerError pulls the human message out of an error body. OpenAI-style and
// Anthropic bodies carry {"error":{"message":...}}, Gemini's OpenAI layer wraps
// that object in an array, Mistral answers {"detail":...}. Anything else is
// returned trimmed and capped.
func providerError(body []byte) string {
	var one struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail any `json:"detail"`
	}
	if json.Unmarshal(body, &one) == nil {
		if one.Error != nil && one.Error.Message != "" {
			return one.Error.Message
		}
		if d, ok := one.Detail.(string); ok && d != "" {
			return d
		}
	}
	var many []struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &many) == nil && len(many) > 0 && many[0].Error != nil && many[0].Error.Message != "" {
		return many[0].Error.Message
	}
	s := strings.TrimSpace(string(body))
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

type openAIClient struct {
	baseClient
	completionTokens bool
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	Stream              bool          `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			// A string for most providers; an array of typed parts for some
			// (Mistral's reasoning models put a "thinking" part before the text).
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// contentText flattens a chat message content: a plain string, or the "text"
// parts of a typed-part array (thinking parts are dropped).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Generate POSTs a single-turn chat completion and returns the assistant content.
func (c *openAIClient) Generate(ctx context.Context, system, user string) (string, error) {
	cr := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: c.temperature,
		Stream:      false,
	}
	if c.completionTokens {
		cr.MaxCompletionTokens = c.maxTokens
	} else {
		cr.MaxTokens = c.maxTokens
	}
	payload, err := json.Marshal(cr)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	body, err := c.do(req)
	if err != nil {
		return "", err
	}
	var resp chatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("ai: decode response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", fmt.Errorf("ai: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("ai: empty response (no choices)")
	}
	ch := resp.Choices[0]
	return answer(stripReasoning(contentText(ch.Message.Content)), ch.FinishReason == "length")
}

// answer turns a finished completion into the result: an empty one is an error,
// with the likely cause when the model ran out of output budget.
func answer(text string, truncated bool) (string, error) {
	if text != "" {
		return text, nil
	}
	if truncated {
		return "", fmt.Errorf("ai: the model used its whole output budget before answering (a reasoning model thinking at length?)")
	}
	return "", fmt.Errorf("ai: the model returned an empty answer")
}

// ListModels lists the endpoint's models (GET /models), minus the ones that
// cannot write a review.
func (c *openAIClient) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return modelIDs(body)
}

// modelIDs reads the {"data":[{"id":...}]} list both dialects return, drops
// models that are not text generators, and sorts the rest. Gemini's ids carry a
// "models/" prefix that its chat endpoint does not want.
func modelIDs(body []byte) ([]string, error) {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("ai: decode model list: %w", err)
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		id := strings.TrimPrefix(m.ID, "models/")
		if id == "" || seen[id] || !textModel(id) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// nonTextModel lists id fragments of models that embed, transcribe, speak,
// moderate or draw rather than write.
var nonTextModel = []string{
	"embed", "tts", "whisper", "transcribe", "audio", "realtime", "speech",
	"dall-e", "image", "imagen", "veo", "sora", "lyria",
	"moderation", "ocr", "aqa", "babbage", "davinci",
}

func textModel(id string) bool {
	l := strings.ToLower(id)
	for _, f := range nonTextModel {
		if strings.Contains(l, f) {
			return false
		}
	}
	return true
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
