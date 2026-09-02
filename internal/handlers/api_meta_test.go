package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	tj "github.com/w4jnl/trading-journal"
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
		"listStrategies", "getStrategy", "getDailyStats", "getVersion",
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

// TestAPISurfaceDocsInSync guards the documentation-drift class the API has
// already suffered once (three endpoints lived in the mux, index and spec but
// not in API.md): every path in the discovery index must be in the OpenAPI
// spec (the spec's own path aside) AND be mentioned literally in API.md, and
// every spec path must be in the index. Prose can still drift; paths cannot.
func TestAPISurfaceDocsInSync(t *testing.T) {
	var spec struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(apiOpenAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	doc := string(tj.APIDoc)
	indexed := map[string]bool{}
	for _, e := range apiIndexEndpoints {
		p := e["path"]
		indexed[p] = true
		if p == "/api/v1/openapi.json" {
			continue
		}
		if _, ok := spec.Paths[strings.TrimPrefix(p, "/api/v1")]; !ok {
			t.Errorf("index path %s missing from api_openapi.json", p)
		}
		if !strings.Contains(doc, p) {
			t.Errorf("index path %s not documented in API.md", p)
		}
	}
	for p := range spec.Paths {
		if !indexed["/api/v1"+p] {
			t.Errorf("spec path %s missing from the discovery index", p)
		}
	}
}
