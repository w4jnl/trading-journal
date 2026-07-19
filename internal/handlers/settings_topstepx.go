package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/topstepx"
)

// topstepxGuard redirects with a helpful error when the feature is not
// configured on this server (no topstepx.encryption_key). Returns false when
// the caller should stop.
func (h *Handlers) topstepxGuard(w http.ResponseWriter, r *http.Request) bool {
	if h.TopstepxSync == nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(errors.New(
			"TopstepX sync is not configured on this server (set topstepx.encryption_key in config.yaml)")),
			http.StatusSeeOther)
		return false
	}
	return true
}

// SettingsTopstepxSave: POST /settings/topstepx - store (or replace) the
// user's TopstepX API credentials. The pair is verified with a live loginKey
// round-trip BEFORE being encrypted and stored, so bad credentials never
// enter the database silently.
func (h *Handlers) SettingsTopstepxSave(w http.ResponseWriter, r *http.Request) {
	if !h.topstepxGuard(w, r) {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("topstepx_username"))
	apiKey := strings.TrimSpace(r.PostFormValue("topstepx_api_key"))
	if username == "" || apiKey == "" {
		http.Redirect(w, r, "/settings?err=username+and+API+key+are+required", http.StatusSeeOther)
		return
	}
	if err := h.TopstepxSync.TestCredentials(ctx, username, apiKey); err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(topstepxUserError(err)), http.StatusSeeOther)
		return
	}
	ciphertext, nonce, err := h.TopstepxSync.Box.Seal(apiKey)
	if err != nil {
		h.serverError(w, fmt.Errorf("encrypt topstepx key: %w", err))
		return
	}
	if err := h.Queries.UpsertTopstepxCredentials(ctx, models.UpsertTopstepxCredentialsParams{
		UserID: userID(ctx), Username: username, ApiKeyEnc: ciphertext, ApiKeyNonce: nonce,
	}); err != nil {
		h.serverError(w, fmt.Errorf("store topstepx credentials: %w", err))
		return
	}
	// Kick off the first sync right away - the user just wired it up and
	// expects trades to appear.
	h.TopstepxSync.MaybeEnqueueUser(userID(ctx))
	http.Redirect(w, r, "/settings?flash=topstepx", http.StatusSeeOther)
}

// SettingsTopstepxTest: POST /settings/topstepx/test - re-verify the stored
// credentials with a live loginKey and refresh last_verified_at.
func (h *Handlers) SettingsTopstepxTest(w http.ResponseWriter, r *http.Request) {
	if !h.topstepxGuard(w, r) {
		return
	}
	ctx := r.Context()
	creds, err := h.Queries.GetTopstepxCredentials(ctx, userID(ctx))
	if err != nil {
		http.Redirect(w, r, "/settings?err=no+stored+TopstepX+credentials", http.StatusSeeOther)
		return
	}
	apiKey, err := h.TopstepxSync.Box.Open(creds.ApiKeyEnc, creds.ApiKeyNonce)
	if err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(errors.New(
			"stored credentials cannot be decrypted (encryption key changed?); remove and re-enter them")),
			http.StatusSeeOther)
		return
	}
	if err := h.TopstepxSync.TestCredentials(ctx, creds.Username, apiKey); err != nil {
		http.Redirect(w, r, "/settings?err="+errToQuery(topstepxUserError(err)), http.StatusSeeOther)
		return
	}
	if err := h.Queries.TouchTopstepxVerified(ctx, userID(ctx)); err != nil {
		h.logger().Warn("touch topstepx verified", "err", err)
	}
	http.Redirect(w, r, "/settings?flash=topstepx-test-ok", http.StatusSeeOther)
}

// SettingsTopstepxDelete: POST /settings/topstepx/delete - remove the stored
// credentials; automatic sync stops for this user.
func (h *Handlers) SettingsTopstepxDelete(w http.ResponseWriter, r *http.Request) {
	if !h.topstepxGuard(w, r) {
		return
	}
	ctx := r.Context()
	if err := h.Queries.DeleteTopstepxCredentials(ctx, userID(ctx)); err != nil {
		h.serverError(w, fmt.Errorf("delete topstepx credentials: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=topstepx-removed", http.StatusSeeOther)
}

// topstepxUserError converts client errors into actionable user-facing text.
func topstepxUserError(err error) error {
	switch {
	case errors.Is(err, topstepx.ErrSubscription):
		return errors.New("TopstepX rejected the key: API subscription missing or API-key auth disabled - enable API access in your TopstepX account first")
	case errors.Is(err, topstepx.ErrAuth):
		return errors.New("TopstepX rejected the credentials: check the username and API key")
	default:
		return fmt.Errorf("TopstepX API unreachable: %v", err)
	}
}
