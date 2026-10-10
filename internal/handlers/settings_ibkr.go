package handlers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// ibkrGuard redirects with a helpful error when IBKR live sync is disabled
// on this server. Returns false when the caller should stop.
func (h *Handlers) ibkrGuard(w http.ResponseWriter, r *http.Request) bool {
	if h.IbkrSync == nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(errors.New(
			"IBKR live sync is disabled on this server (ibkr.enabled in config.yaml)")), http.StatusSeeOther)
		return false
	}
	return true
}

// SettingsIbkrGatewayCreate: POST /settings/ibkr/gateways - register a
// Client Portal Gateway for the current user. No secrets involved: the
// gateway's session is established by the user's browser login; the app
// just needs the URL.
func (h *Handlers) SettingsIbkrGatewayCreate(w http.ResponseWriter, r *http.Request) {
	if !h.ibkrGuard(w, r) {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("gateway_name"))
	rawURL := strings.TrimSpace(strings.TrimSuffix(r.PostFormValue("gateway_url"), "/"))
	insecure := r.PostFormValue("tls_insecure") == "on"
	if name == "" || rawURL == "" {
		http.Redirect(w, r, "/settings?err=gateway+name+and+URL+are+required", http.StatusSeeOther)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		http.Redirect(w, r, "/settings?err=gateway+URL+must+be+http(s)%3A%2F%2Fhost%3Aport", http.StatusSeeOther)
		return
	}
	if !h.ibkrGatewayHostAllowed(ctx, u.Hostname()) {
		http.Redirect(w, r, "/settings?err="+url.QueryEscape(
			"gateway host "+u.Hostname()+" is not allowed - ask an admin to add it to ibkr.allowed_gateway_hosts"), http.StatusSeeOther)
		return
	}
	tlsInsecure := int64(0)
	if insecure {
		tlsInsecure = 1
	}
	if _, err := h.Queries.CreateIbkrGateway(ctx, models.CreateIbkrGatewayParams{
		UserID: userID(ctx), Name: name, Url: rawURL, TlsInsecure: tlsInsecure,
	}); err != nil {
		// UNIQUE(user_id, name) violation is the likely cause.
		http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf("could not add gateway (duplicate name?): %v", err)), http.StatusSeeOther)
		return
	}
	// Probe it right away so trades appear without waiting for the schedule.
	h.IbkrSync.MaybeEnqueueUser(userID(ctx))
	http.Redirect(w, r, "/settings?flash=ibkr-added", http.StatusSeeOther)
}

// ibkrGatewayFromPath resolves the {id} path param to the caller's gateway.
func (h *Handlers) ibkrGatewayFromPath(w http.ResponseWriter, r *http.Request) (models.IbkrGateway, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return models.IbkrGateway{}, false
	}
	gw, err := h.Queries.GetIbkrGateway(r.Context(), models.GetIbkrGatewayParams{ID: id, UserID: userID(r.Context())})
	if err != nil {
		http.Error(w, "gateway not found", http.StatusNotFound)
		return models.IbkrGateway{}, false
	}
	return gw, true
}

// SettingsIbkrGatewayTest: POST /settings/ibkr/gateways/{id}/test - live
// auth-status probe with a user-facing verdict.
func (h *Handlers) SettingsIbkrGatewayTest(w http.ResponseWriter, r *http.Request) {
	if !h.ibkrGuard(w, r) {
		return
	}
	gw, ok := h.ibkrGatewayFromPath(w, r)
	if !ok {
		return
	}
	st, err := h.IbkrSync.TestGateway(r.Context(), gw)
	switch {
	case err != nil:
		// The client error carries the gateway's status and a body snippet -
		// useful in the log, but echoing it to the page would turn the test
		// button into a read primitive against whatever host was registered.
		h.logger().Warn("ibkr gateway test failed", "gateway", gw.Name, "err", err)
		http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf("gateway %s is unreachable or returned an error (details in the server log)", gw.Name)), http.StatusSeeOther)
	case !st.Authenticated:
		http.Redirect(w, r, "/settings?err="+errToQuery(fmt.Errorf(
			"gateway %s responds but is not authenticated - open %s in a browser and log in", gw.Name, gw.Url)), http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/settings?flash=ibkr-test-ok", http.StatusSeeOther)
	}
}

// SettingsIbkrGatewayToggle: POST /settings/ibkr/gateways/{id}/toggle.
func (h *Handlers) SettingsIbkrGatewayToggle(w http.ResponseWriter, r *http.Request) {
	if !h.ibkrGuard(w, r) {
		return
	}
	gw, ok := h.ibkrGatewayFromPath(w, r)
	if !ok {
		return
	}
	next := int64(1)
	if gw.Enabled == 1 {
		next = 0
	}
	if err := h.Queries.SetIbkrGatewayEnabled(r.Context(), models.SetIbkrGatewayEnabledParams{
		Enabled: next, ID: gw.ID, UserID: userID(r.Context()),
	}); err != nil {
		h.serverError(w, fmt.Errorf("toggle gateway: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ibkr-updated", http.StatusSeeOther)
}

// SettingsIbkrGatewayDelete: POST /settings/ibkr/gateways/{id}/delete.
// Synced data and account cutovers stay - only the polling source goes away.
func (h *Handlers) SettingsIbkrGatewayDelete(w http.ResponseWriter, r *http.Request) {
	if !h.ibkrGuard(w, r) {
		return
	}
	gw, ok := h.ibkrGatewayFromPath(w, r)
	if !ok {
		return
	}
	if err := h.Queries.DeleteIbkrGateway(r.Context(), models.DeleteIbkrGatewayParams{
		ID: gw.ID, UserID: userID(r.Context()),
	}); err != nil {
		h.serverError(w, fmt.Errorf("delete gateway: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=ibkr-removed", http.StatusSeeOther)
}

// AccountsLivesyncReset: POST /accounts/{id}/livesync-reset - move the
// CSV/API cutover to now. Used to close a gap the 7-day API window can no
// longer see (gateway down too long): MotiveWave rows before the new cutover
// import again, so one fresh MW export backfills the hole.
func (h *Handlers) AccountsLivesyncReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.IbkrSync == nil {
		http.Redirect(w, r, "/accounts?err="+errToQuery(errors.New("IBKR live sync is disabled on this server")), http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	acct, err := h.Queries.GetAccountByID(ctx, models.GetAccountByIDParams{ID: id, UserID: userID(ctx)})
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if acct.LiveSyncSince == nil {
		http.Redirect(w, r, "/accounts?err=account+is+not+live-synced", http.StatusSeeOther)
		return
	}
	now := time.Now().UTC()
	if err := h.Queries.SetAccountLiveSyncSince(ctx, models.SetAccountLiveSyncSinceParams{
		LiveSyncSince: &now, ID: acct.ID, UserID: userID(ctx),
	}); err != nil {
		h.serverError(w, fmt.Errorf("reset cutover: %w", err))
		return
	}
	http.Redirect(w, r, "/accounts?flash=cutover-reset", http.StatusSeeOther)
}

// ibkrGatewayHostAllowed decides whether the caller may point the server at
// this gateway host. The server fetches the URL itself - a server-side
// request primitive - so non-admins are limited to ibkr.allowed_gateway_hosts
// (hostnames compared case-insensitively, IP literals, or CIDRs matched
// against an IP-literal host); admins may register anything.
func (h *Handlers) ibkrGatewayHostAllowed(ctx context.Context, host string) bool {
	if u := auth.UserFrom(ctx); u != nil && u.IsAdmin {
		return true
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	for _, a := range h.IbkrGatewayHosts {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
		case strings.Contains(a, "/"):
			if _, n, err := net.ParseCIDR(a); err == nil && ip != nil && n.Contains(ip) {
				return true
			}
		case a == host:
			return true
		}
	}
	return false
}
