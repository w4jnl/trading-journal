package ai

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A review runs detached, marks its entry and trades pending until it ends,
// refuses a second start, and leaves its error for one reader.
func TestReviewsLifecycle(t *testing.T) {
	var changes atomic.Int64
	r := NewReviews(nil, 2)
	r.OnChange = func(int64) { changes.Add(1) }
	release := make(chan struct{})
	if !r.Start(7, 100, []int64{1, 2}, func(ctx context.Context) error { <-release; return errors.New("provider said no") }) {
		t.Fatal("Start refused an idle entry")
	}
	if r.Start(7, 100, nil, func(context.Context) error { return nil }) {
		t.Error("second Start for a pending entry accepted")
	}
	if !r.EntryPending(100) || !r.TradePending(1) || !r.TradePending(2) || r.TradePending(3) {
		t.Error("pending state not reported")
	}
	if done, ok := r.Track(7, 100, nil); ok || done != nil {
		t.Error("Track accepted a pending entry")
	}
	close(release)
	wctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if r.EntryPending(100) || r.TradePending(1) {
		t.Error("still pending after the review ended")
	}
	if got := r.TakeError(8, 100); got != "" {
		t.Errorf("another user read the error: %q", got)
	}
	if got := r.TakeError(7, 100); got != "provider said no" {
		t.Errorf("error = %q", got)
	}
	if got := r.TakeError(7, 100); got != "" {
		t.Errorf("error read twice: %q", got)
	}
	if changes.Load() != 2 {
		t.Errorf("changes = %d, want start + finish", changes.Load())
	}

	// Track is the synchronous form; a new review clears an old failure.
	done, ok := r.Track(7, 101, []int64{5})
	if !ok || !r.TradePending(5) {
		t.Fatal("Track did not mark")
	}
	done(errors.New("boom"))
	r.Track(7, 101, nil)
	if got := r.TakeError(7, 101); got != "" {
		t.Errorf("a new review kept the old failure: %q", got)
	}
}

// At most perUser reviews of one user run at once; the rest wait, pending.
func TestReviewsPerUserLimit(t *testing.T) {
	r := NewReviews(nil, 2)
	var running, peak atomic.Int64
	release := make(chan struct{})
	for i := int64(0); i < 5; i++ {
		r.Start(7, 200+i, nil, func(context.Context) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-release
			running.Add(-1)
			return nil
		})
	}
	time.Sleep(100 * time.Millisecond)
	if peak.Load() != 2 {
		t.Errorf("peak concurrency = %d, want 2", peak.Load())
	}
	for i := int64(0); i < 5; i++ {
		if !r.EntryPending(200 + i) {
			t.Errorf("entry %d not pending while queued", 200+i)
		}
	}
	close(release)
	if err := r.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Shutdown cancels running reviews and drops queued ones as failed.
func TestReviewsParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	r := NewReviews(parent, 1)
	// Either may win the single slot; whichever runs blocks until shutdown,
	// the other waits queued.
	started := make(chan struct{})
	var once sync.Once
	block := func(ctx context.Context) error { once.Do(func() { close(started) }); <-ctx.Done(); return ctx.Err() }
	r.Start(7, 300, nil, block)
	r.Start(7, 301, nil, block)
	<-started
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	if err := r.Wait(wctx); err != nil {
		t.Fatal(err)
	}
	if r.EntryPending(300) || r.EntryPending(301) {
		t.Error("reviews still pending after shutdown")
	}
	if r.TakeError(7, 300) == "" || r.TakeError(7, 301) == "" {
		t.Error("cancelled reviews should report the cancellation")
	}
}

// A nil tracker answers "nothing pending" so views need no nil checks.
func TestReviewsNilSafe(t *testing.T) {
	var r *Reviews
	if r.EntryPending(1) || r.TradePending(1) || r.TakeError(1, 1) != "" {
		t.Error("nil tracker reported state")
	}
}
