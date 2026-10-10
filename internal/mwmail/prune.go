package mwmail

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Archive retention. The .eml archive is an audit trail that is not part of
// the backup and never shrinks on its own (8,800 files after one season).
// PruneArchive removes archived messages older than the cutoff by
// modification time; nothing else in the directory is touched.
func PruneArchive(dir string, cutoff time.Time) (removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".eml") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// RunArchiveRetention prunes once after a short delay and then daily until
// ctx is done. days <= 0 disables it.
func RunArchiveRetention(ctx context.Context, dir string, days int, log *slog.Logger) {
	if days <= 0 || dir == "" {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	run := func() {
		n, err := PruneArchive(dir, time.Now().AddDate(0, 0, -days))
		if err != nil {
			log.Warn("mw-email archive retention failed", "dir", dir, "err", err)
			return
		}
		if n > 0 {
			log.Info("mw-email archive retention", "removed", n, "older_than_days", days)
		}
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Minute):
	}
	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
