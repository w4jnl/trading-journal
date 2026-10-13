package ai

import (
	"context"
	"sync"
)

// Tasks runs AI work that is not a note review - drafting a strategy,
// finding strategies, proposing a trade's levels, explaining the stop/target
// lab, drafting a day plan - in the background, keyed by what it is about
// (callers put the user in the key: "7:strategy:12"). Like Reviews, it carries
// on when the trader leaves, at most perUser tasks of one user run at once,
// and a failure waits with its key until a view shows it once. OnChange hears
// every start and finish.
type Tasks struct {
	OnChange func(userID int64)

	parent  context.Context
	perUser int
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[string]int64        // key -> user
	failed  map[string]failure      // key -> last error
	slots   map[int64]chan struct{} // per-user concurrency
}

// NewTasks returns a tracker whose tasks descend from parent (nil means
// context.Background, for tests) and run at most perUser at a time per user.
func NewTasks(parent context.Context, perUser int) *Tasks {
	if parent == nil {
		parent = context.Background()
	}
	if perUser < 1 {
		perUser = 1
	}
	return &Tasks{parent: parent, perUser: perUser, pending: map[string]int64{},
		failed: map[string]failure{}, slots: map[int64]chan struct{}{}}
}

// Start runs run in the background under key unless a task with that key is
// pending (then false). A new start clears the key's previous failure.
func (t *Tasks) Start(userID int64, key string, run func(ctx context.Context) error) bool {
	t.mu.Lock()
	if _, busy := t.pending[key]; busy {
		t.mu.Unlock()
		return false
	}
	t.pending[key] = userID
	delete(t.failed, key)
	slot, ok := t.slots[userID]
	if !ok {
		slot = make(chan struct{}, t.perUser)
		t.slots[userID] = slot
	}
	t.wg.Add(1)
	t.mu.Unlock()
	t.changed(userID)
	go func() {
		defer t.wg.Done()
		var err error
		select {
		case slot <- struct{}{}:
			if err = t.parent.Err(); err == nil {
				err = run(t.parent)
			}
			<-slot
		case <-t.parent.Done():
			err = t.parent.Err()
		}
		t.mu.Lock()
		delete(t.pending, key)
		if err != nil {
			t.failed[key] = failure{userID: userID, msg: err.Error()}
		}
		t.mu.Unlock()
		t.changed(userID)
	}()
	return true
}

func (t *Tasks) changed(userID int64) {
	if t.OnChange != nil {
		t.OnChange(userID)
	}
}

// Pending reports whether the task under key is running or queued.
func (t *Tasks) Pending(key string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.pending[key]
	return ok
}

// TakeError returns and forgets the error of the user's last task under key.
func (t *Tasks) TakeError(userID int64, key string) string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	f, ok := t.failed[key]
	if !ok || f.userID != userID {
		return ""
	}
	delete(t.failed, key)
	return f.msg
}

// Wait blocks until every task has finished or ctx ends.
func (t *Tasks) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
