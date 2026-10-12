package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/copyset"
)

// PrepTrade is one closed trade on the target day plus what the clusterer
// and the GEX annotator need. Exit fields are present because the query
// filters status='closed' (ExitPrice stays a pointer to match the column).
type PrepTrade struct {
	ID         int64
	Symbol     string
	SymbolRoot string
	Direction  string
	EntryTime  time.Time
	EntryPrice float64
	ExitTime   time.Time
	ExitPrice  *float64
	AccountID  int64
	Account    string
	NetPnl     *float64
}

// PreparedEntry is one journal entry day-prep produced (created or reused).
type PreparedEntry struct {
	EntryID         int64    `json:"entryId"`
	Scope           string   `json:"scope"`
	Created         bool     `json:"created"`
	AlreadyAnalyzed bool     `json:"alreadyAnalyzed"`
	Label           string   `json:"label"`
	SymbolRoot      string   `json:"symbolRoot,omitempty"`
	Direction       string   `json:"direction,omitempty"`
	Accounts        int      `json:"accounts,omitempty"`
	NetPnl          *float64 `json:"netPnl,omitempty"`
	TradeIDs        []int64  `json:"tradeIds,omitempty"`
}

// PrepResult is PrepareDay's answer: the resolved day, its entries, and the
// instruction the analyst follows (it names the MCP tools on purpose).
type PrepResult struct {
	Date    string          `json:"date"`
	Entries []PreparedEntry `json:"entries"`
	Note    string          `json:"note"`
}

// PrepareDay clusters the day's closed trades (copytrader fan-out -> group,
// solo -> trade), creates the missing entries idempotently with the
// placeholder body, annotates them, and returns them. dateStr "" resolves
// to the last trading day before today. The analyst workflow used to make
// the model cluster trades and pass raw trade-id arrays, which weaker
// models get wrong; doing it here means the model only ever handles the
// entry ids it gets back.
func (s *Service) PrepareDay(ctx context.Context, userID int64, dateStr string) (*PrepResult, error) {
	tz := s.tz()
	var day time.Time
	if v := strings.TrimSpace(dateStr); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, tz)
		if err != nil {
			return nil, BadInput("bad date; expected YYYY-MM-DD")
		}
		day = d
	} else {
		d, err := s.LastTradingDay(ctx, userID)
		if err != nil {
			return nil, err
		}
		if d.IsZero() {
			return &PrepResult{Entries: []PreparedEntry{}, Note: "no closed trades found on any prior day"}, nil
		}
		day = d
	}

	trades, err := s.LoadDayTrades(ctx, userID, day)
	if err != nil {
		return nil, err
	}
	groups, solos := ClusterCopytrader(trades)
	var ann Annotator
	if s.NewAnnotator != nil {
		ann = s.NewAnnotator()
	}
	annotate := func(entryID int64, t PrepTrade) {
		if ann != nil {
			ann.Annotate(ctx, entryID, t)
		}
	}

	entries := make([]PreparedEntry, 0, len(groups)+len(solos))
	for _, g := range groups {
		ids := tradeIDsOf(g)
		pe := PreparedEntry{
			Scope: "group", Label: clusterLabel(g), SymbolRoot: g[0].SymbolRoot,
			Direction: g[0].Direction, Accounts: distinctAccounts(g), NetPnl: sumNet(g), TradeIDs: ids,
		}
		if eid, ok, err := s.FindGroupForTrades(ctx, userID, ids); err != nil {
			return nil, err
		} else if ok {
			pe.EntryID, pe.Created, pe.AlreadyAnalyzed = eid, false, s.EntryHasMarker(ctx, userID, eid, MarkerMCP)
		} else {
			e, _, err := s.Create(ctx, userID, CreateInput{Scope: "group", TradeIDs: ids, Body: PlaceholderBody})
			if err != nil {
				return nil, err
			}
			pe.EntryID, pe.Created = e.ID, true
		}
		annotate(pe.EntryID, g[0])
		entries = append(entries, pe)
	}
	for _, t := range solos {
		tid := t.ID
		pe := PreparedEntry{
			Scope: "trade", Label: t.SymbolRoot + " " + t.Direction + " (" + t.Account + ")",
			SymbolRoot: t.SymbolRoot, Direction: t.Direction, NetPnl: t.NetPnl, TradeIDs: []int64{tid},
		}
		e, conflict, err := s.Create(ctx, userID, CreateInput{Scope: "trade", TradeID: &tid, Body: PlaceholderBody})
		if err != nil {
			return nil, err
		}
		if conflict != nil {
			pe.EntryID, pe.Created, pe.AlreadyAnalyzed = conflict.EntryID, false, s.EntryHasMarker(ctx, userID, conflict.EntryID, MarkerMCP)
		} else {
			pe.EntryID, pe.Created = e.ID, true
		}
		annotate(pe.EntryID, t)
		entries = append(entries, pe)
	}

	created := 0
	for _, e := range entries {
		if e.Created {
			created++
		}
	}
	slog.Debug("prepare_day", "date", day.Format("2006-01-02"), "trades", len(trades),
		"groups", len(groups), "solos", len(solos), "entries", len(entries), "created", created)

	return &PrepResult{
		Date:    day.Format("2006-01-02"),
		Entries: entries,
		Note:    "For each entry with alreadyAnalyzed=false: get_chart_image(entryId=...) (+ get_strategy if one is assigned), then append_note(entryId, analysis ending in <!-- " + MarkerMCP + " -->). Then synthesize the day. Only ever pass the entryId values above - never raw trade ids.",
	}, nil
}

// LastTradingDay returns the trading-tz date of the user's most recent
// closed trade strictly before today (zero time if none). Accounts hidden in
// Settings (excluded_from_stats) do not count, here or in LoadDayTrades: the
// flag keeps an account out of the default views, and the journal's
// prepare-day is one of them.
func (s *Service) LastTradingDay(ctx context.Context, userID int64) (time.Time, error) {
	tz := s.tz()
	var et time.Time
	err := s.DB.QueryRowContext(ctx,
		`SELECT t.exit_time FROM trades t JOIN accounts a ON a.id = t.account_id
		 WHERE a.user_id = ? AND a.active = 1 AND a.excluded_from_stats = 0 AND t.status = 'closed' AND t.exit_time IS NOT NULL AND t.exit_time < ?
		 ORDER BY t.exit_time DESC LIMIT 1`, userID, todayIn(tz).UTC()).Scan(&et)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("resolve last trading day: %w", err)
	}
	y, m, d := et.In(tz).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, tz), nil
}

// LoadDayTrades lists the user's closed trades that exited on the trading-tz
// calendar day (midnight in the trading zone), oldest entry first. The
// bounds are UTC instants: exit_time is UTC text and compares as text, so a
// local-midnight bound would be read as 00:00 UTC (handlers/daybounds.go).
func (s *Service) LoadDayTrades(ctx context.Context, userID int64, day time.Time) ([]PrepTrade, error) {
	fromU, toU := day.UTC(), day.AddDate(0, 0, 1).UTC()
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id, t.symbol, t.symbol_root, t.direction, t.entry_time, t.entry_price, t.exit_time, t.exit_price, t.account_id, a.name, t.net_pnl
		FROM trades t JOIN accounts a ON a.id = t.account_id
		WHERE t.status = 'closed' AND t.exit_time IS NOT NULL AND a.active = 1 AND a.excluded_from_stats = 0 AND a.user_id = ?
		  AND t.exit_time >= ? AND t.exit_time < ?
		ORDER BY t.entry_time ASC, t.id ASC`, userID, fromU, toU)
	if err != nil {
		return nil, fmt.Errorf("load day trades: %w", err)
	}
	defer rows.Close()
	var out []PrepTrade
	for rows.Next() {
		var t PrepTrade
		if err := rows.Scan(&t.ID, &t.Symbol, &t.SymbolRoot, &t.Direction, &t.EntryTime, &t.EntryPrice, &t.ExitTime, &t.ExitPrice, &t.AccountID, &t.Account, &t.NetPnl); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindGroupForTrades returns the id of an existing group entry of the user
// that already includes any of the given trades, so day-prep reuses it
// rather than creating a duplicate group.
func (s *Service) FindGroupForTrades(ctx context.Context, userID int64, ids []int64) (int64, bool, error) {
	if len(ids) == 0 {
		return 0, false, nil
	}
	ph := strings.Repeat("?,", len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, userID)
	for _, id := range ids {
		args = append(args, id)
	}
	var eid int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT jet.journal_entry_id FROM journal_entry_trades jet
		 JOIN journal_entries je ON je.id = jet.journal_entry_id
		 WHERE je.user_id = ? AND je.scope = 'group' AND jet.trade_id IN (`+ph[:len(ph)-1]+`) LIMIT 1`, args...).Scan(&eid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find group for trades: %w", err)
	}
	return eid, true, nil
}

// ClusterCopytrader groups trades that look like one decision fanned across
// accounts (the copy-set rule in internal/copyset: same underlying and
// direction, entries within two seconds, two or more accounts). Those
// clusters become group entries; everything else is a solo.
func ClusterCopytrader(trs []PrepTrade) (groups [][]PrepTrade, solos []PrepTrade) {
	cs := make([]copyset.Trade, len(trs))
	for i, t := range trs {
		cs[i] = copyset.Trade{ID: t.ID, AccountID: t.AccountID, SymbolRoot: t.SymbolRoot, Direction: t.Direction, EntryTime: t.EntryTime}
	}
	sets, solo := copyset.Cluster(cs)
	for _, g := range sets {
		grp := make([]PrepTrade, 0, len(g))
		for _, k := range g {
			grp = append(grp, trs[k])
		}
		groups = append(groups, grp)
	}
	for _, k := range solo {
		solos = append(solos, trs[k])
	}
	return groups, solos
}

// CopyClusterKey is the copy-set underlying key (see copyset.Key).
func CopyClusterKey(symbolRoot string) string { return copyset.Key(symbolRoot) }

func distinctAccounts(c []PrepTrade) int {
	seen := map[int64]bool{}
	for _, t := range c {
		seen[t.AccountID] = true
	}
	return len(seen)
}

func tradeIDsOf(c []PrepTrade) []int64 {
	ids := make([]int64, len(c))
	for i, t := range c {
		ids[i] = t.ID
	}
	return ids
}

func sumNet(c []PrepTrade) *float64 {
	var sum float64
	present := false
	for _, t := range c {
		if t.NetPnl != nil {
			sum += *t.NetPnl
			present = true
		}
	}
	if !present {
		return nil
	}
	return &sum
}

func clusterLabel(c []PrepTrade) string {
	return fmt.Sprintf("%s %s x%d accts", strings.Join(distinctRoots(c), "/"), c[0].Direction, distinctAccounts(c))
}

// distinctRoots returns the unique symbol roots in a cluster (sorted), so a
// cross-instrument copytrader group reads as e.g. "ES/MES".
func distinctRoots(c []PrepTrade) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c {
		if !seen[t.SymbolRoot] {
			seen[t.SymbolRoot] = true
			out = append(out, t.SymbolRoot)
		}
	}
	sort.Strings(out)
	return out
}
