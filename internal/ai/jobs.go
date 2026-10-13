package ai

import (
	"context"
	"sync"
	"time"
)

// Job states (string so they render straight into the HTMX status fragment).
const (
	JobIdle      = "idle"
	JobRunning   = "running"
	JobDone      = "done"
	JobCancelled = "cancelled"
	JobError     = "error"
)

// JobStatus is a snapshot of a user's day-analysis job.
type JobStatus struct {
	State     string    `json:"state"`
	Date      string    `json:"date"`
	Done      int       `json:"done"`
	Total     int       `json:"total"`
	Current   string    `json:"current"`
	Err       string    `json:"err,omitempty"`
	Model     string    `json:"model,omitempty"` // the model the job runs on, for display
	StartedAt time.Time `json:"startedAt,omitempty"`
}

type job struct {
	status JobStatus
	cancel context.CancelFunc
}

// JobManager runs at most one day-analysis job per user at a time (single-flight),
// tracking progress and supporting cancellation. It is domain-agnostic: the caller
// supplies the work as a func, so this package needs no handler/auth imports. A
// cancelled or finished job leaves its terminal status visible until the next Start
// for that user (so the UI can show "cancelled"/"done"); the marker-based skip in the
// work func makes a restart resume where it stopped.
type JobManager struct {
	parent context.Context // every job's context descends from it
	wg     sync.WaitGroup  // running job goroutines, for Wait
	mu     sync.Mutex
	jobs   map[int64]*job // keyed by user id
}

// NewJobManager returns a manager whose jobs descend from parent: cancelling
// it (app shutdown) cancels every running analysis, which then records
// itself as cancelled and can be resumed by the next Start. A nil parent
// means context.Background (tests).
func NewJobManager(parent context.Context) *JobManager {
	if parent == nil {
		parent = context.Background()
	}
	return &JobManager{parent: parent, jobs: make(map[int64]*job)}
}

// Start launches run in a goroutine for userID unless one is already running for
// that user (then it returns false). model names the model it runs on, for the
// status line. run receives a cancellable context and a progress callback it
// should call as it advances and check ctx for cancellation.
func (m *JobManager) Start(userID int64, date, model string, run func(ctx context.Context, progress func(done, total int, current string)) error) bool {
	m.mu.Lock()
	if j, ok := m.jobs[userID]; ok && j.status.State == JobRunning {
		m.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(m.parent)
	j := &job{status: JobStatus{State: JobRunning, Date: date, Model: model, StartedAt: time.Now()}, cancel: cancel}
	m.jobs[userID] = j
	m.wg.Add(1)
	m.mu.Unlock()

	progress := func(done, total int, current string) {
		m.mu.Lock()
		j.status.Done, j.status.Total, j.status.Current = done, total, current
		m.mu.Unlock()
	}
	go func() {
		defer m.wg.Done()
		err := run(ctx, progress)
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case ctx.Err() != nil:
			j.status.State = JobCancelled
		case err != nil:
			j.status.State = JobError
			j.status.Err = err.Error()
		default:
			j.status.State = JobDone
		}
		j.status.Current = ""
		cancel()
	}()
	return true
}

// Cancel signals a running job for userID to stop (no-op otherwise). The job ends
// after the in-flight entry; a subsequent Start resumes the remaining entries.
func (m *JobManager) Cancel(userID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[userID]; ok && j.status.State == JobRunning {
		j.cancel()
	}
}

// Status returns the user's current job snapshot (idle when none has run).
func (m *JobManager) Status(userID int64) JobStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[userID]; ok {
		return j.status
	}
	return JobStatus{State: JobIdle}
}

// Wait blocks until every running job has finished or ctx ends. Shutdown
// cancels the parent first, so this is the bounded wait for the in-flight
// entry to notice and write its terminal state.
func (m *JobManager) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Running reports whether a job is currently in progress for userID (used to lock
// journaling while analysis runs).
func (m *JobManager) Running(userID int64) bool {
	return m.Status(userID).State == JobRunning
}
