package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/web"
)

func TestTradeSortParsingAndLinks(t *testing.T) {
	h := &Handlers{}
	parse := func(q string) tradeFilters {
		req := httptest.NewRequest(http.MethodGet, "/trades"+q, nil).WithContext(auth.WithUser(context.Background(), &auth.User{ID: 1}))
		f, err := h.parseTradeFilters(req)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if f := parse(""); f.Sort != "" || orderBySQL(f) != "t.entry_time DESC, t.id DESC" {
		t.Errorf("default order = %q (sort %q)", orderBySQL(f), f.Sort)
	}
	if f := parse("?sort=net"); f.Sort != "net" || f.Dir != "desc" || !strings.HasPrefix(orderBySQL(f), "t.net_pnl DESC NULLS LAST") {
		t.Errorf("sort=net -> %+v %q", f, orderBySQL(f))
	}
	if f := parse("?sort=account"); f.Dir != "asc" { // text columns start ascending
		t.Errorf("sort=account dir = %q", f.Dir)
	}
	if f := parse("?sort=net&dir=asc"); f.Dir != "asc" || !strings.Contains(orderBySQL(f), "ASC NULLS LAST") {
		t.Errorf("explicit asc: %+v", f)
	}
	// Anything not in the whitelist is ignored, never interpolated.
	if f := parse("?sort=t.id%3B+DROP+TABLE+trades&dir=asc"); f.Sort != "" || f.Dir != "" {
		t.Errorf("unknown sort accepted: %+v", f)
	}
	if f := parse("?sort=qty&dir=sideways"); f.Dir != "desc" {
		t.Errorf("bad dir not defaulted: %+v", f)
	}

	// Header links: the active column flips, others start in their default
	// direction, page resets, filters stay.
	req := httptest.NewRequest(http.MethodGet, "/trades?symbol_root=MES&page=3&sort=net&dir=desc", nil)
	f := tradeFilters{Sort: "net", Dir: "desc"}
	if got := sortRef(req, f, "net"); !strings.Contains(got, "dir=asc") || !strings.Contains(got, "symbol_root=MES") || strings.Contains(got, "page=") {
		t.Errorf("flip link = %q", got)
	}
	if got := sortRef(req, f, "account"); !strings.Contains(got, "sort=account") || !strings.Contains(got, "dir=asc") {
		t.Errorf("other column link = %q", got)
	}
	if got := sortRef(req, f, "gross"); !strings.Contains(got, "dir=desc") {
		t.Errorf("numeric column default = %q", got)
	}
	if refs := sortRefs(req, f); len(refs) != len(tradeSortColumns) || refs["entry"] == "" {
		t.Errorf("sortRefs = %v", refs)
	}
}

// sortFixture seeds two users with one account and one closed +50 trade
// each (bob's is only there to prove totals stay user-scoped).
type sortFixture struct {
	h          *Handlers
	alice, bob *auth.User
}

func newSortFixture(t *testing.T) *sortFixture {
	t.Helper()
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	mk := func(name, ref string) *auth.User {
		u, err := q.CreateUser(ctx, models.CreateUserParams{Username: name})
		if err != nil {
			t.Fatal(err)
		}
		acct, err := q.CreateAccount(ctx, models.CreateAccountParams{
			UserID: u.ID, Name: ref, ImportSource: "motivewave_csv", AccountRef: ref, Currency: "USD", CommissionSource: "rate",
		})
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		exit := now.Add(time.Hour)
		if _, err := q.CreateTrade(ctx, models.CreateTradeParams{
			AccountID: acct.ID, Symbol: "MESU26", SymbolRoot: "MES", Direction: "long",
			Qty: 1, EntryPrice: 5000, ExitPrice: ptrF(5010), EntryTime: now, ExitTime: &exit,
			NetPnl: ptrF(50), Status: "closed",
		}); err != nil {
			t.Fatal(err)
		}
		return &auth.User{ID: u.ID, Username: name, DisplayName: name}
	}
	return &sortFixture{
		h:     &Handlers{DB: conn, Queries: q, Timezone: time.UTC, Templates: web.TemplatesFS},
		alice: mk("alice", "APEX-A"), bob: mk("bob", "APEX-B"),
	}
}

// Totals cover the whole filtered set, and the rendered table carries the
// sortable headers, the footer, keyboard-focusable rows and labelled
// checkboxes.
func TestTradesPageTotalsAndSortableHeaders(t *testing.T) {
	f := newSortFixture(t)
	ctx := auth.WithUser(context.Background(), f.alice)
	req := httptest.NewRequest(http.MethodGet, "/trades?sort=net&dir=asc", nil).WithContext(ctx)
	filters, err := f.h.parseTradeFilters(req)
	if err != nil {
		t.Fatal(err)
	}
	tt, err := totalsTradesFiltered(ctx, f.h.DB, filters)
	if err != nil {
		t.Fatal(err)
	}
	if tt.Count != 1 || tt.Wins != 1 || tt.Losses != 0 || tt.Net < 49.999 || tt.Net > 50.001 {
		t.Errorf("totals = %+v, want alice's single +50 trade", tt)
	}
	// Bob's trade is not in alice's totals.
	bctx := auth.WithUser(context.Background(), f.bob)
	bf, _ := f.h.parseTradeFilters(httptest.NewRequest(http.MethodGet, "/trades", nil).WithContext(bctx))
	if bt, _ := totalsTradesFiltered(bctx, f.h.DB, bf); bt.Count != 1 {
		t.Errorf("bob totals = %+v", bt)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /trades", f.h.TradesPage)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /trades = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<table class="trades-table">`,
		`class="table-scroll sticky-head"`,
		`aria-sort="ascending"`,    // the active net column
		`dir=desc&amp;sort=net`,    // flip link on the active column
		`dir=asc&amp;sort=account`, // another column's default
		`<tfoot>`, `1</span> trades in this filter`, `>50.00</td>`,
		`tabindex="0"`,
		`aria-label="Select trade MESU26`,
		`data-label="Net"`,
		`<details class="filters-collapse" id="trades-filters" open>`,
		`onchange="this.form.requestSubmit()"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}
