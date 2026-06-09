package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
)

func TestApiRewriteAttachmentRefs(t *testing.T) {
	md := "a ![](/journal/attachments/88) b ![](/journal/attachments/5) c ![](/journal/attachments/88)"
	out, ids := apiRewriteAttachmentRefs(md)
	if strings.Contains(out, "/journal/attachments/") {
		t.Errorf("refs not rewritten: %s", out)
	}
	if !strings.Contains(out, "/api/v1/attachments/88") || !strings.Contains(out, "/api/v1/attachments/5") {
		t.Errorf("output missing api refs: %s", out)
	}
	if len(ids) != 2 || ids[0] != 88 || ids[1] != 5 {
		t.Errorf("ids = %v, want [88 5] (deduped, first-seen order)", ids)
	}

	out2, ids2 := apiRewriteAttachmentRefs("no images here")
	if out2 != "no images here" || len(ids2) != 0 {
		t.Errorf("no-ref input changed: %q ids=%v", out2, ids2)
	}
}

func TestFormatAppendedNote(t *testing.T) {
	loc := time.FixedZone("CET", 3600)
	now := time.Date(2026, 6, 6, 14, 32, 0, 0, time.UTC) // 15:32 CET
	got := formatAppendedNote(now, loc, "did X")
	want := "\n\n---\n\n_Note added 2026-06-06 15:32 CET_\n\ndid X\n"
	if got != want {
		t.Errorf("formatAppendedNote =\n%q\nwant\n%q", got, want)
	}
	// Trailing whitespace on the note is trimmed; the block always ends in \n.
	if !strings.HasSuffix(formatAppendedNote(now, loc, "hi\n\n  "), "hi\n") {
		t.Error("trailing whitespace not trimmed to single newline")
	}
}

func TestApiParseTag(t *testing.T) {
	if got, ok := apiParseTag(nil); !ok || got != nil {
		t.Errorf("nil tag = (%v, %v), want (nil, true)", got, ok)
	}
	empty := "  "
	if got, ok := apiParseTag(&empty); !ok || got != nil {
		t.Errorf("blank tag = (%v, %v), want (nil, true)", got, ok)
	}
	for _, v := range []string{"good", "neutral", "bad"} {
		if got, ok := apiParseTag(&v); !ok || got == nil || *got != v {
			t.Errorf("tag %q = (%v, %v), want valid", v, got, ok)
		}
	}
	bad := "great"
	if _, ok := apiParseTag(&bad); ok {
		t.Error("tag \"great\" accepted, want rejected")
	}
}

func TestApiParseMood(t *testing.T) {
	if got, ok := apiParseMood(nil); !ok || got != nil {
		t.Errorf("nil mood = (%v, %v), want (nil, true)", got, ok)
	}
	for _, v := range []int64{1, 3, 5} {
		if got, ok := apiParseMood(&v); !ok || got == nil || *got != v {
			t.Errorf("mood %d rejected, want accepted", v)
		}
	}
	for _, v := range []int64{0, 6, -1} {
		if _, ok := apiParseMood(&v); ok {
			t.Errorf("mood %d accepted, want rejected", v)
		}
	}
}

func TestApiTradeRowFrom(t *testing.T) {
	net := 33.3
	firm := "apex"
	row := tradeRow{
		ID: 42, AccountID: 7, Symbol: "MESM6", SymbolRoot: "MES", Direction: "long",
		Qty: 3, EntryPrice: 7410.25, NetPnl: &net, Status: "closed",
		AccountName: "APEX-1", AccountFirm: &firm, HasJournal: true, GroupCount: 2,
	}
	b, err := json.Marshal(apiTradeRowFrom(row))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"symbolRoot":"MES"`, `"accountId":7`, `"netPnl":33.3`, `"groupCount":2`, `"hasJournal":true`, `"firm":"apex"`} {
		if !strings.Contains(s, want) {
			t.Errorf("projection missing %s\n got: %s", want, s)
		}
	}
	if strings.Contains(s, "symbol_root") || strings.Contains(s, "account_id") {
		t.Errorf("projection leaked snake_case: %s", s)
	}
}

// TestAPIJournalCreateValidation covers the request-validation paths that fail
// before any DB access, so a DB-less Handlers is sufficient. The 201/409/group
// link behaviors run against the real schema in the DB-clone smoke test.
func TestAPIJournalCreateValidation(t *testing.T) {
	h := &Handlers{Timezone: time.UTC}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"invalid json", "{not json", 400},
		{"unknown scope", `{"scope":"weekly","body":"x"}`, 400},
		{"bad tag", `{"scope":"day","date":"2026-06-02","body":"x","tag":"great"}`, 400},
		{"day missing date", `{"scope":"day","body":"x"}`, 400},
		{"day missing body", `{"scope":"day","date":"2026-06-02"}`, 400},
		{"day future date", `{"scope":"day","date":"2099-01-01","body":"x"}`, 400},
		{"week missing week", `{"scope":"week","body":"x"}`, 400},
		{"week bad format", `{"scope":"week","week":"2026-99","body":"x"}`, 400},
		{"trade missing tradeId", `{"scope":"trade","body":"x"}`, 400},
		{"group missing tradeIds", `{"scope":"group","body":"x"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/journal", strings.NewReader(c.body))
			req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: 1, Username: "t"}))
			rec := httptest.NewRecorder()
			h.APIJournalCreate(rec, req)
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestClusterCopytrader(t *testing.T) {
	base := time.Date(2026, 5, 12, 17, 25, 43, 0, time.UTC)
	mk := func(id int64, root, dir string, acct int64, at time.Time) prepDayTrade {
		return prepDayTrade{ID: id, SymbolRoot: root, Direction: dir, AccountID: acct, EntryTime: at}
	}
	trs := []prepDayTrade{
		// copytrader fan-out: MES short across 3 accounts within ~1s -> one group
		mk(1, "MES", "short", 10, base),
		mk(2, "MES", "short", 11, base.Add(500*time.Millisecond)),
		mk(3, "MES", "short", 12, base.Add(time.Second)),
		// solo on another symbol
		mk(4, "NQ", "long", 10, base.Add(time.Hour)),
		// same symbol+dir but 5s apart on 2 accounts -> NOT a cluster (two solos)
		mk(5, "MES", "short", 10, base.Add(10*time.Minute)),
		mk(6, "MES", "short", 11, base.Add(10*time.Minute+5*time.Second)),
		// two trades within the window but on ONE account -> not a group (solos)
		mk(7, "CL", "long", 20, base),
		mk(8, "CL", "long", 20, base.Add(time.Second)),
	}
	groups, solos := clusterCopytrader(trs)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if len(groups[0]) != 3 || distinctAccounts(groups[0]) != 3 {
		t.Errorf("group size/accts = %d/%d, want 3/3 (ids %v)", len(groups[0]), distinctAccounts(groups[0]), tradeIDsOf(groups[0]))
	}
	if len(solos) != 5 { // 4 (NQ), 5, 6 (far-apart MES), 7, 8 (same-account CL)
		t.Errorf("solos = %d, want 5 (ids %v)", len(solos), tradeIDsOf(solos))
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		v    any
		want int64
		ok   bool
	}{
		{float64(42), 42, true},
		{int(7), 7, true},
		{int64(9), 9, true},
		{json.Number("123"), 123, true},
		{"456", 456, true},
		{"  789 ", 789, true},
		{"nope", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt64(c.v)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("toInt64(%#v) = (%d, %v), want (%d, %v)", c.v, got, ok, c.want, c.ok)
		}
	}
}

func TestClusterCopytraderCrossInstrument(t *testing.T) {
	base := time.Date(2026, 5, 12, 17, 25, 43, 0, time.UTC)
	mk := func(id int64, root string, acct int64, at time.Time) prepDayTrade {
		return prepDayTrade{ID: id, SymbolRoot: root, Direction: "short", AccountID: acct, EntryTime: at}
	}
	// Master trades ES; followers get the micro MES at the same instant -> one
	// copytrader decision (ES and MES share the ES=F underlying).
	groups, solos := clusterCopytrader([]prepDayTrade{
		mk(1, "ES", 10, base),
		mk(2, "MES", 11, base.Add(200*time.Millisecond)),
		mk(3, "MES", 12, base.Add(400*time.Millisecond)),
	})
	if len(groups) != 1 || len(solos) != 0 {
		t.Fatalf("ES+MES: got %d groups / %d solos, want 1/0", len(groups), len(solos))
	}
	if n := distinctAccounts(groups[0]); n != 3 {
		t.Errorf("group accounts = %d, want 3", n)
	}
	if lbl := clusterLabel(groups[0]); !strings.Contains(lbl, "ES") || !strings.Contains(lbl, "MES") {
		t.Errorf("label %q should name both roots", lbl)
	}
	// Different underlyings at the same instant must NOT merge.
	g2, s2 := clusterCopytrader([]prepDayTrade{mk(1, "ES", 10, base), mk(2, "NQ", 11, base)})
	if len(g2) != 0 || len(s2) != 2 {
		t.Errorf("ES+NQ same instant: got %d groups / %d solos, want 0/2", len(g2), len(s2))
	}
}
