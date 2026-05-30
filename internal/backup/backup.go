// Package backup takes self-contained snapshots of the app's on-disk state:
// a consistent SQLite copy plus the authored files (attachments, import audit
// trail, config). Archives are written as backup-YYYY-MM-DD.tar.gz and pruned
// grandfather-father-son: the 10 newest are kept, and one per calendar month
// is kept forever for anything older.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// keepDaily is the number of most-recent archives kept regardless of age.
const keepDaily = 10

// Options configures a backup run. All paths are as provided to the server.
type Options struct {
	Dir         string         // backup output directory, e.g. ./backups
	Hour        int            // local hour (0-23) for the daily run
	Loc         *time.Location // server timezone for scheduling + date naming
	DBPath      string         // live SQLite path (cfg.Database.Path)
	ConfigPath  string         // config.yaml path (the -config flag value)
	ImportsRoot string         // cfg.Imports.Root; processed/ + failed/ are archived
	DB          *sql.DB        // shared connection; VACUUM INTO runs on it
}

var archiveDateRe = regexp.MustCompile(`^backup-(\d{4}-\d{2}-\d{2})\.tar\.gz$`)

// Schedule runs an initial backup immediately (deduped: skipped if today's
// archive already exists), then fires daily at o.Hour local time until ctx is
// cancelled. Backup failures are logged, never fatal.
func Schedule(ctx context.Context, o Options) {
	if err := RunOnce(ctx, o); err != nil {
		slog.Error("startup backup failed", "err", err)
	}
	for {
		next := nextRun(time.Now().In(o.Loc), o.Hour, o.Loc)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := RunOnce(ctx, o); err != nil {
				slog.Error("scheduled backup failed", "err", err)
			}
		}
	}
}

// nextRun returns the next instant at hour:00:00 local time strictly after now.
func nextRun(now time.Time, hour int, loc *time.Location) time.Time {
	n := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
	if !n.After(now) {
		n = n.AddDate(0, 0, 1)
	}
	return n
}

// RunOnce writes one archive for today's date and prunes old archives. It is a
// no-op (returns nil) when today's archive already exists, so frequent restarts
// don't accumulate backups.
func RunOnce(ctx context.Context, o Options) error {
	if err := os.MkdirAll(o.Dir, 0o755); err != nil {
		return fmt.Errorf("mkdir backup dir: %w", err)
	}
	date := time.Now().In(o.Loc).Format("2006-01-02")
	target := filepath.Join(o.Dir, "backup-"+date+".tar.gz")
	if _, err := os.Stat(target); err == nil {
		slog.Debug("backup skipped; today's archive exists", "file", target)
		return nil
	}

	snapshot := filepath.Join(o.Dir, fmt.Sprintf(".snapshot-%d.db", os.Getpid()))
	if err := vacuumInto(ctx, o.DB, snapshot); err != nil {
		return fmt.Errorf("snapshot db: %w", err)
	}
	defer os.Remove(snapshot)

	tmp := filepath.Join(o.Dir, ".tmp-"+date+".tar.gz")
	if err := writeArchive(tmp, snapshot, o); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write archive: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("publish archive: %w", err)
	}
	slog.Info("backup written", "file", target)

	if err := prune(o.Dir); err != nil {
		slog.Warn("backup prune failed", "err", err)
	}
	return nil
}

// vacuumInto produces a consistent, fully-checkpointed copy of the database at
// dst via SQLite's VACUUM INTO. dst must not already exist. The path is app
// controlled but still single-quote-escaped since VACUUM takes a string literal
// (bound parameters aren't accepted here).
func vacuumInto(ctx context.Context, db *sql.DB, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("clear stale snapshot: %w", err)
		}
	}
	lit := "'" + strings.ReplaceAll(dst, "'", "''") + "'"
	if _, err := db.ExecContext(ctx, "VACUUM INTO "+lit); err != nil {
		return err
	}
	return nil
}

// writeArchive builds the tar.gz at path: the db snapshot under db/, plus the
// attachments dir, the imports processed/failed audit trail, and config.yaml.
// Missing sources are skipped silently (e.g. attachments before first upload).
func writeArchive(path, snapshot string, o Options) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	if err := addFile(tw, snapshot, "db/trading.db"); err != nil {
		return err
	}
	attachments := filepath.Join(filepath.Dir(o.DBPath), "attachments")
	if err := addTree(tw, attachments, "attachments"); err != nil {
		return err
	}
	if err := addTree(tw, filepath.Join(o.ImportsRoot, "processed"), "imports/processed"); err != nil {
		return err
	}
	if err := addTree(tw, filepath.Join(o.ImportsRoot, "failed"), "imports/failed"); err != nil {
		return err
	}
	if o.ConfigPath != "" {
		if err := addFile(tw, o.ConfigPath, "config.yaml"); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// addFile copies a single file into the archive under name. A missing source
// is not an error (returns nil).
func addFile(tw *tar.Writer, src, name string) error {
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return copyInto(tw, src, name, info)
}

// addTree walks a directory and copies every regular file into the archive
// under prefix/<relpath>. A missing directory is not an error.
func addTree(tw *tar.Writer, root, prefix string) error {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		return copyInto(tw, p, name, fi)
	})
}

func copyInto(tw *tar.Writer, src, name string, info os.FileInfo) error {
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = io.Copy(tw, in)
	return err
}

// prune enforces retention: keep the keepDaily newest archives, then keep one
// archive per calendar month (the newest in that month) for everything older,
// deleting the rest.
func prune(dir string) error {
	type arch struct {
		path string
		date string // YYYY-MM-DD
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var archives []arch
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if m := archiveDateRe.FindStringSubmatch(e.Name()); m != nil {
			archives = append(archives, arch{path: filepath.Join(dir, e.Name()), date: m[1]})
		}
	}
	// Newest first.
	sort.Slice(archives, func(i, j int) bool { return archives[i].date > archives[j].date })

	seenMonth := map[string]bool{}
	for i, a := range archives {
		if i < keepDaily {
			continue // always keep the newest keepDaily
		}
		month := a.date[:7] // YYYY-MM
		if !seenMonth[month] {
			seenMonth[month] = true
			continue // keep the newest archive of this older month
		}
		if err := os.Remove(a.path); err != nil {
			slog.Warn("prune remove failed", "file", a.path, "err", err)
			continue
		}
		slog.Debug("backup pruned", "file", a.path)
	}
	return nil
}
