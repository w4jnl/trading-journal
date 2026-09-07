package topstepx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AggregateBarUnit is ProjectX's bar size unit (History/retrieveBars).
type AggregateBarUnit int

const (
	UnitSecond AggregateBarUnit = 1
	UnitMinute AggregateBarUnit = 2
	UnitHour   AggregateBarUnit = 3
	UnitDay    AggregateBarUnit = 4
)

// AggregateBar is one OHLCV bar from History/retrieveBars.
type AggregateBar struct {
	Time   time.Time `json:"t"`
	Open   float64   `json:"o"`
	High   float64   `json:"h"`
	Low    float64   `json:"l"`
	Close  float64   `json:"c"`
	Volume int64     `json:"v"`
}

// RetrieveBars fetches bars for a contract (POST /api/History/retrieveBars).
// includePartial asks for the still-forming current bar too, which is what a
// "price right now" caller wants. live selects the live data subscription
// instead of the sim feed; both carry exchange data.
func (c *Client) RetrieveBars(ctx context.Context, token, contractID string, unit AggregateBarUnit, unitNumber int, start, end time.Time, limit int, includePartial, live bool) ([]AggregateBar, error) {
	var out struct {
		apiEnvelope
		Bars []AggregateBar `json:"bars"`
	}
	body := map[string]any{
		"contractId":        contractID,
		"live":              live,
		"startTime":         start.UTC().Format(time.RFC3339),
		"endTime":           end.UTC().Format(time.RFC3339),
		"unit":              int(unit),
		"unitNumber":        unitNumber,
		"limit":             limit,
		"includePartialBar": includePartial,
	}
	if err := c.post(ctx, "retrieveBars", "/api/History/retrieveBars", token, body, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, &APIError{Op: "retrieveBars", Status: http.StatusOK, ErrorCode: out.ErrorCode, Message: out.ErrorMessage}
	}
	// The API returns newest first; callers want chronological order.
	sort.Slice(out.Bars, func(i, j int) bool { return out.Bars[i].Time.Before(out.Bars[j].Time) })
	return out.Bars, nil
}

// projectxRoots maps the journal's instrument roots (Rithmic / MotiveWave
// style) to the exchange product codes ProjectX uses inside contract ids
// (CON.F.US.<code>.<M><YY>). CME's E-mini S&P is EP, the Nasdaq ENQ; crude
// and gold carry NYMEX/COMEX codes. Micros keep their names. Roots not listed
// here simply do not get a TopstepX quote and fall back to other sources.
var projectxRoots = map[string]string{
	"ES": "EP", "EP": "EP", "MES": "MES",
	"NQ": "ENQ", "ENQ": "ENQ", "MNQ": "MNQ",
	"CL": "CLE", "CLE": "CLE", "MCL": "MCL",
	"GC": "GCE", "GCE": "GCE", "MGC": "MGC",
	"YM": "YM", "MYM": "MYM",
	"RTY": "RTY", "M2K": "M2K",
}

// ContractIDForSymbol builds the ProjectX contract id for a journal contract
// symbol such as "ESU6" (root "ES"): CON.F.US.EP.U26. It is the inverse of
// SymbolFromContractID. The symbol carries only the last digit of the year,
// so the decade is chosen so the contract lies within [now-2y, now+8y) -
// open positions are always in a near contract. ok=false when the root has
// no ProjectX code or the symbol does not end in a month code + digit.
func ContractIDForSymbol(symbol, root string, now time.Time) (string, bool) {
	code, ok := projectxRoots[strings.ToUpper(root)]
	if !ok {
		return "", false
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if len(sym) < 2 {
		return "", false
	}
	month, digit := sym[len(sym)-2], sym[len(sym)-1]
	if !strings.ContainsRune(futuresMonthCodes, rune(month)) || digit < '0' || digit > '9' {
		return "", false
	}
	d := int(digit - '0')
	year := now.Year() - 2
	for ; year%10 != d; year++ {
	}
	return fmt.Sprintf("CON.F.US.%s.%c%02d", code, month, year%100), true
}

// ErrNoCredentials means the user has no TopstepX connection configured.
var ErrNoCredentials = errors.New("topstepx: no credentials for user")

// barsClient is the optional slice of the API used for quotes; the concrete
// Client implements it, test fakes need not.
type barsClient interface {
	RetrieveBars(ctx context.Context, token, contractID string, unit AggregateBarUnit, unitNumber int, start, end time.Time, limit int, includePartial, live bool) ([]AggregateBar, error)
}

// LatestBar returns the newest one-minute bar (partial bar included) for a
// contract using the user's own TopstepX session, so an open position can be
// priced with the exchange data the account trades on. Nil-receiver safe:
// a disabled sync service answers ErrNoCredentials.
func (s *SyncService) LatestBar(ctx context.Context, userID int64, contractID string) (AggregateBar, error) {
	if s == nil {
		return AggregateBar{}, ErrNoCredentials
	}
	bc, ok := s.Client.(barsClient)
	if !ok {
		return AggregateBar{}, ErrNoCredentials
	}
	token, err := s.TokenForUser(ctx, userID)
	if err != nil {
		return AggregateBar{}, err
	}
	now := time.Now()
	bars, err := bc.RetrieveBars(ctx, token, contractID, UnitMinute, 1, now.Add(-3*time.Hour), now, 5, true, false)
	if err != nil {
		return AggregateBar{}, err
	}
	if len(bars) == 0 {
		return AggregateBar{}, fmt.Errorf("topstepx: no bars for %s", contractID)
	}
	return bars[len(bars)-1], nil
}

// TokenForUser returns a usable session token for the user's stored TopstepX
// credentials (reusing the persisted token, refreshing inline only when it is
// about to expire). ErrNoCredentials when the user has none.
func (s *SyncService) TokenForUser(ctx context.Context, userID int64) (string, error) {
	if s == nil {
		return "", ErrNoCredentials
	}
	creds, err := s.Queries.GetTopstepxCredentials(ctx, userID)
	if err != nil {
		return "", ErrNoCredentials
	}
	apiKey, err := s.Box.Open(creds.ApiKeyEnc, creds.ApiKeyNonce)
	if err != nil {
		return "", fmt.Errorf("topstepx: decrypt credentials: %w", err)
	}
	return s.token(ctx, creds, apiKey)
}
