package ai

import (
	"context"
	"sync"
)

// Reviews runs single-note AI reviews in the background, detached from the
// request that asked for one, so a review keeps going when the trader closes
// the sheet or leaves the page. It knows which journal entries are under
// review, and which trades they cover, so every view can show a spinner on
// them and the editors can refuse manual edits until the review lands. A
// failed review leaves its error with the entry until a view shows it.
//
// At most perUser reviews of one user call the model at once; the rest wait
// their turn and count as pending meanwhile. Every review's context descends
// from parent: app shutdown cancels them. OnChange, when set, hears every
// start and finish (the live-data notifier, so open lists redraw).
type Reviews struct {
	OnChange func(userID int64)

	parent  context.Context
	perUser int
	wg      sync.WaitGroup
	mu      sync.Mutex
	pending map[int64]review        // by entry id (entry ids are global)
	trades  map[int64]int           // trade id -> pending reviews covering it
	failed  map[int64]failure       // by entry id, until taken
	slots   map[int64]chan struct{} // per-user concurrency
}

type review struct {
	userID int64
	trades []int64
}

type failure struct {
	userID int64
	msg    string
}

// NewReviews returns a tracker whose reviews descend from parent (nil means
// context.Background, for tests) and run at most perUser at a time per user.
func NewReviews(parent context.Context, perUser int) *Reviews {
	if parent == nil {
		parent = context.Background()
	}
	if perUser < 1 {
		perUser = 1
	}
	return &Reviews{
		parent: parent, perUser: perUser,
		pending: map[int64]review{}, trades: map[int64]int{},
		failed: map[int64]failure{}, slots: map[int64]chan struct{}{},
	}
}

// Start reviews entryID in the background with run, unless a review of it is
// already pending (then false). trades are the trades the entry covers, for
// the row indicators. A new review clears the entry's previous failure.
func (r *Reviews) Start(userID, entryID int64, trades []int64, run func(ctx context.Context) error) bool {
	if !r.mark(userID, entryID, trades) {
		return false
	}
	r.mu.Lock()
	slot, ok := r.slots[userID]
	if !ok {
		slot = make(chan struct{}, r.perUser)
		r.slots[userID] = slot
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		var err error
		select {
		case slot <- struct{}{}:
			// A slot freed by shutdown must not start a queued review.
			if err = r.parent.Err(); err == nil {
				err = run(r.parent)
			}
			<-slot
		case <-r.parent.Done():
			err = r.parent.Err()
		}
		r.finish(userID, entryID, err)
	}()
	return true
}

// Track marks entryID pending while the caller reviews it itself (the day
// job, which runs its entries in order). It returns the func that ends it
// with the review's outcome, and false - with no func - when a review of the
// entry is already pending, so the caller should skip it (or when r is nil).
func (r *Reviews) Track(userID, entryID int64, trades []int64) (func(error), bool) {
	if r == nil || !r.mark(userID, entryID, trades) {
		return nil, false
	}
	return func(err error) { r.finish(userID, entryID, err) }, true
}

func (r *Reviews) mark(userID, entryID int64, trades []int64) bool {
	r.mu.Lock()
	if _, busy := r.pending[entryID]; busy {
		r.mu.Unlock()
		return false
	}
	r.pending[entryID] = review{userID: userID, trades: trades}
	for _, t := range trades {
		r.trades[t]++
	}
	delete(r.failed, entryID)
	r.mu.Unlock()
	r.changed(userID)
	return true
}

func (r *Reviews) finish(userID, entryID int64, err error) {
	r.mu.Lock()
	if rv, ok := r.pending[entryID]; ok {
		for _, t := range rv.trades {
			if r.trades[t]--; r.trades[t] <= 0 {
				delete(r.trades, t)
			}
		}
		delete(r.pending, entryID)
	}
	if err != nil {
		r.failed[entryID] = failure{userID: userID, msg: err.Error()}
	}
	r.mu.Unlock()
	r.changed(userID)
}

func (r *Reviews) changed(userID int64) {
	if r.OnChange != nil {
		r.OnChange(userID)
	}
}

// EntryPending reports whether a review of entryID is pending. Entry ids are
// global, so no user is needed; callers only ask about entries they show.
func (r *Reviews) EntryPending(entryID int64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.pending[entryID]
	return ok
}

// TradePending reports whether a pending review covers tradeID.
func (r *Reviews) TradePending(tradeID int64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.trades[tradeID] > 0
}

// TakeError returns and forgets the error of the user's last review of
// entryID ("" when it succeeded, is still pending, or was shown already).
func (r *Reviews) TakeError(userID, entryID int64) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.failed[entryID]
	if !ok || f.userID != userID {
		return ""
	}
	delete(r.failed, entryID)
	return f.msg
}

// Wait blocks until every background review has finished or ctx ends.
func (r *Reviews) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
