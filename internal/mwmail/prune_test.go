package mwmail

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneArchiveRemovesOnlyOldEml(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	write := func(name string, mtime time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	write("20260801T100000Z_abcdef123456.eml", old)
	write("20261010T100000Z_abcdef123457.eml", time.Now())
	write("notes.txt", old)                                            // not an archive file
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil { // directories are left alone
		t.Fatal(err)
	}
	n, err := PruneArchive(dir, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("removed=%d err=%v, want 1 removal", n, err)
	}
	for _, keep := range []string{"20261010T100000Z_abcdef123457.eml", "notes.txt", "sub"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s removed: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "20260801T100000Z_abcdef123456.eml")); !os.IsNotExist(err) {
		t.Error("old .eml not removed")
	}
	// A missing directory is not an error (archive never written yet).
	if n, err := PruneArchive(filepath.Join(dir, "missing"), time.Now()); err != nil || n != 0 {
		t.Errorf("missing dir: n=%d err=%v", n, err)
	}
}
