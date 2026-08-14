package livedata

import (
	"sync"
	"testing"
)

// Concurrent bumps must never lose a count: the SMTP email-ingest path runs
// one goroutine per connection alongside the boot replay and UI retries.
func TestNotifierConcurrent(t *testing.T) {
	n := NewNotifier()
	const workers, each = 32, 100
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				n.Bump(1)
				n.Bump(2)
				n.BumpAll()
			}
		}()
	}
	wg.Wait()

	want := uint64(workers * each * 2) // global + own bumps
	if got := n.Version(1); got != want {
		t.Errorf("Version(1) = %d, want %d", got, want)
	}
	if got := n.Version(2); got != want {
		t.Errorf("Version(2) = %d, want %d", got, want)
	}
	// A user nobody bumped still sees the global generation.
	if got := n.Version(99); got != uint64(workers*each) {
		t.Errorf("Version(99) = %d, want %d", got, workers*each)
	}
}

// Nil receivers must be safe: the notifier is an optional dependency, and
// tests build bare Handlers/ImportService structs without one.
func TestNotifierNil(t *testing.T) {
	var n *Notifier
	n.Bump(1)
	n.BumpAll()
	if got := n.Version(1); got != 0 {
		t.Errorf("nil Version = %d, want 0", got)
	}
}
