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

// archive owns the verbatim .eml store and the restart-surviving dedup set.
// Filenames are <utc-timestamp>_<hash12>.eml; the hash suffix doubles as the
// persistent dedup key, so a restart rebuilds the set from a directory scan
// instead of needing any database state.
type archive struct {
	dir  string
	mu   sync.Mutex
	seen map[string]bool // 12-hex short hash -> captured
}

func newArchive(dir string) (*archive, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &archive{dir: dir, seen: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".eml") {
			continue
		}
		// <timestamp>_<hash12>.eml
		base := strings.TrimSuffix(name, ".eml")
		if i := strings.LastIndexByte(base, '_'); i >= 0 && len(base)-i-1 == 12 {
			a.seen[base[i+1:]] = true
		}
	}
	return a, nil
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
	// already holds the message, so an end-of-day CSV import (or a manual
	// `mwmail parse` inspection) recovers anything the live path missed.
	switch {
	case s.server.Ingest == nil:
		attrs = append(attrs, "ingest", "capture_only")
	case res.IsTest:
		attrs = append(attrs, "ingest", "test_email")
	case !res.IngestReady():
		attrs = append(attrs, "ingest", "not_ingestable")
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
			attrs = append(attrs, "ingest", action)
		}
	}
	log.Info("mwmail: message captured", attrs...)
	return nil
}
