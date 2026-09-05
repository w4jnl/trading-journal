package oauth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Purge removes only what has expired; live rows and revoked-but-unexpired
// rows survive (the latter so refresh-token reuse stays detectable).
func TestPurgeDeletesOnlyExpired(t *testing.T) {
	conn := dbtest.Open(t, "bootstrap")
	q := models.New(conn)
	ctx := context.Background()
	s := &Server{Queries: q, Cfg: Config{Issuer: testIssuer}}
	u, _ := q.CreateUser(ctx, models.CreateUserParams{Username: "alice"})
	c, _ := q.CreateOAuthClient(ctx, models.CreateOAuthClientParams{ClientID: "tjc_j", RedirectUris: `["https://claude.ai/cb"]`})
	g, _ := q.CreateOAuthGrant(ctx, models.CreateOAuthGrantParams{UserID: u.ID, ClientID: c.ClientID})
	past, future := time.Now().UTC().Add(-time.Hour), time.Now().UTC().Add(time.Hour)

	mk := func(kind, tag string, exp time.Time) string {
		sha := HashToken(kind + tag)
		var err error
		switch kind {
		case "code":
			_, err = q.CreateOAuthAuthCode(ctx, models.CreateOAuthAuthCodeParams{CodeSha256: sha, GrantID: g.ID, ClientID: c.ClientID, UserID: u.ID,
				RedirectUri: "https://claude.ai/cb", CodeChallenge: "x", CodeChallengeMethod: "S256", ExpiresAt: exp})
		case "access":
			_, err = q.CreateOAuthAccessToken(ctx, models.CreateOAuthAccessTokenParams{TokenSha256: sha, GrantID: g.ID, ClientID: c.ClientID, UserID: u.ID, ExpiresAt: exp})
		case "refresh":
			_, err = q.CreateOAuthRefreshToken(ctx, models.CreateOAuthRefreshTokenParams{TokenSha256: sha, GrantID: g.ID, ClientID: c.ClientID, UserID: u.ID, ExpiresAt: exp})
		}
		if err != nil {
			t.Fatalf("%s %s: %v", kind, tag, err)
		}
		return sha
	}
	codeOld, codeNew := mk("code", "old", past), mk("code", "new", future)
	mk("access", "old", past)
	mk("access", "new", future)
	rtOld, rtNew := mk("refresh", "old", past), mk("refresh", "new", future)
	// A revoked but unexpired refresh token must survive the purge.
	rtRevoked := mk("refresh", "revoked", future)
	if _, err := q.RotateOAuthRefreshToken(ctx, models.RotateOAuthRefreshTokenParams{ID: 3, RotatedTo: nil}); err != nil {
		t.Fatal(err)
	}

	if err := s.Purge(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := q.GetOAuthAuthCodeBySHA256(ctx, codeOld); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expired code must be gone, err=%v", err)
	}
	if _, err := q.GetOAuthAuthCodeBySHA256(ctx, codeNew); err != nil {
		t.Errorf("live code must remain: %v", err)
	}
	if _, err := q.GetOAuthRefreshTokenBySHA256(ctx, rtOld); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expired refresh must be gone, err=%v", err)
	}
	for _, sha := range []string{rtNew, rtRevoked} {
		if _, err := q.GetOAuthRefreshTokenBySHA256(ctx, sha); err != nil {
			t.Errorf("unexpired refresh must remain: %v", err)
		}
	}
	var nAccess int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM oauth_access_tokens").Scan(&nAccess); err != nil || nAccess != 1 {
		t.Errorf("access tokens after purge = %d (err %v), want 1", nAccess, err)
	}
}
