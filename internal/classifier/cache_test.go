package classifier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/anilist"
)

type fakeAniList struct {
	calls atomic.Int32
	mu    sync.Mutex
	next  func(title string) (anilist.Result, error)
}

func (f *fakeAniList) Lookup(_ context.Context, title string) (anilist.Result, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next == nil {
		return anilist.Result{}, nil
	}
	return f.next(title)
}

func TestCachingAniListClient_CachesSuccess(t *testing.T) {
	inner := &fakeAniList{}
	inner.next = func(title string) (anilist.Result, error) {
		return anilist.Result{CountryOfOrigin: "JP"}, nil
	}
	c := NewCachingAniListClient(inner, time.Hour, time.Hour)

	for i := 0; i < 5; i++ {
		got, err := c.Lookup(context.Background(), "Berserk")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.CountryOfOrigin != "JP" {
			t.Errorf("call %d: country=%q", i, got.CountryOfOrigin)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("inner Lookup called %d times; want 1 (subsequent hits should be cache)", got)
	}
}

func TestCachingAniListClient_CachesNotFound(t *testing.T) {
	inner := &fakeAniList{}
	inner.next = func(title string) (anilist.Result, error) {
		return anilist.Result{}, anilist.ErrNotFound
	}
	c := NewCachingAniListClient(inner, time.Hour, time.Hour)

	for i := 0; i < 3; i++ {
		_, err := c.Lookup(context.Background(), "Missing Title")
		if !errors.Is(err, anilist.ErrNotFound) {
			t.Fatalf("call %d: want ErrNotFound, got %v", i, err)
		}
	}
	if got := inner.calls.Load(); got != 1 {
		t.Errorf("inner called %d times; want 1 (negative cache should hold)", got)
	}
}

func TestCachingAniListClient_DoesNotCacheTransportErrors(t *testing.T) {
	inner := &fakeAniList{}
	transportErr := fmt.Errorf("anilist rate limited")
	inner.next = func(title string) (anilist.Result, error) {
		return anilist.Result{}, transportErr
	}
	c := NewCachingAniListClient(inner, time.Hour, time.Hour)

	for i := 0; i < 4; i++ {
		_, err := c.Lookup(context.Background(), "Berserk")
		if err == nil || err.Error() != "anilist rate limited" {
			t.Fatalf("call %d: want rate-limited error, got %v", i, err)
		}
	}
	if got := inner.calls.Load(); got != 4 {
		t.Errorf("inner called %d times; want 4 (transport errors must NOT be cached)", got)
	}
}

func TestCachingAniListClient_TTLExpiry(t *testing.T) {
	inner := &fakeAniList{}
	inner.next = func(title string) (anilist.Result, error) {
		return anilist.Result{CountryOfOrigin: "KR"}, nil
	}
	c := NewCachingAniListClient(inner, time.Millisecond, time.Millisecond)

	if _, err := c.Lookup(context.Background(), "Solo Leveling"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // way past TTL
	if _, err := c.Lookup(context.Background(), "Solo Leveling"); err != nil {
		t.Fatal(err)
	}
	if got := inner.calls.Load(); got != 2 {
		t.Errorf("inner called %d times; want 2 (TTL expired between calls)", got)
	}
}

func TestCachingAniListClient_ConcurrentSafe(t *testing.T) {
	inner := &fakeAniList{}
	inner.next = func(title string) (anilist.Result, error) {
		return anilist.Result{CountryOfOrigin: "JP"}, nil
	}
	c := NewCachingAniListClient(inner, time.Hour, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			title := fmt.Sprintf("title-%d", i%20) // 20 distinct, each hit 10x
			if _, err := c.Lookup(context.Background(), title); err != nil {
				t.Errorf("lookup: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := inner.calls.Load(); got > 20 {
		t.Errorf("inner called %d times; want at most 20 (one per distinct title)", got)
	}
}
