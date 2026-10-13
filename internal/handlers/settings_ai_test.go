package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
	"github.com/w4jnl/trading-journal/web"
)

// fakeProvider is an OpenAI-compatible endpoint that accepts one key, answers
// every completion with reply, and lists two models. It records the keys and
// models it was called with.
type fakeProvider struct {
	*httptest.Server
	key    string
	reply  string
	mu     sync.Mutex
	auths  []string
	model  string
	system string // the last completion's system and user messages
	user   string
	calls  int
}

func newFakeProvider(t *testing.T, key, reply string) *fakeProvider {
	t.Helper()
	f := &fakeProvider{key: key, reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		auth := r.Header.Get("Authorization")
		f.auths = append(f.auths, auth)
		w.Header().Set("Content-Type", "application/json")
		if f.key != "" && auth != "Bearer "+f.key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
			return
		}
		if r.URL.Path == "/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"model-b"},{"id":"model-a"},{"id":"text-embedding-x"}]}`))
			return
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.model = body.Model
		for _, m := range body.Messages {
			switch m.Role {
			case "system":
				f.system = m.Content
			case "user":
				f.user = m.Content
			}
		}
		f.calls++
		out, _ := json.Marshal(f.reply)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(out) + `},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auths) == 0 {
		return ""
	}
	return f.auths[len(f.auths)-1]
}

// withPresetURL points a hosted preset at a test server for the test's span.
func withPresetURL(t *testing.T, id, baseURL string) {
	t.Helper()
	for i := range ai.Presets {
		if ai.Presets[i].ID == id {
			old := ai.Presets[i].BaseURL
			ai.Presets[i].BaseURL = baseURL
			t.Cleanup(func() { ai.Presets[i].BaseURL = old })
			return
		}
	}
	t.Fatalf("no preset %q", id)
}

var (
	aiUser  = &auth.User{ID: 7, Username: "seven", DisplayName: "Seven"}
	aiAdmin = &auth.User{ID: 8, Username: "eight", DisplayName: "Eight", IsAdmin: true}
)

// newAIHandlers is a Handlers with per-user AI keys on and no server default.
func newAIHandlers(t *testing.T) (*Handlers, *sql.DB, *models.Queries) {
	t.Helper()
	db := dbtest.Open(t, "aikeys")
	if _, err := db.Exec(`INSERT INTO users (id, username, is_admin) VALUES (7, 'seven', 0), (8, 'eight', 1)`); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.New(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatal(err)
	}
	q := models.New(db)
	h := &Handlers{
		DB: db, Queries: q, Templates: web.TemplatesFS, Timezone: time.UTC,
		AISecrets: box, AIJobs: ai.NewJobManager(nil),
	}
	return h, db, q
}

func aiPost(h http.HandlerFunc, user *auth.User, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.WithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// redirectQuery returns the flash/err of a /settings redirect.
func redirectQuery(t *testing.T, rec *httptest.ResponseRecorder) (flash, errMsg string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("flash"), u.Query().Get("err")
}

// Saving runs a live test call first, then stores the key encrypted with a
// hint of its last four characters.
func TestSettingsAISaveTestsThenEncrypts(t *testing.T) {
	h, _, q := newAIHandlers(t)
	const key = "sk-mistral-0123456789"
	fp := newFakeProvider(t, key, "OK")
	withPresetURL(t, "mistral", fp.URL)

	rec := aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"mistral"}, "model": {"mistral-large-latest"}, "api_key": {key},
	})
	if flash, errMsg := redirectQuery(t, rec); flash != "ai-saved" {
		t.Fatalf("flash=%q err=%q", flash, errMsg)
	}
	if fp.calls != 1 || fp.model != "mistral-large-latest" || fp.lastAuth() != "Bearer "+key {
		t.Errorf("test call: calls=%d model=%q auth=%q", fp.calls, fp.model, fp.lastAuth())
	}
	row, err := q.GetUserAISettings(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.Provider != "mistral" || row.Model != "mistral-large-latest" || row.BaseUrl != "" || row.KeyHint != "6789" || row.LastVerifiedAt == nil {
		t.Errorf("row = %+v", row)
	}
	if bytes.Contains(row.ApiKeyEnc, []byte(key)) {
		t.Error("key stored in plaintext")
	}
	if got, err := h.AISecrets.Open(row.ApiKeyEnc, row.ApiKeyNonce); err != nil || got != key {
		t.Errorf("decrypt = %q, %v", got, err)
	}
}

func TestSettingsAISaveValidation(t *testing.T) {
	h, _, q := newAIHandlers(t)
	ctx := context.Background()
	const key = "sk-good-key-0001"
	fp := newFakeProvider(t, key, "OK")
	withPresetURL(t, "openai", fp.URL)
	withPresetURL(t, "gemini", fp.URL)

	// A key the provider rejects: nothing stored, the provider's reason shown.
	rec := aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"openai"}, "model": {"m1"}, "api_key": {"sk-wrong-key-0001"},
	})
	if _, errMsg := redirectQuery(t, rec); !strings.Contains(errMsg, "nothing was saved") || !strings.Contains(errMsg, "Incorrect API key") {
		t.Errorf("rejected key: err=%q", errMsg)
	}
	if _, err := q.GetUserAISettings(ctx, 7); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row stored after a failed test: %v", err)
	}

	// Missing key, missing model, unknown provider, a model with a space.
	for _, c := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"provider": {"openai"}, "model": {"m1"}}, "paste your API key"},
		{url.Values{"provider": {"openai"}, "api_key": {key}}, "enter a model"},
		{url.Values{"provider": {"bogus"}, "model": {"m1"}, "api_key": {key}}, "choose a provider"},
		{url.Values{"provider": {"openai"}, "model": {"m 1"}, "api_key": {key}}, "model id looks wrong"},
	} {
		_, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiUser, "/settings/ai", c.form))
		if !strings.Contains(errMsg, c.want) {
			t.Errorf("%v: err=%q, want %q", c.form, errMsg, c.want)
		}
	}

	// Saved once; a model change with the key left blank reuses the stored key.
	if flash, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"openai"}, "model": {"m1"}, "api_key": {key},
	})); flash != "ai-saved" {
		t.Fatalf("save: %q", errMsg)
	}
	if flash, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"openai"}, "model": {"m2"},
	})); flash != "ai-saved" {
		t.Fatalf("blank key, same provider: %q", errMsg)
	}
	if fp.lastAuth() != "Bearer "+key || fp.model != "m2" {
		t.Errorf("stored key not reused: auth=%q model=%q", fp.lastAuth(), fp.model)
	}
	// ...but never for a different provider: the key belongs to its endpoint.
	_, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"gemini"}, "model": {"g1"},
	}))
	if !strings.Contains(errMsg, "paste your API key") {
		t.Errorf("blank key, other provider: err=%q", errMsg)
	}
	if row, _ := q.GetUserAISettings(ctx, 7); row.Provider != "openai" || row.Model != "m2" {
		t.Errorf("row changed by a refused save: %+v", row)
	}
}

// Any-URL endpoints would let a user point the server at its own network, so
// only admins get them; an admin's local server may need no key.
func TestSettingsAICustomIsAdminOnly(t *testing.T) {
	h, _, q := newAIHandlers(t)
	fp := newFakeProvider(t, "", "OK")

	_, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiUser, "/settings/ai", url.Values{
		"provider": {"custom"}, "model": {"local"}, "base_url": {fp.URL},
	}))
	if !strings.Contains(errMsg, "admins") || fp.calls != 0 {
		t.Errorf("non-admin custom: err=%q calls=%d", errMsg, fp.calls)
	}
	_, errMsg = redirectQuery(t, aiPost(h.SettingsAISave, aiAdmin, "/settings/ai", url.Values{
		"provider": {"custom"}, "model": {"local"}, "base_url": {"ftp://x"},
	}))
	if !strings.Contains(errMsg, "http(s) URL") {
		t.Errorf("bad url: err=%q", errMsg)
	}
	if flash, errMsg := redirectQuery(t, aiPost(h.SettingsAISave, aiAdmin, "/settings/ai", url.Values{
		"provider": {"custom"}, "model": {"local"}, "base_url": {fp.URL},
	})); flash != "ai-saved" {
		t.Fatalf("admin custom: %q", errMsg)
	}
	row, err := q.GetUserAISettings(context.Background(), 8)
	if err != nil || row.BaseUrl != fp.URL || row.ApiKeyEnc != nil || fp.lastAuth() != "" {
		t.Errorf("row=%+v err=%v auth=%q", row, err, fp.lastAuth())
	}
	// A stored custom row stops working once its owner is not an admin (a
	// demotion, or an admin acting as a regular user).
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 8})
	if _, _, err := h.aiFor(ctx); err == nil || !strings.Contains(err.Error(), "admins") {
		t.Errorf("custom row used by a non-admin: %v", err)
	}
}

func TestSettingsAIModels(t *testing.T) {
	h, _, _ := newAIHandlers(t)
	const key = "sk-list-key-0001"
	fp := newFakeProvider(t, key, "OK")
	withPresetURL(t, "openrouter", fp.URL)

	rec := aiPost(h.SettingsAIModels, aiUser, "/settings/ai/models", url.Values{"provider": {"openrouter"}, "api_key": {key}})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `<option value="model-a">`) || !strings.Contains(body, `<option value="model-b">`) {
		t.Fatalf("models = %d %s", rec.Code, body)
	}
	if strings.Contains(body, "embedding") || !strings.Contains(body, `id="ai-model-list"`) {
		t.Errorf("list not filtered or datalist id lost: %s", body)
	}
	rec = aiPost(h.SettingsAIModels, aiUser, "/settings/ai/models", url.Values{"provider": {"openrouter"}, "api_key": {"sk-wrong-key-01"}})
	if body := rec.Body.String(); !strings.Contains(body, "ai-models-msg--err") || !strings.Contains(body, "rejected the API key") {
		t.Errorf("error not inline: %s", body)
	}
}

// The user's own provider wins over the server default; without either the
// caller gets errNoAIProvider; a key the server can no longer open says so.
func TestAIForResolution(t *testing.T) {
	h, db, q := newAIHandlers(t)
	ctx := auth.WithUser(context.Background(), aiUser)

	if _, _, err := h.aiFor(ctx); !errors.Is(err, errNoAIProvider) {
		t.Errorf("no provider: err = %v", err)
	}
	if h.aiAvailable(ctx) {
		t.Error("aiAvailable without a provider")
	}

	def := &stubAI{out: "default"}
	h.AI, h.AIModel = def, "server-model"
	c, model, err := h.aiFor(ctx)
	if err != nil || c != def || model != "server-model via this server" || !h.aiAvailable(ctx) {
		t.Errorf("default: c=%v model=%q err=%v", c, model, err)
	}

	enc, nonce, _ := h.AISecrets.Seal("sk-own-key-000001")
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{
		UserID: 7, Provider: "anthropic", Model: "claude-sonnet-5-5", ApiKeyEnc: enc, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	c, model, err = h.aiFor(ctx)
	if err != nil || c == def || model != "claude-sonnet-5-5 via Anthropic (Claude)" {
		t.Errorf("own provider: c=%v model=%q err=%v", c, model, err)
	}

	if _, err := db.Exec(`UPDATE user_ai_settings SET api_key_enc = X'00112233' WHERE user_id = 7`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.aiFor(ctx); err == nil || !strings.Contains(err.Error(), "Settings > AI") {
		t.Errorf("undecryptable key: err = %v", err)
	}
}

// A review runs on the user's own provider, not the server default.
func TestAnalyzeEntryUsesOwnProvider(t *testing.T) {
	h, db, q := newAIHandlers(t)
	const key = "sk-review-key-0001"
	fp := newFakeProvider(t, key, "- **Verdict:** disciplined day.\n\n<!-- tj-ai:verdict tag=good strategy=none -->")
	withPresetURL(t, "gemini", fp.URL)
	def := &stubAI{out: "server default review"}
	h.AI = def

	day := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO journal_entries (id, user_id, body, entry_date, scope) VALUES (1, 7, 'Notes.', ?, 'day')`, day); err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithUser(context.Background(), aiUser)
	enc, nonce, _ := h.AISecrets.Seal(key)
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{
		UserID: 7, Provider: "gemini", Model: "gemini-pro", ApiKeyEnc: enc, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.analyzeEntry(ctx, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if def.calls.Load() != 0 || fp.calls != 1 || fp.model != "gemini-pro" || fp.lastAuth() != "Bearer "+key {
		t.Errorf("default calls=%d own calls=%d model=%q auth=%q", def.calls.Load(), fp.calls, fp.model, fp.lastAuth())
	}
	var body string
	if err := db.QueryRow(`SELECT body FROM journal_entries WHERE id = 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "disciplined day") || strings.Contains(body, "tj-ai:verdict") || res.Tag != "good" {
		t.Errorf("body=%q tag=%q", body, res.Tag)
	}
	if !strings.Contains(body, "### AI review — ") || !strings.Contains(body, "\n*gemini-pro via Google Gemini*\n") {
		t.Errorf("review does not name its model: %q", body)
	}
	if !strings.Contains(fp.system, "WHOLE TRADING DAY") || strings.Contains(fp.system, "mistakes=") {
		t.Errorf("day review got the wrong prompt: %.200q", fp.system)
	}
}

// Without a provider the AI routes answer 409 with a pointer to Settings, and
// pages mark <body> so the AI buttons hide; saving a key brings them back.
func TestNoProviderHidesAndRefusesAI(t *testing.T) {
	h, _, q := newAIHandlers(t)

	req := httptest.NewRequest(http.MethodPost, "/ai/entry/1", nil)
	req.SetPathValue("id", "1")
	req = req.WithContext(auth.WithUser(req.Context(), aiUser))
	rec := httptest.NewRecorder()
	h.AIAnalyzeEntry(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Settings > AI") {
		t.Errorf("analyze without provider = %d %q", rec.Code, rec.Body.String())
	}

	page := func() string {
		req := httptest.NewRequest(http.MethodGet, "/settings", nil)
		req = req.WithContext(auth.WithUser(req.Context(), aiUser))
		rec := httptest.NewRecorder()
		h.SettingsPage(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("settings = %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := page()
	if !strings.Contains(body, `<body data-ai="off">`) {
		t.Error(`no data-ai="off" for a user without a provider`)
	}
	if !strings.Contains(body, `id="ai-provider-form"`) || strings.Contains(body, `value="custom"`) {
		t.Error("provider form missing, or custom offered to a non-admin")
	}

	enc, nonce, _ := h.AISecrets.Seal("sk-some-key-00001")
	if err := q.UpsertUserAISettings(context.Background(), models.UpsertUserAISettingsParams{
		UserID: 7, Provider: "openai", Model: "m", ApiKeyEnc: enc, ApiKeyNonce: nonce, KeyHint: "0001",
	}); err != nil {
		t.Fatal(err)
	}
	body = page()
	if strings.Contains(body, `data-ai="off"`) {
		t.Error(`data-ai="off" with a stored provider`)
	}
	if !strings.Contains(body, "…0001") || !strings.Contains(body, "/settings/ai/test") {
		t.Error("stored provider not shown")
	}
}

// A trade review gets the trade brief and the full trade (plan, price path,
// fills), fills the unset tag, strategy and labels the verdict names, says so
// at the foot of the note, and on a re-run leaves what is now set alone.
func TestAnalyzeTradeFillsWhatFits(t *testing.T) {
	h, db, q := newAIHandlers(t)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, 7, 'apex-1', 'motivewave_csv', 'A-1')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, 7, 'ORB Breakout', 'opening range')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl, gross_pnl, commission, planned_stop, planned_risk, mae_pnl, mfe_pnl, mae_points, mfe_points, excursion_tf)
			VALUES (10, 1, 'ESZ6', 'ES', 'long', 2, 6800, 6802, '2026-10-09 14:00:00 +0000 UTC', '2026-10-09 14:12:00 +0000 UTC', 'closed', 190, 200, 10, 6796, 400, 150, 600, 1.5, 6, '1m')`,
		`INSERT INTO executions (id, account_id, broker_exec_id, symbol, symbol_root, side, qty, price, exec_time) VALUES
			(1, 1, 'e1', 'ESZ6', 'ES', 'buy', 2, 6800, '2026-10-09 14:00:00 +0000 UTC'),
			(2, 1, 'e2', 'ESZ6', 'ES', 'sell', 2, 6802, '2026-10-09 14:12:00 +0000 UTC')`,
		`INSERT INTO trade_executions (trade_id, execution_id, role, qty_used) VALUES (10, 1, 'entry', 2), (10, 2, 'exit', 2)`,
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope) VALUES (1, 7, 10, '2026-10-09', 'Felt rushed, jumped in late.', 'trade')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	const key = "sk-trade-key-0001"
	fp := newFakeProvider(t, key, "- **Setup & entry:** late.\n\n<!-- tj-ai:verdict tag=bad strategy=ORB Breakout mistakes=chased, late-exit, made-up emotions=rushed -->")
	withPresetURL(t, "openai", fp.URL)
	ctx := auth.WithUser(context.Background(), aiUser)
	enc, nonce, _ := h.AISecrets.Seal(key)
	if err := q.UpsertUserAISettings(ctx, models.UpsertUserAISettingsParams{
		UserID: 7, Provider: "openai", Model: "gpt-x", ApiKeyEnc: enc, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := h.analyzeEntry(ctx, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "bad" || res.StrategyID != 5 ||
		!reflect.DeepEqual(res.Mistakes, []string{"chased", "late-exit"}) || !reflect.DeepEqual(res.Emotions, []string{"rushed"}) {
		t.Errorf("filled = %+v", res)
	}

	// The model saw the trade brief and the whole trade.
	if !strings.Contains(fp.system, "ONE TRADE") || !strings.Contains(fp.system, "mistakes=<keys|none>") {
		t.Errorf("trade review got the wrong prompt: %.200q", fp.system)
	}
	var sent struct {
		Timezone string `json:"timezone"`
		Entry    struct {
			Trades []aiTrade `json:"trades"`
		} `json:"entry"`
		Vocabulary map[string][]map[string]string `json:"labelVocabulary"`
	}
	if err := json.Unmarshal([]byte(fp.user), &sent); err != nil {
		t.Fatalf("context is not JSON: %v", err)
	}
	if len(sent.Entry.Trades) != 1 {
		t.Fatalf("trades = %+v", sent.Entry.Trades)
	}
	tr := sent.Entry.Trades[0]
	if tr.PlannedStop == nil || *tr.PlannedStop != 6796 || tr.R == nil || *tr.R != 0.475 ||
		tr.HoldMinutes == nil || *tr.HoldMinutes != 12 || tr.ExitEfficiency == nil || len(tr.Fills) != 2 ||
		tr.EntryTime != "2026-10-09 14:00:00" || sent.Timezone != "UTC" || len(sent.Vocabulary["mistakes"]) == 0 {
		t.Errorf("trade as sent = %+v (tz %q)", tr, sent.Timezone)
	}

	labels, _ := h.journalSvc().EntryLabels(ctx, 7, 1)
	if !reflect.DeepEqual(labels["mistake"], []string{"chased", "late-exit"}) || !reflect.DeepEqual(labels["emotion"], []string{"rushed"}) {
		t.Errorf("labels = %v", labels)
	}
	body := func() string {
		var b string
		if err := db.QueryRow(`SELECT body FROM journal_entries WHERE id = 1`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	got := body()
	if !strings.Contains(got, "\n*gpt-x via OpenAI*\n") ||
		!strings.Contains(got, "*Filled in by this review: tag **bad** · strategy **ORB Breakout** · mistakes **Chased the entry, Exited too late** · emotions **Rushed**.*") {
		t.Errorf("note = %q", got)
	}

	// A re-run proposing other labels: everything is set now, nothing changes.
	fp.mu.Lock()
	fp.reply = "- **Verdict:** fine.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=fomo emotions=calm -->"
	fp.mu.Unlock()
	res, err = h.analyzeEntry(ctx, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "" || res.StrategyID != 0 || res.Mistakes != nil || res.Emotions != nil {
		t.Errorf("re-run filled = %+v", res)
	}
	if got := body(); strings.Count(got, "Filled in by this review") != 1 || strings.Count(got, "### AI review") != 2 {
		t.Errorf("re-run note = %q", got)
	}
	if l, _ := h.journalSvc().EntryLabels(ctx, 7, 1); !reflect.DeepEqual(l["mistake"], []string{"chased", "late-exit"}) {
		t.Errorf("labels changed by a re-run: %v", l)
	}
}
