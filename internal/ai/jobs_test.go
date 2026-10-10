package ai

import (
	"context"
	"testing"
	"time"
)

// A job's context descends from the manager's parent: cancelling the parent
// (app shutdown) ends the job as cancelled, and Wait returns once it has.
func TestJobManagerParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	m := NewJobManager(parent)
	started := make(chan struct{})
	ok := m.Start(7, "2026-10-09", func(ctx context.Context, progress func(int, int, string)) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if !ok {
		t.Fatal("Start refused an idle user")
	}
	if m.Start(7, "2026-10-09", nil) {
		t.Fatal("second Start for a running user was accepted")
	}
	<-started
	cancel()
	wctx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer wcancel()
	if err := m.Wait(wctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if st := m.Status(7); st.State != JobCancelled {
		t.Fatalf("state after parent cancel = %q, want %q", st.State, JobCancelled)
	}
	if m.Running(7) {
		t.Fatal("still running")
	}
}

// Wait gives up at its own deadline when a job ignores cancellation.
func TestJobManagerWaitDeadline(t *testing.T) {
	m := NewJobManager(nil)
	release := make(chan struct{})
	m.Start(1, "d", func(ctx context.Context, _ func(int, int, string)) error { <-release; return nil })
	wctx, wcancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer wcancel()
	if err := m.Wait(wctx); err == nil {
		t.Fatal("Wait returned before the job finished")
	}
	close(release)
	if err := m.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
