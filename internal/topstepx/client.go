// Package topstepx syncs trade history from the TopstepX (ProjectX Gateway)
// API into the journal. The API delivers "half-turn trades" — one row per
// fill side, carrying broker-reported fees+commissions — which are aggregated
// per order and fed through the existing CSV import pipeline so API and CSV
// sources share account rows, dedup keys, and the FIFO position builder.
package topstepx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const DefaultBaseURL = "https://api.topstepx.com"

// Sentinel errors the sync service branches on. Both wrap the underlying
// APIError via errors.Join at the call site.
var (
	// ErrAuth marks invalid credentials or an expired/rejected token
	// (loginKey InvalidCredentials, HTTP 401, or validate failure).
	ErrAuth = errors.New("topstepx: authentication failed")
	// ErrSubscription marks a TopstepX login whose API subscription add-on
	// is missing or disabled (loginKey ApiSubscriptionNotFound /
	// ApiKeyAuthenticationDisabled).
	ErrSubscription = errors.New("topstepx: api subscription missing or disabled")
)

// loginKey error codes from the swagger enum (docs/projectx-openapi.json).
const (
	loginErrInvalidCredentials  = 3
	loginErrSubscriptionMissing = 9
	loginErrKeyAuthDisabled     = 10
)

// APIError is a non-sentinel API failure: unexpected HTTP status or a body
// with success=false and an errorCode we don't special-case.
type APIError struct {
	Op        string
	Status    int
	ErrorCode int
	Message   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("topstepx: %s: status %d, errorCode %d: %s", e.Op, e.Status, e.ErrorCode, e.Message)
}

// Client is a minimal ProjectX Gateway API client. All endpoints are JSON
// POST; everything except LoginKey carries an Authorization: Bearer token.
type Client struct {
	BaseURL string       // "" -> DefaultBaseURL; httptest server URL in tests
	HTTP    *http.Client // "" -> 15s-timeout client
	Logger  *slog.Logger // "" -> slog.Default()
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Account is one TopstepX trading account as reported by /api/Account/search.
// Name matches the AccountName column of the CSV exports (and therefore our
// accounts.account_ref); ID is the numeric id /api/Trade/search wants.
type Account struct {
	ID        int32   `json:"id"`
	Name      string  `json:"name"`
	Balance   float64 `json:"balance"`
	CanTrade  bool    `json:"canTrade"`
	IsVisible bool    `json:"isVisible"`
	Simulated bool    `json:"simulated"`
}

// HalfTrade is one half-turn trade (a single fill side) from /api/Trade/search.
type HalfTrade struct {
	ID                int64     `json:"id"`
	AccountID         int32     `json:"accountId"`
	ContractID        string    `json:"contractId"`
	CreationTimestamp time.Time `json:"creationTimestamp"`
	Price             float64   `json:"price"`
	ProfitAndLoss     *float64  `json:"profitAndLoss"` // null = opening half; ignored (FIFO derives P&L)
	Fees              float64   `json:"fees"`
	Commissions       *float64  `json:"commissions"`
	Side              int       `json:"side"` // 0 = Bid (buy), 1 = Ask (sell)
	Size              int32     `json:"size"`
	Voided            bool      `json:"voided"`
	OrderID           int64     `json:"orderId"`
}

// Contract is /api/Contract/searchById's model; Name is the human contract
// code (fallback symbol source when the contractId format surprises us).
type Contract struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	TickSize  float64 `json:"tickSize"`
	TickValue float64 `json:"tickValue"`
	SymbolID  string  `json:"symbolId"`
}

// apiEnvelope is the success/errorCode wrapper every response shares.
type apiEnvelope struct {
	Success      bool   `json:"success"`
	ErrorCode    int    `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// post sends a JSON POST and decodes the response into out (which must embed
// or include the envelope fields for the caller to inspect). token is added
// as a Bearer header when non-empty. Every call is debug-logged (never the
// token or apiKey).
func (c *Client) post(ctx context.Context, op, path, token string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("topstepx: %s: encode: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("topstepx: %s: request: %w", op, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	start := time.Now()
	resp, err := c.httpClient().Do(req)
	if err != nil {
		c.logger().Debug("topstepx api call failed", "op", op, "path", path, "err", err)
		return fmt.Errorf("topstepx: %s: %w", op, err)
	}
	defer resp.Body.Close()
	c.logger().Debug("topstepx api call", "op", op, "path", path,
		"status", resp.StatusCode, "duration_ms", time.Since(start).Milliseconds())

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w (%s: status %d)", ErrAuth, op, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &APIError{Op: op, Status: resp.StatusCode, Message: string(snippet)}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("topstepx: %s: decode: %w", op, err)
	}
	return nil
}

// LoginKey exchanges a user's API key for a session token (POST /api/Auth/loginKey).
func (c *Client) LoginKey(ctx context.Context, userName, apiKey string) (string, error) {
	var out struct {
		apiEnvelope
		Token string `json:"token"`
	}
	body := map[string]string{"userName": userName, "apiKey": apiKey}
	if err := c.post(ctx, "loginKey", "/api/Auth/loginKey", "", body, &out); err != nil {
		return "", err
	}
	if !out.Success {
		switch out.ErrorCode {
		case loginErrSubscriptionMissing, loginErrKeyAuthDisabled:
			return "", fmt.Errorf("%w (errorCode %d: %s)", ErrSubscription, out.ErrorCode, out.ErrorMessage)
		default:
			// Everything else (user not found, invalid credentials, ...) is
			// an auth failure from the caller's perspective.
			return "", fmt.Errorf("%w (errorCode %d: %s)", ErrAuth, out.ErrorCode, out.ErrorMessage)
		}
	}
	c.logger().Debug("topstepx login ok", "user", userName)
	return out.Token, nil
}

// Validate refreshes a session token (POST /api/Auth/validate). Returns the
// rolled token; ErrAuth when the session is no longer valid (caller falls
// back to LoginKey). An empty newToken with success=true means the current
// token is still good.
func (c *Client) Validate(ctx context.Context, token string) (string, error) {
	var out struct {
		apiEnvelope
		NewToken string `json:"newToken"`
	}
	if err := c.post(ctx, "validate", "/api/Auth/validate", token, struct{}{}, &out); err != nil {
		return "", err
	}
	if !out.Success {
		return "", fmt.Errorf("%w (validate errorCode %d: %s)", ErrAuth, out.ErrorCode, out.ErrorMessage)
	}
	if out.NewToken == "" {
		return token, nil
	}
	return out.NewToken, nil
}

// SearchAccounts lists the user's trading accounts (POST /api/Account/search).
func (c *Client) SearchAccounts(ctx context.Context, token string, onlyActive bool) ([]Account, error) {
	var out struct {
		apiEnvelope
		Accounts []Account `json:"accounts"`
	}
	body := map[string]bool{"onlyActiveAccounts": onlyActive}
	if err := c.post(ctx, "searchAccounts", "/api/Account/search", token, body, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, &APIError{Op: "searchAccounts", Status: http.StatusOK, ErrorCode: out.ErrorCode, Message: out.ErrorMessage}
	}
	c.logger().Debug("topstepx accounts", "count", len(out.Accounts))
	return out.Accounts, nil
}

// SearchTrades lists half-turn trades for one account in [start, end]
// (POST /api/Trade/search). Timestamps are sent RFC3339 in UTC.
func (c *Client) SearchTrades(ctx context.Context, token string, accountID int32, start, end time.Time) ([]HalfTrade, error) {
	var out struct {
		apiEnvelope
		Trades []HalfTrade `json:"trades"`
	}
	body := map[string]any{
		"accountId":      accountID,
		"startTimestamp": start.UTC().Format(time.RFC3339),
		"endTimestamp":   end.UTC().Format(time.RFC3339),
	}
	if err := c.post(ctx, "searchTrades", "/api/Trade/search", token, body, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, &APIError{Op: "searchTrades", Status: http.StatusOK, ErrorCode: out.ErrorCode, Message: out.ErrorMessage}
	}
	c.logger().Debug("topstepx trades", "account", accountID,
		"window_start", start.UTC().Format(time.RFC3339), "window_end", end.UTC().Format(time.RFC3339),
		"count", len(out.Trades))
	return out.Trades, nil
}

// SearchContractByID resolves a contractId to its contract metadata
// (POST /api/Contract/searchById) — the fallback when SymbolFromContractID
// cannot parse the id.
func (c *Client) SearchContractByID(ctx context.Context, token, contractID string) (Contract, error) {
	var out struct {
		apiEnvelope
		Contract Contract `json:"contract"`
	}
	body := map[string]string{"contractId": contractID}
	if err := c.post(ctx, "searchContractById", "/api/Contract/searchById", token, body, &out); err != nil {
		return Contract{}, err
	}
	if !out.Success {
		return Contract{}, &APIError{Op: "searchContractById", Status: http.StatusOK, ErrorCode: out.ErrorCode, Message: out.ErrorMessage}
	}
	return out.Contract, nil
}
