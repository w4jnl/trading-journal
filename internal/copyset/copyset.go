// Package copyset finds copy sets: one trading decision fanned across
// accounts by a copytrader. Trades on the same underlying, in the same
// direction, with entries within two seconds of each other on two or more
// accounts are one set. Sets are derived from closed trades and rebuilt
// whole per user after every trade rebuild or import; nothing links to a
// set by id (journal entries link to the member trades).
package copyset

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/marketdata"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Window is how close two entries must be to count as the same decision.
const Window = 2 * time.Second

// GroupWindow is the window between two accounts the trader declared as
// copied together (accounts.copy_group): the copier adds latency, IBKR
// reports fills to the second and broker clocks differ, so legs of one
// decision can sit further apart than Window without being a new decision.
const GroupWindow = 30 * time.Second

// Trade is what clustering needs from a trade. CopyGroup is the account's
// copy group ("" for none).
type Trade struct {
	ID         int64
	AccountID  int64
	SymbolRoot string
	Direction  string
	EntryTime  time.Time
	CopyGroup  string
}

// sameDecision reports whether t belongs to the cluster that first opened:
// within Window, or within GroupWindow when both accounts are in the same
// declared copy group.
func sameDecision(first, t Trade) bool {
	if Key(t.SymbolRoot) != Key(first.SymbolRoot) || t.Direction != first.Direction {
		return false
	}
	gap := t.EntryTime.Sub(first.EntryTime)
	if gap <= Window {
		return true
	}
	return first.CopyGroup != "" && first.CopyGroup == t.CopyGroup && gap <= GroupWindow
}

// Key is the underlying-price key for clustering: micros fold into their
// full-size series (MES to ES=F, MNQ to NQ=F, ...) so a master's ES and a
// follower's MES at the same instant are one decision. Unmapped roots key
// on themselves.
func Key(symbolRoot string) string {
	if sym, ok := marketdata.YahooSymbol(symbolRoot); ok {
		return sym
	}
	return strings.ToUpper(strings.TrimSpace(symbolRoot))
}

// Cluster returns the index groups of trades that form a set (two or more
// distinct accounts), each ordered by entry time; every other index is a
// trade on its own. Chaining: a trade joins the cluster when its entry is
// within Window of the cluster's first entry.
func Cluster(trs []Trade) (sets [][]int, solos []int) {
	idx := make([]int, len(trs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		x, y := trs[idx[a]], trs[idx[b]]
		kx, ky := Key(x.SymbolRoot), Key(y.SymbolRoot)
		if kx != ky {
			return kx < ky
		}
		if x.Direction != y.Direction {
			return x.Direction < y.Direction
		}
		return x.EntryTime.Before(y.EntryTime)
	})
	for i := 0; i < len(idx); {
		first := trs[idx[i]]
		j := i + 1
		for j < len(idx) {
			if !sameDecision(first, trs[idx[j]]) {
				break
			}
			j++
		}
		group := idx[i:j]
		accounts := map[int64]bool{}
		for _, k := range group {
			accounts[trs[k].AccountID] = true
		}
		if len(accounts) >= 2 {
			sets = append(sets, append([]int(nil), group...))
		} else {
			solos = append(solos, group...)
		}
		i = j
	}
	return sets, solos
}

// Rebuild recomputes the user's sets from their closed trades. Returns how
// many sets were written.
func Rebuild(ctx context.Context, q *models.Queries, userID int64) (int, error) {
	rows, err := q.ListClosedTradesForSets(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("copyset: list trades: %w", err)
	}
	trs := make([]Trade, len(rows))
	for i, r := range rows {
		trs[i] = Trade{ID: r.ID, AccountID: r.AccountID, SymbolRoot: r.SymbolRoot, Direction: r.Direction, EntryTime: r.EntryTime, CopyGroup: groupOf(r.CopyGroup)}
	}
	sets, _ := Cluster(trs)
	if err := q.DeleteTradeSetsForUser(ctx, userID); err != nil {
		return 0, fmt.Errorf("copyset: clear: %w", err)
	}
	for _, g := range sets {
		p := summarize(userID, rows, g)
		id, err := q.InsertTradeSet(ctx, p)
		if err != nil {
			return 0, fmt.Errorf("copyset: insert set: %w", err)
		}
		for _, k := range g {
			if err := q.InsertTradeSetMember(ctx, models.InsertTradeSetMemberParams{TradeID: rows[k].ID, SetID: id}); err != nil {
				return 0, fmt.Errorf("copyset: insert member: %w", err)
			}
		}
	}
	return len(sets), nil
}

// RebuildForAccount rebuilds the sets of the account's owner (the import
// and sync paths know the account, not the user).
func RebuildForAccount(ctx context.Context, q *models.Queries, accountID int64) (int, error) {
	uid, err := q.GetAccountOwner(ctx, accountID)
	if err != nil {
		return 0, fmt.Errorf("copyset: account %d owner: %w", accountID, err)
	}
	return Rebuild(ctx, q, uid)
}

// Backfill builds sets for every user who has closed trades but no sets
// yet (the first start after the feature landed, or a database restored
// from before it). Users with sets are left alone: their sets follow the
// rebuild hooks. Errors are logged, never fatal.
func Backfill(ctx context.Context, q *models.Queries, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	users, err := q.ListUsers(ctx)
	if err != nil {
		log.Warn("copyset backfill: list users", "err", err)
		return
	}
	for _, u := range users {
		n, err := q.CountTradeSetsForUser(ctx, u.ID)
		if err != nil || n > 0 {
			continue
		}
		built, err := Rebuild(ctx, q, u.ID)
		if err != nil {
			log.Warn("copyset backfill", "user", u.ID, "err", err)
			continue
		}
		if built > 0 {
			log.Info("copyset backfill: sets built from existing trades", "user", u.ID, "sets", built)
		}
	}
}

// groupOf reads a nullable copy group as its name, "" for none.
func groupOf(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// summarize folds a cluster's members into the set row.
func summarize(userID int64, rows []models.ListClosedTradesForSetsRow, g []int) models.InsertTradeSetParams {
	first := rows[g[0]]
	p := models.InsertTradeSetParams{
		UserID: userID, ClusterKey: Key(first.SymbolRoot), SymbolRoot: first.SymbolRoot, Direction: first.Direction,
		EntryTime: first.EntryTime, Members: int64(len(g)),
	}
	accounts := map[int64]bool{}
	var entrySum, entryMin, entryMax, exitSum float64
	var exits int
	var gross, comm, net float64
	var hasGross, hasComm, hasNet bool
	for i, k := range g {
		r := rows[k]
		accounts[r.AccountID] = true
		p.Qty += r.Qty
		entrySum += r.EntryPrice
		if i == 0 || r.EntryPrice < entryMin {
			entryMin = r.EntryPrice
		}
		if i == 0 || r.EntryPrice > entryMax {
			entryMax = r.EntryPrice
		}
		if r.EntryTime.Before(p.EntryTime) {
			p.EntryTime = r.EntryTime
		}
		if r.ExitTime != nil && (p.ExitTime == nil || r.ExitTime.After(*p.ExitTime)) {
			t := *r.ExitTime
			p.ExitTime = &t
		}
		if r.ExitPrice != nil {
			exitSum += *r.ExitPrice
			exits++
		}
		if r.GrossPnl != nil {
			gross += *r.GrossPnl
			hasGross = true
		}
		if r.Commission != nil {
			comm += *r.Commission
			hasComm = true
		}
		if r.NetPnl != nil {
			net += *r.NetPnl
			hasNet = true
		}
	}
	p.Accounts = int64(len(accounts))
	p.EntryAvg = entrySum / float64(len(g))
	p.EntrySpread = entryMax - entryMin
	if exits > 0 {
		v := exitSum / float64(exits)
		p.ExitAvg = &v
	}
	if hasGross {
		p.GrossPnl = &gross
	}
	if hasComm {
		p.Commission = &comm
	}
	if hasNet {
		p.NetPnl = &net
	}
	return p
}
