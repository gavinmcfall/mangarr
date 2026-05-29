package tasks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// makeTask returns a Task with a no-op RunFn.
func makeTask(id, name string, interval time.Duration, fn func(ctx context.Context) error) Task {
	if fn == nil {
		fn = func(ctx context.Context) error { return nil }
	}
	return Task{ID: id, Name: name, Interval: interval, RunFn: fn}
}

func TestRegisterRejectsDuplicateID(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(makeTask("a", "A", 0, nil)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(makeTask("a", "A2", 0, nil))
	if err == nil {
		t.Fatal("expected error for duplicate ID, got nil")
	}
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("want ErrAlreadyRegistered, got %v", err)
	}
}

func TestListIsSortedByID(t *testing.T) {
	r := NewRegistry()
	// Register out-of-order.
	for _, id := range []string{"charlie", "alpha", "bravo"} {
		if err := r.Register(makeTask(id, id, 0, nil)); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	list := r.List()
	if len(list) != 3 {
		t.Fatalf("want 3 entries, got %d", len(list))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, info := range list {
		if info.ID != want[i] {
			t.Errorf("list[%d].ID = %q, want %q", i, info.ID, want[i])
		}
	}
}

func TestRunNowRecordsLastRun(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(makeTask("noop", "No-op", 0, nil)); err != nil {
		t.Fatalf("register: %v", err)
	}

	before := time.Now()
	info, err := r.RunNow(context.Background(), "noop")
	after := time.Now()

	if err != nil {
		t.Fatalf("RunNow error: %v", err)
	}
	if info.LastRun.Before(before) || info.LastRun.After(after) {
		t.Errorf("LastRun %v not in [%v, %v]", info.LastRun, before, after)
	}
	if info.LastErr != "" {
		t.Errorf("want empty LastErr, got %q", info.LastErr)
	}
	if info.Running {
		t.Error("Running should be false after completion")
	}
}

func TestRunNowRecordsErr(t *testing.T) {
	r := NewRegistry()
	boom := errors.New("disk exploded")
	fn := func(ctx context.Context) error { return boom }
	if err := r.Register(makeTask("fail", "Fail", 0, fn)); err != nil {
		t.Fatalf("register: %v", err)
	}

	info, err := r.RunNow(context.Background(), "fail")
	if err == nil {
		t.Fatal("expected error from RunNow, got nil")
	}
	if info.LastErr != boom.Error() {
		t.Errorf("want LastErr %q, got %q", boom.Error(), info.LastErr)
	}
	if info.Running {
		t.Error("Running should be false after failed completion")
	}
}

func TestRunNowConcurrentSameIDBlocks(t *testing.T) {
	r := NewRegistry()

	var mu sync.Mutex
	callCount := 0

	started := make(chan struct{})
	release := make(chan struct{})

	fn := func(ctx context.Context) error {
		mu.Lock()
		callCount++
		mu.Unlock()
		// Signal that we've started, then block until released.
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	if err := r.Register(makeTask("slow", "Slow", 0, fn)); err != nil {
		t.Fatalf("register: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		r.RunNow(context.Background(), "slow") //nolint:errcheck
	}()

	// Wait until the first goroutine enters RunFn before launching the second.
	<-started

	go func() {
		defer wg.Done()
		r.RunNow(context.Background(), "slow") //nolint:errcheck
	}()

	// Let both finish.
	close(release)
	wg.Wait()

	mu.Lock()
	total := callCount
	mu.Unlock()

	if total != 2 {
		t.Errorf("want callCount=2 (both ran, serialised), got %d", total)
	}
}

func TestRunNowUnknownID(t *testing.T) {
	r := NewRegistry()
	_, err := r.RunNow(context.Background(), "no-such-task")
	if err == nil {
		t.Fatal("expected error for unknown id, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
