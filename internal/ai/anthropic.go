package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

// anthropicVersion is the Messages API version header every request carries.
const anthropicVersion = "2023-06-01"

// anthropicClient speaks Anthropic's Messages API (POST /messages).
type anthropicClient struct {
	baseClient
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
}

// anthropicMessage's Content is a string, or []anthropicBlock with images.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthropicSource `json:"source,omitempty"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
}

func (c *anthropicClient) auth(req *http.Request) {
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

// Generate sends one user turn under the system prompt and returns the text
// blocks of the reply.
func (c *anthropicClient) Generate(ctx context.Context, system, user string) (string, error) {
	return c.GenerateImages(ctx, system, user, nil)
}

// GenerateImages is Generate with PNGs as base64 image blocks before the text.
func (c *anthropicClient) GenerateImages(ctx context.Context, system, user string, pngs [][]byte) (string, error) {
	var content any = user
	if len(pngs) > 0 {
		blocks := make([]anthropicBlock, 0, len(pngs)+1)
		for _, p := range pngs {
			blocks = append(blocks, anthropicBlock{Type: "image", Source: &anthropicSource{
				Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(p)}})
		}
		content = append(blocks, anthropicBlock{Type: "text", Text: user})
	}
	payload, err := json.Marshal(anthropicRequest{
		Model:       c.model,
		MaxTokens:   c.maxTokens,
		System:      system,
		Messages:    []anthropicMessage{{Role: "user", Content: content}},
		Temperature: c.temperature,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	body, err := c.do(req)
	if err != nil {
		return "", err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("ai: decode response: %w", err)
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return answer(stripReasoning(text), resp.StopReason == "max_tokens")
}

// ListModels lists the models the key can use (GET /models, one page of up to
// 1000, far more than Anthropic offers).
func (c *anthropicClient) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models?limit=1000", nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return modelIDs(body)
}
