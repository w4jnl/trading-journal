package marketdata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
)

// LatestPrice returns the newest cached close and its time; a source failure
// still yields whatever was cached, and an empty cache is ErrNoData.
func TestLatestPrice(t *testing.T) {
	db := dbtest.Open(t, "bootstrap")
	loc := time.UTC
	now := time.Now().In(loc)
	t1 := now.Add(-40 * time.Minute).Truncate(time.Minute)
	t2 := now.Add(-20 * time.Minute).Truncate(time.Minute)
	fake := &fakeSource{bars: []Bar{
		{Time: t1.Unix(), Open: 6500, High: 6510, Low: 6495, Close: 6505, Volume: 1},
		{Time: t2.Unix(), Open: 6505, High: 6550, Low: 6500, Close: 6545.25, Volume: 1},
	}}
	c := &CachingSource{DB: db, Source: fake, Loc: loc}

	q, err := c.LatestPrice(context.Background(), "ES=F")
	if err != nil {
		t.Fatalf("LatestPrice: %v", err)
	}
	if q.Price != 6545.25 || !q.At.Equal(t2) {
		t.Errorf("quote = %+v, want 6545.25 @ %s", q, t2)
	}
	calls := fake.calls

	// Within the TTL nothing is re-fetched and the answer is served from cache.
	if q2, err := c.LatestPrice(context.Background(), "ES=F"); err != nil || q2 != q {
		t.Errorf("second call = %+v, %v", q2, err)
	}
	if fake.calls != calls {
		t.Errorf("second call re-fetched (%d -> %d calls) despite the intraday TTL", calls, fake.calls)
	}

	// Source down: cached bars still answer.
	fake.err = errors.New("vendor down")
	c.IntradayTTL = time.Nanosecond // force a refresh attempt
	time.Sleep(2 * time.Millisecond)
	if q3, err := c.LatestPrice(context.Background(), "ES=F"); err != nil || q3.Price != 6545.25 {
		t.Errorf("with source down = %+v, %v (want cached 6545.25)", q3, err)
	}

	// Never cached and source down -> the error surfaces; never cached and the
	// source has nothing -> ErrNoData.
	if _, err := c.LatestPrice(context.Background(), "NQ=F"); err == nil {
		t.Errorf("uncached symbol with source down must fail")
	}
	fake.err = nil
	fake.bars = nil
	if _, err := c.LatestPrice(context.Background(), "CL=F"); !errors.Is(err, ErrNoData) {
		t.Errorf("empty source: err = %v, want ErrNoData", err)
	}
}
