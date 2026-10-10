package marketdata

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPurgeIntradayKeepsDailyAndRecent(t *testing.T) {
	conn, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bars.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	for _, stmt := range []string{
		`CREATE TABLE price_bars (yahoo_symbol TEXT, timeframe TEXT, bar_time INTEGER, open REAL, high REAL, low REAL, close REAL, volume REAL, fetched_at INTEGER, PRIMARY KEY (yahoo_symbol, timeframe, bar_time))`,
		`CREATE TABLE price_bar_days (yahoo_symbol TEXT, timeframe TEXT, day TEXT, fetched_at INTEGER, PRIMARY KEY (yahoo_symbol, timeframe, day))`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.AddDate(0, 0, -120)
	recent := now.AddDate(0, 0, -10)
	ins := func(tf string, at time.Time) {
		if _, err := conn.Exec(`INSERT INTO price_bars VALUES ('ES=F', ?, ?, 1, 1, 1, 1, 0, 0)`, tf, at.Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(`INSERT OR IGNORE INTO price_bar_days VALUES ('ES=F', ?, ?, 0)`, tf, at.Format("2006-01-02")); err != nil {
			t.Fatal(err)
		}
	}
	ins("1m", old)
	ins("1m", recent)
	ins("1h", old)
	ins("1d", old) // daily bars are kept whatever their age

	c := &CachingSource{DB: conn, Loc: time.UTC}
	n, err := c.PurgeIntraday(context.Background(), now.AddDate(0, 0, -90))
	if err != nil || n != 2 {
		t.Fatalf("removed=%d err=%v, want 2 (old 1m + old 1h)", n, err)
	}
	count := func(q string) int {
		var k int
		if err := conn.QueryRow(q).Scan(&k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	if count(`SELECT count(*) FROM price_bars`) != 2 {
		t.Errorf("bars left = %d, want 2 (recent 1m, old 1d)", count(`SELECT count(*) FROM price_bars`))
	}
	if count(`SELECT count(*) FROM price_bar_days WHERE timeframe <> '1d'`) != 1 {
		t.Errorf("intraday coverage rows left = %d, want 1 (recent day)", count(`SELECT count(*) FROM price_bar_days WHERE timeframe <> '1d'`))
	}
	if count(`SELECT count(*) FROM price_bar_days WHERE timeframe = '1d'`) != 1 {
		t.Error("daily coverage row removed")
	}
	// Disabled retention never runs.
	done := make(chan struct{})
	go func() { Retention{Cache: c, Days: 0}.Start(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Retention with Days=0 should return immediately")
	}
}
