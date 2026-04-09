package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/config"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
)

// setupTestOrigin creates a test HTTP server that returns the given body.
func setupTestOrigin(t *testing.T, status int, headers map[string]string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func newTestCDN(t *testing.T, originURL string) (*CDN, *cache.Cache) {
	t.Helper()
	c, err := cache.New(1000, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	cfg := &config.Config{
		OriginURL:  originURL,
		DefaultTTL: 5 * time.Minute,
	}
	log := logging.New()
	return New(c, o, cfg, log), c
}

func TestCacheMissAndHit(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Content-Type":  "text/plain",
		"Cache-Control": "max-age=300",
	}, "hello world")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// First request — cache miss.
	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Cache") != "MISS" {
		t.Errorf("X-Cache = %q, want MISS", w.Header().Get("X-Cache"))
	}
	if w.Body.String() != "hello world" {
		t.Errorf("body = %q, want %q", w.Body.String(), "hello world")
	}

	// Second request — cache hit.
	w2 := httptest.NewRecorder()
	cdn.ServeHTTP(w2, httptest.NewRequest("GET", "http://example.com/test", nil))

	if w2.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", w2.Header().Get("X-Cache"))
	}
}

func TestConditionalRequestETag(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Content-Type":  "text/plain",
		"Cache-Control": "max-age=300",
		"ETag":          `"test-etag"`,
	}, "content")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// Populate cache.
	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/etag", nil))

	// Conditional request.
	req := httptest.NewRequest("GET", "http://example.com/etag", nil)
	req.Header.Set("If-None-Match", `"test-etag"`)
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, req)

	if w.Code != 304 {
		t.Errorf("status = %d, want 304", w.Code)
	}
}

func TestNoCacheBypass(t *testing.T) {
	callCount := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Cache-Control", "max-age=300")
		w.Write([]byte("response"))
	}))
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// First request — populates cache.
	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/bypass", nil))

	// Second request with no-cache — should hit origin again.
	req := httptest.NewRequest("GET", "http://example.com/bypass", nil)
	req.Header.Set("Cache-Control", "no-cache")
	cdn.ServeHTTP(httptest.NewRecorder(), req)

	if callCount != 2 {
		t.Errorf("origin called %d times, want 2", callCount)
	}
}

func TestNoStoreNotCached(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "no-store",
	}, "private data")
	defer origin.Close()

	cdn, c := newTestCDN(t, origin.URL)

	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/private", nil))

	if c.Len() != 0 {
		t.Errorf("cache len = %d, want 0 (no-store should not cache)", c.Len())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	cdn, _ := newTestCDN(t, "http://unused")

	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("POST", "http://example.com/", nil))

	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestPathTraversal(t *testing.T) {
	cdn, _ := newTestCDN(t, "http://unused")

	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/../../../etc/passwd", nil))

	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for path traversal", w.Code)
	}
}

func TestAgeHeader(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "max-age=300",
	}, "aged content")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// Populate.
	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/age", nil))

	// Hit.
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/age", nil))

	age := w.Header().Get("Age")
	if age == "" {
		t.Error("expected Age header on cache hit")
	}
}

func TestBypassOnOriginDown(t *testing.T) {
	// Origin that always 502s.
	origin := setupTestOrigin(t, 502, nil, "bad gateway")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/down", nil))

	// Should get BYPASS since 502 is not in the cacheable list but still passes through.
	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHeadRequest(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "max-age=300",
	}, "head content")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("HEAD", "http://example.com/head", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200 for HEAD", w.Code)
	}
}

func TestIsValidPath(t *testing.T) {
	tests := []struct {
		path  string
		valid bool
	}{
		{"/", true},
		{"/foo/bar", true},
		{"/foo/../bar", false},
		{"foo", false},
		{"/foo/./bar", true},
	}
	for _, tt := range tests {
		if got := isValidPath(tt.path); got != tt.valid {
			t.Errorf("isValidPath(%q) = %v, want %v", tt.path, got, tt.valid)
		}
	}
}

func TestCacheControlParsing(t *testing.T) {
	tests := []struct {
		cc        string
		directive string
		want      bool
	}{
		{"no-store", "no-store", true},
		{"max-age=300, no-store", "no-store", true},
		{"public, max-age=600", "no-store", false},
		{"private", "private", true},
		{"no-cache", "no-cache", true},
	}
	for _, tt := range tests {
		if got := hasCacheDirective(tt.cc, tt.directive); got != tt.want {
			t.Errorf("hasCacheDirective(%q, %q) = %v, want %v", tt.cc, tt.directive, got, tt.want)
		}
	}
}

func TestGetCacheDirectiveValue(t *testing.T) {
	tests := []struct {
		cc        string
		directive string
		want      time.Duration
	}{
		{"max-age=300", "max-age", 300 * time.Second},
		{"s-maxage=60, max-age=300", "s-maxage", 60 * time.Second},
		{"public", "max-age", 0},
		{"stale-while-revalidate=120", "stale-while-revalidate", 120 * time.Second},
	}
	for _, tt := range tests {
		if got := getCacheDirectiveValue(tt.cc, tt.directive); got != tt.want {
			t.Errorf("getCacheDirectiveValue(%q, %q) = %v, want %v", tt.cc, tt.directive, got, tt.want)
		}
	}
}

func TestOriginDownServesStale(t *testing.T) {
	requestCount := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if requestCount == 1 {
			w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=3600")
			w.Write([]byte("fresh"))
		} else {
			w.WriteHeader(500)
			w.Write([]byte("error"))
		}
	}))
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// Populate cache.
	w1 := httptest.NewRecorder()
	cdn.ServeHTTP(w1, httptest.NewRequest("GET", "http://example.com/stale-test", nil))
	if w1.Body.String() != "fresh" {
		t.Fatalf("expected fresh content, got %q", w1.Body.String())
	}

	// Wait for TTL to expire (1 second).
	time.Sleep(1100 * time.Millisecond)

	// Should serve stale.
	w2 := httptest.NewRecorder()
	cdn.ServeHTTP(w2, httptest.NewRequest("GET", "http://example.com/stale-test", nil))
	if w2.Body.String() != "fresh" {
		t.Errorf("expected stale content %q, got %q", "fresh", w2.Body.String())
	}
	if w2.Header().Get("X-Cache") != "STALE" {
		t.Errorf("X-Cache = %q, want STALE", w2.Header().Get("X-Cache"))
	}
}

func TestSMaxagePreferred(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "max-age=10, s-maxage=300",
	}, "shared cache")
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// Populate.
	cdn.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://example.com/smaxage", nil))

	// Immediately hit — should use s-maxage=300, not max-age=10.
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com/smaxage", nil))

	if w.Header().Get("X-Cache") != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", w.Header().Get("X-Cache"))
	}

	body, _ := io.ReadAll(w.Body)
	if string(body) != "shared cache" {
		t.Errorf("body = %q", body)
	}
}

func TestByteRangePassthrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			t.Error("expected Range header to be forwarded to origin")
		}
		w.Header().Set("Content-Range", "bytes 0-4/11")
		w.WriteHeader(206)
		w.Write([]byte("hello"))
	}))
	defer origin.Close()

	cdn, c := newTestCDN(t, origin.URL)

	req := httptest.NewRequest("GET", "http://example.com/range", nil)
	req.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, req)

	if w.Code != 206 {
		t.Errorf("status = %d, want 206", w.Code)
	}
	if w.Header().Get("X-Cache") != "BYPASS" {
		t.Errorf("X-Cache = %q, want BYPASS for range requests", w.Header().Get("X-Cache"))
	}
	if c.Len() != 0 {
		t.Error("range request should not populate cache")
	}
}

func TestCacheKeyLengthLimit(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "max-age=300",
	}, "content")
	defer origin.Close()

	// Create cache with 100 byte key limit.
	c, _ := cache.NewWithOptions(cache.Options{MaxItems: 1000, MaxKeyLen: 100})
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	cfg := &config.Config{OriginURL: origin.URL, DefaultTTL: 5 * time.Minute}
	cdn := New(c, o, cfg, logging.New())

	// Very long URL that exceeds key limit.
	longPath := "/" + strings.Repeat("a", 200)
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, httptest.NewRequest("GET", "http://example.com"+longPath, nil))

	// Should still respond (passthrough) but not cache.
	// The entry won't be cached because the key is too long.
	if c.Len() != 0 {
		t.Errorf("cache len = %d, want 0 for oversized key", c.Len())
	}
}
