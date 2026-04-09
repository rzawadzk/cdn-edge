package cache

import (
	"net/http"
	"os"
	"testing"
	"time"
)

func TestPutAndGet(t *testing.T) {
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
		ETag:       `"abc"`,
	}
	c.Put("key1", entry)

	got := c.Get("key1")
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if string(got.Body) != "hello" {
		t.Errorf("body = %q, want %q", got.Body, "hello")
	}
	if got.ETag != `"abc"` {
		t.Errorf("etag = %q, want %q", got.ETag, `"abc"`)
	}
}

func TestGetMiss(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	if got := c.Get("nonexistent"); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestExpiry(t *testing.T) {
	c, _ := New(100, 0, "", 0)

	entry := &Entry{
		Body:     []byte("old"),
		StoredAt: time.Now().Add(-10 * time.Minute),
		TTL:      1 * time.Minute,
	}
	c.Put("expired", entry)

	if got := c.Get("expired"); got != nil {
		t.Error("expected expired entry to return nil")
	}
}

func TestStaleWhileRevalidate(t *testing.T) {
	c, _ := New(100, 0, "", 0)

	entry := &Entry{
		Body:                 []byte("stale"),
		StoredAt:             time.Now().Add(-6 * time.Minute),
		TTL:                  5 * time.Minute,
		StaleWhileRevalidate: 5 * time.Minute,
	}
	c.Put("stale", entry)

	got := c.Get("stale")
	if got == nil {
		t.Fatal("expected stale entry to be returned within SWR window")
	}
	if !got.IsStaleServable() {
		t.Error("expected IsStaleServable to be true")
	}
}

func TestStaleExpiredBeyondSWR(t *testing.T) {
	c, _ := New(100, 0, "", 0)

	entry := &Entry{
		Body:                 []byte("very stale"),
		StoredAt:             time.Now().Add(-20 * time.Minute),
		TTL:                  5 * time.Minute,
		StaleWhileRevalidate: 5 * time.Minute,
	}
	c.Put("verystale", entry)

	if got := c.Get("verystale"); got != nil {
		t.Error("expected entry beyond SWR window to return nil")
	}
}

func TestLRUEviction(t *testing.T) {
	c, _ := New(3, 0, "", 0)

	for i := 0; i < 5; i++ {
		c.Put(string(rune('a'+i)), &Entry{
			Body:     []byte{byte(i)},
			StoredAt: time.Now(),
			TTL:      time.Hour,
		})
	}

	if c.Len() != 3 {
		t.Errorf("len = %d, want 3", c.Len())
	}
	// Oldest entries should be evicted.
	if c.Get("a") != nil {
		t.Error("expected 'a' to be evicted")
	}
	if c.Get("b") != nil {
		t.Error("expected 'b' to be evicted")
	}
	if c.Get("e") == nil {
		t.Error("expected 'e' to still exist")
	}
}

func TestMaxEntrySize(t *testing.T) {
	c, _ := New(100, 10, "", 0) // max 10 bytes per entry

	small := &Entry{Body: []byte("hi"), StoredAt: time.Now(), TTL: time.Hour}
	big := &Entry{Body: []byte("this is way too large"), StoredAt: time.Now(), TTL: time.Hour}

	c.Put("small", small)
	c.Put("big", big)

	if c.Get("small") == nil {
		t.Error("expected small entry to be cached")
	}
	if c.Get("big") != nil {
		t.Error("expected big entry to be rejected")
	}
}

func TestDelete(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	c.Put("key", &Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})
	c.Delete("key")
	if c.Get("key") != nil {
		t.Error("expected nil after delete")
	}
}

func TestPurge(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	c.Put("a", &Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})
	c.Put("b", &Entry{Body: []byte("y"), StoredAt: time.Now(), TTL: time.Hour})
	c.Purge()
	if c.Len() != 0 {
		t.Errorf("len after purge = %d, want 0", c.Len())
	}
}

func TestStats(t *testing.T) {
	c, _ := New(100, 0, "", 0)
	c.Put("a", &Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})
	c.Get("a") // hit
	c.Get("b") // miss

	hits, misses, _, _ := c.GetStats()
	if hits != 1 {
		t.Errorf("hits = %d, want 1", hits)
	}
	if misses != 1 {
		t.Errorf("misses = %d, want 1", misses)
	}
}

func TestDiskSpill(t *testing.T) {
	dir := t.TempDir()
	c, err := New(2, 0, dir, 1<<20) // 2 items in memory, 1MB disk
	if err != nil {
		t.Fatal(err)
	}

	// Fill memory and force spill.
	for i := 0; i < 4; i++ {
		c.Put(string(rune('a'+i)), &Entry{
			Body:     []byte{byte(i)},
			Header:   http.Header{},
			StoredAt: time.Now(),
			TTL:      time.Hour,
		})
	}

	// 'a' and 'b' should have been evicted from memory and spilled to disk.
	if c.Len() != 2 {
		t.Errorf("memory len = %d, want 2", c.Len())
	}

	// Should be loadable from disk.
	got := c.Get("a")
	if got == nil {
		t.Error("expected 'a' to be loaded from disk")
	}
}

func TestNormalizeKey(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		path     string
		query    string
		vary     string
		headers  http.Header
		wantKey  string
	}{
		{
			name:    "simple",
			host:    "Example.COM",
			path:    "/foo",
			query:   "",
			wantKey: "example.com/foo",
		},
		{
			name:    "sorted query",
			host:    "example.com",
			path:    "/foo",
			query:   "z=1&a=2",
			wantKey: "example.com/foo?a=2&z=1",
		},
		{
			name:    "vary aware",
			host:    "example.com",
			path:    "/foo",
			vary:    "Accept-Encoding",
			headers: http.Header{"Accept-Encoding": {"gzip"}},
			wantKey: "example.com/foo|vary:Accept-Encoding=gzip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeKey(tt.host, tt.path, tt.query, tt.vary, tt.headers)
			if got != tt.wantKey {
				t.Errorf("got %q, want %q", got, tt.wantKey)
			}
		})
	}
}

func TestDiskCacheMaxSize(t *testing.T) {
	dir := t.TempDir()
	// Very small disk budget: 500 bytes.
	c, err := New(1, 0, dir, 500)
	if err != nil {
		t.Fatal(err)
	}

	bigBody := make([]byte, 200)
	for i := 0; i < 5; i++ {
		c.Put(string(rune('a'+i)), &Entry{
			Body:     bigBody,
			Header:   http.Header{},
			StoredAt: time.Now(),
			TTL:      time.Hour,
		})
	}

	// Disk should not exceed budget by much.
	entries, _ := os.ReadDir(dir)
	var totalSize int64
	for _, e := range entries {
		info, _ := e.Info()
		totalSize += info.Size()
	}
	if totalSize > 600 { // some tolerance for JSON overhead
		t.Errorf("disk usage = %d, exceeds budget of 500", totalSize)
	}
}
