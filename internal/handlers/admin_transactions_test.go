package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseExecTime(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// datetime-local in the trading tz -> UTC. June = CDT (UTC-5), so 07:16 -> 12:16Z.
	got, err := parseExecTime("2026-06-29T07:16", chicago)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := time.Date(2026, 6, 29, 12, 16, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("result not UTC: %s", got.Location())
	}
	// Seconds variant.
	if _, err := parseExecTime("2026-06-29T07:16:38", chicago); err != nil {
		t.Errorf("seconds variant should parse: %v", err)
	}
	// Junk rejected.
	if _, err := parseExecTime("not-a-time", chicago); err == nil {
		t.Error("want error for junk input")
	}
}

func TestManualBrokerExecID(t *testing.T) {
	a := manualBrokerExecID()
	b := manualBrokerExecID()
	if !strings.HasPrefix(a, "MANUAL-") {
		t.Errorf("missing MANUAL- prefix: %q", a)
	}
	if a == b {
		t.Errorf("ids not unique: %q == %q", a, b)
	}
}

func TestTxnFilterQuery(t *testing.T) {
	f := txnFilter{AccountID: 20, Symbol: "MESU26", From: "2026-06-01", Side: "buy", IncludeDeleted: true}
	q := f.query()
	if q.Get("account_id") != "20" || q.Get("symbol") != "MESU26" || q.Get("from") != "2026-06-01" ||
		q.Get("side") != "buy" || q.Get("deleted") != "1" {
		t.Errorf("query() lost a field: %v", q)
	}
	// Empty filter encodes to nothing (so a clean view has no stray query string).
	if got := (txnFilter{}).query().Encode(); got != "" {
		t.Errorf("empty filter query = %q, want empty", got)
	}
	// Three-way deleted filter, outcome and view round-trip through query() and parseTxnFilter.
	f2 := txnFilter{IncludeDeleted: true, OnlyDeleted: true, Outcome: "loss", View: "trades"}
	if q := f2.query(); q.Get("deleted") != "only" || q.Get("outcome") != "loss" || q.Get("view") != "trades" {
		t.Errorf("query() = %v", q)
	}
	back := parseTxnFilter(httptest.NewRequest("GET", "/admin/transactions?"+f2.query().Encode(), nil))
	if back != f2 {
		t.Errorf("round trip = %+v, want %+v", back, f2)
	}
	if p := parseTxnFilter(httptest.NewRequest("GET", "/admin/transactions?deleted=1&outcome=bogus", nil)); !p.IncludeDeleted || p.OnlyDeleted || p.Outcome != "" {
		t.Errorf("deleted=1 / bogus outcome parsed as %+v", p)
	}
}
