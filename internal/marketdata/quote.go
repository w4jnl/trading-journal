package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// QuoteSource is the optional "price right now" capability of a bar source.
// Yahoo's chart endpoint carries a live regularMarketPrice in its meta block
// even when it has no intraday bars for the day, which is exactly the case a
// bar cache cannot serve (holidays, thin sessions, the first minutes).
type QuoteSource interface {
	Quote(ctx context.Context, yahooSymbol string) (LatestQuote, error)
}

// Quote returns the freshest price the cache can offer: the source's live
// quote when it provides one, else the newest cached bar (LatestPrice).
func (c *CachingSource) Quote(ctx context.Context, yahooSymbol string) (LatestQuote, error) {
	if qs, ok := c.Source.(QuoteSource); ok {
		if q, err := qs.Quote(ctx, yahooSymbol); err == nil {
			return q, nil
		}
	}
	return c.LatestPrice(ctx, yahooSymbol)
}

// Quote implements QuoteSource for Yahoo: one small chart request whose meta
// block carries regularMarketPrice/regularMarketTime.
func (y *Yahoo) Quote(ctx context.Context, yahooSymbol string) (LatestQuote, error) {
	u := fmt.Sprintf("%s/v8/finance/chart/%s?interval=1m&range=1d", y.baseURL(), url.PathEscape(yahooSymbol))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return LatestQuote{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; TradingJournal/1.0)")
	req.Header.Set("Accept", "application/json")
	resp, err := y.client().Do(req)
	if err != nil {
		return LatestQuote{}, fmt.Errorf("marketdata: yahoo quote request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusTooManyRequests, http.StatusForbidden, http.StatusUnauthorized:
		return LatestQuote{}, ErrRateLimited
	case http.StatusNotFound:
		return LatestQuote{}, ErrNoData
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return LatestQuote{}, fmt.Errorf("marketdata: yahoo quote status %d: %s", resp.StatusCode, string(body))
	}
	var yr struct {
		Chart struct {
			Result []struct {
				Meta struct {
					RegularMarketPrice float64 `json:"regularMarketPrice"`
					RegularMarketTime  int64   `json:"regularMarketTime"`
				} `json:"meta"`
			} `json:"result"`
			Error *struct {
				Code        string `json:"code"`
				Description string `json:"description"`
			} `json:"error"`
		} `json:"chart"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&yr); err != nil {
		return LatestQuote{}, fmt.Errorf("marketdata: decode yahoo quote: %w", err)
	}
	if yr.Chart.Error != nil {
		return LatestQuote{}, fmt.Errorf("marketdata: yahoo error %s: %s", yr.Chart.Error.Code, yr.Chart.Error.Description)
	}
	if len(yr.Chart.Result) == 0 || yr.Chart.Result[0].Meta.RegularMarketPrice == 0 {
		return LatestQuote{}, ErrNoData
	}
	m := yr.Chart.Result[0].Meta
	at := time.Unix(m.RegularMarketTime, 0).UTC()
	if m.RegularMarketTime == 0 {
		at = time.Now().UTC()
	}
	return LatestQuote{Price: m.RegularMarketPrice, At: at}, nil
}
