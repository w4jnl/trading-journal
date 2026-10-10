package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/livedata"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// The editors submit review labels as repeated checkbox fields; a save
// replaces the set, the read view shows them as pills and the picker
// re-checks them.
func TestJournalLabelsEditorRoundTrip(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	save := func(form url.Values) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/journal/2026-10-09", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("date", "2026-10-09")
		rec := httptest.NewRecorder()
		h.JournalSave(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("JournalSave -> %d: %s", rec.Code, rec.Body.String())
		}
	}
	page := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/journal/2026-10-09", nil).WithContext(ctx)
		req.SetPathValue("date", "2026-10-09")
		rec := httptest.NewRecorder()
		h.JournalPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("JournalPage -> %d", rec.Code)
		}
		return rec.Body.String()
	}

	save(url.Values{"body": {"chased it, then moved the stop"}, "mistake": {"chased", "moved-stop"}, "emotion": {"tilted"}})
	entry, err := h.Queries.GetDayJournalEntry(ctx, models.GetDayJournalEntryParams{UserID: &uid, EntryDate: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	m, e := h.entryLabels(ctx, entry.ID)
	if strings.Join(m, ",") != "chased,moved-stop" || strings.Join(e, ",") != "tilted" {
		t.Fatalf("labels after save = %v / %v", m, e)
	}
	body := page()
	for _, want := range []string{
		`label-pill--mistake" title="Mistake">Chased the entry<`,
		`label-pill--mistake" title="Mistake">Moved the stop<`,
		`label-pill--emotion" title="Emotion">Tilted<`,
		`name="mistake" value="chased" checked`,
		`name="emotion" value="tilted" checked`,
		`name="mistake" value="fomo"><span>`, // unselected chip renders unchecked
	} {
		if !strings.Contains(body, want) {
			t.Errorf("day page missing %q", want)
		}
	}

	// Re-saving with a different set replaces it (the editor shows the full set).
	save(url.Values{"body": {"second thoughts"}, "mistake": {"fomo"}})
	m, e = h.entryLabels(ctx, entry.ID)
	if strings.Join(m, ",") != "fomo" || len(e) != 0 {
		t.Fatalf("labels after second save = %v / %v, want fomo and none", m, e)
	}
	if body := page(); strings.Contains(body, `value="chased" checked`) || !strings.Contains(body, `value="fomo" checked`) {
		t.Error("picker did not follow the replaced set")
	}

	// An unknown key is rejected as bad input, not silently dropped.
	req := httptest.NewRequest(http.MethodPost, "/journal/2026-10-09", strings.NewReader(url.Values{"body": {"x"}, "mistake": {"yolo"}}.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("date", "2026-10-09")
	rec := httptest.NewRecorder()
	h.JournalSave(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown label -> %d, want 400", rec.Code)
	}
}

// The API and the MCP tool add labels without ever removing any, the detail
// response carries them, and ownership / vocabulary are enforced.
func TestJournalLabelsAPIAndMCP(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	const other = int64(2)
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (1, ` + itoa64(uid) + `, '2026-10-07', 'mine', 'day')`,
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES (2, ` + itoa64(other) + `, '2026-10-07', 'theirs', 'day')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC, LiveData: livedata.NewNotifier()}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	post := func(id int64, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/journal/"+strconv.FormatInt(id, 10)+"/labels", strings.NewReader(body)).WithContext(ctx)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		h.APIJournalAddLabels(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	labels := func(out map[string]any) string {
		b, _ := json.Marshal(out["labels"])
		return string(b)
	}

	code, out := post(1, `{"mistakes": ["chased"], "emotions": ["calm"]}`)
	if code != http.StatusOK || labels(out) != `{"emotions":["calm"],"mistakes":["chased"]}` {
		t.Fatalf("add -> %d %s", code, labels(out))
	}
	// Additive: a second call keeps what is there; duplicates are ignored.
	code, out = post(1, `{"mistakes": ["chased", "moved-stop"]}`)
	if code != http.StatusOK || labels(out) != `{"emotions":["calm"],"mistakes":["chased","moved-stop"]}` {
		t.Fatalf("second add -> %d %s", code, labels(out))
	}
	if code, out := post(1, `{"mistakes": ["yolo"]}`); code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "yolo") {
		t.Errorf("unknown key -> %d %v, want 400 naming it", code, out)
	}
	if code, _ := post(1, `{}`); code != http.StatusBadRequest {
		t.Errorf("empty body -> %d, want 400", code)
	}
	if code, _ := post(2, `{"mistakes": ["chased"]}`); code != http.StatusNotFound {
		t.Errorf("other user's entry -> %d, want 404", code)
	}
	if code, _ := post(999, `{"mistakes": ["chased"]}`); code != http.StatusNotFound {
		t.Errorf("missing entry -> %d, want 404", code)
	}

	// Detail carries the labels; both kinds are always present.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/journal/1", nil).WithContext(ctx)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	h.APIJournalDetail(rec, req)
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("detail -> %d: %s", rec.Code, rec.Body.String())
	}
	if labels(detail) != `{"emotions":["calm"],"mistakes":["chased","moved-stop"]}` {
		t.Errorf("detail labels = %s", labels(detail))
	}

	// MCP: same semantics through the tool.
	mreq := mcpgo.CallToolRequest{}
	mreq.Params.Name = "add_labels"
	mreq.Params.Arguments = map[string]any{"entryId": float64(1), "emotions": []any{"tilted", " calm "}}
	res, err := h.mcpAddLabels(ctx, mreq)
	if err != nil || res.IsError {
		t.Fatalf("mcp add_labels: err=%v res=%+v", err, res)
	}
	var mout map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &mout); err != nil {
		t.Fatal(err)
	}
	if labels(mout) != `{"emotions":["calm","tilted"],"mistakes":["chased","moved-stop"]}` {
		t.Errorf("mcp labels = %s", labels(mout))
	}
	mreq.Params.Arguments = map[string]any{"entryId": float64(1)}
	if res, _ := h.mcpAddLabels(ctx, mreq); !res.IsError {
		t.Error("mcp add_labels with nothing to add should error")
	}
	mreq.Params.Arguments = map[string]any{"entryId": float64(2), "mistakes": []any{"chased"}}
	if res, _ := h.mcpAddLabels(ctx, mreq); !res.IsError || !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "not found") {
		t.Errorf("mcp add_labels on another user's entry = %+v, want not found", res)
	}

	// Create with labels (API path shared with the MCP create tool).
	creq := httptest.NewRequest(http.MethodPost, "/api/v1/journal", strings.NewReader(`{"scope":"day","date":"2026-10-08","body":"b","mistakes":["revenge"],"emotions":["frustrated"]}`)).WithContext(ctx)
	crec := httptest.NewRecorder()
	h.APIJournalCreate(crec, creq)
	var created map[string]any
	if err := json.Unmarshal(crec.Body.Bytes(), &created); err != nil || crec.Code != http.StatusCreated {
		t.Fatalf("create -> %d: %s", crec.Code, crec.Body.String())
	}
	if labels(created) != `{"emotions":["frustrated"],"mistakes":["revenge"]}` {
		t.Errorf("created labels = %s", labels(created))
	}
}

// A trade with several labels counts in each bucket; trades without one
// land in the placeholder bucket.
func TestBucketByMulti(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	trades := []analyticsTrade{
		{NetPnl: f(-100), Mistakes: []string{"chased", "moved-stop"}},
		{NetPnl: f(-50), Mistakes: []string{"chased"}},
		{NetPnl: f(80)},
	}
	got := bucketByMulti(trades, labelKeys("mistake", func(t analyticsTrade) []string { return t.Mistakes }, "(none logged)"))
	want := map[string][2]float64{"Chased the entry": {2, -150}, "Moved the stop": {1, -100}, "(none logged)": {1, 80}}
	if len(got) != len(want) {
		t.Fatalf("buckets = %+v", got)
	}
	for _, b := range got {
		w, ok := want[b.Key]
		if !ok || float64(b.Trades) != w[0] || b.Net != w[1] {
			t.Errorf("bucket %q = trades %d net %.0f, want %v", b.Key, b.Trades, b.Net, w)
		}
	}
	if got[0].Key != "(none logged)" {
		t.Errorf("sorted by net desc, got %q first", got[0].Key)
	}
}
