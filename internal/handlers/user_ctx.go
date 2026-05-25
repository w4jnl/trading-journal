package handlers

import (
	"context"

	"github.com/w4jnl/trading-journal/internal/auth"
)

// userID returns the request-scoped user's id. Convenience wrapper around
// auth.MustUser for handler call sites that need an int64 directly.
func userID(ctx context.Context) int64 {
	return auth.MustUser(ctx).ID
}

// userIDPtr returns the request-scoped user's id as a *int64. sqlc generates
// pointer-typed user_id params for tables where the column is nullable
// (journal_entries, sync_log); the value is never actually nil in practice.
func userIDPtr(ctx context.Context) *int64 {
	id := auth.MustUser(ctx).ID
	return &id
}
