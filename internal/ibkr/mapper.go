package ibkr

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/importer"
)

// ibkrSource is written to Fill.Source; matches the MotiveWave export's
// Source column for IBKR rows so GuessFirm and any source-based logic see
// the same string for both eras of an account.
const ibkrSource = "Interactive Brokers"

// MapTrade converts one gateway execution into an importer.Fill. resolve
// turns a conid into a contract symbol ("ESM6") and is only consulted when
// contract_description_1 doesn't already look like one - the caller wraps
// ContractInfo with a per-sync cache. The symbol MUST match the MotiveWave
// representation so CSV-era and API-era fills share FIFO queues.
func MapTrade(t Trade, resolve func(conid int64) (string, error)) (importer.Fill, error) {
	var side string
	switch strings.ToUpper(strings.TrimSpace(t.Side)) {
	case "B":
		side = "buy"
	case "S":
		side = "sell"
	default:
		return importer.Fill{}, fmt.Errorf("ibkr: trade %s: unknown side %q", t.ExecutionID, t.Side)
	}
	if t.ExecutionID == "" || t.Account == "" || t.Size <= 0 {
		return importer.Fill{}, fmt.Errorf("ibkr: trade %s: missing execution_id/account or non-positive size", t.ExecutionID)
	}

	symbol, err := contractSymbol(t, resolve)
	if err != nil {
		return importer.Fill{}, err
	}
	root := importer.CanonicalizeSymbolRoot(importer.DeriveSymbolRoot(symbol), ibkrSource)

	execTime, err := tradeTime(t)
	if err != nil {
		return importer.Fill{}, err
	}

	// Commission is the broker-reported total for this execution. Absent or
	// zero means "not reported" -> nil, so the builder falls back to the
	// rate table instead of recording a free trade.
	var commission *float64
	if c := float64(t.Commission); c > 0 {
		commission = &c
	}

	raw, _ := json.Marshal(map[string]any{
		"execution_id": t.ExecutionID, "conid": t.Conid, "exchange": t.Exchange,
		"sec_type": t.SecType, "price": float64(t.Price), "size": t.Size,
	})
	return importer.Fill{
		AccountRef:   t.Account,
		BrokerExecID: t.ExecutionID,
		Symbol:       symbol,
		SymbolRoot:   root,
		Exchange:     t.Exchange,
		Source:       ibkrSource,
		Side:         side,
		Qty:          t.Size,
		Price:        float64(t.Price),
		Commission:   commission,
		ExecTime:     execTime,
		RawData:      string(raw),
	}, nil
}

// contractSymbol yields the MW-style contract code ("ESM6"): use
// contract_description_1 when it already parses as root+month+year, else
// resolve the conid via the gateway.
func contractSymbol(t Trade, resolve func(conid int64) (string, error)) (string, error) {
	desc := strings.ToUpper(strings.TrimSpace(t.Description1))
	if looksLikeContractCode(desc) {
		return desc, nil
	}
	if resolve == nil {
		return "", fmt.Errorf("ibkr: trade %s: description %q not a contract code and no resolver", t.ExecutionID, t.Description1)
	}
	sym, err := resolve(t.Conid)
	if err != nil {
		return "", fmt.Errorf("ibkr: trade %s: resolve conid %d: %w", t.ExecutionID, t.Conid, err)
	}
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if !looksLikeContractCode(sym) {
		return "", fmt.Errorf("ibkr: trade %s: resolved symbol %q is not a contract code", t.ExecutionID, sym)
	}
	return sym, nil
}

// looksLikeContractCode reports whether s parses as <root><month letter><year
// digit(s)> with no spaces - i.e. DeriveSymbolRoot found a suffix to strip.
func looksLikeContractCode(s string) bool {
	return s != "" && !strings.ContainsRune(s, ' ') && importer.DeriveSymbolRoot(s) != s
}

// tradeTime prefers the epoch-ms field; falls back to the "YYYYMMDD-HH:mm:ss"
// string (UTC). Both are converted to UTC.
func tradeTime(t Trade) (time.Time, error) {
	if t.TradeTimeR > 0 {
		return time.UnixMilli(t.TradeTimeR).UTC(), nil
	}
	if ts, err := time.ParseInLocation("20060102-15:04:05", strings.TrimSpace(t.TradeTime), time.UTC); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("ibkr: trade %s: unparseable trade time %q / %d", t.ExecutionID, t.TradeTime, t.TradeTimeR)
}
