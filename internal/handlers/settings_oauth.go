package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/w4jnl/trading-journal/internal/models"
)

// oauthGrantRow is one connected app on the Settings page: a client the user
// approved at /oauth/authorize. Revoked grants stay listed (like revoked API
// keys) until the user deletes the row.
type oauthGrantRow struct {
	ID           int64
	ClientName   string
	RedirectHost string // where the app receives the browser after approval
	Scope        string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	RevokedAt    *time.Time
}

// oauthGrantRows lists the user's OAuth grants for the Connected-apps card.
// Nil when the OAuth server is off (the card is not rendered); a lookup error
// logs and hides the list rather than failing the whole Settings page.
func (h *Handlers) oauthGrantRows(ctx context.Context) []oauthGrantRow {
	if h.OAuth == nil {
		return nil
	}
	rows, err := h.Queries.ListOAuthGrantsForUser(ctx, userID(ctx))
	if err != nil {
		h.logger().Warn("settings: list oauth grants", "err", err)
		return nil
	}
	out := make([]oauthGrantRow, 0, len(rows))
	for _, r := range rows {
		row := oauthGrantRow{ID: r.ID, ClientName: r.ClientName, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt}
		if row.ClientName == "" {
			row.ClientName = "Unnamed app"
		}
		if r.Scope != nil {
			row.Scope = *r.Scope
		}
		var uris []string
		if json.Unmarshal([]byte(r.RedirectUris), &uris) == nil && len(uris) > 0 {
			if u, err := url.Parse(uris[0]); err == nil {
				row.RedirectHost = u.Host
			}
		}
		out = append(out, row)
	}
	return out
}

// OAuthGrantRevoke: POST /settings/oauth/{id}/revoke. Cuts the app off at
// once - every access and refresh token under the grant fails on next use.
func (h *Handlers) OAuthGrantRevoke(w http.ResponseWriter, r *http.Request) {
	h.oauthGrantMutate(w, r, false)
}

// OAuthGrantDelete: POST /settings/oauth/{id}/delete. Removes a revoked row.
func (h *Handlers) OAuthGrantDelete(w http.ResponseWriter, r *http.Request) {
	h.oauthGrantMutate(w, r, true)
}

// oauthGrantMutate mirrors apiKeyMutate: id from the path, scoped by the
// signed-in user in SQL so a guessed id cannot touch another user's grant,
// then back to /settings with a flash.
func (h *Handlers) oauthGrantMutate(w http.ResponseWriter, r *http.Request, del bool) {
	if h.OAuth == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	uid := userID(ctx)
	flash := "oauth-revoked"
	if del {
		flash = "oauth-deleted"
		err = h.Queries.DeleteOAuthGrantForUser(ctx, models.DeleteOAuthGrantForUserParams{ID: id, UserID: uid})
	} else {
		var n int64
		n, err = h.Queries.RevokeOAuthGrantForUser(ctx, models.RevokeOAuthGrantForUserParams{ID: id, UserID: uid})
		if err == nil && n == 0 {
			http.Redirect(w, r, "/settings?err=connection+not+found+or+already+revoked", http.StatusSeeOther)
			return
		}
	}
	if err != nil {
		h.logger().Error("settings: oauth grant mutate", "err", err, "delete", del)
		http.Redirect(w, r, "/settings?err=could+not+update+connection", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?flash="+flash, http.StatusSeeOther)
}
