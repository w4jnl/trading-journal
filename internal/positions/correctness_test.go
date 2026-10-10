package positions

import (
	"context"
	"testing"
)

// Two trades can open in the same second (one-second CSV timestamps; a sell
// that closes one position and opens the next at the same T). The relink
// after a rebuild used to look the trade up by entry_time alone, which
// attached a note to whichever of the two sorted first.
func TestRebuild_RelinkSurvivesSameEntryTime(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	ctx := context.Background()
	// buy 1 @T, sell 1 @T (closes A), buy 1 @T (opens B), sell 1 @T+1 (closes B):
	// trade A +2 pts and trade B -2 pts, both with entry_time T.
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 1, 7250, at(0), nil},
		{"sell", 1, 7252, at(0), nil},
		{"buy", 1, 7260, at(0), nil},
		{"sell", 1, 7258, at(1), nil},
	})
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(ctx, accID)
	if len(trades) != 2 {
		t.Fatalf("setup expected 2 trades, got %d", len(trades))
	}
	var loser int64
	for _, tr := range trades {
		if tr.GrossPnl != nil && *tr.GrossPnl < 0 {
			loser = tr.ID
		}
	}
	if loser == 0 {
		t.Fatal("setup: no losing trade")
	}
	if _, err := conn.Exec(`INSERT INTO journal_entries (trade_id, account_id, entry_date, body, scope) VALUES (?, ?, '2026-05-10', 'note on the loser', 'trade')`, loser, accID); err != nil {
		t.Fatal(err)
	}
	// A group note linked to the loser too.
	res, err := conn.Exec(`INSERT INTO journal_entries (account_id, entry_date, body, scope) VALUES (?, '2026-05-10', 'group note', 'group')`, accID)
	if err != nil {
		t.Fatal(err)
	}
	groupID, _ := res.LastInsertId()
	if _, err := conn.Exec(`INSERT INTO journal_entry_trades (journal_entry_id, trade_id) VALUES (?, ?)`, groupID, loser); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("second rebuild: %v", err)
	}
	var linked int64
	if err := conn.QueryRow(`SELECT trade_id FROM journal_entries WHERE body = 'note on the loser'`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	var gross float64
	if err := conn.QueryRow(`SELECT gross_pnl FROM trades WHERE id = ?`, linked).Scan(&gross); err != nil {
		t.Fatal(err)
	}
	if gross >= 0 {
		t.Errorf("note re-linked to a trade with gross %v; it belonged to the loser", gross)
	}
	var groupGross float64
	if err := conn.QueryRow(`SELECT t.gross_pnl FROM journal_entry_trades jet JOIN trades t ON t.id = jet.trade_id WHERE jet.journal_entry_id = ?`, groupID).Scan(&groupGross); err != nil {
		t.Fatalf("group link lost: %v", err)
	}
	if groupGross >= 0 {
		t.Errorf("group link re-attached to a trade with gross %v; it belonged to the loser", groupGross)
	}
}

// A complete tick spec is all an instrument needs for realized P&L (CLAUDE.md
// invariant: ticks x tick value); multiplier is only the legacy fallback.
func TestRebuild_TickSpecWithoutMultiplier(t *testing.T) {
	b, q, conn, accID := setupBuilder(t)
	ctx := context.Background()
	// Tick spec only - no multiplier, no commission classes (NULL classes
	// avoid the lookup-table FKs; no rate means commission stays NULL).
	if _, err := conn.Exec(`INSERT INTO instruments (symbol_root, exchange, tick_size, tick_value, multiplier, currency, needs_setup)
		VALUES ('GC', 'COMEX', 0.1, 10, NULL, 'USD', 0)`); err != nil {
		t.Fatal(err)
	}
	insertSynthExecutions(t, q, accID, "GCZ6", "GC", []synthFill{
		{"buy", 2, 2650.0, at(0), nil},
		{"sell", 2, 2651.5, at(5), nil},
	})
	if _, err := b.Rebuild(ctx, []Key{{accID, "GCZ6"}}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(ctx, accID)
	if len(trades) != 1 || trades[0].GrossPnl == nil {
		t.Fatalf("trades = %+v, want one with gross P&L", trades)
	}
	// 1.5 points = 15 ticks x $10 x 2 contracts = $300.
	if got := *trades[0].GrossPnl; got < 299.999 || got > 300.001 {
		t.Errorf("gross = %v, want 300 from tick math", got)
	}
}

// A zero-quantity fill (hand-edited or a broken feed) must not become a NaN
// trade; the builder skips it.
func TestRebuild_SkipsNonPositiveQty(t *testing.T) {
	b, q, _, accID := setupBuilder(t)
	ctx := context.Background()
	insertSynthExecutions(t, q, accID, "ESM6", "ES", []synthFill{
		{"buy", 0, 7000, at(0), nil}, // junk
		{"buy", 1, 7250, at(1), nil},
		{"sell", 1, 7252, at(5), nil},
	})
	if _, err := b.Rebuild(ctx, []Key{{accID, "ESM6"}}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	trades, _ := q.ListTradesForAccount(ctx, accID)
	if len(trades) != 1 || trades[0].EntryPrice != 7250 || trades[0].Qty != 1 {
		t.Fatalf("trades = %+v, want one clean trade from the real fills", trades)
	}
}
