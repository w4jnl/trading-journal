package mwmail

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// TestParseRealMWFill: the verified real MotiveWave notification shape -
// labeled key-value block in a multipart/mixed message. Everything must come
// from the labels, including the account (the "Account ID:" line used to trip
// the heuristic into extracting the literal word "ID").
func TestParseRealMWFill(t *testing.T) {
	res := Parse(readFixture(t, "fill_real_mw.eml"))
	if !res.Parsed || res.IsTest {
		t.Fatalf("parsed/test = %v/%v: %+v", res.Parsed, res.IsTest, res)
	}
	if res.Side != "sell" || res.Qty != 3 || res.Symbol != "MESU6" || res.Price != 7516.25 {
		t.Errorf("side/qty/symbol/price = %s/%v/%s/%v", res.Side, res.Qty, res.Symbol, res.Price)
	}
	if res.Account != "LFE050-EXAMPLE1-TEST018" {
		t.Errorf("account = %q, want the full Account ID value", res.Account)
	}
	if res.OrderID != "172416726" {
		t.Errorf("order id = %q", res.OrderID)
	}
	if res.Broker != "Rithmic WS" {
		t.Errorf("broker = %q", res.Broker)
	}
	// Execution Time carries no year and no zone; both come from the Date
	// header (+0200) -> 09:19:33 local = 07:19:33 UTC.
	want := time.Date(2026, 7, 27, 7, 19, 33, 0, time.UTC)
	if !res.ExecTime.Equal(want) {
		t.Errorf("exec time = %v, want %v", res.ExecTime, want)
	}
	if !res.IngestReady() {
		t.Errorf("IngestReady = false: %+v", res)
	}
}

// TestParseTestEmail: MW's Email Settings test message must be flagged and
// never considered ingestable.
func TestParseTestEmail(t *testing.T) {
	res := Parse(readFixture(t, "test_email.eml"))
	if !res.IsTest {
		t.Error("IsTest = false for the MW test email")
	}
	if res.IngestReady() {
		t.Error("test email reported IngestReady")
	}
}

// TestDeriveExecTime_YearBoundary: a fill executed moments before midnight
// Dec 31 composed into a Jan 1 Date header must roll back to the old year.
func TestDeriveExecTime_YearBoundary(t *testing.T) {
	got := deriveExecTime("Dec-31 11:59:58 PM", "Thu, 1 Jan 2026 00:00:05 +0100")
	want := time.Date(2025, 12, 31, 22, 59, 58, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("year-boundary exec time = %v, want %v", got, want)
	}
	// And the trivial same-day case stays put.
	got = deriveExecTime("Jul-27 09:19:33 AM", "Mon, 27 Jul 2026 09:19:31 +0200")
	want = time.Date(2026, 7, 27, 7, 19, 33, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("same-day exec time = %v, want %v", got, want)
	}
}

// TestDeriveExecTime_24HourWithMarker: MW appends AM/PM to a 24-HOUR clock -
// afternoon fills read "15:31:26 PM" (seen live) and used to fail the
// 12-hour parse, making every after-noon fill not_ingestable.
func TestDeriveExecTime_24HourWithMarker(t *testing.T) {
	cases := []struct {
		fill string
		want time.Time
	}{
		// The live formats: 24-hour clock with a redundant marker.
		{"Jul-27 15:31:26 PM", time.Date(2026, 7, 27, 13, 31, 26, 0, time.UTC)},
		{"Jul-27 09:04:52 AM", time.Date(2026, 7, 27, 7, 4, 52, 0, time.UTC)},
		// A true-12-hour variant must land on the same instant.
		{"Jul-27 03:31:26 PM", time.Date(2026, 7, 27, 13, 31, 26, 0, time.UTC)},
		// Midnight/noon edges under 12-hour convention.
		{"Jul-27 12:00:01 AM", time.Date(2026, 7, 26, 22, 0, 1, 0, time.UTC)},
		{"Jul-27 12:00:01 PM", time.Date(2026, 7, 27, 10, 0, 1, 0, time.UTC)},
		// 24-hour without any marker still parses.
		{"Jul-27 22:15:00", time.Date(2026, 7, 27, 20, 15, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := deriveExecTime(c.fill, "Mon, 27 Jul 2026 23:00:00 +0200")
		if !got.Equal(c.want) {
			t.Errorf("deriveExecTime(%q) = %v, want %v", c.fill, got, c.want)
		}
	}
}

func TestParseBasicFill(t *testing.T) {
	res := Parse(readFixture(t, "fill_basic.eml"))
	if !res.Parsed {
		t.Fatalf("Parsed = false, want true: %+v", res)
	}
	if res.Side != "buy" || res.Qty != 2 || res.Symbol != "MESU6" || res.Price != 6852.25 {
		t.Errorf("side/qty/symbol/price = %s/%v/%s/%v", res.Side, res.Qty, res.Symbol, res.Price)
	}
	if res.Account != "APEX-100002-49" {
		t.Errorf("account = %q", res.Account)
	}
	if res.OrderID != "1234567" {
		t.Errorf("order id = %q", res.OrderID)
	}
	if res.Subject != "Order Filled: BOT 2 MESU6 @ 6,852.25" {
		t.Errorf("subject = %q", res.Subject)
	}
	if res.FillTimeRaw == "" {
		t.Error("fill time not captured")
	}
}

func TestParseSoldPriceWord(t *testing.T) {
	res := Parse(readFixture(t, "fill_sold_price_word.eml"))
	if !res.Parsed {
		t.Fatalf("Parsed = false, want true: %+v", res)
	}
	if res.Side != "sell" || res.Qty != 10 || res.Price != 6851.50 {
		t.Errorf("side/qty/price = %s/%v/%v", res.Side, res.Qty, res.Price)
	}
	if res.Symbol != "ES" {
		t.Errorf("symbol = %q, want bare-root fallback ES", res.Symbol)
	}
	if res.Account != "120042" {
		t.Errorf("account = %q", res.Account)
	}
	if res.OrderID != "98765" {
		t.Errorf("order id = %q", res.OrderID)
	}
}

func TestParseUnparseable(t *testing.T) {
	res := Parse(readFixture(t, "unparseable.eml"))
	if res.Parsed {
		t.Errorf("Parsed = true for a non-fill notification: %+v", res)
	}
	if res.Subject != "Connection restored" {
		t.Errorf("subject = %q (headers should still be extracted)", res.Subject)
	}
}

// The "Order Filled" trap: a bare "order" qualifier must NOT yield an order
// id from the word "Filled"; the id requires an explicit qualifier and a
// digit in the token.
func TestParseOrderIDTrap(t *testing.T) {
	res := Parse([]byte("Subject: Order Filled\r\n\r\nBUY 1 MESU6 @ 6800.00\r\nOrder Filled successfully\r\n"))
	if res.OrderID != "" {
		t.Errorf("order id = %q, want empty (no explicit id present)", res.OrderID)
	}
	if !res.Parsed {
		t.Error("fill fields present, should still parse")
	}
}

func TestParseThousandsAndDateTrap(t *testing.T) {
	res := Parse([]byte("Subject: SLD 3 E-mini S&P JUN26 @ 6,900.75\r\n\r\nAccount: LFE050-X\r\n"))
	if res.Side != "sell" || res.Qty != 3 || res.Price != 6900.75 {
		t.Errorf("side/qty/price = %s/%v/%v", res.Side, res.Qty, res.Price)
	}
	if res.Symbol == "JUN26" {
		t.Errorf("symbol = JUN26 - the month-abbreviation trap fired")
	}
}

// Raw non-RFC5322 bytes must not panic and can still parse.
func TestParseHeaderless(t *testing.T) {
	res := Parse([]byte("BOT 1 NQZ5 @ 20,100.25"))
	if !res.Parsed || res.Symbol != "NQZ5" || res.Price != 20100.25 {
		t.Errorf("headerless parse = %+v", res)
	}
}
