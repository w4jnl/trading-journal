package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher watches an inbox directory and dispatches new CSV files to an
// ImportService. It debounces filesystem events so the import runs only after
// a file has been quiet for a configurable interval (the file may still be
// copying when the first event arrives).
//
// Per-user routing: imports/inbox/<username>/ subdirs are scanned at startup
// and each one is watched as its own user-scoped inbox. A file in
// /path/inbox/alice/foo.csv is imported with alice's user_id and moves to
// /path/processed/alice/<ts>_foo.csv on success. Subdirs whose name doesn't
// resolve to an active user are logged and skipped.
//
// The flat /path/inbox/foo.csv layout is still honored for backward compat:
// files directly under InboxDir are attributed to Service.UserID and move
// into the flat processed/failed dirs. We log this once at startup so users
// know to migrate to subdirs.
//
// On startup it sweeps each watched dir for files left from a previous run
// before the live event loop begins.
//
// Imports are serialized through a single processing goroutine — the FIFO
// position builder assumes one rebuild per (account, symbol) at a time.
type Watcher struct {
	InboxDir   string
	Service    *ImportService
	QuietDelay time.Duration // debounce window; defaults to 2s
	Logger     *slog.Logger

	// DB is consulted at startup to resolve subdir names to user_ids.
	// If nil the watcher only honors the flat layout (legacy behavior).
	DB *sql.DB
}

// watchedDir is one (path, user) pair the watcher is tracking. Flat-layout
// drops have UserID = Service.UserID and Username = "" (move dirs are not
// per-user). Per-user subdirs carry both.
type watchedDir struct {
	Path     string
	UserID   int64
	Username string // "" = flat layout
}

// importJob pairs a file path with the user-scoped dir it came from. Used
// as the queue element between the event/sweep producers and the single
// processor goroutine.
type importJob struct {
	path string
	dir  watchedDir
}

// Run starts the boot sweep and the fsnotify event loop. It blocks until
// ctx is cancelled. Errors during individual imports are logged, not returned —
// only setup failures (creating the watcher, adding the inbox dir) cause Run
// to return an error.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Service == nil {
		return errors.New("Watcher.Service is nil")
	}
	if w.InboxDir == "" {
		return errors.New("Watcher.InboxDir is empty")
	}
	if err := os.MkdirAll(w.InboxDir, 0o755); err != nil {
		return fmt.Errorf("ensure inbox: %w", err)
	}
	delay := w.QuietDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	log := w.logger()

	dirs, err := w.resolveWatchedDirs()
	if err != nil {
		return err
	}

	importCh := make(chan importJob, 64)
	processorDone := make(chan struct{})
	stop := make(chan struct{}) // closed on return; see the deferred shutdown below
	var mu sync.Mutex
	timers := map[string]*time.Timer{}
	go func() {
		defer close(processorDone)
		for {
			select {
			case p := <-importCh:
				if _, err := w.Service.ImportFileForUser(ctx, p.path, p.dir.UserID, p.dir.Username); err != nil {
					log.Error("watcher import failed", "path", p.path, "err", err)
				}
			case <-stop:
				return
			}
		}
	}()
	// Shutdown order matters: a debounce timer that fires inside the quiet
	// window right as we leave would otherwise send on a channel this
	// function had just closed - a panic, not a dropped job. importCh is
	// therefore never closed; the processor and every pending timer select
	// on stop instead, and the timers are stopped under the lock first.
	// Anything still queued is re-imported by the next boot sweep.
	defer func() {
		mu.Lock()
		for path, t := range timers {
			t.Stop()
			delete(timers, path)
		}
		mu.Unlock()
		close(stop)
		<-processorDone
	}()

	// Boot sweep for each watched dir.
	for _, d := range dirs {
		w.bootSweep(ctx, d, importCh)
	}

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer fw.Close()

	// Map path -> watchedDir so an event tells us who the file belongs to.
	dirByPath := map[string]watchedDir{}
	for _, d := range dirs {
		if err := fw.Add(d.Path); err != nil {
			log.Warn("add watch", "dir", d.Path, "err", err)
			continue
		}
		dirByPath[d.Path] = d
		if d.Username != "" {
			log.Info("watching user inbox", "dir", d.Path, "user", d.Username, "user_id", d.UserID)
		} else {
			log.Info("watching flat inbox", "dir", d.Path, "fallback_user_id", d.UserID)
		}
	}
	if len(dirByPath) == 0 {
		log.Warn("no watchable inbox dirs resolved; nothing will be auto-imported")
	}

	enqueueAfterQuiet := func(path string, d watchedDir) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[path]; ok {
			t.Stop()
		}
		timers[path] = time.AfterFunc(delay, func() {
			mu.Lock()
			delete(timers, path)
			mu.Unlock()
			if _, err := os.Stat(path); err != nil {
				return
			}
			select {
			case importCh <- importJob{path: path, dir: d}:
			case <-ctx.Done():
			case <-stop:
			}
		})
	}

	for {
		select {
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
				continue
			}
			if !isImportable(ev.Name) {
				continue
			}
			// The event's parent dir tells us which user owns this drop.
			parent := filepath.Dir(ev.Name)
			d, ok := dirByPath[parent]
			if !ok {
				log.Warn("event from unwatched dir; ignoring", "path", ev.Name)
				continue
			}
			enqueueAfterQuiet(ev.Name, d)
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			log.Warn("fsnotify error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// resolveWatchedDirs scans InboxDir for per-user subdirs and resolves each
// to a user_id. The flat InboxDir itself is always included as a fallback
// (attributed to Service.UserID) so a single-user deploy keeps working
// without restructuring its filesystem.
func (w *Watcher) resolveWatchedDirs() ([]watchedDir, error) {
	log := w.logger()
	out := []watchedDir{{
		Path:     w.InboxDir,
		UserID:   w.Service.UserID, // bootstrap fallback
		Username: "",
	}}
	entries, err := os.ReadDir(w.InboxDir)
	if err != nil {
		return out, fmt.Errorf("read inbox: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		sub := filepath.Join(w.InboxDir, name)
		uid, ok := w.resolveUser(name)
		if !ok {
			log.Warn("inbox subdir does not match an active user; skipping",
				"dir", sub, "username", name,
				"hint", "run `./trading-journal users add "+name+"` or rename the directory")
			continue
		}
		out = append(out, watchedDir{Path: sub, UserID: uid, Username: name})
	}
	return out, nil
}

// resolveUser maps a subdir name to a user_id. Empty / missing DB returns
// (0, false). Inactive users count as missing — we don't import files into
// a disabled account.
func (w *Watcher) resolveUser(username string) (int64, bool) {
	if w.DB == nil || username == "" {
		return 0, false
	}
	var (
		id     int64
		active int64
	)
	row := w.DB.QueryRow(`SELECT id, is_active FROM users WHERE username = ?`, username)
	if err := row.Scan(&id, &active); err != nil {
		return 0, false
	}
	if active == 0 {
		return 0, false
	}
	return id, true
}

// bootSweep imports any non-hidden CSV files already sitting in d.Path at
// startup. Files are dispatched through the channel that the run loop
// drains, attributed to d's user.
func (w *Watcher) bootSweep(ctx context.Context, d watchedDir, importCh chan<- importJob) {
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		w.logger().Warn("boot sweep readdir", "dir", d.Path, "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(d.Path, e.Name())
		if !isImportable(path) {
			continue
		}
		select {
		case importCh <- importJob{path: path, dir: d}:
		case <-ctx.Done():
			return
		}
	}
}

func (w *Watcher) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// isImportable filters out hidden files (.gitkeep, .DS_Store) and non-CSV files.
func isImportable(path string) bool {
	base := filepath.Base(path)
	if strings.HasPrefix(base, ".") {
		return false
	}
	return strings.EqualFold(filepath.Ext(base), ".csv")
}
