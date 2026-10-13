package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// jsonServer answers every request with status and body, recording the last
// request's path, headers and decoded JSON body.
type jsonServer struct {
	*httptest.Server
	path, query string
	header      http.Header
	body        map[string]any
}

func newJSONServer(t *testing.T, status int, reply string) *jsonServer {
	t.Helper()
	s := &jsonServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path, s.query, s.header = r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		s.body = nil
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &s.body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(s.Close)
	return s
}

// A hosted preset leaves the temperature to the provider and, for OpenAI,
// sends the budget as max_completion_tokens.
func TestPresetOptionsShapeTheRequest(t *testing.T) {
	srv := newJSONServer(t, 200, `{"choices":[{"message":{"content":"Review."}}]}`)
	p, _ := PresetByID("openai")
	p.BaseURL = srv.URL
	c, err := New(p.Options("gpt-x", "sk-user"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.body["temperature"]; ok {
		t.Errorf("temperature sent: %v", srv.body["temperature"])
	}
	if _, ok := srv.body["max_tokens"]; ok {
		t.Error("max_tokens sent for OpenAI")
	}
	if srv.body["max_completion_tokens"] != float64(userMaxTokens) {
		t.Errorf("max_completion_tokens = %v", srv.body["max_completion_tokens"])
	}
	if got := srv.header.Get("Authorization"); got != "Bearer sk-user" {
		t.Errorf("Authorization = %q", got)
	}

	// The other OpenAI-compatible presets keep max_tokens.
	g, _ := PresetByID("mistral")
	g.BaseURL = srv.URL
	c, _ = New(g.Options("m", "k"))
	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if srv.body["max_tokens"] != float64(userMaxTokens) {
		t.Errorf("mistral max_tokens = %v", srv.body["max_tokens"])
	}
}

func TestPresetsAreHostedHTTPS(t *testing.T) {
	ids := map[string]bool{}
	for _, p := range Presets {
		if ids[p.ID] {
			t.Errorf("duplicate preset %q", p.ID)
		}
		ids[p.ID] = true
		if !strings.HasPrefix(p.BaseURL, "https://") || !strings.HasPrefix(p.KeyURL, "https://") {
			t.Errorf("%s: base %q / key page %q must be https", p.ID, p.BaseURL, p.KeyURL)
		}
		if p.Dialect != "openai" && p.Dialect != "anthropic" {
			t.Errorf("%s: dialect %q", p.ID, p.Dialect)
		}
		if p.ID == ProviderCustom {
			t.Error("custom must not be a hosted preset")
		}
	}
	if _, ok := PresetByID("nope"); ok {
		t.Error("PresetByID found an unknown id")
	}
}

// Mistral's reasoning models answer with typed parts; only the text is kept.
func TestContentPartsKeepOnlyText(t *testing.T) {
	srv := newJSONServer(t, 200, `{"choices":[{"message":{"content":[
		{"type":"thinking","thinking":[{"type":"text","text":"hmm"}]},
		{"type":"text","text":"Part one. "},{"type":"text","text":"Part two."}]}}]}`)
	c, _ := New(Options{BaseURL: srv.URL, Model: "magistral"})
	out, err := c.Generate(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Part one. Part two." {
		t.Errorf("out = %q", out)
	}
}

func TestEmptyAnswerIsAnError(t *testing.T) {
	srv := newJSONServer(t, 200, `{"choices":[{"message":{"content":""},"finish_reason":"length"}]}`)
	c, _ := New(Options{BaseURL: srv.URL, Model: "m"})
	if _, err := c.Generate(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "output budget") {
		t.Errorf("err = %v, want the output-budget explanation", err)
	}
	srv2 := newJSONServer(t, 200, `{"choices":[{"message":{"content":"<think>only thoughts</think>"},"finish_reason":"stop"}]}`)
	c, _ = New(Options{BaseURL: srv2.URL, Model: "m"})
	if _, err := c.Generate(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "empty answer") {
		t.Errorf("err = %v, want empty answer", err)
	}
}

func TestAnthropicGenerate(t *testing.T) {
	srv := newJSONServer(t, 200, `{"content":[{"type":"text","text":"  Claude review. "},{"type":"text","text":"More."}],"stop_reason":"end_turn"}`)
	p, _ := PresetByID("anthropic")
	p.BaseURL = srv.URL + "/v1"
	c, err := New(p.Options("claude-sonnet-5-5", "sk-ant"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Generate(context.Background(), "SYS", "USR")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Claude review. More." {
		t.Errorf("out = %q", out)
	}
	if srv.path != "/v1/messages" {
		t.Errorf("path = %q", srv.path)
	}
	if srv.header.Get("x-api-key") != "sk-ant" || srv.header.Get("anthropic-version") != anthropicVersion {
		t.Errorf("headers = %v", srv.header)
	}
	if srv.header.Get("Authorization") != "" {
		t.Error("Anthropic request carried a bearer header")
	}
	if srv.body["system"] != "SYS" || srv.body["model"] != "claude-sonnet-5-5" || srv.body["max_tokens"] != float64(userMaxTokens) {
		t.Errorf("body = %v", srv.body)
	}
	if _, ok := srv.body["temperature"]; ok {
		t.Error("temperature sent")
	}
	msgs, _ := srv.body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" || msgs[0].(map[string]any)["content"] != "USR" {
		t.Errorf("messages = %v", srv.body["messages"])
	}
}

func TestAnthropicTruncatedAndRejected(t *testing.T) {
	srv := newJSONServer(t, 200, `{"content":[],"stop_reason":"max_tokens"}`)
	c, _ := New(Options{Provider: "anthropic", BaseURL: srv.URL, Model: "m"})
	if _, err := c.Generate(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "output budget") {
		t.Errorf("err = %v", err)
	}
	bad := newJSONServer(t, 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	c, _ = New(Options{Provider: "anthropic", BaseURL: bad.URL, Model: "m"})
	_, err := c.Generate(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("err = %v", err)
	}
}

func TestListModels(t *testing.T) {
	srv := newJSONServer(t, 200, `{"data":[{"id":"models/gemini-pro"},{"id":"gpt-b"},{"id":"text-embedding-3-large"},
		{"id":"whisper-1"},{"id":"gpt-a"},{"id":"gpt-image-1"},{"id":"gpt-a"},{"id":"omni-moderation-latest"}]}`)
	got, err := ListModels(context.Background(), Options{BaseURL: srv.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gemini-pro", "gpt-a", "gpt-b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
	if srv.path != "/v1/models" || srv.header.Get("Authorization") != "Bearer k" {
		t.Errorf("request = %s auth %q", srv.path, srv.header.Get("Authorization"))
	}

	ant := newJSONServer(t, 200, `{"data":[{"id":"claude-sonnet-5-5"},{"id":"claude-haiku-5-5"}],"has_more":false}`)
	got, err = ListModels(context.Background(), Options{Provider: "anthropic", BaseURL: ant.URL, APIKey: "sk-ant"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude-haiku-5-5", "claude-sonnet-5-5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("anthropic models = %v", got)
	}
	if ant.path != "/models" || ant.query != "limit=1000" || ant.header.Get("x-api-key") != "sk-ant" {
		t.Errorf("anthropic request = %s?%s", ant.path, ant.query)
	}
}

func TestProviderError(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error":{"message":"Incorrect API key provided: sk-...abcd."}}`, "Incorrect API key provided: sk-...abcd."},
		{`[{"error":{"code":400,"message":"Please pass a valid API key","status":"INVALID_ARGUMENT"}}]`, "Please pass a valid API key"},
		{`{"detail":"Invalid API Key"}`, "Invalid API Key"},
		{`  plain text  `, "plain text"},
	}
	for _, c := range cases {
		if got := providerError([]byte(c.body)); got != c.want {
			t.Errorf("providerError(%s) = %q, want %q", c.body, got, c.want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := providerError([]byte(long)); len([]rune(got)) != 301 {
		t.Errorf("long body not capped: %d runes", len([]rune(got)))
	}
}

// Images ride on the user turn: data URLs in the OpenAI format, base64
// blocks before the text for Anthropic. A rejection is a *StatusError.
func TestGenerateImages(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	srv := newJSONServer(t, 200, `{"choices":[{"message":{"content":"seen"}}]}`)
	c, _ := New(Options{BaseURL: srv.URL, Model: "vision"})
	out, err := c.(ImageGenerator).GenerateImages(context.Background(), "s", "look", [][]byte{png})
	if err != nil || out != "seen" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	msgs := srv.body["messages"].([]any)
	parts, ok := msgs[1].(map[string]any)["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %v", msgs[1])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || !strings.HasPrefix(img["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,iVBORw") {
		t.Errorf("image part = %v", img)
	}
	if msgs[0].(map[string]any)["content"] != "s" {
		t.Error("system turn changed shape")
	}

	ant := newJSONServer(t, 200, `{"content":[{"type":"text","text":"seen"}],"stop_reason":"end_turn"}`)
	c, _ = New(Options{Provider: "anthropic", BaseURL: ant.URL, Model: "claude"})
	if _, err := c.(ImageGenerator).GenerateImages(context.Background(), "s", "look", [][]byte{png}); err != nil {
		t.Fatal(err)
	}
	blocks := ant.body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	b0, b1 := blocks[0].(map[string]any), blocks[1].(map[string]any)
	if b0["type"] != "image" || b0["source"].(map[string]any)["media_type"] != "image/png" || b1["type"] != "text" || b1["text"] != "look" {
		t.Errorf("anthropic blocks = %v", blocks)
	}

	bad := newJSONServer(t, 400, `{"error":{"message":"this model does not support images"}}`)
	c, _ = New(Options{BaseURL: bad.URL, Model: "text-only"})
	_, err = c.(ImageGenerator).GenerateImages(context.Background(), "s", "look", [][]byte{png})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 || !strings.Contains(se.Msg, "does not support images") {
		t.Errorf("err = %v", err)
	}
}
