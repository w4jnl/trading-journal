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

// The feed shows each entry's labels, filters by them (OR within a kind,
// AND across kinds, "none" for entries without a mistake), counts them in
// the patterns strip, and the API list rows carry them.
func TestJournalFeedLabels(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope) VALUES
			(1, ` + itoa64(uid) + `, '2026-10-07', 'chased it', 'day'),
			(2, ` + itoa64(uid) + `, '2026-10-08', 'moved the stop, tilted', 'day'),
			(3, ` + itoa64(uid) + `, '2026-10-09', 'clean day', 'day')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES
			(1, 'mistake', 'chased'), (2, 'mistake', 'chased'), (2, 'mistake', 'moved-stop'), (2, 'emotion', 'tilted')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	feed := func(qs string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.JournalFeed(rec, httptest.NewRequest(http.MethodGet, "/journal"+qs, nil).WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("feed%s -> %d", qs, rec.Code)
		}
		return rec.Body.String()
	}
	count := func(body string) int { return strings.Count(body, `class="entry-link"`) }

	all := feed("")
	if count(all) != 3 {
		t.Fatalf("unfiltered feed lists %d entries, want 3", count(all))
	}
	for _, want := range []string{
		`label-pill--mistake" title="Mistake">Chased the entry<`,
		`label-pill--emotion" title="Emotion">Tilted<`,
		`Patterns in these <span class="num">3</span> entries`,
		`Chased the entry <span class="num">×2</span>`,
		`Moved the stop <span class="num">×1</span>`,
		`href="/journal?mistake=chased"`,
		`name="mistake" value="none"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("feed missing %q", want)
		}
	}
	if got := feed("?mistake=chased"); count(got) != 2 || !strings.Contains(got, `jf-pattern--active" href="/journal"`) || !strings.Contains(got, `Chased the entry <span class="fchip-x">`) {
		t.Errorf("mistake=chased: %d entries (want 2); active pattern / chip present=%v", count(got), strings.Contains(got, "jf-pattern--active"))
	}
	if got := feed("?mistake=chased&emotion=tilted"); count(got) != 1 || !strings.Contains(got, "moved the stop, tilted") {
		t.Errorf("chased AND tilted: %d entries, want only entry 2", count(got))
	}
	if got := feed("?mistake=moved-stop&mistake=none"); count(got) != 2 || strings.Contains(got, ">chased it<") {
		t.Errorf("moved-stop OR none: %d entries, want entries 2 and 3", count(got))
	}
	if got := feed("?mistake=yolo"); count(got) != 3 {
		t.Errorf("unknown key should be ignored, got %d entries", count(got))
	}
	if qs := exportQueryString(parseJournalFilters(httptest.NewRequest(http.MethodGet, "/journal?emotion=tilted&mistake=none", nil), time.UTC)); qs != "emotion=tilted&mistake=none" {
		t.Errorf("export qs = %q", qs)
	}

	// API list rows carry labels and honour the same filter.
	rec := httptest.NewRecorder()
	h.APIJournalList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/journal?mistake=chased", nil).WithContext(ctx))
	var out struct {
		Items []struct {
			ID     int64               `json:"id"`
			Labels map[string][]string `json:"labels"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("api list -> %d: %s", rec.Code, rec.Body.String())
	}
	if len(out.Items) != 2 || out.Items[0].ID != 2 || strings.Join(out.Items[0].Labels["mistakes"], ",") != "chased,moved-stop" || strings.Join(out.Items[0].Labels["emotions"], ",") != "tilted" {
		t.Errorf("api list = %+v", out.Items)
	}
}

// The exports carry the labels: lines in the markdown, a meta line in the
// PDF, and the label filters in the header's filter description.
func TestJournalExportIncludesLabels(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope, tag) VALUES (1, ` + itoa64(uid) + `, '2026-10-08', 'moved the stop again', 'day', 'bad')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (1, 'mistake', 'chased'), (1, 'mistake', 'moved-stop'), (1, 'emotion', 'tilted')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})

	rec := httptest.NewRecorder()
	h.JournalExportMarkdown(rec, httptest.NewRequest(http.MethodGet, "/journal/export.md?mistake=chased&emotion=tilted", nil).WithContext(ctx))
	md := rec.Body.String()
	for _, want := range []string{
		"- **Tag:** bad\n- **Mistakes:** Chased the entry, Moved the stop\n- **Emotions:** Tilted\n\n",
		"mistake: chased; emotion: tilted",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown export missing %q in:\n%s", want, md)
		}
	}
	rec = httptest.NewRecorder()
	h.JournalExportPDF(rec, httptest.NewRequest(http.MethodGet, "/journal/export.pdf", nil).WithContext(ctx))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Errorf("pdf export -> %d %s (%d bytes)", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}

// A filter pick also lists the entries where the AI review suggests that
// value and the suggestion is open: a tag only while the entry is untagged,
// a strategy only while unassigned, a label unless already carried. A
// dismissed suggestion does not count, and Untagged / Unassigned / None
// logged keep their plain meaning. The REST list follows.
func TestJournalFilterMatchesSuggestions(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := itoa64(uid)
	for _, stmt := range []string{
		`INSERT INTO strategies (id, user_id, name) VALUES (7, ` + u + `, 'Opening drive')`,
		// 1: untagged, bad suggested. 2: tagged good, a stale bad suggestion.
		// 3: bad dismissed. 4: tagged bad. 5: clean.
		`INSERT INTO journal_entries (id, user_id, entry_date, body, scope, tag, strategy_id) VALUES
			(1, ` + u + `, '2026-10-05', 'one', 'trade', NULL, NULL),
			(2, ` + u + `, '2026-10-06', 'two', 'trade', 'good', 7),
			(3, ` + u + `, '2026-10-07', 'three', 'trade', NULL, NULL),
			(4, ` + u + `, '2026-10-08', 'four', 'trade', 'bad', NULL),
			(5, ` + u + `, '2026-10-09', 'five', 'day', NULL, NULL)`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label) VALUES (4, 'mistake', 'chased')`,
		`INSERT INTO journal_suggestions (journal_entry_id, kind, value, status) VALUES
			(1, 'tag', 'bad', 'open'), (1, 'strategy', '7', 'open'), (1, 'mistake', 'chased', 'open'), (1, 'emotion', 'tilted', 'open'),
			(2, 'tag', 'bad', 'open'), (2, 'strategy', '7', 'open'),
			(3, 'tag', 'bad', 'dismissed'), (3, 'mistake', 'chased', 'dismissed')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Templates: web.TemplatesFS, Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid, Username: "boot", DisplayName: "Boot"})
	ids := func(qs string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.APIJournalList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/journal"+qs, nil).WithContext(ctx))
		if rec.Code != http.StatusOK {
			t.Fatalf("list%s -> %d: %s", qs, rec.Code, rec.Body.String())
		}
		var out struct {
			Items []struct {
				ID int64 `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, it := range out.Items {
			got = append(got, strconv.FormatInt(it.ID, 10))
		}
		return strings.Join(got, ",")
	}
	for qs, want := range map[string]string{
		"?tag=bad":                "4,1",   // the tagged one and the untagged one with the open proposal; 2 holds good
		"?tag=untagged":           "5,3,1", // a proposal is not a tag
		"?tag=good":               "2",
		"?strategy=7":             "2,1",
		"?strategy=none":          "5,4,3,1",
		"?mistake=chased":         "4,1",
		"?mistake=none":           "5,3,2,1",
		"?emotion=tilted":         "1",
		"?mistake=chased&tag=bad": "4,1",
		"?mistake=no-stop":        "",
	} {
		if got := ids(qs); got != want {
			t.Errorf("%s = [%s], want [%s]", qs, got, want)
		}
	}

	// The feed lists the suggested-only entry and shows the pick as a proposal.
	rec := httptest.NewRecorder()
	h.JournalFeed(rec, httptest.NewRequest(http.MethodGet, "/journal?tag=bad", nil).WithContext(ctx))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Count(body, `class="entry-link"`) != 2 || !strings.Contains(body, ">one<") || !strings.Contains(body, "the AI review suggests it") {
		t.Errorf("feed tag=bad -> %d, %d entries, has one=%v", rec.Code, strings.Count(body, `class="entry-link"`), strings.Contains(body, ">one<"))
	}
}
