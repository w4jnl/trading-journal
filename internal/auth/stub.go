package auth

import (
	"database/sql"
	"fmt"
	"net/http"
)

// Stub is a single-user Authenticator that always resolves to the configured
// bootstrap user. Useful as the transitional authenticator while real
// local/proxy modes are being built out, and as a "no-auth on private VPN"
// option for trusted deployments.
type Stub struct {
	User *User
}

// NewStub loads the bootstrap user by username. Returns an error if the row
// is missing (the migration should have created it).
func NewStub(conn *sql.DB, username string) (*Stub, error) {
	var u User
	row := conn.QueryRow(
		`SELECT id, username, COALESCE(display_name, username), COALESCE(email, ''), is_admin
		 FROM users WHERE username = ?`, username)
	if err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.IsAdmin); err != nil {
		return nil, fmt.Errorf("load bootstrap user %q: %w", username, err)
	}
	return &Stub{User: &u}, nil
}

func (s *Stub) ResolveUser(_ *http.Request) (*User, error) { return s.User, nil }

func (s *Stub) Routes(_ *http.ServeMux) {}

func (s *Stub) HandleUnauthenticated(w http.ResponseWriter, _ *http.Request) {
	// Should not happen — ResolveUser always returns a user.
	http.Error(w, "no user", http.StatusInternalServerError)
}
