// Package health provides a generic health-check registry for mangarr.
//
// A Registry holds named Check functions. RunAll executes them sequentially
// with per-check timeouts and returns sorted results.
package health

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Status is the health-check result level.
type Status string

const (
	StatusOK    Status = "ok"
	StatusWarn  Status = "warn"
	StatusError Status = "error"
)

// Result is one check's outcome.
type Result struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// Check is a single named check function. Should be fast (<200ms) and non-blocking.
type Check struct {
	ID   string
	Name string
	Run  func(ctx context.Context) Result
}

// Registry holds the configured checks.
type Registry struct {
	mu     sync.Mutex
	checks []Check
	byID   map[string]struct{}
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]struct{})}
}

// Register adds a check to the registry. Returns an error if the ID is already registered.
func (r *Registry) Register(c Check) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[c.ID]; exists {
		return &duplicateIDError{id: c.ID}
	}
	r.byID[c.ID] = struct{}{}
	r.checks = append(r.checks, c)
	return nil
}

// RunAll executes each check sequentially with a per-check 5-second timeout.
// Results are sorted by ID.
func (r *Registry) RunAll(ctx context.Context) []Result {
	r.mu.Lock()
	checks := make([]Check, len(r.checks))
	copy(checks, r.checks)
	r.mu.Unlock()

	results := make([]Result, 0, len(checks))
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		res := runWithTimeout(cctx, c)
		cancel()
		results = append(results, res)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].ID < results[j].ID
	})
	return results
}

// runWithTimeout runs the check in the given context. If the context times
// out before the check returns, it returns a timeout error result.
func runWithTimeout(ctx context.Context, c Check) Result {
	type outcome struct{ r Result }
	ch := make(chan outcome, 1)
	go func() {
		ch <- outcome{c.Run(ctx)}
	}()
	select {
	case out := <-ch:
		return out.r
	case <-ctx.Done():
		return Result{
			ID:          c.ID,
			Name:        c.Name,
			Status:      StatusError,
			Message:     "check timed out",
			Remediation: "The health check did not complete within 5 seconds.",
		}
	}
}

// WorstStatus returns the overall worst status from a slice of results.
// StatusError > StatusWarn > StatusOK. Returns StatusOK for an empty slice.
func WorstStatus(results []Result) Status {
	worst := StatusOK
	for _, r := range results {
		switch r.Status {
		case StatusError:
			return StatusError // can't get worse
		case StatusWarn:
			if worst == StatusOK {
				worst = StatusWarn
			}
		}
	}
	return worst
}

// duplicateIDError is returned when a check ID is registered twice.
type duplicateIDError struct{ id string }

func (e *duplicateIDError) Error() string {
	return "health: duplicate check ID: " + e.id
}
