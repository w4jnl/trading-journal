package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// TestAPIOpenAPIServerURL verifies that the served OpenAPI spec carries an ABSOLUTE
// server URL derived from the request (ChatGPT Actions require one) and that every
// operation has an operationId. No DB/auth needed - APIOpenAPI only reads the
// embedded spec + the request.
func TestAPIOpenAPIServerURL(t *testing.T) {
	h := &Handlers{}
	fetch := func(host, xfProto string) map[string]any {
		req := httptest.NewRequest("GET", "/api/v1/openapi.json", nil)
		req.Host = host
		if xfProto != "" {
			req.Header.Set("X-Forwarded-Proto", xfProto)
		}
		rec := httptest.NewRecorder()
		h.APIOpenAPI(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code = %d", rec.Code)
		}
		var d map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("invalid JSON served: %v", err)
		}
		return d
	}
	serverURL := func(d map[string]any) string {
		return d["servers"].([]any)[0].(map[string]any)["url"].(string)
	}

	if u := serverURL(fetch("tradingtest.example.com", "https")); u != "https://tradingtest.example.com/api/v1" {
		t.Errorf("behind-proxy server URL = %q, want https://tradingtest.example.com/api/v1", u)
	}
	if u := serverURL(fetch("127.0.0.1:8080", "")); u != "http://127.0.0.1:8080/api/v1" {
		t.Errorf("plain-host server URL = %q, want http://127.0.0.1:8080/api/v1", u)
	}

	// Every operation carries an operationId; spot-check the key ones.
	d := fetch("h", "")
	got := map[string]bool{}
	for _, p := range d["paths"].(map[string]any) {
		for _, op := range p.(map[string]any) {
			if id, ok := op.(map[string]any)["operationId"].(string); ok && id != "" {
				got[id] = true
			}
		}
	}
	for _, want := range []string{
		"prepareDay", "listTrades", "getTrade", "listJournal", "getJournalEntry",
		"createJournalEntry", "appendNote", "assignStrategy", "setTag",
		"listStrategies", "getStrategy",
	} {
		if !got[want] {
			t.Errorf("missing operationId %q", want)
		}
	}

	// ChatGPT Actions accept exactly one security scheme.
	schemes := d["components"].(map[string]any)["securitySchemes"].(map[string]any)
	if len(schemes) != 1 {
		t.Errorf("want exactly 1 security scheme, got %d (%v)", len(schemes), schemes)
	}
}
