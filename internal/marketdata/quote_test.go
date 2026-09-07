package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
)

// Yahoo's meta quote is served even when the day has no intraday bars - the
// exact situation a bar cache cannot answer.
func TestYahooQuote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v8/finance/chart/ES=F" || r.URL.Query().Get("range") != "1d" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"chart":{"result":[{"meta":{"regularMarketPrice":7722.0,"regularMarketTime":1788780267},"timestamp":[],"indicators":{"quote":[{}]}}],"error":null}}`))
	}))
	defer srv.Close()
	y := &Yahoo{BaseURL: srv.URL, Client: srv.Client()}
	q, err := y.Quote(context.Background(), "ES=F")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price != 7722.0 || q.At.Unix() != 1788780267 {
		t.Errorf("quote = %+v", q)
	}
	if _, err := y.Quote(context.Background(), "NOPE=F"); err == nil {
		t.Errorf("unknown symbol must fail")
	}
}

// quoteFake is a bar source that also offers a live quote.
type quoteFake struct {
	fakeSource
	quote LatestQuote
	err   error
}

func (q *quoteFake) Quote(context.Context, string) (LatestQuote, error) { return q.quote, q.err }

// CachingSource.Quote prefers the source's live quote and falls back to the
// newest cached bar when the source has none.
func TestCachingSourceQuotePrefersLive(t *testing.T) {
	db := dbtest.Open(t, "bootstrap")
	bar := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	live := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	src := &quoteFake{fakeSource: fakeSource{bars: []Bar{{Time: bar.Unix(), Close: 6505, Open: 6505, High: 6505, Low: 6505, Volume: 1}}},
		quote: LatestQuote{Price: 6545.25, At: live}}
	c := &CachingSource{DB: db, Source: src, Loc: time.UTC}

	q, err := c.Quote(context.Background(), "ES=F")
	if err != nil || q.Price != 6545.25 || !q.At.Equal(live) {
		t.Errorf("live quote = %+v, %v", q, err)
	}
	src.err = ErrNoData
	q, err = c.Quote(context.Background(), "ES=F")
	if err != nil || q.Price != 6505 || !q.At.Equal(bar) {
		t.Errorf("bar fallback = %+v, %v", q, err)
	}
	// A plain bar source (no Quote) goes straight to bars.
	plain := &CachingSource{DB: db, Source: &fakeSource{bars: src.bars}, Loc: time.UTC}
	if q, err := plain.Quote(context.Background(), "ES=F"); err != nil || q.Price != 6505 {
		t.Errorf("plain source = %+v, %v", q, err)
	}
}
