// Package ibkr syncs executed trades from Interactive Brokers via one or
// more self-hosted Client Portal Gateways. The app never logs in - the
// gateway session is established by the user's browser login (2FA) outside
// the app; this package polls trades, keeps the session alive (tickle),
// restores a dropped brokerage session (reauthenticate) while the SSO
// session lives, and surfaces "needs manual login" when it cannot.
//
// Trades arrive only for a rolling 7-day window, so history remains the
// MotiveWave CSV's job; the accounts.live_sync_since cutover partitions the
// two eras (see internal/importer's filter).
package ibkr

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Sentinel errors the sync service branches on.
var (
	// ErrNotAuthenticated marks a gateway whose brokerage session is not
	// authenticated (auth/status authenticated=false, or HTTP 401).
	ErrNotAuthenticated = errors.New("ibkr: gateway session not authenticated")
	// ErrGatewayUnreachable marks a connection-level failure (gateway down,
	// DNS, TLS).
	ErrGatewayUnreachable = errors.New("ibkr: gateway unreachable")
)

// Client talks to one Client Portal Gateway. Gateways ship a self-signed
// TLS certificate by default; Insecure toggles verification per gateway.
type Client struct {
	BaseURL  string // e.g. "https://ibgw.example.internal:4005"; no trailing slash
	Insecure bool
	HTTP     *http.Client // nil -> built from Insecure with a 15s timeout
	Logger   *slog.Logger // nil -> slog.Default()
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	transport := http.DefaultTransport
	if c.Insecure {
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	c.HTTP = &http.Client{Timeout: 15 * time.Second, Transport: transport}
	return c.HTTP
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// AuthStatus is the gateway's session state from POST /iserver/auth/status.
type AuthStatus struct {
	Authenticated bool `json:"authenticated"`
	Connected     bool `json:"connected"`
	Competing     bool `json:"competing"`
}

// GatewayAccount is one entry of GET /iserver/accounts.
type GatewayAccount struct {
	ID string // e.g. "U10000001"
}

// Trade is one execution from GET /iserver/account/trades.
type Trade struct {
	ExecutionID  string  `json:"execution_id"`
	Symbol       string  `json:"symbol"`
	Description1 string  `json:"contract_description_1"`
	SecType      string  `json:"sec_type"`
	Conid        int64   `json:"conid"`
	Side         string  `json:"side"` // "B" / "S"
	Size         float64 `json:"size"`
	Price        jsonNum `json:"price"`      // gateway sometimes serializes as string
	Commission   jsonNum `json:"commission"` // per execution, may be absent
	TradeTime    string  `json:"trade_time"` // "20260504-12:23:31" (UTC)
	TradeTimeR   int64   `json:"trade_time_r"`
	Account      string  `json:"account"`
	Exchange     string  `json:"exchange"`
}

// Contract is GET /iserver/contract/{conid}/info - the symbol-resolution
// fallback when contract_description_1 doesn't parse.
type Contract struct {
	Conid       int64  `json:"con_id"`
	LocalSymbol string `json:"local_symbol"`
	Symbol      string `json:"symbol"`
	SecType     string `json:"instrument_type"`
}

// jsonNum tolerates a number arriving as either a JSON number or a string
// (the CP gateway does both depending on field/version).
type jsonNum float64

func (n *jsonNum) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" || string(b) == `""` {
		*n = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("ibkr: numeric string %q: %w", s, err)
		}
		*n = jsonNum(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*n = jsonNum(v)
	return nil
}

// do issues a request against the gateway and decodes JSON into out (nil to
// discard). Debug-logs every call; never logs cookies or bodies.
func (c *Client) do(ctx context.Context, op, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("ibkr: %s: request: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	// The gateway rejects requests without a User-Agent.
	req.Header.Set("User-Agent", "TradingJournal/1.0")

	start := time.Now()
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.logger().Debug("ibkr gateway call failed", "op", op, "url", c.BaseURL, "err", err)
		return fmt.Errorf("%w (%s: %v)", ErrGatewayUnreachable, op, err)
	}
	defer resp.Body.Close()
	c.logger().Debug("ibkr gateway call", "op", op, "url", c.BaseURL,
		"status", resp.StatusCode, "duration_ms", time.Since(start).Milliseconds())

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (%s: status %d)", ErrNotAuthenticated, op, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("ibkr: %s: status %d: %s", op, resp.StatusCode, string(snippet))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("ibkr: %s: decode: %w", op, err)
	}
	return nil
}

// Status checks the brokerage session (POST /iserver/auth/status).
func (c *Client) Status(ctx context.Context) (AuthStatus, error) {
	var st AuthStatus
	err := c.do(ctx, "authStatus", http.MethodPost, "/v1/api/iserver/auth/status", &st)
	return st, err
}

// Tickle keeps the session alive (POST /tickle); call ~every minute.
func (c *Client) Tickle(ctx context.Context) error {
	return c.do(ctx, "tickle", http.MethodPost, "/v1/api/tickle", nil)
}

// Reauthenticate restores a dropped brokerage session while the SSO session
// is still valid (POST /iserver/auth/reauthenticate).
func (c *Client) Reauthenticate(ctx context.Context) error {
	return c.do(ctx, "reauthenticate", http.MethodPost, "/v1/api/iserver/auth/reauthenticate", nil)
}

// Accounts lists the trading accounts of the gateway's login
// (GET /iserver/accounts).
func (c *Client) Accounts(ctx context.Context) ([]GatewayAccount, error) {
	var out struct {
		Accounts []string `json:"accounts"`
	}
	if err := c.do(ctx, "accounts", http.MethodGet, "/v1/api/iserver/accounts", &out); err != nil {
		return nil, err
	}
	accounts := make([]GatewayAccount, 0, len(out.Accounts))
	for _, id := range out.Accounts {
		accounts = append(accounts, GatewayAccount{ID: id})
	}
	c.logger().Debug("ibkr accounts", "url", c.BaseURL, "count", len(accounts))
	return accounts, nil
}

// Trades returns the executions of the last `days` days (max 7 - the
// endpoint's rolling window) across the login's accounts
// (GET /iserver/account/trades).
func (c *Client) Trades(ctx context.Context, days int) ([]Trade, error) {
	if days <= 0 || days > 7 {
		days = 7
	}
	var trades []Trade
	if err := c.do(ctx, "trades", http.MethodGet,
		fmt.Sprintf("/v1/api/iserver/account/trades?days=%d", days), &trades); err != nil {
		return nil, err
	}
	c.logger().Debug("ibkr trades", "url", c.BaseURL, "days", days, "count", len(trades))
	return trades, nil
}

// ContractInfo resolves a conid to contract metadata
// (GET /iserver/contract/{conid}/info) - the symbol fallback.
func (c *Client) ContractInfo(ctx context.Context, conid int64) (Contract, error) {
	var ct Contract
	err := c.do(ctx, "contractInfo", http.MethodGet,
		fmt.Sprintf("/v1/api/iserver/contract/%d/info", conid), &ct)
	return ct, err
}
