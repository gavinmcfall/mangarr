// Package tasks provides a concurrency-safe registry of named background jobs.
//
// Each Task has a stable ID, a human-readable name, an optional cadence, and a
// RunFn. Callers schedule tasks via RunNow; the registry records LastRun,
// LastErr, and Running state so the UI can reflect current health.
//
// Design notes:
//   - RunNow is synchronous. Callers that want non-blocking behaviour must
//     wrap it in a goroutine themselves.
//   - Per-task locking prevents two concurrent RunNow calls from racing for the
//     same task. A second caller blocks until the first finishes.
//   - Registry-level locking is used only for brief metadata reads/writes, so
//     long-running tasks do not block List or Get for the whole duration.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned by RunNow / Get when the id is not registered.
var ErrNotFound = errors.New("task not found")

// ErrAlreadyRegistered is returned by Register when the id already exists.
var ErrAlreadyRegistered = errors.New("task id already registered")

// Info is the read-only view of a task for UI / JSON responses.
type Info struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	IntervalMs int64         `json:"interval_ms"` // 0 = on-demand only
	LastRun    time.Time     `json:"last_run"`
	LastErr    string        `json:"last_err"`
	Running    bool          `json:"running"`
}

// Task is a registered job.
type Task struct {
	ID       string
	Name     string
	Interval time.Duration
	RunFn    func(ctx context.Context) error
}

// entry is the internal mutable record kept per task.
type entry struct {
	task    Task
	mu      sync.Mutex // per-task lock for RunNow calls
	lastRun time.Time
	lastErr string
	running bool
}

// Registry is a concurrency-safe collection of named tasks.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
}

// Register adds a Task to the registry.
// Returns ErrAlreadyRegistered if an entry with the same ID already exists.
func (r *Registry) Register(t Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[t.ID]; ok {
		return fmt.Errorf("%w: %s", ErrAlreadyRegistered, t.ID)
	}
	r.entries[t.ID] = &entry{task: t}
	return nil
}

// List returns a snapshot of all registered tasks, sorted by ID.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, infoOf(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the current Info for the task identified by id.
// Returns ErrNotFound if no task with that id is registered.
func (r *Registry) Get(id string) (Info, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return Info{}, false
	}
	return infoOf(e), true
}

// RunNow runs the task identified by id synchronously.
//
// It acquires a per-task mutex so that concurrent calls for the same task
// serialise — the second caller waits for the first to finish. Running is set
// to true before the call and cleared after; LastRun and LastErr are updated
// on completion.
//
// Returns ErrNotFound if the id is unknown.
func (r *Registry) RunNow(ctx context.Context, id string) (Info, error) {
	r.mu.RLock()
	e, ok := r.entries[id]
	r.mu.RUnlock()
	if !ok {
		return Info{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	// Per-task serialisation — blocks if another caller is already running this task.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Mark running.
	r.mu.Lock()
	e.running = true
	r.mu.Unlock()

	start := time.Now()
	runErr := e.task.RunFn(ctx)

	r.mu.Lock()
	e.lastRun = start
	if runErr != nil {
		e.lastErr = runErr.Error()
	} else {
		e.lastErr = ""
	}
	e.running = false
	snap := infoOf(e)
	r.mu.Unlock()

	return snap, runErr
}

// infoOf builds an Info snapshot from an entry.
// Must be called with r.mu (read or write) held.
func infoOf(e *entry) Info {
	return Info{
		ID:         e.task.ID,
		Name:       e.task.Name,
		IntervalMs: e.task.Interval.Milliseconds(),
		LastRun:    e.lastRun,
		LastErr:    e.lastErr,
		Running:    e.running,
	}
}
