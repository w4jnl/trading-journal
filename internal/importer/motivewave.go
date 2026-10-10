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
		if qty <= 0 {
			return nil, fmt.Errorf("row %d: quantity must be positive, got %v", line, qty)
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
		// "Order ID" is an optional column: present in the Executions-panel
		// export shape, absent from the older 10-column shape. It carries the
		// broker order number - the join key that lets this import supersede
		// provisional rows ingested from MW fill-notification emails.
		orderID := ""
		if oi, ok := cols["Order ID"]; ok && oi < len(row) {
			orderID = strings.TrimSpace(row[oi])
		}
		fills = append(fills, Fill{
			AccountRef:   strings.TrimSpace(row[cols["Account"]]),
			BrokerExecID: strings.TrimSpace(row[cols["Ticket"]]),
			OrderID:      orderID,
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
	disambiguateCoTicketFills(fills)
	return fills, nil
}

// disambiguateCoTicketFills makes BrokerExecID unique per (account, ticket) when one order
// fills in multiple lots and MotiveWave emits a row per lot sharing the Ticket. The importer
// dedups on (account_id, broker_exec_id) via INSERT OR IGNORE, so without this the 2nd+ lots
// collapse into the first and are silently dropped - which understates the position and can
// suppress trades (verified against an IBKR statement: the co-ticket rows are genuine
// separate fills). The first occurrence keeps the bare ticket so re-importing an
// already-imported file still matches existing rows; the 2nd, 3rd, ... get a "#2"/"#3"
// suffix. Numbering follows file order, which MotiveWave keeps stable across re-exports, so
// the mapped id set is deterministic and the whole thing stays idempotent on re-import.
// (Tickets are numeric, so the "#" suffix can never collide with a real one.)
func disambiguateCoTicketFills(fills []Fill) {
	seen := make(map[string]int, len(fills))
	for i := range fills {
		if fills[i].BrokerExecID == "" {
			continue
		}
		key := fills[i].AccountRef + "\x00" + fills[i].BrokerExecID
		seen[key]++
		if n := seen[key]; n > 1 {
			fills[i].BrokerExecID = fills[i].BrokerExecID + "#" + strconv.Itoa(n)
		}
	}
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
