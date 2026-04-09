package cache

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func BenchmarkCacheGetHit(b *testing.B) {
	c, _ := New(10000, 0, "", 0)
	c.Put("key", &Entry{
		Body:     []byte("hello world"),
		Header:   http.Header{"Content-Type": {"text/plain"}},
		StoredAt: time.Now(),
		TTL:      time.Hour,
	})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Get("key")
	}
}

func BenchmarkCacheGetMiss(b *testing.B) {
	c, _ := New(10000, 0, "", 0)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Get("nonexistent")
	}
}

func BenchmarkCachePut(b *testing.B) {
	c, _ := New(100000, 0, "", 0)
	entry := &Entry{
		Body:     []byte("hello world"),
		Header:   http.Header{"Content-Type": {"text/plain"}},
		StoredAt: time.Now(),
		TTL:      time.Hour,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Put(fmt.Sprintf("key-%d", i), entry)
	}
}

func BenchmarkCacheGetParallel(b *testing.B) {
	c, _ := New(10000, 0, "", 0)
	// Pre-populate.
	for i := 0; i < 1000; i++ {
		c.Put(fmt.Sprintf("key-%d", i), &Entry{
			Body:     []byte("body"),
			StoredAt: time.Now(),
			TTL:      time.Hour,
		})
	}

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			c.Get(fmt.Sprintf("key-%d", i%1000))
			i++
		}
	})
}

func BenchmarkNormalizeKey(b *testing.B) {
	headers := http.Header{"Accept-Encoding": {"gzip"}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		NormalizeKey("example.com", "/foo/bar", "a=1&b=2", "Accept-Encoding", headers)
	}
}

func BenchmarkLRUEviction(b *testing.B) {
	c, _ := New(100, 0, "", 0)
	entry := &Entry{
		Body:     []byte("body"),
		StoredAt: time.Now(),
		TTL:      time.Hour,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Put(fmt.Sprintf("key-%d", i), entry)
	}
}
