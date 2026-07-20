package ibkr

import (
	"errors"
	"testing"
	"time"
)

func sampleTrade() Trade {
	return Trade{
		ExecutionID: "0000e0d5.68762d31.01.01", Symbol: "ES", Description1: "ESM6",
		SecType: "FUT", Conid: 495512563, Side: "B", Size: 2, Price: 7222.50,
		Commission: 4.28, TradeTime: "20260504-12:23:31", TradeTimeR: 1777976611000,
		Account: "U10000001", Exchange: "CME",
	}
}

func TestMapTrade(t *testing.T) {
	f, err := MapTrade(sampleTrade(), nil)
	if err != nil {
		t.Fatalf("MapTrade: %v", err)
	}
	if f.AccountRef != "U10000001" || f.BrokerExecID != "0000e0d5.68762d31.01.01" {
		t.Errorf("account/id = %q/%q", f.AccountRef, f.BrokerExecID)
	}
	if f.Symbol != "ESM6" || f.SymbolRoot != "ES" {
		t.Errorf("symbol = %q root %q, want ESM6/ES (must match MotiveWave)", f.Symbol, f.SymbolRoot)
	}
	if f.Side != "buy" || f.Qty != 2 || f.Price != 7222.50 {
		t.Errorf("side/qty/price = %q/%v/%v", f.Side, f.Qty, f.Price)
	}
	if f.Commission == nil || *f.Commission != 4.28 {
		t.Errorf("commission = %v, want 4.28", f.Commission)
	}
	if f.Source != "Interactive Brokers" {
		t.Errorf("source = %q", f.Source)
	}
	// trade_time_r (epoch ms) wins.
	if want := time.UnixMilli(1777976611000).UTC(); !f.ExecTime.Equal(want) {
		t.Errorf("time = %v, want %v", f.ExecTime, want)
	}
}

func TestMapTradeStringTimeFallback(t *testing.T) {
	tr := sampleTrade()
	tr.TradeTimeR = 0
	f, err := MapTrade(tr, nil)
	if err != nil {
		t.Fatalf("MapTrade: %v", err)
	}
	want := time.Date(2026, 5, 4, 12, 23, 31, 0, time.UTC)
	if !f.ExecTime.Equal(want) {
		t.Errorf("time = %v, want %v (parsed as UTC)", f.ExecTime, want)
	}
}

func TestMapTradeSides(t *testing.T) {
	tr := sampleTrade()
	tr.Side = "S"
	if f, _ := MapTrade(tr, nil); f.Side != "sell" {
		t.Errorf("S -> %q, want sell", f.Side)
	}
	tr.Side = "X"
	if _, err := MapTrade(tr, nil); err == nil {
		t.Error("unknown side accepted")
	}
}

func TestMapTradeZeroCommissionIsNil(t *testing.T) {
	tr := sampleTrade()
	tr.Commission = 0
	f, err := MapTrade(tr, nil)
	if err != nil {
		t.Fatalf("MapTrade: %v", err)
	}
	if f.Commission != nil {
		t.Errorf("commission = %v, want nil (falls back to rate table)", *f.Commission)
	}
}

func TestMapTradeResolverFallback(t *testing.T) {
	tr := sampleTrade()
	tr.Description1 = "E-mini S&P JUN26" // not a contract code
	called := false
	f, err := MapTrade(tr, func(conid int64) (string, error) {
		called = true
		if conid != 495512563 {
			t.Errorf("resolver conid = %d", conid)
		}
		return "esm6", nil // lowercase on purpose - mapper normalizes
	})
	if err != nil || !called || f.Symbol != "ESM6" {
		t.Fatalf("resolver path = (%q, called=%v, %v)", f.Symbol, called, err)
	}

	// Resolver error -> fill rejected, never an invented symbol.
	if _, err := MapTrade(tr, func(int64) (string, error) {
		return "", errors.New("conid unknown")
	}); err == nil {
		t.Error("resolver error swallowed")
	}
	// No resolver at all -> rejected.
	if _, err := MapTrade(tr, nil); err == nil {
		t.Error("unresolvable description accepted without resolver")
	}
}
