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
