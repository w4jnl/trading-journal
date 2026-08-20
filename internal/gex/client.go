// Package gex is a thin client for the gamma-exposure engine (gex-engine)
// snapshot API. The journal uses it to annotate reviewed trades with the
// gamma regime, flip side, and wall distances that were live at the trade's
// entry and exit moments (design §8 / P4).
//
// main.go maps config.GEX onto Options so this package never imports
// internal/config (mirrors ai / marketdata).
package gex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors let the annotator map a response to a skip instead of a
// warning. ErrNoSnapshot is the engine's 404: no snapshot at or before the
// requested moment (expected for trades before the engine's history starts).
var (
	ErrUnauthorized = errors.New("gex: unauthorized (check gex.api_key / TJ_GEX_API_KEY)")
	ErrNoSnapshot   = errors.New("gex: no snapshot at or before requested time")
	// ErrNoProfile is the profile endpoint's 404: an engine build that
	// predates GET /v1/profile. The Exposure page degrades to an
	// explanatory empty state rather than erroring.
	ErrNoProfile = errors.New("gex: profile endpoint unavailable (engine too old?)")
	// ErrNoExpiryData is the profile endpoint's 404 for ?expiry= against a
	// snapshot persisted before engine v0.7.0 or past the ~90-day blob
	// retention: "breakdown unavailable for this moment", not an error.
	ErrNoExpiryData = errors.New("gex: no per-expiry data for this snapshot")
	// ErrExpiryUnsupported means ?expiry= was requested but the engine
	// predates v0.7.0 and would have silently served the whole book.
	ErrExpiryUnsupported = errors.New("gex: engine does not support expiry filtering (needs v0.7.0+)")
)

// Options configures a Client.
type Options struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

// Client fetches point-in-time gamma snapshots from a gex-engine instance.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New validates Options and returns a Client. The default timeout is short on
// purpose: annotation runs inside the prepare-day request path and must never
// stall a review because the engine is down.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, fmt.Errorf("gex: base_url is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		apiKey:  opts.APIKey,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// Snapshot is the engine's point-in-time gamma state. Pointer fields are
// legitimately null in the API: percentile until 20 sessions are stored, flip
// when there is no zero crossing that day, every futures-denominated value
// when the basis is cold, and levels can be empty. Render those as "-",
// never treat them as errors.
type Snapshot struct {
	Market string    `json:"market"`
	AsOf   time.Time `json:"as_of"`
	// SpotIndex is always populated; SpotFutures is the engine's own
	// conversion of it (engine >= v0.3.0), null on index-only markets and
	// whenever the basis is cold. Prefer it over multiplying by the ratio -
	// the contract asks consumers never to re-derive a futures price.
	SpotIndex         float64            `json:"spot_index"`
	SpotFutures       *float64           `json:"spot_futures"`
	FuturesConversion *FuturesConversion `json:"futures_conversion"`
	Regime            Regime             `json:"regime"`
	Flip              Flip               `json:"flip"`
	// Flip0DTE is today's expiring book's zero crossing (engine v0.5.0,
	// update 002). JSON null - no crossing today, or a pre-v0.5.0 snapshot -
	// leaves both fields nil.
	Flip0DTE Flip `json:"flip_0dte"`
	// FlipIntraday is the front-week hedging flip: the zero crossing of the
	// <=5-calendar-DTE book (engine v0.6.0, update 003). It usually sits far
	// closer to spot than the whole-book flip, making it the more relevant
	// regime line for day trades. Null pre-v0.6.0 and on SPX/NDX.
	FlipIntraday Flip    `json:"flip_intraday"`
	Levels       []Level `json:"levels"`
	Profile      Profile `json:"profile"`
	// NetGexChange1H/1D compare the whole-book net GEX against the same
	// session's grid ~1h earlier / the prior session's final snapshot. They
	// are computed at SERVE time relative to the requested moment, so under
	// ?at= they are reproducible history, not drift from "now".
	NetGexChange1H *float64 `json:"net_gex_change_1h"`
	NetGexChange1D *float64 `json:"net_gex_change_1d"`
	ExpectedMove1D *float64 `json:"expected_move_1d"`
}

// Profile is the per-strike net-GEX curve behind the summary levels
// (index-denominated strikes, ~150 points). Powers the Market page's chart.
type Profile struct {
	StrikesIndex []float64 `json:"strikes_index"`
	NetGex       []float64 `json:"net_gex"`
}

// FuturesConversion records the index->futures ratio stored with the
// snapshot (historical provenance, not today's ratio).
type FuturesConversion struct {
	Ratio    float64   `json:"ratio"`
	Source   string    `json:"source"`
	PairedAt time.Time `json:"paired_at"`
}

// Regime is the book's net-GEX state.
type Regime struct {
	State string `json:"state"`
	// NetGexTotal is the whole-book net gamma - the sum across every strike,
	// which is why the profile's running total ends exactly there.
	// NetGexAtSpot is the engine's DEPRECATED alias carrying the identical
	// value; the old name misdescribed it (nothing is evaluated at spot). It
	// is still decoded so a pre-v0.3.1 engine keeps rendering. Both are
	// pointers so "field absent" is distinguishable from a genuine zero.
	NetGexTotal   *float64 `json:"net_gex_total"`
	NetGexAtSpot  *float64 `json:"net_gex_at_spot"`
	Percentile20D *float64 `json:"percentile_20d"`
	// IntradayState is the front-week regime - spot vs flip_intraday, derived
	// ENGINE-side at serve time (engine v0.6.1, update 004), so it stays
	// consistent with the repriced spot and is correct as-of-then under ?at=.
	// "positive" | "negative"; "" when null (pre-v0.6.1 or SPX/NDX). It
	// disagrees with State exactly when spot sits between the two flips -
	// the front-week book often turns negative while the whole book is long.
	IntradayState string `json:"intraday_state"`
}

// Total returns the whole-book net gamma, preferring the current field name
// and falling back to the deprecated alias. ok is false only when the engine
// sent neither.
func (r Regime) Total() (v float64, ok bool) {
	switch {
	case r.NetGexTotal != nil:
		return *r.NetGexTotal, true
	case r.NetGexAtSpot != nil:
		return *r.NetGexAtSpot, true
	}
	return 0, false
}

// Flip is the gamma zero-crossing level, in index and futures terms.
type Flip struct {
	Index   *float64 `json:"index"`
	Futures *float64 `json:"futures"`
}

// Level is one structural level. Types as of engine v0.6.0: call_wall /
// put_wall (bucket "all"; also bucket "0dte" for today's expiring book and
// "0dte_vol" for today's book weighted by session volume instead of OI),
// gamma_wall (buckets "0dte" / "0dte_vol"), gamma_1..gamma_3 (bucket "all",
// ranked gamma magnets with the walls excluded), gamma_near_1..gamma_near_3
// (bucket "all", magnets ranked within +/-1% of spot, walls and G1-G3
// excluded), and em_1d_min / em_1d_max (prior session close +/- its expected
// move, gex 0.0). Consumers must ignore types they do not recognize and
// match walls on type AND bucket.
type Level struct {
	Type    string   `json:"type"`
	Index   float64  `json:"index"`
	Futures *float64 `json:"futures"`
	Gex     float64  `json:"gex"`
	Bucket  string   `json:"bucket"`
}

// Snapshot returns the snapshot that was live at the given moment
// (at-or-before semantics; an overnight trade gets the prior session's close
// snapshot - callers must check AsOf against the trade time for staleness).
// market is "ES" or "NQ". at is sent in UTC RFC3339; the engine 422s naive
// timestamps.
func (c *Client) Snapshot(ctx context.Context, market string, at time.Time) (*Snapshot, error) {
	// RFC3339Nano, not RFC3339: snapshot as_of values carry a fractional
	// second (19:00:02.501055Z), and truncating to whole seconds asks for an
	// instant strictly BEFORE the snapshot, which at-or-before answers with
	// the previous one. Callers that pass a snapshot's own as_of back in -
	// pinning an analysis to it - would silently address its predecessor.
	u := c.baseURL + "/v1/snapshots/" + url.PathEscape(market) +
		"?at=" + url.QueryEscape(at.UTC().Format(time.RFC3339Nano))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("gex: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gex request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		return nil, ErrNoSnapshot
	default:
		snippet := body
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, fmt.Errorf("gex: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("gex: decode snapshot: %w", err)
	}
	return &s, nil
}

// ProfileSet is one per-strike GEX profile. Strikes are INDEX-denominated;
// scale by the payload's futures_conversion (v0.7.0+) or the snapshot's.
// call_gex/put_gex are the signed per-strike components whose sum is
// net_gex. CallOI/PutOI are plain contract counts (engine v0.7.0+); nil on
// older engines and pre-v0.7.0 snapshots - consumers fall back to the GEX
// split. On the blend view they sum index + ETF counts across different
// notionals (one SPX contract ~ 10 SPY), so label them as counts, not size.
type ProfileSet struct {
	StrikesIndex []float64 `json:"strikes_index"`
	NetGex       []float64 `json:"net_gex"`
	CallGex      []float64 `json:"call_gex"`
	PutGex       []float64 `json:"put_gex"`
	CallOI       []float64 `json:"call_oi"`
	PutOI        []float64 `json:"put_oi"`
}

// ProfileDetail is GET /v1/profile/{market}: the full per-strike profile
// set (index|etf|blend|0dte|0dte_vol) plus expiry_decay - net GEX rolling
// off per expiry date. Engine v0.7.0 added ?at= (snapshot semantics),
// ?expiry= filtering (served views index|etf|blend only; expiry_decay null
// on filtered responses), per-strike OI, and the futures fields.
type ProfileDetail struct {
	Market      string                `json:"market"`
	AsOf        time.Time             `json:"as_of"`
	OIDate      string                `json:"oi_date"`
	SpotIndex   float64               `json:"spot_index"`
	BucketWidth float64               `json:"bucket_width"`
	Sources     []string              `json:"sources"`
	Profiles    map[string]ProfileSet `json:"profiles"`
	ExpiryDecay map[string]float64    `json:"expiry_decay"`

	// SpotFutures/FuturesConversion mirror the snapshot's denomination
	// fields (v0.7.0+; null on SPX/NDX and when the basis is cold).
	SpotFutures       *float64           `json:"spot_futures"`
	FuturesConversion *FuturesConversion `json:"futures_conversion"`

	// ExpiryFilter echoes the applied ?expiry= set, sorted. SupportsExpiry
	// reports whether the engine emitted the expiry_filter KEY at all
	// (even as null): pre-v0.7.0 engines omit it and silently ignore the
	// param, so key presence is the only honest capability probe.
	ExpiryFilter   []string `json:"expiry_filter"`
	SupportsExpiry bool     `json:"-"`
}

// ProfileQuery narrows a ProfileDetail request. Zero value = live whole book.
type ProfileQuery struct {
	At       time.Time // at-or-before snapshot; zero = live
	Expiries []string  // ISO dates; the engine accepts comma-separated sets
}

// ProfileDetail fetches the profile set for market ("ES"/"NQ").
func (c *Client) ProfileDetail(ctx context.Context, market string, q ProfileQuery) (*ProfileDetail, error) {
	u := c.baseURL + "/v1/profile/" + url.PathEscape(market)
	vals := url.Values{}
	if !q.At.IsZero() {
		vals.Set("at", q.At.UTC().Format(time.RFC3339Nano))
	}
	if len(q.Expiries) > 0 {
		vals.Set("expiry", strings.Join(q.Expiries, ","))
	}
	if len(vals) > 0 {
		u += "?" + vals.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("gex: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gex request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	case http.StatusNotFound:
		// Three distinct 404s share this endpoint; the detail string is
		// the only discriminator the engine offers.
		switch {
		case strings.Contains(string(body), "per-expiry"):
			return nil, ErrNoExpiryData
		case strings.Contains(string(body), "no snapshot"):
			return nil, ErrNoSnapshot
		default:
			return nil, ErrNoProfile
		}
	default:
		snippet := body
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, fmt.Errorf("gex: status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var pd ProfileDetail
	if err := json.Unmarshal(body, &pd); err != nil {
		return nil, fmt.Errorf("gex: decode profile: %w", err)
	}
	var probe struct {
		ExpiryFilter json.RawMessage `json:"expiry_filter"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		pd.SupportsExpiry = len(probe.ExpiryFilter) > 0 // JSON null still counts
	}
	// A pre-v0.7.0 engine returns 200 with the UNFILTERED book for any
	// ?expiry= - presenting that as filtered would be silently wrong.
	if len(q.Expiries) > 0 && !pd.SupportsExpiry {
		return nil, ErrExpiryUnsupported
	}
	return &pd, nil
}
