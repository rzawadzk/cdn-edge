package cache

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPredictor_MarkAndCheck(t *testing.T) {
	p := NewPredictor(1000, time.Minute)

	key := "example.com|/api/users|"

	// Unmarked: should be miss.
	if p.IsLikelyUncacheable(key) {
		t.Fatalf("unmarked key reported as uncacheable")
	}

	// After one observation, the default threshold (2) is not yet met.
	p.MarkUncacheable(key)
	if p.IsLikelyUncacheable(key) {
		t.Fatalf("threshold should require >= 2 observations, was 1")
	}

	// Second observation crosses the threshold.
	p.MarkUncacheable(key)
	if !p.IsLikelyUncacheable(key) {
		t.Fatalf("key should be uncacheable after 2 observations")
	}
}

func TestPredictor_UnmarkedKeyFalse(t *testing.T) {
	p := NewPredictor(1000, time.Minute)

	p.MarkUncacheable("example.com|/api/users|")
	p.MarkUncacheable("example.com|/api/users|")

	// A totally different key should still be reported as cacheable.
	if p.IsLikelyUncacheable("example.com|/static/style.css|") {
		t.Fatalf("unrelated key falsely reported as uncacheable")
	}
}

func TestPredictor_DecayForgetsStaleEntries(t *testing.T) {
	p := NewPredictor(1000, time.Minute)

	key := "example.com|/api/checkout|"
	p.MarkUncacheable(key)
	p.MarkUncacheable(key)
	if !p.IsLikelyUncacheable(key) {
		t.Fatalf("precondition: key should be uncacheable after marking")
	}

	// Two rotations fully purge an entry:
	//   rotation 1: active (with entry) becomes shadow; fresh bucket active.
	//   rotation 2: shadow is cleared; it becomes the new active bucket.
	p.Rotate()
	p.Rotate()

	if p.IsLikelyUncacheable(key) {
		t.Fatalf("stale entry should have decayed after two rotations")
	}
}

func TestPredictor_Concurrent(t *testing.T) {
	p := NewPredictor(10000, time.Minute)

	const (
		writers      = 8
		readers      = 8
		opsPerWorker = 2000
	)

	keys := []string{
		"example.com|/api/a|",
		"example.com|/api/b|",
		"example.com|/api/c|",
		"example.com|/api/d|",
	}

	var wg sync.WaitGroup
	wg.Add(writers + readers)

	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				p.MarkUncacheable(keys[i%len(keys)])
			}
		}()
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				_ = p.IsLikelyUncacheable(keys[i%len(keys)])
			}
		}()
	}

	// Also exercise concurrent Rotate() to ensure the RWMutex keeps things
	// safe under rotation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			p.Rotate()
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	// After all writers ran, the marked keys should be uncacheable (unless a
	// rotation just wiped them). We just ensure no panic/race and that stats
	// were updated.
	hits, misses := p.Stats()
	if hits+misses == 0 {
		t.Fatalf("expected stats to be updated, got hits=%d misses=%d", hits, misses)
	}
}

func TestPredictor_Threshold(t *testing.T) {
	p := NewPredictor(1000, time.Minute)
	// Manually raise the threshold.
	p.threshold = 3

	key := "example.com|/health|"

	p.MarkUncacheable(key)
	if p.IsLikelyUncacheable(key) {
		t.Fatalf("threshold=3, 1 obs: should not be uncacheable")
	}
	p.MarkUncacheable(key)
	if p.IsLikelyUncacheable(key) {
		t.Fatalf("threshold=3, 2 obs: should not be uncacheable")
	}
	p.MarkUncacheable(key)
	if !p.IsLikelyUncacheable(key) {
		t.Fatalf("threshold=3, 3 obs: should be uncacheable")
	}
}

func TestPredictor_Stats(t *testing.T) {
	p := NewPredictor(1000, time.Minute)

	h0, m0 := p.Stats()
	if h0 != 0 || m0 != 0 {
		t.Fatalf("expected zero stats, got hits=%d misses=%d", h0, m0)
	}

	// Two misses.
	_ = p.IsLikelyUncacheable("foo")
	_ = p.IsLikelyUncacheable("bar")

	// Mark and then check — should be a hit.
	p.MarkUncacheable("foo")
	p.MarkUncacheable("foo")
	_ = p.IsLikelyUncacheable("foo")

	hits, misses := p.Stats()
	if hits != 1 {
		t.Fatalf("expected 1 hit, got %d", hits)
	}
	if misses != 2 {
		t.Fatalf("expected 2 misses, got %d", misses)
	}
}

func TestPredictor_StartDecayCancellation(t *testing.T) {
	// Use a very short decay so we actually observe a rotation.
	p := NewPredictor(1000, 10*time.Millisecond)

	key := "example.com|/api/x|"
	p.MarkUncacheable(key)
	p.MarkUncacheable(key)
	if !p.IsLikelyUncacheable(key) {
		t.Fatalf("precondition: key should be uncacheable after marking")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.StartDecay(ctx)
		close(done)
	}()

	// Wait long enough for at least two rotations to occur.
	time.Sleep(50 * time.Millisecond)

	if p.IsLikelyUncacheable(key) {
		t.Fatalf("entry should have been decayed by automatic rotation")
	}

	cancel()
	select {
	case <-done:
		// StartDecay returned as expected.
	case <-time.After(time.Second):
		t.Fatalf("StartDecay did not return after context cancel")
	}
}

func TestPredictor_ZeroDecayIsNoop(t *testing.T) {
	// decay <= 0 means StartDecay should return immediately.
	p := NewPredictor(1000, 0)

	done := make(chan struct{})
	go func() {
		p.StartDecay(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("StartDecay with zero decay should return immediately")
	}
}
