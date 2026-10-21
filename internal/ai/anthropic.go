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
	System      []anthropicBlock   `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Temperature *float64           `json:"temperature,omitempty"`
}

// anthropicMessage's Content is a string, or []anthropicBlock with images.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text,omitempty"`
	Source       *anthropicSource `json:"source,omitempty"`
	CacheControl *anthropicCache  `json:"cache_control,omitempty"`
}

// anthropicCache marks a prompt-cache breakpoint: everything up to and
// including the block is written to the cache and read back on the next
// call with the same prefix.
type anthropicCache struct {
	Type string `json:"type"`
}

var ephemeral = &anthropicCache{Type: "ephemeral"}

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
	Usage      struct {
		Input      int `json:"input_tokens"`
		Output     int `json:"output_tokens"`
		CacheRead  int `json:"cache_read_input_tokens"`
		CacheWrite int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (c *anthropicClient) auth(req *http.Request) {
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

// Generate sends one user turn under the system prompt and returns the text
// blocks of the reply.
func (c *anthropicClient) Generate(ctx context.Context, system, user string) (string, error) {
	r, err := c.Complete(ctx, Text(system, user))
	return r.Text, err
}

// GenerateImages is Generate with PNGs as base64 image blocks before the text.
func (c *anthropicClient) GenerateImages(ctx context.Context, system, user string, pngs [][]byte) (string, error) {
	r, err := c.Complete(ctx, Request{System: []Block{{Text: system}}, User: []Block{{Text: user}}, Images: pngs})
	return r.Text, err
}

// Complete sends the blocks: the system blocks with a cache breakpoint on
// the last cached one, then the user turn as its cached blocks (breakpoint
// on the last), the images and the rest. Two of the four breakpoints the
// API allows; the prefix is read back on every call that repeats it.
func (c *anthropicClient) Complete(ctx context.Context, req Request) (Response, error) {
	var system []anthropicBlock
	sysCached, sysRest := splitCached(req.System)
	for i, b := range sysCached {
		blk := anthropicBlock{Type: "text", Text: b.Text}
		if i == len(sysCached)-1 {
			blk.CacheControl = ephemeral
		}
		system = append(system, blk)
	}
	for _, b := range sysRest {
		if b.Text != "" {
			system = append(system, anthropicBlock{Type: "text", Text: b.Text})
		}
	}
	var content []anthropicBlock
	userCached, userRest := splitCached(req.User)
	for i, b := range userCached {
		blk := anthropicBlock{Type: "text", Text: b.Text}
		if i == len(userCached)-1 {
			blk.CacheControl = ephemeral
		}
		content = append(content, blk)
	}
	for _, p := range req.Images {
		content = append(content, anthropicBlock{Type: "image", Source: &anthropicSource{
			Type: "base64", MediaType: "image/png", Data: base64.StdEncoding.EncodeToString(p)}})
	}
	for _, b := range userRest {
		if b.Text != "" {
			content = append(content, anthropicBlock{Type: "text", Text: b.Text})
		}
	}
	if len(content) == 0 {
		content = []anthropicBlock{{Type: "text", Text: ""}}
	}
	payload, err := json.Marshal(anthropicRequest{
		Model:       c.model,
		MaxTokens:   c.maxTokens,
		System:      system,
		Messages:    []anthropicMessage{{Role: "user", Content: content}},
		Temperature: c.temperature,
	})
	if err != nil {
		return Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	c.auth(hreq)
	body, err := c.do(hreq)
	if err != nil {
		return Response{}, err
	}
	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Response{}, fmt.Errorf("ai: decode response: %w", err)
	}
	var text string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	usage := Usage{Input: resp.Usage.Input, Output: resp.Usage.Output, CacheRead: resp.Usage.CacheRead, CacheWrite: resp.Usage.CacheWrite}
	logUsage(c.model, usage)
	out, err := answer(stripReasoning(text), resp.StopReason == "max_tokens")
	return Response{Text: out, Usage: usage}, err
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
