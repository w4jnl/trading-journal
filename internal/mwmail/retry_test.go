package mwmail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A capture skipped because no account owned its ref must become bookable
// once the operator creates that account: RetryUnattributed re-offers it,
// journals the new outcome over the old skip, and the alert listing stops
// reporting it. Refs the operator did NOT touch stay untouched.
func TestRetryUnattributed(t *testing.T) {
	dir := t.TempDir()
	fill, err := os.ReadFile(filepath.Join("testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	// Two captures of the same (unknown) account, one of a different one.
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), fill, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("20260727T071933.000000000Z_aaaaaaaaaaaa.eml")
	write("20260727T071934.000000000Z_bbbbbbbbbbbb.eml")
	other := strings.Replace(string(fill), "LFE050-EXAMPLE1-TEST018", "APEX-999-1", 1)
	if err := os.WriteFile(filepath.Join(dir, "20260727T071935.000000000Z_cccccccccccc.eml"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, replayJournalName), []byte(
		"aaaaaaaaaaaa skipped_unknown_account\n"+
			"bbbbbbbbbbbb skipped_unknown_account\n"+
			"cccccccccccc skipped_unknown_account\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The account for the first ref now exists, so the importer books it.
	var offered []string
	ingest := func(_ context.Context, res Result) (string, error) {
		offered = append(offered, res.Account)
		return "inserted", nil
	}
	got, err := RetryUnattributed(context.Background(), dir, ingest,
		map[string]bool{"LFE050-EXAMPLE1-TEST018": true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempted != 2 || got.Ingested != 2 || got.Skipped != 0 {
		t.Errorf("result = %+v, want 2 attempted / 2 ingested", got)
	}
	for _, a := range offered {
		if a != "LFE050-EXAMPLE1-TEST018" {
			t.Errorf("retried an unrequested ref: %s", a)
		}
	}

	// The alert listing now reports only the ref nobody claimed.
	left, err := UnprocessedFills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].Account != "APEX-999-1" {
		t.Fatalf("still-unattributed = %+v, want only APEX-999-1", left)
	}

	// Re-running is a no-op for what was already booked.
	again, err := RetryUnattributed(context.Background(), dir, ingest, map[string]bool{"LFE050-EXAMPLE1-TEST018": true})
	if err != nil || again.Attempted != 0 {
		t.Errorf("second pass = %+v, %v; want nothing left to do", again, err)
	}
}

// A capture that is still unattributable stays flagged, and an ingest error
// must NOT be journaled - the capture has to remain eligible for a later try.
func TestRetryUnattributedKeepsUnresolved(t *testing.T) {
	dir := t.TempDir()
	fill, err := os.ReadFile(filepath.Join("testdata", "fill_real_mw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260727T071933.000000000Z_aaaaaaaaaaaa.eml"), fill, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, replayJournalName),
		[]byte("aaaaaaaaaaaa skipped_unknown_account\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Still unknown: the outcome is journaled, but it is still a skip, so the
	// listing keeps reporting it.
	skip := func(_ context.Context, _ Result) (string, error) { return "skipped_unknown_account", nil }
	got, err := RetryUnattributed(context.Background(), dir, skip, nil)
	if err != nil || got.Attempted != 1 || got.Ingested != 0 || got.Skipped != 1 {
		t.Fatalf("= %+v, %v", got, err)
	}
	if left, _ := UnprocessedFills(dir); len(left) != 1 {
		t.Errorf("a still-unknown capture stopped being reported: %+v", left)
	}

	// Transient failure: nothing journaled, still eligible.
	boom := func(_ context.Context, _ Result) (string, error) { return "", os.ErrDeadlineExceeded }
	if got, err := RetryUnattributed(context.Background(), dir, boom, nil); err != nil || got.Attempted != 1 || got.Ingested != 0 {
		t.Fatalf("error pass = %+v, %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, replayJournalName))
	if strings.Contains(string(raw), "deadline") {
		t.Errorf("an ingest error was journaled: %s", raw)
	}
	if left, _ := UnprocessedFills(dir); len(left) != 1 {
		t.Errorf("capture dropped after a transient error: %+v", left)
	}
}

// No archive dir, no ingest hook, or a ref filter matching nothing: all
// no-ops rather than errors - the caller is an account save, not a job.
func TestRetryUnattributedNoOps(t *testing.T) {
	if got, err := RetryUnattributed(context.Background(), "", nil, nil); err != nil || got.Attempted != 0 {
		t.Errorf("empty dir = %+v, %v", got, err)
	}
	dir := t.TempDir()
	ingest := func(_ context.Context, _ Result) (string, error) { return "inserted", nil }
	if got, err := RetryUnattributed(context.Background(), dir, ingest, nil); err != nil || got.Attempted != 0 {
		t.Errorf("no journal = %+v, %v", got, err)
	}
}
