package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/gex"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

// gexExpoPost sends a form POST through the same mux as gexExpoGet.
func gexExpoPost(t *testing.T, h *Handlers, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /gex/viewstate", h.GEXViewStateSave)
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, Username: "tester", DisplayName: "Tester"})
	req := httptest.NewRequest("POST", target, strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestGEXViewStateSave(t *testing.T) {
	db := gexTestDB(t)
	c, _ := gex.New(gex.Options{BaseURL: "http://gex-engine:8420"})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}
	pref := func(page string) string {
		v, _ := h.Queries.GetViewPref(context.Background(), models.GetViewPrefParams{UserID: 7, Page: page})
		return v
	}

	// A filtered exposure state: view is dropped (a filter forces the
	// blend slice), the URL is rebuilt canonically server-side.
	rec := gexExpoPost(t, h, "/gex/viewstate", url.Values{
		"page": {"gex_exposure"}, "market": {"NQ"},
		"expiry": {"2026-08-22,2026-08-25"}, "bucket": {"25"}, "view": {"0dte"},
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	if got := pref("gex_exposure"); got != "/gex/exposure/NQ?bucket=25&expiry=2026-08-22%2C2026-08-25" {
		t.Errorf("stored = %q", got)
	}

	// Overwrite with an unfiltered 0DTE view.
	gexExpoPost(t, h, "/gex/viewstate", url.Values{
		"page": {"gex_exposure"}, "market": {"ES"}, "view": {"0dte"},
	})
	if got := pref("gex_exposure"); got != "/gex/exposure/ES?view=0dte" {
		t.Errorf("overwrite = %q", got)
	}

	// Overview stores the market only, whatever else is sent.
	gexExpoPost(t, h, "/gex/viewstate", url.Values{
		"page": {"gex_overview"}, "market": {"NQ"}, "bucket": {"25"},
	})
	if got := pref("gex_overview"); got != "/gex/NQ" {
		t.Errorf("overview = %q", got)
	}

	// Junk 400s and never touches the stored rows.
	for name, form := range map[string]url.Values{
		"page":   {"page": {"dashboard"}, "market": {"ES"}},
		"market": {"page": {"gex_exposure"}, "market": {"CL"}},
		"expiry": {"page": {"gex_exposure"}, "market": {"ES"}, "expiry": {"zzz"}},
		"bucket": {"page": {"gex_exposure"}, "market": {"ES"}, "bucket": {"nope"}},
		"view":   {"page": {"gex_exposure"}, "market": {"ES"}, "view": {"etf"}},
	} {
		if rec := gexExpoPost(t, h, "/gex/viewstate", form); rec.Code != http.StatusBadRequest {
			t.Errorf("bad %s = %d", name, rec.Code)
		}
	}
	if got := pref("gex_exposure"); got != "/gex/exposure/ES?view=0dte" {
		t.Errorf("junk clobbered the stored row: %q", got)
	}

	// Feature off: 404 like the rest of the gex surface.
	hOff := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC}
	if rec := gexExpoPost(t, hOff, "/gex/viewstate", url.Values{"page": {"gex_overview"}, "market": {"ES"}}); rec.Code != http.StatusNotFound {
		t.Errorf("nil GEX = %d", rec.Code)
	}
}

// Bare entry paths 303 to the stored view; explicit paths and query params
// always win; garbage prefs are ignored.
func TestGEXStickyRedirect(t *testing.T) {
	db := gexTestDB(t)
	srv, _ := gexExpoEngineV070(t)
	defer srv.Close()
	c, _ := gex.New(gex.Options{BaseURL: srv.URL, APIKey: "test"})
	h := &Handlers{DB: db, Queries: models.New(db), Templates: web.TemplatesFS, Timezone: time.UTC, GEX: c}
	seed := func(page, value string) {
		t.Helper()
		if err := h.Queries.UpsertViewPref(context.Background(), models.UpsertViewPrefParams{
			UserID: 7, Page: page, Value: value, UpdatedAt: "2026-08-21T10:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// No pref: bare paths serve the ES default, no redirect.
	if rec := gexExpoGet(t, h, "/gex/exposure"); rec.Code != http.StatusOK {
		t.Fatalf("no pref = %d", rec.Code)
	}

	seed("gex_exposure", "/gex/exposure/NQ?bucket=20&expiry=2026-08-22")
	seed("gex_overview", "/gex/NQ")

	rec := gexExpoGet(t, h, "/gex/exposure")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/gex/exposure/NQ?bucket=20&expiry=2026-08-22" {
		t.Errorf("bare exposure = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = gexExpoGet(t, h, "/gex")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/gex/NQ" {
		t.Errorf("bare overview = %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	// Explicit market or any query wins over the stored view.
	if rec := gexExpoGet(t, h, "/gex/exposure/ES"); rec.Code != http.StatusOK {
		t.Errorf("explicit market = %d", rec.Code)
	}
	if rec := gexExpoGet(t, h, "/gex/exposure?expiry=2026-08-21"); rec.Code != http.StatusOK {
		t.Errorf("query present = %d", rec.Code)
	}
	if rec := gexExpoGet(t, h, "/gex/ES"); rec.Code != http.StatusOK {
		t.Errorf("explicit overview = %d", rec.Code)
	}

	// Garbage or off-allowlist stored values are ignored, never redirected
	// to (defense in depth for rows that predate a format change).
	for _, bad := range []string{
		"https://evil.example/phish",
		"/dashboard",
		"/gex/exposure/CL",
		"/gex/exposure/NQ?bogus=1",
		"/gex/exposure/NQ?expiry=zzz",
		"::: not a url :::",
	} {
		seed("gex_exposure", bad)
		if rec := gexExpoGet(t, h, "/gex/exposure"); rec.Code != http.StatusOK {
			t.Errorf("stored %q -> %d, want 200", bad, rec.Code)
		}
	}
}

// The canonical builder and the stored-value validator agree with each other.
func TestGexCanonicalViewURL(t *testing.T) {
	for _, tc := range []struct {
		page, market, view string
		exps               []string
		bucket             int
		want               string
	}{
		{gexPrefOverview, "NQ", "0dte", []string{"2026-08-22"}, 25, "/gex/NQ"}, // overview: market only
		{gexPrefExposure, "ES", "", nil, 0, "/gex/exposure/ES"},
		{gexPrefExposure, "ES", "0dte_vol", nil, 0, "/gex/exposure/ES?view=0dte_vol"},
		{gexPrefExposure, "NQ", "0dte", []string{"2026-08-22"}, 50, "/gex/exposure/NQ?bucket=50&expiry=2026-08-22"}, // filter drops view
	} {
		got := gexCanonicalViewURL(tc.page, tc.market, tc.exps, tc.bucket, tc.view)
		if got != tc.want {
			t.Errorf("canonical(%s,%s) = %q, want %q", tc.page, tc.market, got, tc.want)
		}
		round, ok := gexValidateStoredURL(tc.page, got)
		if !ok || round != got {
			t.Errorf("round-trip %q -> %q ok=%v", got, round, ok)
		}
	}
}
