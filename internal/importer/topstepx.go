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

// topstepxTimeLayout matches "05/08/2026 11:16:14 +02:00" (space-separated, colon in offset).
const topstepxTimeLayout = "01/02/2006 15:04:05 -07:00"

var topstepxRequired = []string{
	"Id", "AccountName", "ContractName", "Status", "Size", "Side",
	"FilledAt", "ExecutePrice",
}

// ParseTopstepX reads a TopstepX order export. Rows are orders, not fills:
// only Status="Filled" rows contribute executions; cancelled and triggered-but-
// not-filled rows are skipped.
func ParseTopstepX(r io.Reader) ([]Fill, error) {
	reader := csv.NewReader(stripBOMReader(r))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	cols, err := indexHeaders(header, topstepxRequired)
	if err != nil {
		return nil, err
	}

	var fills []Fill
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

		if !strings.EqualFold(strings.TrimSpace(row[cols["Status"]]), "Filled") {
			continue
		}

		side, err := topstepxSide(row[cols["Side"]])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}
		qty, err := strconv.ParseFloat(strings.TrimSpace(row[cols["Size"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse size: %w", line, err)
		}
		if qty <= 0 {
			return nil, fmt.Errorf("row %d: size must be positive, got %v", line, qty)
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(row[cols["ExecutePrice"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse execute price: %w", line, err)
		}
		execTime, err := time.Parse(topstepxTimeLayout, strings.TrimSpace(row[cols["FilledAt"]]))
		if err != nil {
			return nil, fmt.Errorf("row %d: parse filled_at %q: %w", line, row[cols["FilledAt"]], err)
		}

		contract := strings.TrimSpace(row[cols["ContractName"]])
		fills = append(fills, Fill{
			AccountRef:   strings.TrimSpace(row[cols["AccountName"]]),
			BrokerExecID: strings.TrimSpace(row[cols["Id"]]),
			Symbol:       contract,
			SymbolRoot:   DeriveSymbolRoot(contract),
			Exchange:     "",
			Side:         side,
			Qty:          qty,
			Price:        price,
			ExecTime:     execTime.UTC(),
			RawData:      encodeRow(header, row),
		})
	}
	return fills, nil
}

// topstepxSide maps the TopstepX "Side" column to internal buy/sell. The
// field reflects which side of the position book the order participates in,
// not which side of the spread the trader crosses:
//   - Side="Ask" -> SELL execution (sell-to-open short, sell-to-close long)
//   - Side="Bid" -> BUY  execution (buy-to-open long, buy-to-close short)
//
// Verified by joining the orders CSV against the trades CSV on (timestamp,
// price, contract): every Limit|Ask|Opening|Trader row matches a Short trade
// entry; every Limit|Bid|Closing|TakeProfit and Stop|Bid|Closing|StopLoss
// matches a short exit. An earlier "Ask=BUY" reading was based on the
// long-only assumption that didn't survive cross-reference.
func topstepxSide(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ask":
		return "sell", nil
	case "bid":
		return "buy", nil
	default:
		return "", fmt.Errorf("unknown Side %q", s)
	}
}
