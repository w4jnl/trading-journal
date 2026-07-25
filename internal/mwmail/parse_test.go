package mwmail

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
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
