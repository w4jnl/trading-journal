package oauth

import (
	"context"
	"errors"
	"time"
)

// StartJanitor deletes expired codes and tokens every interval until ctx is
// done. Expired rows are already refused by every lookup (the SQL compares
// expires_at against CURRENT_TIMESTAMP), so this is housekeeping, not
// security: it keeps the tables small. Mirrors sessions.StartJanitor.
func (s *Server) StartJanitor(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Purge(ctx); err != nil {
				s.logger().Warn("oauth janitor", "err", err)
			}
		}
	}
}

// Purge runs one housekeeping pass. Revoked-but-unexpired rows are kept so a
// reused refresh token can still be recognized as reuse until it would have
// expired anyway.
func (s *Server) Purge(ctx context.Context) error {
	return errors.Join(
		s.Queries.DeleteExpiredOAuthAuthCodes(ctx),
		s.Queries.DeleteExpiredOAuthAccessTokens(ctx),
		s.Queries.DeleteExpiredOAuthRefreshTokens(ctx),
	)
}
