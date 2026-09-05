package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

const (
	testResource = "https://trading.example.com/mcp"
	testPRM      = "https://trading.example.com/.well-known/oauth-protected-resource"
)

// mintAccess stores an access token with the given properties and returns its
// plaintext, mirroring what the token endpoint does.
func mintAccess(t *testing.T, q *models.Queries, tag string, grantID, userID int64, clientID, resource string, ttl time.Duration) string {
	t.Helper()
	full := "tjo_" + tag + strings.Repeat("a", 43-len(tag))
	res := resource
	_, err := q.CreateOAuthAccessToken(context.Background(), models.CreateOAuthAccessTokenParams{
		TokenSha256: HashAPIKey(full), GrantID: grantID, ClientID: clientID, UserID: userID,
		Resource: &res, ExpiresAt: time.Now().UTC().Add(ttl),
	})
	if err != nil {
		t.Fatal(err)
	}
	return full
}

// /mcp keeps accepting tj_ API keys, additionally accepts live tjo_ access
// tokens minted for this resource, and challenges everything else with the
// RFC 9728 header so OAuth-capable clients can discover how to sign in.
func TestMCPAuthMiddleware(t *testing.T) {
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	user, err := q.CreateUser(ctx, models.CreateUserParams{Username: "keyholder"})
	if err != nil {
		t.Fatal(err)
	}
	full, prefix, sha, _ := GenerateAPIKey()
	if _, err := q.CreateAPIKey(ctx, models.CreateAPIKeyParams{UserID: user.ID, Name: "k", KeyPrefix: prefix, KeySha256: sha}); err != nil {
		t.Fatal(err)
	}
	client, err := q.CreateOAuthClient(ctx, models.CreateOAuthClientParams{ClientID: "tjc_test", RedirectUris: `["https://claude.ai/cb"]`})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := q.CreateOAuthGrant(ctx, models.CreateOAuthGrantParams{UserID: user.ID, ClientID: client.ClientID})
	if err != nil {
		t.Fatal(err)
	}
	revokedGrant, err := q.CreateOAuthGrant(ctx, models.CreateOAuthGrantParams{UserID: user.ID, ClientID: client.ClientID})
	if err != nil {
		t.Fatal(err)
	}
	_ = q.RevokeOAuthGrantByID(ctx, revokedGrant.ID)

	live := mintAccess(t, q, "live", grant.ID, user.ID, client.ClientID, testResource, time.Hour)
	expired := mintAccess(t, q, "expired", grant.ID, user.ID, client.ClientID, testResource, -time.Minute)
	wrongAud := mintAccess(t, q, "wrongaud", grant.ID, user.ID, client.ClientID, "https://other.example/mcp", time.Hour)
	deadGrant := mintAccess(t, q, "deadgrant", revokedGrant.ID, user.ID, client.ClientID, testResource, time.Hour)

	var seen *User
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = MustUser(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	mw := (&MCPAuth{Keys: &APIKeyAuth{Queries: q}, Queries: q, Resource: testResource, ResourceMetadataURL: testPRM}).Middleware(next)

	call := func(authz string) (int, string) {
		seen = nil
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		mw.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("WWW-Authenticate")
	}

	for name, tok := range map[string]string{"api key": full, "oauth token": live} {
		if code, ch := call("Bearer " + tok); code != http.StatusOK || ch != "" || seen == nil || seen.ID != user.ID || seen.Username != "keyholder" {
			t.Errorf("%s: code=%d challenge=%q user=%+v", name, code, ch, seen)
		}
	}

	if code, ch := call(""); code != http.StatusUnauthorized || ch != `Bearer resource_metadata="`+testPRM+`"` {
		t.Errorf("no token: code=%d challenge=%q", code, ch)
	}
	for name, tok := range map[string]string{
		"garbage":           "nonsense",
		"unknown oauth":     "tjo_" + strings.Repeat("z", 43),
		"expired":           expired,
		"wrong audience":    wrongAud,
		"revoked grant":     deadGrant,
		"refresh as bearer": "tjr_" + strings.Repeat("z", 43),
		"revoked key":       "tj_" + strings.Repeat("z", 43),
	} {
		code, ch := call("Bearer " + tok)
		if code != http.StatusUnauthorized || seen != nil {
			t.Errorf("%s: code=%d user=%+v", name, code, seen)
		}
		if !strings.HasPrefix(ch, `Bearer error="invalid_token"`) || !strings.Contains(ch, `resource_metadata="`+testPRM+`"`) {
			t.Errorf("%s: challenge %q", name, ch)
		}
	}
}
