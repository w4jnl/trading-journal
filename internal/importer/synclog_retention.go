package importer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// SyncLogRetention bounds the API polling history in sync_log. Every
// TopstepX / IBKR poll writes one row per account, thousands a day, and the
// dashboard and settings pages read the table on every render, so it must
// not grow without bound. Rules (see PurgeSyncLogBatch): API rows that
// added nothing and did not error go after NoOpDays; API rows that added
// fills or errored go after EventDays; the newest row per user and type and
// per user, type and account always stay, as do all CSV import rows.
type SyncLogRetention struct {
	Queries   *models.Queries
	NoOpDays  int   // 0 disables the purge entirely
	EventDays int   // default 90
	Batch     int64 // rows per DELETE; default 5000
	Logger    *slog.Logger
}

func (r *SyncLogRetention) eventDays() int {
	if r.EventDays > 0 {
		return r.EventDays
	}
	return 90
}

func (r *SyncLogRetention) batch() int64 {
	if r.Batch > 0 {
		return r.Batch
	}
	return 5000
}

func (r *SyncLogRetention) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// PurgeOnce deletes everything the retention rules allow, in batches so no
// single statement holds the write lock for long, and returns the row count.
// A zero NoOpDays is a no-op.
func (r *SyncLogRetention) PurgeOnce(ctx context.Context) (int64, error) {
	if r == nil || r.NoOpDays <= 0 {
		return 0, nil
	}
	params := models.PurgeSyncLogBatchParams{
		NoopAge:  fmt.Sprintf("-%d days", r.NoOpDays),
		EventAge: fmt.Sprintf("-%d days", r.eventDays()),
		Batch:    r.batch(),
	}
	var total int64
	for i := 0; i < 10000; i++ { // 50M rows at the default batch: a bound, not a limit
		n, err := r.Queries.PurgeSyncLogBatch(ctx, params)
		if err != nil {
			return total, err
		}
		total += n
		if n < params.Batch {
			return total, nil
		}
	}
	return total, nil
}

// Start purges once immediately (a backlog should clear on the first boot
// with retention enabled) and then every interval until ctx is done.
// Mirrors the session and OAuth janitors.
func (r *SyncLogRetention) Start(ctx context.Context, interval time.Duration) {
	run := func() {
		n, err := r.PurgeOnce(ctx)
		switch {
		case err != nil:
			r.logger().Warn("sync_log retention", "err", err)
		case n > 0:
			r.logger().Info("sync_log retention purged", "rows", n, "noop_days", r.NoOpDays, "event_days", r.eventDays())
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
