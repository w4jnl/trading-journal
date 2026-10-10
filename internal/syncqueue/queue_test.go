package syncqueue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueueGapAndPending(t *testing.T) {
	q := New[struct{}]("t", Options{Throttle: 15 * time.Minute, APIThrottle: time.Minute}, nil, func(context.Context, Job[struct{}]) error { return nil })
	if !q.EnqueueWithGap(7, time.Minute) || q.Len() != 1 || !q.PendingForUser(7) {
		t.Fatalf("first enqueue: len=%d pending=%v", q.Len(), q.PendingForUser(7))
	}
	if q.EnqueueWithGap(7, time.Minute) || q.Len() != 1 {
		t.Fatalf("pending dup enqueued: len=%d", q.Len())
	}
	if _, ok := q.Take(); !ok {
		t.Fatal("Take: nothing queued")
	}
	q.MarkRan(7, time.Now())
	if q.EnqueueWithGap(7, time.Minute) {
		t.Fatal("enqueued inside the gap")
	}
	q.MarkRan(7, time.Now().Add(-2*time.Minute))
	if !q.EnqueueWithGap(7, time.Minute) {
		t.Fatal("not enqueued after the gap")
	}
	q.Take()
	q.MarkRan(7, time.Now())
	q.MaybeEnqueueUserAPI(7)
	if q.Len() != 0 {
		t.Fatal("API trigger ignored the API gap")
	}
	// Untracked jobs bypass both pending and the gap.
	q.MarkPending(7, true)
	if !q.EnqueueUntracked(7, struct{}{}) || q.Len() != 1 {
		t.Fatal("untracked job dropped")
	}
	if q.EnqueueWithGap(0, 0) {
		t.Fatal("user 0 enqueued")
	}
	var nilQ *Queue[struct{}]
	nilQ.MaybeEnqueueUser(7) // must not panic
	if nilQ.PendingForUser(7) || nilQ.Len() != 0 {
		t.Fatal("nil queue reports state")
	}
}

func TestRunBookkeeping(t *testing.T) {
	var ran atomic.Int64
	done := make(chan Job[int], 4)
	q := New[int]("t", Options{}, nil, func(_ context.Context, j Job[int]) error {
		ran.Add(1)
		done <- j
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	q.MaybeEnqueueUser(3)
	q.EnqueueUntracked(3, 42)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("job did not run")
		}
	}
	deadline := time.Now().Add(time.Second)
	for q.PendingForUser(3) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q.PendingForUser(3) {
		t.Fatal("tracked job left pending set")
	}
	if q.EnqueueWithGap(3, time.Hour) {
		t.Fatal("tracked run did not stamp the throttle clock")
	}
	if ran.Load() != 2 {
		t.Fatalf("ran = %d, want 2", ran.Load())
	}
}

func TestScheduledInterval(t *testing.T) {
	q := New[struct{}]("t", Options{SessionEvery: time.Hour, RTHEvery: 15 * time.Minute, RTHStartMin: 7*60 + 30, RTHEndMin: 16 * 60}, nil, nil)
	chi, _ := time.LoadLocation("America/Chicago")
	ct := func(d, hh, mm int) time.Time { return time.Date(2026, 7, d, hh, mm, 0, 0, chi) } // 2026-07-13 = Monday
	cases := []struct {
		name string
		t    time.Time
		want time.Duration
	}{
		{"monday RTH", ct(13, 10, 0), 15 * time.Minute},
		{"monday overnight", ct(13, 2, 0), time.Hour},
		{"maintenance break", ct(13, 16, 30), 0},
		{"saturday", ct(18, 12, 0), 0},
	}
	for _, c := range cases {
		if got := q.ScheduledInterval(c.t); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	var nilQ *Queue[struct{}]
	if nilQ.ScheduledInterval(ct(13, 10, 0)) != 0 {
		t.Error("nil queue has a cadence")
	}
}

func TestWaitForUser(t *testing.T) {
	q := New[struct{}]("t", Options{}, nil, nil)
	q.MarkPending(5, true)
	go func() { time.Sleep(60 * time.Millisecond); q.MarkPending(5, false) }()
	start := time.Now()
	q.WaitForUser(context.Background(), 5, time.Now().Add(3*time.Second))
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Errorf("wait took %v", d)
	}
	q.MarkPending(5, true)
	start = time.Now()
	q.WaitForUser(context.Background(), 5, time.Now().Add(250*time.Millisecond))
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("deadline wait took %v", d)
	}
}
