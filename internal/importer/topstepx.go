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

// topstepxSide maps the TopstepX "Side" column to internal buy/sell.
// Ask-side fills are buys (lifting the offer); Bid-side fills are sells (hitting the bid).
// Verified against PositionDisposition/CreationDisposition: Opening+Trader rows are Ask
// (going long), Closing+StopLoss on a long position is Bid (selling out).
func topstepxSide(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ask":
		return "buy", nil
	case "bid":
		return "sell", nil
	default:
		return "", fmt.Errorf("unknown Side %q", s)
	}
}
