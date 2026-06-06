package marketdata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sampleChart is a trimmed real-shape Yahoo v8 response: 3 timestamps, the
// middle candle nulled out (a session gap) so the decoder must drop it.
const sampleChart = `{"chart":{"result":[{"meta":{"symbol":"ES=F","gmtoffset":-18000},
"timestamp":[1700000100,1700000400,1700000700],
"indicators":{"quote":[{
"open":[5010.0,null,5012.0],
"high":[5011.0,null,5013.5],
"low":[5009.5,null,5011.0],
"close":[5010.5,null,5013.0],
"volume":[120,null,80]
}]}}],"error":null}}`

func TestYahooFetchBars(t *testing.T) {
	var gotUA, gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sampleChart))
	}))
	defer srv.Close()

	y := &Yahoo{BaseURL: srv.URL, Client: srv.Client()}
	from := time.Unix(1700000000, 0)
	to := time.Unix(1700001000, 0)
	bars, err := y.FetchBars(context.Background(), "ES=F", TF5m, from, to)
	if err != nil {
		t.Fatalf("FetchBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars, want 2 (null candle dropped)", len(bars))
	}
	if bars[0].Time != 1700000100 || bars[0].Close != 5010.5 || bars[0].Volume != 120 {
		t.Errorf("bar0 = %+v", bars[0])
	}
	if bars[1].Time != 1700000700 || bars[1].High != 5013.5 {
		t.Errorf("bar1 = %+v", bars[1])
	}
	if !strings.Contains(gotUA, "Mozilla") {
		t.Errorf("User-Agent = %q, want browser-like", gotUA)
	}
	if !strings.Contains(gotURL, "interval=5m") ||
		!strings.Contains(gotURL, "period1=1700000000") ||
		!strings.Contains(gotURL, "/chart/ES=F") {
		t.Errorf("request URL = %q (symbol / interval / period missing)", gotURL)
	}
}

func TestYahooRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	y := &Yahoo{BaseURL: srv.URL, Client: srv.Client()}
	_, err := y.FetchBars(context.Background(), "ES=F", TF5m, time.Unix(0, 0), time.Unix(1, 0))
	if err != ErrRateLimited {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
}

func TestYahooNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chart":{"result":[],"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`))
	}))
	defer srv.Close()
	y := &Yahoo{BaseURL: srv.URL, Client: srv.Client()}
	_, err := y.FetchBars(context.Background(), "ZZ=F", TF5m, time.Unix(0, 0), time.Unix(1, 0))
	if err != ErrNoData {
		t.Errorf("err = %v, want ErrNoData", err)
	}
}
