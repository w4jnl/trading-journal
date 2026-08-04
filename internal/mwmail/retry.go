package mwmail

import (
	"context"
	"os"
	"path/filepath"

	"github.com/w4jnl/trading-journal/internal/importer"
)

// Retried summarizes one retry pass.
type Retried struct {
	Attempted int // captures re-offered to the importer
	Ingested  int // booked as executions (inserted or updated)
	Skipped   int // still not bookable - usually the ref is STILL unresolvable
}

// RetryUnattributed re-ingests archived captures that were journaled as
// unattributable (skipped_unknown_account / skipped_ambiguous_account),
// optionally narrowed to specific account refs.
//
// The normal replay attempts every capture exactly once and never revisits a
// terminal outcome - deliberately, so a settled archive costs nothing on each
// boot. But "no account matched" is the one skip that a later USER ACTION can
// undo: creating the account, or pointing an existing one at the ref with an
// email alias. This is that second chance, and it is what makes those fills
// arrive without a manual CSV export.
//
// Outcomes are journaled over the old skip, so a capture booked here stops
// being reported as unattributed. Ingest errors are NOT journaled: they are
// transient by definition, and leaving the old line intact keeps the capture
// eligible for the next attempt.
func RetryUnattributed(ctx context.Context, archiveDir string, ingest IngestFunc, refs map[string]bool) (Retried, error) {
	var r Retried
	if archiveDir == "" || ingest == nil {
		return r, nil
	}
	fills, err := UnprocessedFills(archiveDir)
	if err != nil || len(fills) == 0 {
		return r, err
	}
	arch, err := newArchive(archiveDir)
	if err != nil {
		return r, err
	}

	for _, f := range fills {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		if len(refs) > 0 && !refs[f.Account] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(archiveDir, f.File))
		if err != nil {
			continue // the journal still flags it; a later pass can retry
		}
		res := Parse(raw)
		if res.IsTest || !res.IngestReady() {
			continue
		}
		r.Attempted++
		action, err := ingest(ctx, res)
		if err != nil {
			continue
		}
		if hash := hashFromName(f.File); hash != "" {
			if err := arch.journalOutcome(hash, action); err != nil {
				return r, err
			}
		}
		switch action {
		case importer.EmailInserted, importer.EmailUpdated:
			r.Ingested++
		default:
			r.Skipped++
		}
	}
	return r, nil
}
