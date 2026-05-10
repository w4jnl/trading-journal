package importer

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// motivewaveTimeLayout matches "05/01/2026 08:38:54.721+0200" — note the offset has no colon.
const motivewaveTimeLayout = "01/02/2006 15:04:05.000-0700"

var motivewaveRequired = []string{
	"Account", "Ticket", "Underlying", "Symbol", "Exchange",
	"Time", "Action", "Quantity", "Price", "Source",
}

// ParseMotiveWave reads a MotiveWave fill export. Each row is a single fill;
// one file may contain rows for many accounts.
func ParseMotiveWave(r io.Reader) ([]Fill, error) {
	reader := csv.NewReader(stripBOMReader(r))
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	cols, err := indexHeaders(header, motivewaveRequired)
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

		side, err := motivewaveSide(row[cols["Action"]])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", line, err)
		}
		qty, err := strconv.ParseFloat(strings.TrimSpace(row[cols["Quantity"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse quantity: %w", line, err)
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(row[cols["Price"]]), 64)
		if err != nil {
			return nil, fmt.Errorf("row %d: parse price: %w", line, err)
		}
		execTime, err := time.Parse(motivewaveTimeLayout, strings.TrimSpace(row[cols["Time"]]))
		if err != nil {
			return nil, fmt.Errorf("row %d: parse time %q: %w", line, row[cols["Time"]], err)
		}

		source := strings.TrimSpace(row[cols["Source"]])
		fills = append(fills, Fill{
			AccountRef:   strings.TrimSpace(row[cols["Account"]]),
			BrokerExecID: strings.TrimSpace(row[cols["Ticket"]]),
			Symbol:       strings.TrimSpace(row[cols["Symbol"]]),
			SymbolRoot:   CanonicalizeSymbolRoot(strings.TrimSpace(row[cols["Underlying"]]), source),
			Exchange:     strings.TrimSpace(row[cols["Exchange"]]),
			Source:       source,
			Side:         side,
			Qty:          qty,
			Price:        price,
			ExecTime:     execTime.UTC(),
			RawData:      encodeRow(header, row),
		})
	}
	return fills, nil
}

func motivewaveSide(action string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(action)) {
	case "BOT":
		return "buy", nil
	case "SLD":
		return "sell", nil
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

func indexHeaders(header, required []string) (map[string]int, error) {
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	for _, name := range required {
		if _, ok := idx[name]; !ok {
			return nil, fmt.Errorf("missing column %q", name)
		}
	}
	return idx, nil
}

func encodeRow(header, row []string) string {
	m := make(map[string]string, len(header))
	n := len(header)
	if len(row) < n {
		n = len(row)
	}
	for i := 0; i < n; i++ {
		m[strings.TrimSpace(header[i])] = row[i]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
