package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/config"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
)

func BenchmarkCacheHit(b *testing.B) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write([]byte("hello world"))
	}))
	defer origin.Close()

	c, _ := cache.New(10000, 0, "", 0)
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	cdn := New(c, o, &config.Config{OriginURL: origin.URL, DefaultTTL: time.Hour}, logging.New())

	// Warm cache.
	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/bench", nil))

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/bench", nil))
	}
}

func BenchmarkCacheHitParallel(b *testing.B) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "max-age=3600")
		w.Write([]byte("hello world"))
	}))
	defer origin.Close()

	c, _ := cache.New(10000, 0, "", 0)
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	cdn := New(c, o, &config.Config{OriginURL: origin.URL, DefaultTTL: time.Hour}, logging.New())

	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/bench", nil))

	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			w := httptest.NewRecorder()
			cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/bench", nil))
		}
	})
}
