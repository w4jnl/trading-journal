package importer

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// topstepxTradesTimeLayout matches "02/27/2026 19:43:47 +01:00" — colon in
// the offset, space before it. Same shape across EnteredAt / ExitedAt.
const topstepxTradesTimeLayout = "01/02/2006 15:04:05 -07:00"

var topstepxTradesRequired = []string{
	"Id", "ContractName", "EnteredAt", "ExitedAt",
	"EntryPrice", "ExitPrice", "Fees", "PnL", "Size", "Type",
}

// ParseTopstepXTrades reads a TopstepX trades export — one row per closed
// trade, with entry/exit prices+times and pre-computed P&L already in the
// file. Per-trade commission is the sum of the Fees and Commissions columns.
// Both columns can be blank for older rows (we treat blank as 0).
func ParseTopstepXTrades(r io.Reader) ([]TradeRecord, error) {
	reader := csv.NewReader(stripBOMReader(r))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	cols, err := indexHeaders(header, topstepxTradesRequired)
	if err != nil {
		return nil, err
	}
	// Commissions column is optional (older rows have it blank, very early
	// rows may not even include the column).
	commIdx := -1
	if i, ok := cols["Commissions"]; ok {
		commIdx = i
	}

	var out []TradeRecord
	line := 1
	for {
		line++
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}

		dir, err := tradesDirection(row[cols["Type"]])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}
		qty, err := strconv.ParseFloat(strings.TrimSpace(row[cols["Size"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse size: %w", line, err)
		}
		entryPrice, err := strconv.ParseFloat(strings.TrimSpace(row[cols["EntryPrice"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse entry price: %w", line, err)
		}
		exitPrice, err := strconv.ParseFloat(strings.TrimSpace(row[cols["ExitPrice"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse exit price: %w", line, err)
		}
		entryAt, err := time.Parse(topstepxTradesTimeLayout, strings.TrimSpace(row[cols["EnteredAt"]]))
		if err != nil {
			return nil, fmt.Errorf("row %d: parse entered_at %q: %w", line, row[cols["EnteredAt"]], err)
		}
		exitAt, err := time.Parse(topstepxTradesTimeLayout, strings.TrimSpace(row[cols["ExitedAt"]]))
		if err != nil {
			return nil, fmt.Errorf("row %d: parse exited_at %q: %w", line, row[cols["ExitedAt"]], err)
		}
		gross, err := strconv.ParseFloat(strings.TrimSpace(row[cols["PnL"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse pnl: %w", line, err)
		}
		fees, _ := parseFloatOrZero(row[cols["Fees"]])
		comm := 0.0
		if commIdx >= 0 && commIdx < len(row) {
			comm, _ = parseFloatOrZero(row[commIdx])
		}
		totalCost := fees + comm

		contract := strings.TrimSpace(row[cols["ContractName"]])
		out = append(out, TradeRecord{
			BrokerTradeID: strings.TrimSpace(row[cols["Id"]]),
			Symbol:        contract,
			SymbolRoot:    DeriveSymbolRoot(contract),
			Direction:     dir,
			Qty:           qty,
			EntryPrice:    entryPrice,
			ExitPrice:     exitPrice,
			EntryTime:     entryAt.UTC(),
			ExitTime:      exitAt.UTC(),
			GrossPnL:      gross,
			Commission:    totalCost,
			NetPnL:        gross - totalCost,
			RawData:       encodeRow(header, row),
		})
	}
	return out, nil
}

// tradesDirection maps the trades-export "Long" / "Short" Type column to the
// internal slugs. Case-insensitive on input.
func tradesDirection(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "long":
		return "long", nil
	case "short":
		return "short", nil
	default:
		return "", fmt.Errorf("unknown Type %q", s)
	}
}

// parseFloatOrZero treats whitespace / empty as 0 — the trades export leaves
// some commission cells empty for older rows.
func parseFloatOrZero(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	return strconv.ParseFloat(s, 64)
}
