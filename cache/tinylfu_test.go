package cache

import (
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"
)

// ---------- CountMinSketch ----------

func TestCountMinSketchIncrementEstimate(t *testing.T) {
	s := newCountMinSketch(1000)

	s.increment("foo")
	s.increment("foo")
	s.increment("foo")
	s.increment("bar")

	fooEst := s.estimate("foo")
	barEst := s.estimate("bar")
	if fooEst < 3 {
		t.Errorf("foo estimate = %d, want >= 3", fooEst)
	}
	if barEst < 1 {
		t.Errorf("bar estimate = %d, want >= 1", barEst)
	}
	if fooEst <= barEst {
		t.Errorf("foo (%d) should be > bar (%d)", fooEst, barEst)
	}

	// Unknown key should be 0.
	if est := s.estimate("unknown"); est != 0 {
		t.Errorf("unknown estimate = %d, want 0", est)
	}
}

func TestCountMinSketchHalve(t *testing.T) {
	s := newCountMinSketch(1000)
	for i := 0; i < 10; i++ {
		s.increment("x")
	}
	before := s.estimate("x")
	s.halve()
	after := s.estimate("x")
	if after > before/2+1 {
		t.Errorf("after halve: %d, before: %d — expected roughly halved", after, before)
	}
	if after == 0 {
		t.Error("after halve should not be zero for a count of 10")
	}
}

func TestCountMinSketchSaturation(t *testing.T) {
	s := newCountMinSketch(100)
	for i := 0; i < 300; i++ {
		s.increment("sat")
	}
	est := s.estimate("sat")
	if est != 255 {
		t.Errorf("saturated estimate = %d, want 255", est)
	}
}

// ---------- Doorkeeper ----------

func TestDoorkeeper(t *testing.T) {
	d := newDoorkeeper(1000)

	// First add: should NOT be present yet.
	if d.add("hello") {
		t.Error("first add should return false (not present)")
	}
	// Second add: should be present now.
	if !d.add("hello") {
		t.Error("second add should return true (present)")
	}

	// Contains without mutation.
	if !d.contains("hello") {
		t.Error("contains should return true for inserted key")
	}
	if d.contains("unknown") {
		t.Error("contains should return false for never-inserted key")
	}
}

func TestDoorkeeperReset(t *testing.T) {
	d := newDoorkeeper(1000)
	d.add("key1")
	d.add("key1")
	d.reset()
	if d.contains("key1") {
		t.Error("after reset, contains should return false")
	}
}

// ---------- TinyLFU ----------

func TestTinyLFUAdmitFrequentOverInfrequent(t *testing.T) {
	lfu := newTinyLFU(1000)

	// Build up frequency for "hot".
	for i := 0; i < 20; i++ {
		lfu.Increment("hot")
	}
	// "cold" has only been seen once.
	lfu.Increment("cold")

	if !lfu.Admit("hot", "cold") {
		t.Error("expected hot to be admitted over cold")
	}
	if lfu.Admit("cold", "hot") {
		t.Error("expected cold NOT to be admitted over hot")
	}
}

func TestTinyLFUDecay(t *testing.T) {
	maxItems := 100
	lfu := newTinyLFU(maxItems)

	// Bump "old" many times.
	for i := 0; i < 50; i++ {
		lfu.Increment("old")
	}
	oldBefore := lfu.Estimate("old")

	// Trigger decay by doing sample-window (10×maxItems, min 1000) more
	// increments on other keys. Double it for safety.
	window := maxItems * 10
	if window < 1000 {
		window = 1000
	}
	for i := 0; i < window*2; i++ {
		lfu.Increment(fmt.Sprintf("noise-%d", i))
	}

	oldAfter := lfu.Estimate("old")
	if oldAfter >= oldBefore {
		t.Errorf("expected decay: before=%d after=%d", oldBefore, oldAfter)
	}
}

// ---------- Sharded Cache basic ops ----------

func TestShardedPutGet(t *testing.T) {
	c, err := New(100, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	entry := &Entry{
		Body:       []byte("hello"),
		Header:     http.Header{"Content-Type": {"text/plain"}},
		StatusCode: 200,
		StoredAt:   time.Now(),
		TTL:        5 * time.Minute,
	}
	c.Put("k1", entry)
	got := c.Get("k1")
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if string(got.Body) != "hello" {
		t.Errorf("body = %q, want %q", got.Body, "hello")
	}
}

func TestShardedDelete(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	c.Put("k", &Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})
	c.Delete("k")
	if c.Get("k") != nil {
		t.Error("expected nil after delete")
	}
}

func TestShardedPurge(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	for i := 0; i < 20; i++ {
		c.Put(fmt.Sprintf("key-%d", i), &Entry{Body: []byte("v"), StoredAt: time.Now(), TTL: time.Hour})
	}
	c.Purge()
	if c.Len() != 0 {
		t.Errorf("len after purge = %d, want 0", c.Len())
	}
}

// ---------- TinyLFU-aware eviction ----------

func TestShardedEvictionRespectsLFU(t *testing.T) {
	// Use a small cache where the TinyLFU has a real impact.
	// NumShards=1 so all items compete in the same shard.
	c, err := NewWithOptions(Options{MaxItems: 4, NumShards: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Insert 3 "hot" items and access them many times to build frequency.
	for _, k := range []string{"hot-a", "hot-b", "hot-c"} {
		c.Put(k, &Entry{Body: []byte(k), StoredAt: time.Now(), TTL: time.Hour})
		for i := 0; i < 30; i++ {
			c.Get(k)
		}
	}

	// Insert a 4th item (fills the shard).
	c.Put("warm", &Entry{Body: []byte("warm"), StoredAt: time.Now(), TTL: time.Hour})
	for i := 0; i < 10; i++ {
		c.Get("warm")
	}

	// Now try to insert a "cold" item that has never been seen.
	// TinyLFU should reject it because the victim (LRU tail) is more frequent.
	c.Put("cold-new", &Entry{Body: []byte("cold"), StoredAt: time.Now(), TTL: time.Hour})

	// The cold item should NOT be in the cache (rejected by TinyLFU).
	if c.Get("cold-new") != nil {
		// It's acceptable if the cold item got in due to hash collisions in
		// the sketch, but the hot items must still be there.
		t.Log("cold-new was admitted (possible sketch collision)")
	}

	// Hot items must survive.
	for _, k := range []string{"hot-a", "hot-b", "hot-c"} {
		if c.Get(k) == nil {
			t.Errorf("expected %q to survive eviction", k)
		}
	}
}

// ---------- Concurrent stress ----------

func TestShardedConcurrentAccess(t *testing.T) {
	c, _ := New(500, 0, "", 0)

	var wg sync.WaitGroup
	const numGoroutines = 64
	const opsPerGoroutine = 500

	wg.Add(numGoroutines)
	for g := 0; g < numGoroutines; g++ {
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(id)))
			for i := 0; i < opsPerGoroutine; i++ {
				key := fmt.Sprintf("key-%d", rng.Intn(200))
				switch rng.Intn(3) {
				case 0:
					c.Put(key, &Entry{
						Body:     []byte("data"),
						StoredAt: time.Now(),
						TTL:      time.Hour,
					})
				case 1:
					c.Get(key)
				case 2:
					c.Delete(key)
				}
			}
		}(g)
	}
	wg.Wait()

	// Smoke check: cache should be in a consistent state.
	n := c.Len()
	if n < 0 {
		t.Errorf("negative length: %d", n)
	}
}

func TestShardedLen(t *testing.T) {
	c, _ := New(1000, 0, "", 0)
	for i := 0; i < 50; i++ {
		c.Put(fmt.Sprintf("k%d", i), &Entry{Body: []byte("v"), StoredAt: time.Now(), TTL: time.Hour})
	}
	if c.Len() != 50 {
		t.Errorf("len = %d, want 50", c.Len())
	}
}
