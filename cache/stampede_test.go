package cache

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStampedeSingleRequester(t *testing.T) {
	sl := NewStampedeLock()

	entry, waited := sl.Acquire("key1")
	if waited {
		t.Fatal("first acquirer should not wait")
	}
	if entry != nil {
		t.Fatal("first acquirer should get nil entry")
	}

	// Release with an entry.
	want := &Entry{Body: []byte("hello"), StatusCode: 200}
	sl.Release("key1", want, nil)

	// A subsequent acquire for the same key should not block (lock is removed).
	entry2, waited2 := sl.Acquire("key1")
	if waited2 {
		t.Fatal("acquire after release should not wait")
	}
	if entry2 != nil {
		t.Fatal("acquire after release should get nil (fresh lock)")
	}
	sl.Release("key1", nil, nil)
}

func TestStampedeWaitersReceiveEntry(t *testing.T) {
	sl := NewStampedeLock()

	// First goroutine acquires the lock.
	_, waited := sl.Acquire("key1")
	if waited {
		t.Fatal("first acquirer should not wait")
	}

	want := &Entry{Body: []byte("data"), StatusCode: 200, Header: http.Header{"X-Test": {"1"}}}
	const numWaiters = 10
	var wg sync.WaitGroup
	results := make([]*Entry, numWaiters)

	for i := 0; i < numWaiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entry, w := sl.Acquire("key1")
			if !w {
				t.Errorf("waiter %d should have waited", idx)
				return
			}
			results[idx] = entry
		}(i)
	}

	// Give goroutines time to block.
	time.Sleep(50 * time.Millisecond)

	// Release the result.
	sl.Release("key1", want, nil)
	wg.Wait()

	for i, r := range results {
		if r == nil {
			t.Errorf("waiter %d got nil entry", i)
			continue
		}
		if string(r.Body) != "data" {
			t.Errorf("waiter %d got body %q, want %q", i, r.Body, "data")
		}
		if r.StatusCode != 200 {
			t.Errorf("waiter %d got status %d, want 200", i, r.StatusCode)
		}
	}
}

func TestStampedeConcurrentOnlyOneFetches(t *testing.T) {
	sl := NewStampedeLock()
	var fetchCount atomic.Int32
	const numGoroutines = 50

	var wg sync.WaitGroup
	barrier := make(chan struct{})

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier // start all at once
			entry, waited := sl.Acquire("hot-key")
			if !waited {
				// This goroutine is the fetcher.
				fetchCount.Add(1)
				// Simulate fetch.
				time.Sleep(10 * time.Millisecond)
				sl.Release("hot-key", &Entry{Body: []byte("result")}, nil)
			}
			_ = entry
		}()
	}

	close(barrier)
	wg.Wait()

	if fc := fetchCount.Load(); fc != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", fc)
	}
}

func TestStampedeDifferentKeysDontBlock(t *testing.T) {
	sl := NewStampedeLock()

	// Acquire lock for key1.
	_, waited := sl.Acquire("key1")
	if waited {
		t.Fatal("should not wait")
	}

	// Acquire lock for key2 — should not block.
	done := make(chan bool, 1)
	go func() {
		_, w := sl.Acquire("key2")
		done <- w
		sl.Release("key2", &Entry{}, nil)
	}()

	select {
	case w := <-done:
		if w {
			t.Fatal("key2 should not have waited (different key)")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("key2 acquire blocked — different keys should be independent")
	}

	sl.Release("key1", nil, nil)
}

func TestStampedeReleaseWithErrorPropagates(t *testing.T) {
	sl := NewStampedeLock()

	// First goroutine acquires.
	_, waited := sl.Acquire("err-key")
	if waited {
		t.Fatal("should not wait")
	}

	const numWaiters = 5
	var wg sync.WaitGroup
	gotNil := make([]bool, numWaiters)

	for i := 0; i < numWaiters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entry, w := sl.Acquire("err-key")
			if !w {
				t.Errorf("waiter %d should have waited", idx)
				return
			}
			gotNil[idx] = (entry == nil)
		}(i)
	}

	time.Sleep(50 * time.Millisecond)

	// Release with an error.
	sl.Release("err-key", nil, errors.New("origin down"))
	wg.Wait()

	for i, isNil := range gotNil {
		if !isNil {
			t.Errorf("waiter %d should have received nil entry on error", i)
		}
	}
}
