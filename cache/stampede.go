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
	done    chan struct{}
	entry   *Entry
	err     error
	waiters int32
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
		done: make(chan struct{}),
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
