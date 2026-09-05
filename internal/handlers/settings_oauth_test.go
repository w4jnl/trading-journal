package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
	"github.com/w4jnl/trading-journal/internal/oauth"
)

// Connected-apps management is scoped by user in SQL (IDOR probe), revoke is
// one step and delete needs a prior revoke, and the list resolves the
// client's name and callback host for display.
func TestOAuthGrantSettings(t *testing.T) {
	conn, q := newAuthzTestDB(t)
	ctx := context.Background()
	alice, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	bob, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "bob"})
	name := "Claude Code"
	if _, err := q.CreateOAuthClient(ctx, models.CreateOAuthClientParams{ClientID: "tjc_cc", ClientName: &name, RedirectUris: `["http://localhost:4242/callback"]`}); err != nil {
		t.Fatal(err)
	}
	grant, err := q.CreateOAuthGrant(ctx, models.CreateOAuthGrantParams{UserID: alice.ID, ClientID: "tjc_cc"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{DB: conn, Queries: q, Timezone: time.UTC, OAuth: &oauth.Server{Queries: q, Cfg: oauth.Config{Issuer: "https://trading.example.com"}}}

	post := func(user *models.User, path string, fn http.HandlerFunc, id int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		req = req.WithContext(auth.WithUser(req.Context(), &auth.User{ID: user.ID, Username: user.Username, DisplayName: user.Username}))
		rec := httptest.NewRecorder()
		fn(rec, req)
		return rec
	}

	// Bob cannot revoke Alice's connection.
	rec := post(&bob, "/settings/oauth/1/revoke", h.OAuthGrantRevoke, grant.ID)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("IDOR revoke: code=%d loc=%q", rec.Code, rec.Header().Get("Location"))
	}
	if g, _ := q.GetOAuthGrantByID(ctx, grant.ID); g.RevokedAt != nil {
		t.Fatalf("bob revoked alice's grant")
	}
	// Alice sees it listed with name + callback host, live.
	rows := h.oauthGrantRows(auth.WithUser(ctx, &auth.User{ID: alice.ID}))
	if len(rows) != 1 || rows[0].ClientName != "Claude Code" || rows[0].RedirectHost != "localhost:4242" || rows[0].RevokedAt != nil {
		t.Errorf("rows = %+v", rows)
	}
	// Delete before revoke is refused (no-op) - two-step like API keys.
	post(&alice, "/settings/oauth/1/delete", h.OAuthGrantDelete, grant.ID)
	if _, err := q.GetOAuthGrantByID(ctx, grant.ID); err != nil {
		t.Errorf("live grant must not be deletable: %v", err)
	}
	// Alice revokes.
	rec = post(&alice, "/settings/oauth/1/revoke", h.OAuthGrantRevoke, grant.ID)
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != "/settings?flash=oauth-revoked" {
		t.Errorf("revoke: code=%d loc=%q", rec.Code, loc)
	}
	if g, _ := q.GetOAuthGrantByID(ctx, grant.ID); g.RevokedAt == nil {
		t.Errorf("grant not revoked")
	}
	// Revoking again reports not-found rather than pretending.
	if rec = post(&alice, "/settings/oauth/1/revoke", h.OAuthGrantRevoke, grant.ID); !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Errorf("double revoke: loc=%q", rec.Header().Get("Location"))
	}
	// Now delete works, and only for Alice.
	post(&bob, "/settings/oauth/1/delete", h.OAuthGrantDelete, grant.ID)
	if _, err := q.GetOAuthGrantByID(ctx, grant.ID); err != nil {
		t.Errorf("bob deleted alice's grant")
	}
	rec = post(&alice, "/settings/oauth/1/delete", h.OAuthGrantDelete, grant.ID)
	if loc := rec.Header().Get("Location"); loc != "/settings?flash=oauth-deleted" {
		t.Errorf("delete: loc=%q", loc)
	}
	if _, err := q.GetOAuthGrantByID(ctx, grant.ID); err == nil {
		t.Errorf("grant still present after delete")
	}
	if rows := h.oauthGrantRows(auth.WithUser(ctx, &auth.User{ID: alice.ID})); len(rows) != 0 {
		t.Errorf("rows after delete = %+v", rows)
	}

	// Feature off: handlers 404, list is nil.
	h.OAuth = nil
	if rec = post(&alice, "/settings/oauth/1/revoke", h.OAuthGrantRevoke, grant.ID); rec.Code != http.StatusNotFound {
		t.Errorf("disabled: code=%d", rec.Code)
	}
	if h.oauthGrantRows(auth.WithUser(ctx, &auth.User{ID: alice.ID})) != nil {
		t.Errorf("disabled: rows must be nil")
	}
}
