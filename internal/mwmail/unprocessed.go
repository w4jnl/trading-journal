package mwmail

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UnprocessedFill is one captured fill notification that the ingest could
// not attribute to an account (journaled skipped_unknown_account or
// skipped_ambiguous_account). The web layer surfaces these to admins on the
// accounts page - the email itself names no journal user, so the operator is
// the only sensible audience.
type UnprocessedFill struct {
	File     string // archive filename
	Action   string // the journaled skip action
	Account  string // account ref as the email reported it
	OrderID  string
	Symbol   string
	Side     string
	Qty      float64
	Price    float64
	ExecTime time.Time
}

// unattributedActions are the journal outcomes that mean "captured but not
// booked because no account matched". Other skips (authoritative, stale,
// live-sync, ...) are resolved states, not problems.
var unattributedActions = map[string]bool{
	"skipped_unknown_account":   true,
	"skipped_ambiguous_account": true,
}

// UnprocessedFills reads the replay journal in archiveDir and returns the
// parsed fills whose captures were journaled as unattributable, oldest
// first. Callers decide whether an entry is still a problem by re-resolving
// the account ref against current data - a ref that resolves today (alias
// set, account created) is stale journal history, not an alert.
//
// A missing archive dir or journal simply yields nil - the feature may be
// disabled or nothing captured yet.
func UnprocessedFills(archiveDir string) ([]UnprocessedFill, error) {
	raw, err := os.ReadFile(filepath.Join(archiveDir, replayJournalName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// LAST outcome per hash wins: a retry appends its new result, so a
	// capture that has since been ingested must stop being flagged.
	last := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && len(fields[0]) == 12 {
			last[fields[0]] = fields[1]
		}
	}
	flagged := map[string]string{} // hash -> action
	for hash, action := range last {
		if unattributedActions[action] {
			flagged[hash] = action
		}
	}
	if len(flagged) == 0 {
		return nil, nil
	}

	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return nil, err
	}
	var out []UnprocessedFill
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		hash := hashFromName(e.Name())
		action, ok := flagged[hash]
		if !ok {
			continue
		}
		body, err := os.ReadFile(filepath.Join(archiveDir, e.Name()))
		if err != nil {
			continue // listing is best-effort; the log already has the capture
		}
		res := Parse(body)
		out = append(out, UnprocessedFill{
			File:     e.Name(),
			Action:   action,
			Account:  res.Account,
			OrderID:  res.OrderID,
			Symbol:   res.Symbol,
			Side:     res.Side,
			Qty:      res.Qty,
			Price:    res.Price,
			ExecTime: res.ExecTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}
