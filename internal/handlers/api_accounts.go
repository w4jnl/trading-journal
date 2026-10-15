package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ---- Accounts, balances and equity over the API -----------------------------
//
// GET /api/v1/accounts lists the user's accounts with the running balance
// each has (from its balance anchors, as the /accounts page computes it:
// loadBalances) and today's net; GET /api/v1/equity is the dashboard's and
// analytics' equity in one object: the total now by account type, the
// per-day curve, the drawdown over the closed trades, today's change. A
// device key may add and delete balance anchors.

// apiAccount is one account as the API shows it.
type apiAccount struct {
	ID                int64              `json:"id"`
	Name              string             `json:"name"`
	Firm              *string            `json:"firm"`
	Type              *string            `json:"type"`
	TypeLabel         string             `json:"typeLabel"`
	Currency          string             `json:"currency"`
	Active            bool               `json:"active"`
	Retired           bool               `json:"retired"`
	ExcludedFromStats bool               `json:"excludedFromStats"`
	NeedsSetup        bool               `json:"needsSetup"`
	CopyGroup         *string            `json:"copyGroup"`
	ImportSource      string             `json:"importSource"`
	Balance           *apiAccountBalance `json:"balance"`
	DayNet            *float64           `json:"dayNet"`
}

// apiAccountBalance is the running balance block, nil when the account
// has no anchor.
type apiAccountBalance struct {
	Balance              float64            `json:"balance"`
	AsOf                 string             `json:"asOf"`
	AnchorDate           string             `json:"anchorDate"`
	UnchangedSinceAnchor bool               `json:"unchangedSinceAnchor"`
	Anchors              []apiBalanceAnchor `json:"anchors"`
}

type apiBalanceAnchor struct {
	ID      int64   `json:"id"`
	Date    string  `json:"date"`
	Balance float64 `json:"balance"`
	Note    string  `json:"note"`
}

func apiAccountBalanceFrom(b AccountBalance, tz *time.Location) *apiAccountBalance {
	if !b.Known {
		return nil
	}
	out := &apiAccountBalance{Balance: b.Balance, AsOf: b.AsOf.In(tz).Format("2006-01-02"), AnchorDate: b.LatestDate.In(tz).Format("2006-01-02"),
		UnchangedSinceAnchor: b.HasNoChange, Anchors: make([]apiBalanceAnchor, 0, len(b.Anchors))}
	for _, a := range b.Anchors {
		out.Anchors = append(out.Anchors, apiBalanceAnchor{ID: a.ID, Date: a.AnchorDate.In(tz).Format("2006-01-02"), Balance: a.Balance, Note: a.Note})
	}
	return out
}

// dayNetByAccount is today's net per account from the daily_stats cache.
func (h *Handlers) dayNetByAccount(r *http.Request, day time.Time) map[int64]float64 {
	ctx := r.Context()
	rows, err := h.rq().ListDailyStatsForRange(ctx, models.ListDailyStatsForRangeParams{UserID: userID(ctx), StatDate: day, StatDate_2: day.AddDate(0, 0, 1)})
	if err != nil {
		return nil
	}
	out := map[int64]float64{}
	for _, row := range rows {
		out[row.AccountID] += row.NetPnl
	}
	return out
}

// APIAccountsList: GET /api/v1/accounts[?show_retired=1]
func (h *Handlers) APIAccountsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	accounts, err := h.rq().ListAccounts(ctx, userID(ctx))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not list accounts")
		return
	}
	balances, err := h.loadBalances(ctx, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not compute balances")
		return
	}
	dayNet := h.dayNetByAccount(r, todayLocal(tz))
	showRetired := apiQueryBool(r, "show_retired")
	items := make([]apiAccount, 0, len(accounts))
	for _, a := range accounts {
		if a.Active != 1 && !showRetired {
			continue
		}
		items = append(items, h.apiAccountFrom(a, balances, dayNet, tz))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handlers) apiAccountFrom(a models.Account, balances map[int64]AccountBalance, dayNet map[int64]float64, tz *time.Location) apiAccount {
	typeSlug := ""
	if a.Type != nil {
		typeSlug = *a.Type
	}
	out := apiAccount{ID: a.ID, Name: a.Name, Firm: a.Firm, Type: a.Type, TypeLabel: equityTypeLabel(typeSlug), Currency: a.Currency,
		Active: a.Active == 1, Retired: a.Active != 1, ExcludedFromStats: a.ExcludedFromStats != 0, NeedsSetup: a.NeedsSetup != 0,
		CopyGroup: a.CopyGroup, ImportSource: a.ImportSource}
	if b, ok := balances[a.ID]; ok {
		out.Balance = apiAccountBalanceFrom(b, tz)
	}
	if n, ok := dayNet[a.ID]; ok {
		out.DayNet = &n
	}
	return out
}

// apiQueryBool reads a ?flag=1|true|yes|on.
func apiQueryBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// apiOptionalDay parses an optional YYYY-MM-DD query value in the trading
// timezone (keywords allowed); ok false on a bad value.
func (h *Handlers) apiOptionalDay(r *http.Request, name string, tz *time.Location) (*time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, true
	}
	t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(r.Context(), raw, tz), tz)
	if err != nil {
		return nil, false
	}
	return &t, true
}

// APIEquity: GET /api/v1/equity?filter=&from=&to=
func (h *Handlers) APIEquity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tz := h.localTZ()
	af, err := h.resolveAccountFilter(bridgeLegacyAccountID(r))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not resolve the account filter")
		return
	}
	from, ok := h.apiOptionalDay(r, "from", tz)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad from; expected YYYY-MM-DD")
		return
	}
	to, ok := h.apiOptionalDay(r, "to", tz)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad to; expected YYYY-MM-DD")
		return
	}
	var toExclusive *time.Time
	if to != nil {
		t := to.AddDate(0, 0, 1)
		toExclusive = &t
	}
	xs, ys, equityNow, byType, included, excluded, err := h.balanceSeriesForFilter(ctx, af, from, toExclusive, tz)
	if err != nil {
		h.logger().Error("api equity", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "could not compute equity")
		return
	}
	var fromUTC, toUTC *time.Time
	if from != nil {
		t := from.UTC()
		fromUTC = &t
	}
	if toExclusive != nil {
		t := toExclusive.UTC()
		toUTC = &t
	}
	trades, err := h.fetchAnalyticsTrades(ctx, af, fromUTC, toUTC)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not load trades")
		return
	}
	today := todayLocal(tz)
	dayNet := h.dayNetByAccount(r, today)
	accounts, _ := h.rq().ListAccounts(ctx, userID(ctx))
	var dayChange float64
	for _, a := range accounts {
		in := false
		if af.Active {
			for _, id := range af.AccountIDs {
				if id == a.ID {
					in = true
				}
			}
		} else {
			in = a.Active == 1 && a.ExcludedFromStats == 0
		}
		if in {
			dayChange += dayNet[a.ID]
		}
	}
	series := make([]map[string]any, 0, len(xs))
	for i := range xs {
		series = append(series, map[string]any{"date": time.Unix(xs[i], 0).In(tz).Format("2006-01-02"), "equity": ys[i]})
	}
	types := make([]map[string]any, 0, len(byType))
	for _, t := range byType {
		types = append(types, map[string]any{"type": t.Type, "label": t.Label, "total": t.Total, "accounts": t.Accounts})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asOf":      today.Format("2006-01-02"),
		"equityNow": equityNow,
		"byType":    types,
		"included":  included,
		"excluded":  excluded,
		"series":    series,
		"drawdown":  computeDrawdown(trades),
		"dayChange": dayChange,
	})
}

// apiAccountPath resolves {id} to one of the caller's accounts.
func (h *Handlers) apiAccountPath(w http.ResponseWriter, r *http.Request) (models.Account, bool) {
	id, ok := apiPathID(r)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "bad account id")
		return models.Account{}, false
	}
	a, err := h.Queries.GetAccountByID(r.Context(), models.GetAccountByIDParams{ID: id, UserID: userID(r.Context())})
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "account not found")
		return models.Account{}, false
	}
	return a, true
}

// APIAccountBalanceAdd: POST /api/v1/accounts/{id}/balances (device key)
// {"date": "YYYY-MM-DD", "balance": 51230.5, "note": "..."} - record the
// account's balance on a day (a second anchor on the same day replaces).
func (h *Handlers) APIAccountBalanceAdd(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ctx := r.Context()
	tz := h.localTZ()
	a, ok := h.apiAccountPath(w, r)
	if !ok {
		return
	}
	var in struct {
		Date    string   `json:"date"`
		Balance *float64 `json:"balance"`
		Note    string   `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || in.Balance == nil {
		writeJSONError(w, http.StatusBadRequest, "balance is required")
		return
	}
	day := todayLocal(tz)
	if s := strings.TrimSpace(in.Date); s != "" {
		t, err := time.ParseInLocation("2006-01-02", h.resolveDayKeyword(ctx, s, tz), tz)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		day = t
	}
	var note *string
	if n := strings.TrimSpace(in.Note); n != "" {
		note = &n
	}
	if err := h.Queries.UpsertBalanceAnchor(ctx, models.UpsertBalanceAnchorParams{AccountID: a.ID, AnchorDate: day, Balance: *in.Balance, Note: note}); err != nil {
		h.serverError(w, fmt.Errorf("upsert anchor: %w", err))
		return
	}
	h.LiveData.Bump(userID(ctx))
	h.respondAccount(w, r, a, http.StatusCreated)
}

// respondAccount writes one account with its balance block.
func (h *Handlers) respondAccount(w http.ResponseWriter, r *http.Request, a models.Account, status int) {
	ctx := r.Context()
	tz := h.localTZ()
	balances, err := h.loadBalances(ctx, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not compute balances")
		return
	}
	writeJSON(w, status, map[string]any{"account": h.apiAccountFrom(a, balances, h.dayNetByAccount(r, todayLocal(tz)), tz)})
}

// APIAccountBalanceDelete: DELETE /api/v1/accounts/{id}/balances/{anchorId} (device key)
func (h *Handlers) APIAccountBalanceDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireDeviceKey(w, r) {
		return
	}
	ctx := r.Context()
	a, ok := h.apiAccountPath(w, r)
	if !ok {
		return
	}
	anchorID, err := strconv.ParseInt(r.PathValue("anchorId"), 10, 64)
	if err != nil || anchorID <= 0 {
		writeJSONError(w, http.StatusBadRequest, "bad anchor id")
		return
	}
	var accountID int64
	if err := h.DB.QueryRowContext(ctx, `SELECT account_id FROM balance_anchors WHERE id = ?`, anchorID).Scan(&accountID); err != nil || accountID != a.ID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			h.serverError(w, fmt.Errorf("lookup anchor: %w", err))
			return
		}
		writeJSONError(w, http.StatusNotFound, "anchor not found on this account")
		return
	}
	if err := h.Queries.DeleteBalanceAnchor(ctx, models.DeleteBalanceAnchorParams{ID: anchorID, UserID: auth.MustUser(ctx).ID}); err != nil {
		h.serverError(w, fmt.Errorf("delete anchor: %w", err))
		return
	}
	h.LiveData.Bump(userID(ctx))
	w.WriteHeader(http.StatusNoContent)
}
