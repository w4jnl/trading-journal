package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/w4jnl/trading-journal/internal/models"
	_ "modernc.org/sqlite"
)

func TestHashAPIKeyDeterministic(t *testing.T) {
	a := HashAPIKey("tj_abc")
	if a != HashAPIKey("tj_abc") {
		t.Fatal("hash not deterministic")
	}
	if a == HashAPIKey("tj_abd") {
		t.Fatal("different inputs hashed equal")
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex len = %d, want 64", len(a))
	}
}

func TestGenerateAPIKey(t *testing.T) {
	full, prefix, sha, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 20 || full[:3] != "tj_" {
		t.Fatalf("full key %q lacks tj_ prefix / too short", full)
	}
	if prefix != full[:keyPrefixLen] {
		t.Errorf("prefix %q != full[:%d] %q", prefix, keyPrefixLen, full[:keyPrefixLen])
	}
	if sha != HashAPIKey(full) {
		t.Error("returned sha != HashAPIKey(full)")
	}
	full2, _, _, _ := GenerateAPIKey()
	if full2 == full {
		t.Error("two generated keys collided")
	}
}

func TestParseBearer(t *testing.T) {
	cases := []struct {
		hdr, val, want string
	}{
		{"Authorization", "Bearer tj_xyz", "tj_xyz"},
		{"Authorization", "bearer tj_xyz", "tj_xyz"}, // case-insensitive scheme
		{"X-API-Key", "tj_xyz", "tj_xyz"},
		{"Authorization", "", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/v1", nil)
		if c.val != "" {
			r.Header.Set(c.hdr, c.val)
		}
		if got := parseBearer(r); got != c.want {
			t.Errorf("parseBearer(%s=%q) = %q, want %q", c.hdr, c.val, got, c.want)
		}
	}
}

func apikeyTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, s := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT NOT NULL, email TEXT,
			password_hash TEXT, is_admin INTEGER NOT NULL DEFAULT 0, is_active INTEGER NOT NULL DEFAULT 1,
			display_name TEXT, created_at DATETIME, last_login_at DATETIME)`,
		`CREATE TABLE api_keys (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, name TEXT NOT NULL,
			key_prefix TEXT NOT NULL, key_sha256 TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, last_used_at DATETIME, revoked_at DATETIME)`,
		`INSERT INTO users (id, username, display_name, is_active) VALUES (1, 'trader', 'Trader', 1)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	return db
}

func TestAPIKeyMiddleware(t *testing.T) {
	db := apikeyTestDB(t)
	q := models.New(db)
	full, prefix, sha, _ := GenerateAPIKey()
	if _, err := q.CreateAPIKey(context.Background(), models.CreateAPIKeyParams{UserID: 1, Name: "test", KeyPrefix: prefix, KeySha256: sha}); err != nil {
		t.Fatalf("create key: %v", err)
	}
	a := &APIKeyAuth{Queries: q}

	var gotUserID int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserID = MustUser(r.Context()).ID
		w.WriteHeader(http.StatusOK)
	})
	h := a.Middleware(next)

	call := func(token string) int {
		gotUserID = 0
		r := httptest.NewRequest("GET", "/api/v1", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	if code := call(full); code != http.StatusOK || gotUserID != 1 {
		t.Errorf("valid key: code=%d user=%d, want 200 user 1", code, gotUserID)
	}
	if code := call("tj_bogus"); code != http.StatusUnauthorized {
		t.Errorf("bogus key: code=%d, want 401", code)
	}
	if code := call(""); code != http.StatusUnauthorized {
		t.Errorf("no key: code=%d, want 401", code)
	}
	// Deactivate the user -> 401 even with a valid key.
	if _, err := db.Exec(`UPDATE users SET is_active = 0 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if code := call(full); code != http.StatusUnauthorized {
		t.Errorf("inactive user: code=%d, want 401", code)
	}
	// Reactivate + revoke the key -> 401.
	_, _ = db.Exec(`UPDATE users SET is_active = 1 WHERE id = 1`)
	_, _ = db.Exec(`UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE key_sha256 = ?`, sha)
	if code := call(full); code != http.StatusUnauthorized {
		t.Errorf("revoked key: code=%d, want 401", code)
	}
}
