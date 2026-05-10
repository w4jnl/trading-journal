package importer

import (
	"context"
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
// On startup it sweeps the inbox for files left from a previous run before
// the watcher loop begins, so a file dropped while the server was down still
// gets imported on next boot.
//
// Imports are serialized through a single processing goroutine — the FIFO
// position builder assumes one rebuild per (account, symbol) at a time.
type Watcher struct {
	InboxDir   string
	Service    *ImportService
	QuietDelay time.Duration // debounce window; defaults to 2s
	Logger     *slog.Logger
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

	importCh := make(chan string, 64)
	processorDone := make(chan struct{})
	go w.runProcessor(ctx, importCh, processorDone)
	defer func() {
		close(importCh)
		<-processorDone
	}()

	w.bootSweep(ctx, importCh)

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify: %w", err)
	}
	defer fw.Close()
	if err := fw.Add(w.InboxDir); err != nil {
		return fmt.Errorf("watch %s: %w", w.InboxDir, err)
	}
	log.Info("watcher started", "inbox", w.InboxDir, "debounce", delay)

	var mu sync.Mutex
	timers := map[string]*time.Timer{}

	enqueueAfterQuiet := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := timers[path]; ok {
			t.Stop()
		}
		timers[path] = time.AfterFunc(delay, func() {
			mu.Lock()
			delete(timers, path)
			mu.Unlock()
			// Re-check existence — a CHMOD or REMOVE event may have arrived
			// between scheduling and firing.
			if _, err := os.Stat(path); err != nil {
				return
			}
			select {
			case importCh <- path:
			case <-ctx.Done():
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
			enqueueAfterQuiet(ev.Name)
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

func (w *Watcher) runProcessor(ctx context.Context, in <-chan string, done chan<- struct{}) {
	defer close(done)
	for path := range in {
		if _, err := w.Service.ImportFile(ctx, path); err != nil {
			w.logger().Error("watcher import failed", "path", path, "err", err)
		}
	}
}

// bootSweep imports any non-hidden CSV files already sitting in InboxDir at
// startup. Files are dispatched through the same channel as live events so
// processing order remains serialized.
func (w *Watcher) bootSweep(ctx context.Context, importCh chan<- string) {
	entries, err := os.ReadDir(w.InboxDir)
	if err != nil {
		w.logger().Warn("boot sweep readdir", "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !isImportable(filepath.Join(w.InboxDir, e.Name())) {
			continue
		}
		path := filepath.Join(w.InboxDir, e.Name())
		select {
		case importCh <- path:
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
