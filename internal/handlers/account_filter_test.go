package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/db/dbtest"
	"github.com/w4jnl/trading-journal/internal/models"
)

// The account filter resolves type slugs and explicit ids against the
// caller's own accounts only: another user's prop account never joins a
// type:prop selection, and acc:<theirs> resolves to nothing.
func TestResolveAccountFilterScopedToUser(t *testing.T) {
	conn := dbtest.Open(t, "boot")
	uid := dbtest.BootstrapUserID(t, conn, "boot")
	for _, stmt := range []string{
		`INSERT INTO users (id, username) VALUES (2, 'other')`,
		`INSERT INTO accounts (id, user_id, name, import_source, account_ref, type) VALUES
			(1, ` + itoa64(uid) + `, 'mine prop', 'motivewave_csv', 'A-1', 'prop'),
			(2, 2, 'their prop', 'motivewave_csv', 'A-2', 'prop'),
			(3, ` + itoa64(uid) + `, 'mine live', 'motivewave_csv', 'A-3', 'live')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{DB: conn, Queries: models.New(conn)}
	ctx := auth.WithUser(context.Background(), &auth.User{ID: uid})
	resolve := func(qs string) accountFilter {
		t.Helper()
		af, err := h.resolveAccountFilter(httptest.NewRequest("GET", "/trades?"+qs, nil).WithContext(ctx))
		if err != nil {
			t.Fatalf("%s: %v", qs, err)
		}
		return af
	}
	if af := resolve("filter=type:prop"); !af.Active || len(af.AccountIDs) != 1 || af.AccountIDs[0] != 1 {
		t.Errorf("type:prop -> %+v, want only account 1", af)
	}
	if af := resolve("filter=acc:2"); !af.Active || len(af.AccountIDs) != 0 {
		t.Errorf("acc:2 (another user's) -> %+v, want active and empty", af)
	}
	if af := resolve("filter=acc:3&filter=type:prop"); len(af.AccountIDs) != 2 || af.AccountIDs[0] != 1 || af.AccountIDs[1] != 3 || af.SelectedCount != 2 {
		t.Errorf("acc:3 + type:prop -> %+v, want [1 3]", af)
	}
	if af := resolve(""); af.Active {
		t.Errorf("no filter -> active: %+v", af)
	}
}
