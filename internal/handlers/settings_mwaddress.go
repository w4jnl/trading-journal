package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net/http"
	"strings"

	"github.com/w4jnl/trading-journal/internal/models"
)

// userMWFillAddress is the signed-in user's generated MotiveWave fill
// address, "" when none.
func (h *Handlers) userMWFillAddress(ctx context.Context) string {
	u, err := h.rq().GetUserByID(ctx, userID(ctx))
	if err != nil || u.MwFillAddress == nil {
		return ""
	}
	return *u.MwFillAddress
}

// newMWFillAddress makes fills-<token>@<domain>: ten lower-case base32
// characters from the system's random source, so the address is unique and
// not guessable, and plain enough to type into MotiveWave.
func newMWFillAddress(domain string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:10]
	return "fills-" + tok + "@" + domain, nil
}

// SettingsMWAddressSave handles POST /settings/mw-address. action=generate
// makes a new address (replacing the old one, whose mails then fall back to
// the other owner rules); action=clear removes it. The address is what the
// trader puts in MotiveWave's mail "To", so a fill mail for an unknown
// account names its owner without guessing.
func (h *Handlers) SettingsMWAddressSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.MWFillDomain == "" {
		http.Error(w, "the MotiveWave mail listener is not enabled", http.StatusNotFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	var addr *string
	switch r.PostFormValue("action") {
	case "generate":
		a, err := newMWFillAddress(h.MWFillDomain)
		if err != nil {
			h.serverError(w, fmt.Errorf("fill address: %w", err))
			return
		}
		addr = &a
	case "clear":
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err := h.Queries.SetUserMWFillAddress(ctx, models.SetUserMWFillAddressParams{MwFillAddress: addr, ID: userID(ctx)}); err != nil {
		h.serverError(w, fmt.Errorf("set fill address: %w", err))
		return
	}
	http.Redirect(w, r, "/settings?flash=mw-address#mw-address", http.StatusSeeOther)
}
