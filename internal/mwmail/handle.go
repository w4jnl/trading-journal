package mwmail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
)

// replayJournalName is the sidecar in the archive dir recording which
// captured messages have been terminally processed by ingest (live or
// replay), one "<hash12> <action>" line each. Deleting the file forces the
// next boot to re-attempt the whole archive - the documented escape hatch
// after an app upgrade whose improved parser could recover messages that
// were journaled as not_ingestable.
const replayJournalName = "replay-state.log"

// archive owns the verbatim .eml store, the restart-surviving dedup set, and
// the replay journal. Filenames are <utc-timestamp>_<hash12>.eml; the hash
// suffix doubles as the persistent dedup key, so a restart rebuilds the set
// from a directory scan instead of needing any database state.
type archive struct {
	dir  string
	mu   sync.Mutex
	seen map[string]bool // 12-hex short hash -> captured
	done map[string]bool // 12-hex short hash -> terminally processed by ingest
}

func newArchive(dir string) (*archive, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &archive{dir: dir, seen: map[string]bool{}, done: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if h := hashFromName(e.Name()); h != "" {
			a.seen[h] = true
		}
	}
	// Load the replay journal, tolerantly: first field is the hash, the rest
	// is informational. Missing file just means nothing was processed yet.
	if raw, err := os.ReadFile(filepath.Join(dir, replayJournalName)); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && len(fields[0]) == 12 {
				a.done[fields[0]] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read replay journal: %w", err)
	}
	return a, nil
}

// hashFromName extracts the 12-hex content hash from an archived filename
// (<timestamp>_<hash12>.eml); "" when the name does not match.
func hashFromName(name string) string {
	if !strings.HasSuffix(name, ".eml") {
		return ""
	}
	base := strings.TrimSuffix(name, ".eml")
	if i := strings.LastIndexByte(base, '_'); i >= 0 && len(base)-i-1 == 12 {
		return base[i+1:]
	}
	return ""
}

func (a *archive) size() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

// list returns every archived .eml path in capture order (the filename's
// leading UTC timestamp makes the lexical sort chronological - which matters
// for replay: cumulative order snapshots must apply oldest-first).
func (a *archive) list() ([]string, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".eml") {
			paths = append(paths, filepath.Join(a.dir, e.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// markNew records the hash; false if it was already captured (duplicate).
func (a *archive) markNew(hash string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen[hash] {
		return false
	}
	a.seen[hash] = true
	return true
}

func (a *archive) unmark(hash string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.seen, hash)
}

// isDone reports whether the message was already terminally processed by
// ingest (live or a previous replay) - such files are never re-attempted.
func (a *archive) isDone(hash string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done[hash]
}

// markDone journals a terminal ingest outcome ONCE. Append-after-commit
// ordering: a crash between the ingest transaction and this write costs
// exactly one idempotent re-attempt on the next boot. Journal write failures
// are returned for logging but do not undo the ingest.
func (a *archive) markDone(hash, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done[hash] {
		return nil
	}
	return a.appendLocked(hash, action)
}

// journalOutcome appends an outcome unconditionally. markDone is the
// once-only path used by normal processing; a deliberate retry
// (RetryUnattributed) uses this to record its NEW result over an earlier
// skip - the last line for a hash wins, which is what clears the alert.
func (a *archive) journalOutcome(hash, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.appendLocked(hash, action)
}

// appendLocked writes one journal line; callers hold a.mu.
func (a *archive) appendLocked(hash, action string) error {
	a.done[hash] = true
	f, err := os.OpenFile(filepath.Join(a.dir, replayJournalName),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f, "%s %s\n", hash, action); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// store writes the raw message verbatim. O_EXCL + a nanosecond timestamp keep
// concurrent burst captures (copier fan-out: 5+ near-simultaneous emails)
// collision-free without coordination.
func (a *archive) store(hash string, raw []byte) (string, error) {
	name := fmt.Sprintf("%s_%s.eml", time.Now().UTC().Format("20060102T150405.000000000Z"), hash)
	path := filepath.Join(a.dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// Data is the capture contract (spec section 5): read fully + hash; dedup
// (duplicates still get 250); archive verbatim BEFORE parsing (archive
// failure is the only rejection); parse; hand complete fills to the Ingest
// hook; always accept. Parse and ingest problems are never bounced - a 5xx
// surfaces as a cryptic error inside MW and can stall its mailer; the
// archive guarantees nothing is lost.
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return &smtp.SMTPError{Code: 451, Message: "read failed"}
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])[:12]
	log := s.server.logger()

	arch := s.server.archive
	if !arch.markNew(hash) {
		log.Info("mwmail: duplicate message dropped", "hash", hash, "from", s.from, "size", len(raw))
		return nil // still 250: SMTP retries must not error
	}
	path, err := arch.store(hash, raw)
	if err != nil {
		// The one legitimate rejection: nothing hit disk, so let the client
		// retry later rather than silently losing the message.
		arch.unmark(hash)
		log.Error("mwmail: archive write failed", "hash", hash, "err", err)
		return &smtp.SMTPError{Code: 451, Message: "archive failed, retry"}
	}

	res := Parse(raw)
	attrs := []any{
		"hash", hash, "file", filepath.Base(path), "size", len(raw),
		"from", s.from, "to", strings.Join(s.to, ","), "subject", res.Subject,
		"parsed", res.Parsed,
	}
	if res.Parsed {
		attrs = append(attrs,
			"side", res.Side, "qty", res.Qty, "symbol", res.Symbol, "price", res.Price,
			"account", res.Account, "order_id", res.OrderID, "fill_time", res.FillTimeRaw)
	}

	// Provisional ingest. Failures are logged, never bounced: the archive
	// already holds the message, so the boot-time replay (or an end-of-day
	// CSV import) recovers anything the live path missed. Terminal outcomes
	// are journaled so the replay never re-attempts this message; hook
	// errors and capture-only mode are NOT journaled - those retry later.
	terminal := ""
	switch {
	case s.server.Ingest == nil:
		attrs = append(attrs, "ingest", "capture_only")
	case res.IsTest:
		terminal = "test_email"
		attrs = append(attrs, "ingest", terminal)
	case !res.IngestReady():
		terminal = "not_ingestable"
		attrs = append(attrs, "ingest", terminal)
	default:
		ctx := s.server.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		action, err := s.server.Ingest(ctx, res)
		if err != nil {
			log.Error("mwmail: ingest failed", "hash", hash, "order_id", res.OrderID, "err", err)
			attrs = append(attrs, "ingest", "error")
		} else {
			terminal = action
			attrs = append(attrs, "ingest", action)
		}
	}
	if terminal != "" {
		if err := arch.markDone(hash, terminal); err != nil {
			log.Warn("mwmail: replay journal write failed", "hash", hash, "err", err)
		}
	}
	log.Info("mwmail: message captured", attrs...)
	return nil
}
