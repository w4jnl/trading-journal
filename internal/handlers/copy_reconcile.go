package handlers

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"

	"github.com/w4jnl/trading-journal/internal/models"
)

// copyRow is one fill in the merged leader+follower timeline for a symbol,
// carrying the running nets and the drift of the follower from its expected
// (factor x leader) position at that point.
type copyRow struct {
	Time        string
	Who         string // "leader" | "follower"
	Side        string
	Qty         float64
	Price       float64
	Ref         string
	LeaderNet   float64
	FollowerNet float64
	Expected    float64 // factor * LeaderNet
	Drift       float64 // FollowerNet - Expected; nonzero = divergence
}

// copySymbol is the per-symbol comparison of a follower account against
// factor x a leader account.
type copySymbol struct {
	Symbol      string
	LeaderNet   float64
	FollowerNet float64
	Expected    float64
	Drift       float64
	Diverged    bool
	Rows        []copyRow // trimmed to the first divergence onward (empty if aligned)
}

// CopyReconcilePage (GET /reconcile/copy) compares a copy-trading follower
// account against factor x a leader account, per symbol, and surfaces every
// point where the follower's net position drifts from the expected multiple -
// exactly the "the 2x account is short 3 at 14:22" hunt, done automatically.
// Read-only; scoped to the caller's own accounts.
func (h *Handlers) CopyReconcilePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := userID(ctx)

	accounts, err := h.Queries.ListAccounts(ctx, uid)
	if err != nil {
		h.serverError(w, fmt.Errorf("list accounts: %w", err))
		return
	}
	own := map[int64]models.Account{}
	for _, a := range accounts {
		own[a.ID] = a
	}

	q := r.URL.Query()
	leaderID, _ := strconv.ParseInt(q.Get("leader"), 10, 64)
	followerID, _ := strconv.ParseInt(q.Get("follower"), 10, 64)
	factor, ferr := strconv.ParseFloat(q.Get("factor"), 64)
	if ferr != nil || factor <= 0 {
		factor = 2
	}

	data := map[string]any{
		"NavActive": "accounts",
		"BackHref":  validBackHref(r),
		"Accounts":  accounts,
		"LeaderID":  leaderID,
		"Follower":  followerID,
		"Factor":    factor,
	}

	leader, lok := own[leaderID]
	follower, fok := own[followerID]
	if !lok || !fok || leaderID == followerID {
		if q.Get("leader") != "" || q.Get("follower") != "" {
			data["Err"] = "Pick two different accounts you own."
		}
		h.renderPage(w, r, "copy_reconcile.html", data)
		return
	}

	symbols, err := h.compareCopyAccounts(ctx, leaderID, followerID, factor)
	if err != nil {
		h.serverError(w, fmt.Errorf("compare: %w", err))
		return
	}
	divergedTotal := 0
	for _, s := range symbols {
		if s.Diverged {
			divergedTotal++
		}
	}
	data["LeaderName"] = leader.Name
	data["FollowerName"] = follower.Name
	data["Symbols"] = symbols
	data["DivergedCount"] = divergedTotal
	data["Compared"] = true
	h.renderPage(w, r, "copy_reconcile.html", data)
}

// compareCopyAccounts builds the per-symbol drift comparison from both
// accounts' live (non-deleted) executions.
func (h *Handlers) compareCopyAccounts(ctx context.Context, leaderID, followerID int64, factor float64) ([]copySymbol, error) {
	leaderEx, err := h.Queries.ListExecutionsByAccount(ctx, leaderID)
	if err != nil {
		return nil, err
	}
	followerEx, err := h.Queries.ListExecutionsByAccount(ctx, followerID)
	if err != nil {
		return nil, err
	}

	// Per symbol, a merged list of (execution, who) tagged rows.
	type tagged struct {
		e   models.Execution
		who string
	}
	bySym := map[string][]tagged{}
	add := func(exs []models.Execution, who string) {
		for _, e := range exs {
			if e.DeletedAt != nil {
				continue // live position only
			}
			bySym[e.Symbol] = append(bySym[e.Symbol], tagged{e, who})
		}
	}
	add(leaderEx, "leader")
	add(followerEx, "follower")

	symbols := make([]copySymbol, 0, len(bySym))
	for sym, rows := range bySym {
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].e.ExecTime.Equal(rows[j].e.ExecTime) {
				return rows[i].e.ExecTime.Before(rows[j].e.ExecTime)
			}
			// leader before follower at the same instant, then by id.
			if rows[i].who != rows[j].who {
				return rows[i].who == "leader"
			}
			return rows[i].e.ID < rows[j].e.ID
		})

		var lNet, fNet float64
		timeline := make([]copyRow, 0, len(rows))
		firstDiverge := -1
		for _, t := range rows {
			signed := t.e.Qty
			if t.e.Side == "sell" {
				signed = -t.e.Qty
			}
			if t.who == "leader" {
				lNet += signed
			} else {
				fNet += signed
			}
			expected := factor * lNet
			drift := fNet - expected
			if firstDiverge < 0 && !floatZero(drift) {
				firstDiverge = len(timeline)
			}
			timeline = append(timeline, copyRow{
				Time: t.e.ExecTime.In(h.localTZ()).Format("2006-01-02 15:04:05"),
				Who:  t.who, Side: t.e.Side, Qty: t.e.Qty, Price: t.e.Price, Ref: t.e.BrokerExecID,
				LeaderNet: lNet, FollowerNet: fNet, Expected: expected, Drift: drift,
			})
		}

		cs := copySymbol{
			Symbol: sym, LeaderNet: lNet, FollowerNet: fNet,
			Expected: factor * lNet, Drift: fNet - factor*lNet,
		}
		cs.Diverged = !floatZero(cs.Drift)
		if cs.Diverged && firstDiverge >= 0 {
			// Trim to one context row before the first divergence onward, so
			// the table shows where it broke without hundreds of aligned rows.
			start := firstDiverge - 1
			if start < 0 {
				start = 0
			}
			cs.Rows = timeline[start:]
		}
		symbols = append(symbols, cs)
	}
	// Diverged symbols first, then alphabetical.
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Diverged != symbols[j].Diverged {
			return symbols[i].Diverged
		}
		return symbols[i].Symbol < symbols[j].Symbol
	})
	return symbols, nil
}

// floatZero treats tiny residues as zero (contract quantities are whole, but
// factor multiplication can leave FP dust).
func floatZero(v float64) bool { return math.Abs(v) < 1e-9 }
