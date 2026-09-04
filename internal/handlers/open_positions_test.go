package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/w4jnl/trading-journal/internal/auth"
	"github.com/w4jnl/trading-journal/internal/models"
)

// Open-position rows link where the viewer may actually go: admins to the
// reconcile editor, everyone else to the account's trades for that
// instrument (the editor is behind RequireAdmin and would answer "admin
// only"). Data fields stay promoted for the partial.
func TestOpenPositionRowsLinkPerViewer(t *testing.T) {
	rows := []models.ListOpenPositionsRow{{AccountID: 16, AccountName: "U10000001", Symbol: "ESU6", SymbolRoot: "ES", NetQty: 1}}

	admin := auth.WithUser(context.Background(), &auth.User{ID: 1, Username: "a", IsAdmin: true})
	got := openPositionRows(admin, rows)
	if len(got) != 1 || got[0].Href != "/admin/transactions?account_id=16&symbol=ESU6" || got[0].AccountName != "U10000001" {
		t.Errorf("admin row = %+v", got)
	}

	user := auth.WithUser(context.Background(), &auth.User{ID: 2, Username: "u"})
	got = openPositionRows(user, rows)
	if len(got) != 1 || got[0].Href != "/trades?filter=acc%3A16&symbol_root=ES" {
		t.Errorf("non-admin row = %+v", got)
	}
	if strings.Contains(got[0].Href, "/admin/") {
		t.Errorf("non-admin must never be sent to an admin-only page")
	}
	// No user in context (defensive): the non-admin link.
	if got := openPositionRows(context.Background(), rows); len(got) != 1 || strings.Contains(got[0].Href, "/admin/") {
		t.Errorf("anonymous row = %+v", got)
	}
	// Empty input stays an empty, non-nil slice so the partial renders "flat".
	if got := openPositionRows(user, nil); got == nil || len(got) != 0 {
		t.Errorf("empty = %#v", got)
	}
}
