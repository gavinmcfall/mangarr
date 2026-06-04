package classifier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/mangadex"
)

type countingMangaDex struct {
	calls atomic.Int32
	fn    func(title string) (mangadex.Result, error)
}

func (f *countingMangaDex) Lookup(_ context.Context, title string) (mangadex.Result, error) {
	f.calls.Add(1)
	return f.fn(title)
}

func TestCachingMangaDex_CachesSuccess(t *testing.T) {
	inner := &countingMangaDex{fn: func(string) (mangadex.Result, error) {
		return mangadex.Result{OriginalLanguage: "ko"}, nil
	}}
	c := NewCachingMangaDexClient(inner, time.Hour, time.Hour)
	for i := 0; i < 5; i++ {
		got, err := c.Lookup(context.Background(), "Solo Leveling")
		if err != nil || got.OriginalLanguage != "ko" {
			t.Fatalf("call %d: %v %+v", i, err, got)
		}
	}
	if inner.calls.Load() != 1 {
		t.Errorf("inner called %d times; want 1", inner.calls.Load())
	}
}

func TestCachingMangaDex_CachesNotFound(t *testing.T) {
	inner := &countingMangaDex{fn: func(string) (mangadex.Result, error) {
		return mangadex.Result{}, mangadex.ErrNotFound
	}}
	c := NewCachingMangaDexClient(inner, time.Hour, time.Hour)
	for i := 0; i < 3; i++ {
		if _, err := c.Lookup(context.Background(), "x"); !errors.Is(err, mangadex.ErrNotFound) {
			t.Fatalf("call %d: want ErrNotFound, got %v", i, err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Errorf("inner called %d times; want 1 (negative cache)", inner.calls.Load())
	}
}

func TestCachingMangaDex_DoesNotCacheTransportErrors(t *testing.T) {
	inner := &countingMangaDex{fn: func(string) (mangadex.Result, error) {
		return mangadex.Result{}, fmt.Errorf("mangadex rate limited")
	}}
	c := NewCachingMangaDexClient(inner, time.Hour, time.Hour)
	for i := 0; i < 4; i++ {
		if _, err := c.Lookup(context.Background(), "x"); err == nil {
			t.Fatalf("call %d: want error", i)
		}
	}
	if inner.calls.Load() != 4 {
		t.Errorf("inner called %d times; want 4 (transport errors not cached)", inner.calls.Load())
	}
}

func TestCachingMangaDex_TTLExpiry(t *testing.T) {
	inner := &countingMangaDex{fn: func(string) (mangadex.Result, error) {
		return mangadex.Result{OriginalLanguage: "ja"}, nil
	}}
	c := NewCachingMangaDexClient(inner, time.Millisecond, time.Millisecond)
	if _, err := c.Lookup(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := c.Lookup(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 2 {
		t.Errorf("inner called %d times; want 2 (TTL expired)", inner.calls.Load())
	}
}

func TestCachingMangaDex_ConcurrentSingleflight(t *testing.T) {
	inner := &countingMangaDex{fn: func(string) (mangadex.Result, error) {
		time.Sleep(2 * time.Millisecond) // widen the race window
		return mangadex.Result{OriginalLanguage: "ko"}, nil
	}}
	c := NewCachingMangaDexClient(inner, time.Hour, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.Lookup(context.Background(), fmt.Sprintf("title-%d", i%20))
		}(i)
	}
	wg.Wait()
	// 20 distinct titles; singleflight collapses concurrent cold misses so
	// the inner client is called at most once per distinct title.
	if got := inner.calls.Load(); got > 20 {
		t.Errorf("inner called %d times; want <= 20 (singleflight)", got)
	}
}
