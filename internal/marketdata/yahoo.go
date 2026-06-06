package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const defaultYahooBase = "https://query1.finance.yahoo.com"

// Yahoo is a BarSource backed by the public Yahoo Finance v8 chart endpoint.
// It serves CONTINUOUS front-month futures (ES=F, NQ=F, ...) - good market
// context for the liquid front-month contracts traded here, though the price
// basis differs slightly from a specific dated contract.
type Yahoo struct {
	BaseURL string       // override for tests; "" -> query1.finance.yahoo.com
	Client  *http.Client // "" -> a 10s-timeout client
}

func (y *Yahoo) baseURL() string {
	if y.BaseURL != "" {
		return y.BaseURL
	}
	return defaultYahooBase
}

func (y *Yahoo) client() *http.Client {
	if y.Client != nil {
		return y.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

type yahooResp struct {
	Chart struct {
		Result []struct {
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Open   []*float64 `json:"open"`
					High   []*float64 `json:"high"`
					Low    []*float64 `json:"low"`
					Close  []*float64 `json:"close"`
					Volume []*float64 `json:"volume"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// FetchBars implements BarSource.
func (y *Yahoo) FetchBars(ctx context.Context, yahooSymbol string, tf Timeframe, from, to time.Time) ([]Bar, error) {
	meta, ok := tfTable[tf]
	if !ok {
		return nil, fmt.Errorf("marketdata: unknown timeframe %q", tf)
	}
	u := fmt.Sprintf("%s/v8/finance/chart/%s?interval=%s&period1=%d&period2=%d&includePrePost=true",
		y.baseURL(), url.PathEscape(yahooSymbol), meta.yahooInterval, from.Unix(), to.Unix())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// Yahoo 403s the default Go user-agent; a browser-like UA is mandatory.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TradingJournal/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := y.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("marketdata: yahoo request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusForbidden, http.StatusUnauthorized:
		return nil, ErrRateLimited
	case http.StatusNotFound:
		return nil, ErrNoData
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("marketdata: yahoo status %d: %s", resp.StatusCode, string(body))
	}

	var yr yahooResp
	if err := json.NewDecoder(resp.Body).Decode(&yr); err != nil {
		return nil, fmt.Errorf("marketdata: decode yahoo: %w", err)
	}
	if yr.Chart.Error != nil {
		if yr.Chart.Error.Code == "Not Found" {
			return nil, ErrNoData
		}
		return nil, fmt.Errorf("marketdata: yahoo error %s: %s", yr.Chart.Error.Code, yr.Chart.Error.Description)
	}
	if len(yr.Chart.Result) == 0 || len(yr.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, ErrNoData
	}

	res := yr.Chart.Result[0]
	q := res.Indicators.Quote[0]
	bars := make([]Bar, 0, len(res.Timestamp))
	for i := range res.Timestamp {
		if i >= len(q.Open) || i >= len(q.High) || i >= len(q.Low) || i >= len(q.Close) {
			break
		}
		// Yahoo emits null OHLC for session gaps; skip those candles.
		if q.Open[i] == nil || q.High[i] == nil || q.Low[i] == nil || q.Close[i] == nil {
			continue
		}
		var vol float64
		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}
		bars = append(bars, Bar{
			Time:   res.Timestamp[i],
			Open:   *q.Open[i],
			High:   *q.High[i],
			Low:    *q.Low[i],
			Close:  *q.Close[i],
			Volume: vol,
		})
	}
	if len(bars) == 0 {
		return nil, ErrNoData
	}
	sort.Slice(bars, func(i, j int) bool { return bars[i].Time < bars[j].Time })
	return bars, nil
}
