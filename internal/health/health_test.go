package health

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestRegisterRejectsDuplicate(t *testing.T) {
	r := NewRegistry()
	c := Check{ID: "foo", Name: "Foo", Run: func(ctx context.Context) Result {
		return Result{ID: "foo", Name: "Foo", Status: StatusOK, Message: "ok"}
	}}
	if err := r.Register(c); err != nil {
		t.Fatalf("first register: unexpected error: %v", err)
	}
	if err := r.Register(c); err == nil {
		t.Fatal("second register of same ID: expected error, got nil")
	}
}

func TestRunAllSortedByID(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"zzz", "aaa", "mmm"} {
		id := id // capture
		if err := r.Register(Check{
			ID:   id,
			Name: id,
			Run: func(ctx context.Context) Result {
				return Result{ID: id, Status: StatusOK, Message: "ok"}
			},
		}); err != nil {
			t.Fatalf("register %q: %v", id, err)
		}
	}
	results := r.RunAll(context.Background())
	if len(results) != 3 {
		t.Fatalf("want 3 results, got %d", len(results))
	}
	if results[0].ID != "aaa" || results[1].ID != "mmm" || results[2].ID != "zzz" {
		t.Errorf("results not sorted by ID: got %q %q %q", results[0].ID, results[1].ID, results[2].ID)
	}
}

func TestRunAllAppliesTimeout(t *testing.T) {
	r := NewRegistry()
	// This check blocks forever — RunAll's per-check 5-second timeout should
	// kick in and return an error result. We use a very short timeout for the
	// test: override via context cancellation before the 5s internal timeout.
	if err := r.Register(Check{
		ID:   "slow",
		Name: "Slow Check",
		Run: func(ctx context.Context) Result {
			// Block until context expires.
			<-ctx.Done()
			return Result{ID: "slow", Status: StatusOK, Message: "unreachable"}
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Pass a context that cancels almost immediately so the test doesn't
	// actually wait 5 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	results := r.RunAll(ctx)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Status != StatusError {
		t.Errorf("want StatusError for timed-out check, got %q", results[0].Status)
	}
}

func TestRegistryConcurrency(t *testing.T) {
	r := NewRegistry()
	// Register 10 checks.
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		if err := r.Register(Check{
			ID:   id,
			Name: id,
			Run: func(id string) func(context.Context) Result {
				return func(ctx context.Context) Result {
					return Result{ID: id, Status: StatusOK}
				}
			}(id),
		}); err != nil {
			t.Fatalf("register %q: %v", id, err)
		}
	}

	// Run RunAll concurrently from multiple goroutines — race detector will
	// catch any data races.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results := r.RunAll(context.Background())
			if len(results) != 10 {
				t.Errorf("concurrent RunAll: want 10 results, got %d", len(results))
			}
		}()
	}
	wg.Wait()
}

func TestWorstStatus(t *testing.T) {
	cases := []struct {
		results  []Result
		expected Status
	}{
		{nil, StatusOK},
		{[]Result{{Status: StatusOK}}, StatusOK},
		{[]Result{{Status: StatusWarn}}, StatusWarn},
		{[]Result{{Status: StatusError}}, StatusError},
		{[]Result{{Status: StatusOK}, {Status: StatusWarn}}, StatusWarn},
		{[]Result{{Status: StatusOK}, {Status: StatusError}}, StatusError},
		{[]Result{{Status: StatusWarn}, {Status: StatusError}}, StatusError},
	}
	for _, tc := range cases {
		got := WorstStatus(tc.results)
		if got != tc.expected {
			t.Errorf("WorstStatus(%v) = %q, want %q", tc.results, got, tc.expected)
		}
	}
}
