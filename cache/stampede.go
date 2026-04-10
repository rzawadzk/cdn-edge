// Package cache — stampede.go provides a distributed lock to prevent
// thundering-herd (cache stampede) on cache misses. When many concurrent
// requests arrive for the same uncached key, only one goroutine fetches
// from origin; all others wait and receive the same result.
package cache

import (
	"sync"
	"sync/atomic"
)

// StampedeLock prevents multiple concurrent requests from fetching the
// same uncached resource simultaneously. Only one request fetches from
// origin; others wait for the result.
type StampedeLock struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	done        chan struct{}
	streamReady chan struct{} // closed when streaming entry is available
	entry       *Entry
	streaming   *StreamingEntry
	err         error
	waiters     int32
}

// NewStampedeLock creates a new StampedeLock.
func NewStampedeLock() *StampedeLock {
	return &StampedeLock{
		locks: make(map[string]*lockEntry),
	}
}

// Acquire tries to get a lock for the given cache key.
// Returns (entry, true) if another goroutine already fetched the result
// (the caller can use the entry immediately).
// Returns (nil, false) if this goroutine should perform the fetch and
// then call Release to publish the result.
func (sl *StampedeLock) Acquire(key string) (*Entry, bool) {
	sl.mu.Lock()
	le, exists := sl.locks[key]
	if exists {
		// Another goroutine is already fetching. Wait for the result.
		atomic.AddInt32(&le.waiters, 1)
		sl.mu.Unlock()
		<-le.done
		if le.err != nil {
			return nil, true // signal that fetch was attempted but failed
		}
		return le.entry, true
	}

	// First requester — create a lock entry.
	le = &lockEntry{
		done:        make(chan struct{}),
		streamReady: make(chan struct{}),
	}
	sl.locks[key] = le
	sl.mu.Unlock()
	return nil, false
}

// Release publishes the fetch result to all waiting goroutines and
// removes the lock entry for the key.
func (sl *StampedeLock) Release(key string, entry *Entry, err error) {
	sl.mu.Lock()
	le, ok := sl.locks[key]
	if !ok {
		sl.mu.Unlock()
		return
	}
	le.entry = entry
	le.err = err
	delete(sl.locks, key)
	sl.mu.Unlock()
	close(le.done)
}

// SetStreaming publishes a StreamingEntry so that waiters can start
// reading bytes before the full response is downloaded. Must be called
// by the fetcher goroutine (the one that got waited=false from Acquire)
// before Release. Waiters using AcquireStreaming will receive this entry.
func (sl *StampedeLock) SetStreaming(key string, se *StreamingEntry) {
	sl.mu.Lock()
	le, ok := sl.locks[key]
	sl.mu.Unlock()
	if !ok {
		return
	}
	le.streaming = se
	close(le.streamReady)
}

// AcquireStreaming is like Acquire but returns a *StreamingEntry if the
// fetcher has called SetStreaming. The returned streaming entry may be
// non-nil even if *Entry is nil (streaming is still in progress).
//
// Return values:
//   - (nil, nil, false) — this goroutine is the fetcher; perform the fetch.
//   - (entry, nil, true) — waited; fetch completed with a full entry (or nil+error).
//   - (nil, streamingEntry, true) — waited; streaming entry available for progressive reading.
func (sl *StampedeLock) AcquireStreaming(key string) (*Entry, *StreamingEntry, bool) {
	sl.mu.Lock()
	le, exists := sl.locks[key]
	if exists {
		atomic.AddInt32(&le.waiters, 1)
		sl.mu.Unlock()

		// Wait for either the streaming entry or full completion.
		select {
		case <-le.streamReady:
			// Streaming entry is available — return it immediately.
			return nil, le.streaming, true
		case <-le.done:
			// Completed before streaming was set, or streaming was set and done.
			if le.err != nil {
				return nil, nil, true
			}
			if le.streaming != nil {
				return nil, le.streaming, true
			}
			return le.entry, nil, true
		}
	}

	// First requester.
	le = &lockEntry{
		done:        make(chan struct{}),
		streamReady: make(chan struct{}),
	}
	sl.locks[key] = le
	sl.mu.Unlock()
	return nil, nil, false
}

// Waiters returns the number of goroutines waiting on a key (for testing).
func (sl *StampedeLock) Waiters(key string) int32 {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	le, ok := sl.locks[key]
	if !ok {
		return 0
	}
	return atomic.LoadInt32(&le.waiters)
}
