package ai

import (
	"context"
	"log/slog"
	"strings"
)

// ---- Structured requests ------------------------------------------------
//
// A prompt is blocks: the system blocks and the user blocks, each flagged
// when a provider may place a cache breakpoint after it. The stable material
// (the method, the vocabulary, the playbooks, the trader's profile) goes
// first and cached; the item under review follows. Anthropic takes the
// hints as cache_control; every OpenAI-compatible dialect concatenates the
// blocks in order, which is all its automatic prefix caching needs.

// Block is one piece of a prompt.
type Block struct {
	Text  string
	Cache bool // a cache breakpoint may follow this block
}

// Request is one model call.
type Request struct {
	System []Block
	User   []Block
	Images [][]byte // PNGs, placed after the cached user blocks
}

// Usage is what the provider reported about the call, zero where it says
// nothing.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Response is the model's answer.
type Response struct {
	Text  string
	Usage Usage
}

// Requester is a client that takes structured requests; every client New
// returns is one. A test double may implement Client alone.
type Requester interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Text is the simple request: one system string, one user string.
func Text(system, user string) Request {
	return Request{System: []Block{{Text: system}}, User: []Block{{Text: user}}}
}

// Cached is a request whose system prompt is stable across calls.
func Cached(system, user string) Request {
	return Request{System: []Block{{Text: system, Cache: true}}, User: []Block{{Text: user}}}
}

// Do sends the request through the client: Complete when it offers it, else
// the blocks flattened into Generate or GenerateImages.
func Do(ctx context.Context, c Client, req Request) (Response, error) {
	if r, ok := c.(Requester); ok {
		return r.Complete(ctx, req)
	}
	system, user := joinBlocks(req.System), joinBlocks(req.User)
	if ig, ok := c.(ImageGenerator); ok && len(req.Images) > 0 {
		out, err := ig.GenerateImages(ctx, system, user, req.Images)
		return Response{Text: out}, err
	}
	out, err := c.Generate(ctx, system, user)
	return Response{Text: out}, err
}

// joinBlocks is the blocks as one text, a blank line between.
func joinBlocks(bs []Block) string {
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// splitCached is the cached prefix and the rest, in order.
func splitCached(bs []Block) (cached, rest []Block) {
	last := -1
	for i, b := range bs {
		if b.Cache {
			last = i
		}
	}
	if last < 0 {
		return nil, bs
	}
	return bs[:last+1], bs[last+1:]
}

func logUsage(model string, u Usage) {
	slog.Debug("ai usage", "model", model, "input", u.Input, "output", u.Output, "cacheRead", u.CacheRead, "cacheWrite", u.CacheWrite)
}
