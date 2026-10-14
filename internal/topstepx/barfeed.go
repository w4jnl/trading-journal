package topstepx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
)

// BarFeed is the exchange feed behind the shared bar cache
// (marketdata.ExchangeSource): bars for the exact contract through a user's
// TopstepX session. The cache is one for everyone, so the feed borrows the
// session of any active user with credentials; the contract for a vendor
// symbol is the one most recently traded on that root (the front month in
// practice), mapped to its ProjectX id.
type BarFeed struct {
	Sync *SyncService
	DB   *sql.DB
	// Live selects the live data subscription; false reads the sim feed,
	// which carries the same exchange data.
	Live bool
}

// maxBarsPerRequest bounds one history call (the API's own limit is higher;
// a day at one minute is about 1400 bars, four minutes at one second 240).
const maxBarsPerRequest = 20000

// FetchExchangeBars implements marketdata.ExchangeSource.
func (f *BarFeed) FetchExchangeBars(ctx context.Context, yahooSymbol string, unitSeconds int, from, to time.Time) ([]marketdata.Bar, error) {
	if f == nil || f.Sync == nil {
		return nil, marketdata.ErrUnsupported
	}
	bc, ok := f.Sync.Client.(barsClient)
	if !ok {
		return nil, marketdata.ErrUnsupported
	}
	contract, ok := f.contractFor(ctx, yahooSymbol)
	if !ok {
		return nil, marketdata.ErrUnsupported
	}
	token, err := f.token(ctx)
	if err != nil {
		return nil, marketdata.ErrUnsupported
	}
	unit, n := UnitMinute, unitSeconds/60
	if unitSeconds < 60 {
		unit, n = UnitSecond, unitSeconds
	}
	if n <= 0 {
		n = 1
	}
	bars, err := bc.RetrieveBars(ctx, token, contract, unit, n, from, to, maxBarsPerRequest, false, f.Live)
	if err != nil {
		return nil, fmt.Errorf("topstepx bars %s: %w", contract, err)
	}
	out := make([]marketdata.Bar, 0, len(bars))
	for _, b := range bars {
		t := b.Time.Unix()
		if t < from.Unix() || t >= to.Unix() {
			continue
		}
		out = append(out, marketdata.Bar{Time: t, Open: b.Open, High: b.High, Low: b.Low, Close: b.Close, Volume: float64(b.Volume)})
	}
	if len(out) == 0 {
		return nil, marketdata.ErrNoData
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	return out, nil
}

// token is a session token of any active user with credentials.
func (f *BarFeed) token(ctx context.Context) (string, error) {
	users, err := f.Sync.Queries.ListTopstepxSyncUsers(ctx)
	if err != nil {
		return "", err
	}
	var last error
	for _, uid := range users {
		tok, err := f.Sync.TokenForUser(ctx, uid)
		if err == nil && tok != "" {
			return tok, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("no TopstepX session")
	}
	return "", last
}

// contractFor maps a vendor symbol to the ProjectX id of the contract most
// recently traded on one of its roots (MES for ES=F when the trader trades
// micros; the price series is the same underlying).
func (f *BarFeed) contractFor(ctx context.Context, yahooSymbol string) (string, bool) {
	var symbol, root string
	err := f.DB.QueryRowContext(ctx, `
		SELECT e.symbol, e.symbol_root FROM executions e
		WHERE e.deleted_at IS NULL AND e.symbol_root IN (`+rootsIn(yahooSymbol)+`)
		ORDER BY e.exec_time DESC LIMIT 1`).Scan(&symbol, &root)
	if err != nil {
		return "", false
	}
	return ContractIDForSymbol(symbol, root, time.Now())
}

// rootsIn lists, as a SQL literal list, the roots whose vendor symbol is
// yahooSymbol (ES and MES for ES=F).
func rootsIn(yahooSymbol string) string {
	var parts []string
	for root := range projectxRoots {
		if sym, ok := marketdata.YahooSymbol(root); ok && sym == yahooSymbol {
			parts = append(parts, "'"+strings.ToUpper(root)+"'")
		}
	}
	if len(parts) == 0 {
		return "''"
	}
	return strings.Join(parts, ",")
}
