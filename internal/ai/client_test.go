package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAIClientGenerate(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotXTitle, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotXTitle = r.Header.Get("X-Title")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  Hello analysis.  "}}]}`))
	}))
	defer srv.Close()

	c, err := New(Options{
		Provider: "openai", BaseURL: srv.URL + "/v1", Model: "test-model",
		APIKey: "sk-abc", Timeout: 5 * time.Second, MaxTokens: 256, Temperature: 0.3,
		Headers: map[string]string{"X-Title": "trading-journal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Generate(context.Background(), "SYS", "USR")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hello analysis." {
		t.Errorf("content = %q, want trimmed 'Hello analysis.'", out)
	}
	if gotMethod != "POST" || gotPath != "/v1/chat/completions" {
		t.Errorf("request = %s %s, want POST /v1/chat/completions", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sk-abc" {
		t.Errorf("Authorization = %q, want Bearer sk-abc", gotAuth)
	}
	if gotXTitle != "trading-journal" {
		t.Errorf("extra header X-Title = %q", gotXTitle)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v", gotBody["model"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v (want system+user)", gotBody["messages"])
	}
	m0, _ := msgs[0].(map[string]any)
	m1, _ := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "SYS" || m1["role"] != "user" || m1["content"] != "USR" {
		t.Errorf("messages = %v", msgs)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Options{Provider: "openai", Model: "m"}); err == nil {
		t.Error("want error for missing base_url")
	}
	if _, err := New(Options{Provider: "openai", BaseURL: "http://x/v1"}); err == nil {
		t.Error("want error for missing model")
	}
	if _, err := New(Options{Provider: "anthropic", BaseURL: "http://x", Model: "m"}); err == nil {
		t.Error("want error for unknown provider")
	}
}

func TestStripReasoning(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<think>Okay, let's tackle this. First I'll parse the JSON.</think>\n\n### AI review\n\nClean entry.", "### AI review\n\nClean entry."},
		{"<THINK>x</THINK>Answer", "Answer"},             // case-insensitive
		{"<thinking>y\nmultiline</thinking>\nA", "A"},    // <thinking> variant + newlines
		{"intro <think>mid</think> tail", "intro  tail"}, // mid-string
		{"No reasoning here.", "No reasoning here."},     // untouched
	}
	for _, c := range cases {
		if got := stripReasoning(c.in); got != c.want {
			t.Errorf("stripReasoning(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The client strips a reasoning block from a real response and keeps the answer.
func TestGenerateStripsReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"<think>\nOkay, let's tackle this review.\n</think>\n\n### AI review\n\nDisciplined entry.\n\n<!-- tj-ai:verdict tag=good strategy=none -->"}}]}`))
	}))
	defer srv.Close()
	c, _ := New(Options{Provider: "openai", BaseURL: srv.URL, Model: "m"})
	out, err := c.Generate(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Okay, let's") || strings.Contains(out, "<think>") {
		t.Errorf("reasoning not stripped: %q", out)
	}
	if !strings.HasPrefix(out, "### AI review") || !strings.Contains(out, "tj-ai:verdict") {
		t.Errorf("answer (incl. verdict line) not preserved: %q", out)
	}
}

func TestGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()
	c, _ := New(Options{Provider: "openai", BaseURL: srv.URL, Model: "m"})
	if _, err := c.Generate(context.Background(), "s", "u"); err == nil {
		t.Error("want error on HTTP 500")
	}
}
