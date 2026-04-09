package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/tenant"
)

func TestParseRangeValidSingle(t *testing.T) {
	start, end, ok := parseRange("bytes=0-499", 1000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 || end != 499 {
		t.Errorf("got start=%d end=%d, want 0-499", start, end)
	}
}

func TestParseRangeSuffix(t *testing.T) {
	// bytes=-500 means last 500 bytes of a 1000-byte resource.
	start, end, ok := parseRange("bytes=-500", 1000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 500 || end != 999 {
		t.Errorf("got start=%d end=%d, want 500-999", start, end)
	}
}

func TestParseRangeSuffixLargerThanContent(t *testing.T) {
	// bytes=-2000 on a 1000-byte resource: start clamps to 0.
	start, end, ok := parseRange("bytes=-2000", 1000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 || end != 999 {
		t.Errorf("got start=%d end=%d, want 0-999", start, end)
	}
}

func TestParseRangeOpenEnded(t *testing.T) {
	start, end, ok := parseRange("bytes=500-", 1000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 500 || end != 999 {
		t.Errorf("got start=%d end=%d, want 500-999", start, end)
	}
}

func TestParseRangeMultiRangeFalse(t *testing.T) {
	_, _, ok := parseRange("bytes=0-499,600-999", 1000)
	if ok {
		t.Error("expected ok=false for multi-range")
	}
}

func TestParseRangeInvalidInput(t *testing.T) {
	tests := []string{
		"",
		"bytes=",
		"bytes=abc-def",
		"chars=0-499",
		"bytes=500-400", // end < start
		"bytes=2000-3000", // start beyond content
	}
	for _, input := range tests {
		_, _, ok := parseRange(input, 1000)
		if ok {
			t.Errorf("parseRange(%q, 1000) = ok, want false", input)
		}
	}
}

func TestParseRangeClampEnd(t *testing.T) {
	// End beyond content length should clamp.
	start, end, ok := parseRange("bytes=0-5000", 1000)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if start != 0 || end != 999 {
		t.Errorf("got start=%d end=%d, want 0-999", start, end)
	}
}

func TestServeRangeResponse(t *testing.T) {
	entry := &cache.Entry{
		Body:       []byte("Hello, World!"),
		Header:     http.Header{"Content-Type": {"text/plain"}},
		StatusCode: 200,
		StoredAt:   time.Now(),
		TTL:        5 * time.Minute,
	}

	w := httptest.NewRecorder()
	serveRange(w, entry, 0, 4)

	if w.Code != 206 {
		t.Errorf("status = %d, want 206", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-4/13" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 0-4/13")
	}
	if got := w.Header().Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want %q", got, "5")
	}
	if got := w.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", got)
	}
	if got := w.Body.String(); got != "Hello" {
		t.Errorf("body = %q, want %q", got, "Hello")
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}

// Integration test: Range request on already-cached content returns 206.
func TestRangeOnCachedContent(t *testing.T) {
	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		w.Header().Set("Cache-Control", "max-age=300")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		w.Write([]byte("0123456789"))
	}))
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// First request: populate cache with full response.
	w1 := httptest.NewRecorder()
	cdn.ServeHTTP(w1, httptest.NewRequest("GET", "http://example.com/data", nil))
	if w1.Code != 200 {
		t.Fatalf("initial fetch status = %d, want 200", w1.Code)
	}
	if w1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", w1.Header().Get("X-Cache"))
	}

	// Second request: Range on cached content.
	req := httptest.NewRequest("GET", "http://example.com/data", nil)
	req.Header.Set("Range", "bytes=3-7")
	w2 := httptest.NewRecorder()
	cdn.ServeHTTP(w2, req)

	if w2.Code != 206 {
		t.Errorf("status = %d, want 206", w2.Code)
	}
	if got := w2.Body.String(); got != "34567" {
		t.Errorf("body = %q, want %q", got, "34567")
	}
	if got := w2.Header().Get("Content-Range"); got != "bytes 3-7/10" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 3-7/10")
	}
	if got := w2.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("X-Cache = %q, want HIT", got)
	}
	// Origin should only have been hit once (for the initial fetch).
	if originHits != 1 {
		t.Errorf("origin hit %d times, want 1", originHits)
	}
}

// Integration test: Range request on uncached content fetches, caches, returns 206.
func TestRangeOnUncachedContent(t *testing.T) {
	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		// Verify that Range header is stripped when fetching for cache.
		if r.Header.Get("Range") != "" {
			t.Error("origin should not receive Range header on cache-fill fetch")
		}
		w.Header().Set("Cache-Control", "max-age=300")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		w.Write([]byte("abcdefghij"))
	}))
	defer origin.Close()

	cdn, c := newTestCDN(t, origin.URL)

	// Range request with nothing in cache.
	req := httptest.NewRequest("GET", "http://example.com/fresh", nil)
	req.Header.Set("Range", "bytes=0-4")
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, req)

	if w.Code != 206 {
		t.Errorf("status = %d, want 206", w.Code)
	}
	if got := w.Body.String(); got != "abcde" {
		t.Errorf("body = %q, want %q", got, "abcde")
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-4/10" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 0-4/10")
	}

	// The full response should now be cached.
	if c.Len() == 0 {
		t.Error("expected cache to be populated after range fetch")
	}

	// A second range request should be served from cache without hitting origin.
	req2 := httptest.NewRequest("GET", "http://example.com/fresh", nil)
	req2.Header.Set("Range", "bytes=5-9")
	w2 := httptest.NewRecorder()
	cdn.ServeHTTP(w2, req2)

	if w2.Code != 206 {
		t.Errorf("second range status = %d, want 206", w2.Code)
	}
	if got := w2.Body.String(); got != "fghij" {
		t.Errorf("body = %q, want %q", got, "fghij")
	}
	if originHits != 1 {
		t.Errorf("origin hit %d times, want 1", originHits)
	}
}

// Integration test: multi-range on cached content falls through to origin passthrough.
func TestMultiRangeFallsThrough(t *testing.T) {
	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		w.Header().Set("Cache-Control", "max-age=300")
		w.WriteHeader(200)
		w.Write([]byte("full body"))
	}))
	defer origin.Close()

	cdn, _ := newTestCDN(t, origin.URL)

	// First populate the cache with a normal GET.
	w1 := httptest.NewRecorder()
	cdn.ServeHTTP(w1, httptest.NewRequest("GET", "http://example.com/multi", nil))
	if w1.Code != 200 {
		t.Fatalf("initial status = %d, want 200", w1.Code)
	}

	// Multi-range request on cached content should passthrough to origin.
	req := httptest.NewRequest("GET", "http://example.com/multi", nil)
	req.Header.Set("Range", "bytes=0-4,6-8")
	w := httptest.NewRecorder()
	cdn.ServeHTTP(w, req)

	// Multi-range is not parseable, so falls through to proxyPassthrough (BYPASS).
	if got := w.Header().Get("X-Cache"); got != "BYPASS" {
		t.Errorf("X-Cache = %q, want BYPASS for multi-range", got)
	}
	// Origin should be hit twice: once for cache fill, once for multi-range passthrough.
	if originHits != 2 {
		t.Errorf("origin hit %d times, want 2", originHits)
	}
}

// Integration test: multi-tenant Range on cached content.
func TestMultiTenantRangeOnCachedContent(t *testing.T) {
	originHits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		w.Header().Set("Cache-Control", "max-age=300")
		w.WriteHeader(200)
		w.Write([]byte("0123456789"))
	}))
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(
		map[string]*tenant.Domain{
			"cdn.example.com": {
				ID:                   "d1",
				Hostname:             "cdn.example.com",
				OriginURL:            origin.URL,
				Active:               true,
				RespectOriginHeaders: true,
			},
		},
		nil,
	))

	// Populate cache.
	req1 := httptest.NewRequest("GET", "http://cdn.example.com/data", nil)
	req1.Host = "cdn.example.com"
	w1 := httptest.NewRecorder()
	mt.ServeHTTP(w1, req1)
	if w1.Code != 200 {
		t.Fatalf("initial status = %d, want 200", w1.Code)
	}

	// Range on cached content.
	req2 := httptest.NewRequest("GET", "http://cdn.example.com/data", nil)
	req2.Host = "cdn.example.com"
	req2.Header.Set("Range", "bytes=2-5")
	w2 := httptest.NewRecorder()
	mt.ServeHTTP(w2, req2)

	if w2.Code != 206 {
		t.Errorf("status = %d, want 206", w2.Code)
	}
	if got := w2.Body.String(); got != "2345" {
		t.Errorf("body = %q, want %q", got, "2345")
	}
	if got := w2.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Errorf("Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
	if originHits != 1 {
		t.Errorf("origin hit %d times, want 1", originHits)
	}
}
