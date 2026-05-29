package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestPruneRetention(t *testing.T) {
	dir := t.TempDir()

	// 14 consecutive days in May 2026 + a few scattered earlier months.
	dates := []string{
		"2026-05-29", "2026-05-28", "2026-05-27", "2026-05-26", "2026-05-25",
		"2026-05-24", "2026-05-23", "2026-05-22", "2026-05-21", "2026-05-20",
		"2026-05-19", "2026-05-18", "2026-05-17", "2026-05-16", // beyond 10 newest, same month
		"2026-04-15", "2026-04-10", "2026-04-02", // April: keep newest (04-15)
		"2026-03-20", "2026-03-01", // March: keep newest (03-20)
		"2026-01-09", // January: lone, kept
	}
	for _, d := range dates {
		mustTouch(t, filepath.Join(dir, "backup-"+d+".tar.gz"))
	}
	// A non-archive file must be left untouched.
	mustTouch(t, filepath.Join(dir, "notes.txt"))

	if err := prune(dir); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got := listArchives(t, dir)
	want := []string{
		// 10 newest (always kept):
		"backup-2026-05-29.tar.gz", "backup-2026-05-28.tar.gz", "backup-2026-05-27.tar.gz",
		"backup-2026-05-26.tar.gz", "backup-2026-05-25.tar.gz", "backup-2026-05-24.tar.gz",
		"backup-2026-05-23.tar.gz", "backup-2026-05-22.tar.gz", "backup-2026-05-21.tar.gz",
		"backup-2026-05-20.tar.gz",
		// older: one per month (newest in month)
		"backup-2026-05-19.tar.gz", // newest May archive beyond the daily window
		"backup-2026-04-15.tar.gz",
		"backup-2026-03-20.tar.gz",
		"backup-2026-01-09.tar.gz",
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("kept %d archives, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("archive mismatch at %d: got %q want %q\nfull got: %v", i, got[i], want[i], got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatalf("non-archive file was removed: %v", err)
	}
}

func TestRunOnceArchiveContents(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "trading.db")

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT); INSERT INTO t (v) VALUES ('hello');`); err != nil {
		t.Fatal(err)
	}

	// Attachment + config to verify the rest of the archive.
	if err := os.MkdirAll(filepath.Join(dataDir, "attachments"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dataDir, "attachments", "abc.png"), "img")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	mustWrite(t, cfgPath, "server:\n  port: 8080\n")

	o := Options{
		Dir:        dir,
		DBPath:     dbPath,
		ConfigPath: cfgPath,
		DB:         db,
		Loc:        time.UTC,
	}
	if err := RunOnce(context.Background(), o); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	archives := listArchives(t, dir)
	if len(archives) != 1 {
		t.Fatalf("expected 1 archive, got %v", archives)
	}
	names := tarEntries(t, filepath.Join(dir, archives[0]))
	for _, want := range []string{"db/trading.db", "attachments/abc.png", "config.yaml"} {
		if !names[want] {
			t.Fatalf("archive missing %q; entries: %v", want, names)
		}
	}

	// Second run the same day is a no-op (dedupe), no extra archive, no temp.
	if err := RunOnce(context.Background(), o); err != nil {
		t.Fatalf("RunOnce dedupe: %v", err)
	}
	if a := listArchives(t, dir); len(a) != 1 {
		t.Fatalf("dedupe failed, archives now %v", a)
	}
}

func mustTouch(t *testing.T, path string) {
	t.Helper()
	mustWrite(t, path, "")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listArchives(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if archiveDateRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func tarEntries(t *testing.T, path string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[hdr.Name] = true
	}
	return names
}
