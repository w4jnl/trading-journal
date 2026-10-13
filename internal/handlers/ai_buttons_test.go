package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/ai"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/secretbox"
)

// useOwnProvider gives the fixture's user a Gemini key served by a fake
// provider that answers every review with reply.
func useOwnProvider(t *testing.T, h *Handlers, uid int64, reply string) *fakeProvider {
	t.Helper()
	const key = "sk-buttons-key-01"
	fp := newFakeProvider(t, key, reply)
	withPresetURL(t, "gemini", fp.URL)
	box, _ := secretbox.New(bytes.Repeat([]byte{0x22}, 32))
	h.AISecrets, h.AIJobs, h.AIReviews = box, ai.NewJobManager(nil), ai.NewReviews(nil, 2)
	enc, nonce, _ := box.Seal(key)
	if err := h.Queries.UpsertUserAISettings(context.Background(), models.UpsertUserAISettingsParams{
		UserID: uid, Provider: "gemini", Model: "gemini-pro", ApiKeyEnc: enc, ApiKeyNonce: nonce,
	}); err != nil {
		t.Fatal(err)
	}
	return fp
}

func aiButtonMux(h *Handlers) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ai/entry/{id}", h.AIAnalyzeEntry)
	mux.HandleFunc("POST /ai/trade/{id}", h.AIAnalyzeTrade)
	mux.HandleFunc("POST /ai/set/{id}", h.AIAnalyzeSet)
	mux.HandleFunc("POST /ai/note/day/{date}", h.AIAnalyzeDayNote)
	mux.HandleFunc("POST /ai/note/week/{week}", h.AIAnalyzeWeekNote)
	mux.HandleFunc("GET /journal/week/{week}", h.WeekJournalPage)
	mux.HandleFunc("GET /trades/sets/{id}", h.TradeSetDetail)
	mux.HandleFunc("GET /trades/{id}", h.TradeDetail)
	mux.HandleFunc("POST /trades/{id}/journal", h.TradeJournalSave)
	mux.HandleFunc("GET /ai/review/{id}", h.AIReviewStatus)
	return mux
}

// settle waits for the background reviews to land.
func settle(t *testing.T, h *Handlers) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.AIReviews.Wait(ctx); err != nil {
		t.Fatalf("reviews did not finish: %v", err)
	}
}

func serve(ctx context.Context, mux *http.ServeMux, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil).WithContext(ctx)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A day or week with no note yet still offers Analyze: the click creates the
// note and writes the review; a second click on the reviewed note writes a
// fresh review below the first.
func TestAnalyzeNoteCreatesAndReanalyzes(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	fp := useOwnProvider(t, h, uid, "- **Result:** steady.\n\n<!-- tj-ai:verdict tag=good -->")
	mux := aiButtonMux(h)

	// The empty week page already shows the button.
	page := serve(ctx, mux, http.MethodGet, "/journal/week/2026-W41").Body.String()
	if !strings.Contains(page, `hx-post="/ai/note/week/2026-W41"`) || !strings.Contains(page, "✨ Analyze") {
		t.Fatalf("empty week page lacks the Analyze button")
	}

	rec := serve(ctx, mux, http.MethodPost, "/ai/note/week/2026-W41")
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("week analyze = %d %q", rec.Code, rec.Body.String())
	}
	settle(t, h)
	if !strings.Contains(fp.system, "TRADING WEEK") {
		t.Errorf("week note got the wrong brief")
	}
	page = serve(ctx, mux, http.MethodGet, "/journal/week/2026-W41").Body.String()
	if !strings.Contains(page, "✨ Re-analyze") || !strings.Contains(page, "steady.") {
		t.Errorf("reviewed week page: no Re-analyze button or no review")
	}

	countReviews := func(scope string) int {
		var body string
		if err := h.DB.QueryRow(`SELECT body FROM journal_entries WHERE scope = ? AND user_id = ?`, scope, uid).Scan(&body); err != nil {
			t.Fatalf("%s entry: %v", scope, err)
		}
		return strings.Count(body, "### AI review")
	}
	for i := 1; i <= 2; i++ {
		if rec := serve(ctx, mux, http.MethodPost, "/ai/note/day/2026-10-09"); rec.Code != http.StatusOK {
			t.Fatalf("day analyze #%d = %d %q", i, rec.Code, rec.Body.String())
		}
		settle(t, h)
		if n := countReviews("day"); n != i {
			t.Errorf("after click %d the day note holds %d reviews", i, n)
		}
	}
	if !strings.Contains(fp.system, "WHOLE TRADING DAY") {
		t.Errorf("day note got the wrong brief")
	}

	// The existing-entry route re-analyzes too (the group page).
	var weekID int64
	_ = h.DB.QueryRow(`SELECT id FROM journal_entries WHERE scope = 'week'`).Scan(&weekID)
	rec = serve(ctx, mux, http.MethodPost, "/ai/entry/"+itoa64(weekID))
	settle(t, h)
	if rec.Code != http.StatusOK || countReviews("week") != 2 {
		t.Errorf("re-analyze by entry id = %d, reviews %d", rec.Code, countReviews("week"))
	}

	for _, bad := range []string{"/ai/note/week/2026-W99", "/ai/note/day/2026-13-40"} {
		if rec := serve(ctx, mux, http.MethodPost, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
}

// A set review goes to the group note covering every account's leg (created
// on the first click), uses the group brief, and the set sheet shows it.
func TestAnalyzeSetReviewsItsGroupNote(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	fp := useOwnProvider(t, h, uid, "- **Copy consistency:** bravo filled 2 ticks worse.\n\n<!-- tj-ai:verdict tag=neutral strategy=none mistakes=chased emotions=none -->")
	mux := aiButtonMux(h)
	var sid string
	if err := h.DB.QueryRow(`SELECT CAST(id AS TEXT) FROM trade_sets LIMIT 1`).Scan(&sid); err != nil {
		t.Fatal(err)
	}

	sheet := serve(ctx, mux, http.MethodGet, "/trades/sets/"+sid).Body.String()
	if !strings.Contains(sheet, `hx-post="/ai/set/`+sid+`"`) || !strings.Contains(sheet, "✨ Analyze") {
		t.Fatalf("set sheet lacks the Analyze button")
	}

	rec := serve(ctx, mux, http.MethodPost, "/ai/set/"+sid)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "AI review in progress") {
		t.Fatalf("set analyze = %d, not shown under review: %q", rec.Code, rec.Body.String())
	}
	settle(t, h)
	var gid int64
	if err := h.DB.QueryRow(`SELECT id FROM journal_entries WHERE scope = 'group'`).Scan(&gid); err != nil {
		t.Fatal(err)
	}
	rec = serve(ctx, mux, http.MethodGet, "/ai/review/"+itoa64(gid)+"?then=set&set="+sid)
	if rec.Header().Get("HX-Retarget") != "#trade-sheet-content" {
		t.Errorf("landed review does not redraw the sheet: %v", rec.Header())
	}
	sheet = rec.Body.String()
	for _, want := range []string{"✨ AI review added to the group note", "✨ Re-analyze", "bravo filled 2 ticks worse", "Open the group note", "Chased the entry", "gemini-pro via Google Gemini"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("re-rendered set sheet lacks %q", want)
		}
	}
	if !strings.Contains(fp.system, "THIS ITEM IS A GROUP") {
		t.Errorf("set got the wrong brief")
	}
	var links int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entry_trades jt JOIN journal_entries je ON je.id = jt.journal_entry_id WHERE je.scope = 'group' AND jt.trade_id IN (10, 11)`).Scan(&links); err != nil || links != 2 {
		t.Errorf("group note links %d of the set's 2 trades (%v)", links, err)
	}

	// The second click reviews the same note again rather than a new one.
	serve(ctx, mux, http.MethodPost, "/ai/set/"+sid)
	settle(t, h)
	var groups int
	_ = h.DB.QueryRow(`SELECT COUNT(*) FROM journal_entries WHERE scope = 'group'`).Scan(&groups)
	if groups != 1 {
		t.Errorf("group notes = %d, want 1", groups)
	}
	if rec := serve(ctx, mux, http.MethodPost, "/ai/set/99999"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown set = %d", rec.Code)
	}
}

// The trade journal page asks for a reload instead of the sheet swap.
func TestAnalyzeTradeFromPageReloads(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	useOwnProvider(t, h, uid, "- **Exit:** fine.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=none emotions=none -->")
	mux := aiButtonMux(h)
	rec := serve(ctx, mux, http.MethodPost, "/ai/trade/12?page=1")
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "true" || rec.Body.Len() != 0 {
		t.Errorf("page analyze = %d refresh %q body %d", rec.Code, rec.Header().Get("HX-Refresh"), rec.Body.Len())
	}
	settle(t, h)
	if body := serve(ctx, mux, http.MethodGet, "/trades/12").Body.String(); !strings.Contains(body, "✨ Re-analyze") {
		t.Errorf("reviewed trade sheet lacks Re-analyze")
	}
	rec = serve(ctx, mux, http.MethodPost, "/ai/trade/12")
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Refresh") != "" || !strings.Contains(rec.Body.String(), "AI review in progress") {
		t.Errorf("sheet analyze = %d refresh %q", rec.Code, rec.Header().Get("HX-Refresh"))
	}
	settle(t, h)
}

// A review runs on after the request that asked for it is gone; meanwhile
// the note is shown under review everywhere and its editor refuses saves;
// when it lands the sheet redraws with it, and a failure is shown once.
func TestReviewRunsInBackgroundAndLocksTheNote(t *testing.T) {
	h, ctx, uid := setsFixture(t)
	fp := useOwnProvider(t, h, uid, "- **Exit:** patient.\n\n<!-- tj-ai:verdict tag=good strategy=none mistakes=none emotions=none -->")
	gate := make(chan struct{})
	fp.mu.Lock()
	fp.gate = gate
	fp.mu.Unlock()
	mux := aiButtonMux(h)
	mux.HandleFunc("GET /trades", h.TradesPage)
	mux.HandleFunc("GET /journal", h.JournalFeed)

	// The click returns at once; the trader then leaves (the request's
	// context ends) and the review carries on.
	reqCtx, leave := context.WithCancel(ctx)
	rec := serve(reqCtx, mux, http.MethodPost, "/ai/trade/12")
	leave()
	sheet := rec.Body.String()
	var eid int64
	if err := h.DB.QueryRow(`SELECT id FROM journal_entries WHERE trade_id = 12`).Scan(&eid); err != nil {
		t.Fatal(err)
	}
	poll := "/ai/review/" + itoa64(eid) + "?then=trade&amp;trade=12"
	for _, want := range []string{"AI review in progress", "journal-entry--locked", `hx-get="` + poll + `"`} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet under review lacks %q", want)
		}
	}
	if !h.AIReviews.TradePending(12) || !h.AIReviews.EntryPending(eid) {
		t.Fatal("review not pending")
	}
	if rec := serve(ctx, mux, http.MethodGet, "/ai/review/"+itoa64(eid)+"?then=trade&trade=12"); rec.Code != http.StatusNoContent {
		t.Errorf("poll while running = %d, want 204", rec.Code)
	}
	if body := serve(ctx, mux, http.MethodGet, "/trades?view=trades").Body.String(); !strings.Contains(body, `aria-label="AI review in progress"`) {
		t.Error("trades list shows no spinner on the trade under review")
	}
	if body := serve(ctx, mux, http.MethodGet, "/journal").Body.String(); !strings.Contains(body, "ai-pending-pill") {
		t.Error("journal feed shows no spinner on the note under review")
	}
	save := func() int {
		req := httptest.NewRequest(http.MethodPost, "/trades/12/journal", strings.NewReader(url.Values{"body": {"my edit"}}.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := save(); code != http.StatusConflict {
		t.Errorf("manual save during the review = %d, want 409", code)
	}

	close(gate)
	settle(t, h)
	rec = serve(ctx, mux, http.MethodGet, "/ai/review/"+itoa64(eid)+"?then=trade&trade=12")
	if rec.Header().Get("HX-Retarget") != "#trade-sheet-content" || !strings.Contains(rec.Body.String(), "✨ AI review added.") ||
		!strings.Contains(rec.Body.String(), "patient.") || strings.Contains(rec.Body.String(), "journal-entry--locked") {
		t.Errorf("landed review: headers %v body %.300q", rec.Header(), rec.Body.String())
	}
	if code := save(); code != http.StatusOK {
		t.Errorf("manual save after the review = %d", code)
	}

	// A failing review: the sheet says so once.
	fp.mu.Lock()
	fp.gate, fp.status = nil, http.StatusServiceUnavailable
	fp.mu.Unlock()
	serve(ctx, mux, http.MethodPost, "/ai/trade/12")
	settle(t, h)
	if body := serve(ctx, mux, http.MethodGet, "/ai/review/"+itoa64(eid)+"?then=trade&trade=12").Body.String(); !strings.Contains(body, "AI review failed:") || !strings.Contains(body, "overloaded") || strings.Contains(body, "AI review added") {
		t.Errorf("failure not shown: %.300q", body)
	}
	if body := serve(ctx, mux, http.MethodGet, "/trades/12").Body.String(); strings.Contains(body, "AI review failed") {
		t.Error("failure shown twice")
	}
}
