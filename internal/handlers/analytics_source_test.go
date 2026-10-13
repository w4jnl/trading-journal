package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// "Confirmed by me" leaves out the tag, strategy and labels written through
// the API (source 'api') and keeps what the trader set.
func TestAnalyticsConfirmedOnly(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	u := itoa64(uid)
	for _, stmt := range []string{
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref) VALUES (1, ` + u + `, 'mine', 'motivewave_csv', 'A-1')`,
		`INSERT INTO strategies (id, user_id, name, body) VALUES (5, ` + u + `, 'ORB', 'x')`,
		`INSERT INTO trades (id, account_id, symbol, symbol_root, direction, qty, entry_price, exit_price, entry_time, exit_time, status, net_pnl) VALUES
			(10, 1, 'ESU6', 'ES', 'long', 1, 100, 102, '2026-10-06 14:00:00 +0000 UTC', '2026-10-06 15:00:00 +0000 UTC', 'closed', 100),
			(11, 1, 'ESU6', 'ES', 'long', 1, 100, 99, '2026-10-07 14:00:00 +0000 UTC', '2026-10-07 15:00:00 +0000 UTC', 'closed', -50)`,
		// 10: the trader's own tag, strategy and label. 11: all set by the API.
		`INSERT INTO journal_entries (id, user_id, trade_id, entry_date, body, scope, tag, tag_source, strategy_id, strategy_source) VALUES
			(1, ` + u + `, 10, '2026-10-06', 'x', 'trade', 'good', 'trader', 5, 'trader'),
			(2, ` + u + `, 11, '2026-10-07', 'x', 'trade', 'bad', 'api', 5, 'api')`,
		`INSERT INTO journal_entry_labels (journal_entry_id, kind, label, source) VALUES (1, 'mistake', 'chased', 'trader'), (2, 'mistake', 'fomo', 'api')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn), Timezone: time.UTC}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
	get := func(ctx context.Context) map[int64]analyticsTrade {
		ts, err := h.fetchAnalyticsTrades(ctx, accountFilter{}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]analyticsTrade{}
		for _, tr := range ts {
			out[tr.ID] = tr
		}
		return out
	}
	all := get(ctx)
	if all[11].Tag != "bad" || all[11].Strategy != "ORB" || len(all[11].Mistakes) != 1 {
		t.Errorf("all: %+v", all[11])
	}
	mine := get(withConfirmedOnly(ctx))
	if mine[11].Tag != "" || mine[11].Strategy != "" || len(mine[11].Mistakes) != 0 {
		t.Errorf("confirmed only kept API values: %+v", mine[11])
	}
	if mine[10].Tag != "good" || mine[10].Strategy != "ORB" || len(mine[10].Mistakes) != 1 {
		t.Errorf("confirmed only dropped the trader's values: %+v", mine[10])
	}
}
