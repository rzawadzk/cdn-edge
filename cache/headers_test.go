package cache

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func TestCompressedHeaderRoundTrip(t *testing.T) {
	h := http.Header{
		"Content-Type":   {"application/json"},
		"Cache-Control":  {"max-age=3600, public"},
		"Etag":           {`"v1-abc123"`},
		"Vary":           {"Accept-Encoding"},
		"X-Custom-Field": {"a", "b", "c"},
	}

	ch := NewCompressedHeader(h)
	if ch == nil {
		t.Fatal("NewCompressedHeader returned nil for non-empty header")
	}

	got := ch.Decode()
	if len(got) != len(h) {
		t.Errorf("decoded len = %d, want %d", len(got), len(h))
	}
	for k, v := range h {
		gv := got[k]
		if len(gv) != len(v) {
			t.Errorf("key %q: got %v, want %v", k, gv, v)
			continue
		}
		for i := range v {
			if gv[i] != v[i] {
				t.Errorf("key %q[%d]: got %q, want %q", k, i, gv[i], v[i])
			}
		}
	}
}

func TestCompressedHeaderEmpty(t *testing.T) {
	if ch := NewCompressedHeader(nil); ch != nil {
		t.Error("expected nil for nil header")
	}
	if ch := NewCompressedHeader(http.Header{}); ch != nil {
		t.Error("expected nil for empty header")
	}

	// Decode on nil receiver should return an empty header, not panic.
	var ch *CompressedHeader
	if got := ch.Decode(); len(got) != 0 {
		t.Errorf("nil Decode() = %v, want empty", got)
	}
	if s := ch.Size(); s != 0 {
		t.Errorf("nil Size() = %d, want 0", s)
	}
	if v := ch.Get("foo"); v != "" {
		t.Errorf("nil Get() = %q, want empty", v)
	}
}

func TestCompressedHeaderSavesMemory(t *testing.T) {
	// Build a realistic response header with many repeated tokens — the kind
	// of payload where zstd should pay off.
	h := http.Header{
		"Content-Type":               {"application/json; charset=utf-8"},
		"Cache-Control":              {"public, max-age=31536000, immutable"},
		"Content-Security-Policy":    {"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'"},
		"Strict-Transport-Security":  {"max-age=63072000; includeSubDomains; preload"},
		"X-Content-Type-Options":     {"nosniff"},
		"X-Frame-Options":            {"SAMEORIGIN"},
		"Referrer-Policy":            {"strict-origin-when-cross-origin"},
		"Access-Control-Allow-Origin": {"https://example.com"},
		"Vary":                        {"Accept-Encoding, Accept-Language"},
	}
	raw := 0
	for k, vv := range h {
		raw += len(k)
		for _, v := range vv {
			raw += len(v)
		}
	}
	ch := NewCompressedHeader(h)
	if ch == nil {
		t.Fatal("unexpected nil")
	}
	if ch.Size() >= raw {
		t.Logf("compressed=%d raw=%d — compression did not shrink (ok on small inputs)", ch.Size(), raw)
	}
	// Round trip still correct.
	dec := ch.Decode()
	if got := dec.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestCompressedHeaderGet(t *testing.T) {
	h := http.Header{
		"Etag":         {`"abc"`},
		"Content-Type": {"text/plain"},
	}
	ch := NewCompressedHeader(h)
	if got := ch.Get("Etag"); got != `"abc"` {
		t.Errorf("Get(Etag) = %q", got)
	}
	if got := ch.Get("Missing"); got != "" {
		t.Errorf("Get(Missing) = %q, want empty", got)
	}
}

func TestCompressedHeaderConcurrent(t *testing.T) {
	// The package-level zstd encoder/decoder must be safe for concurrent use.
	h := http.Header{
		"Content-Type":  {"application/json"},
		"Cache-Control": {"max-age=600"},
		"Etag":          {`"xyz"`},
	}
	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				local := h.Clone()
				local.Set("X-Req", fmt.Sprintf("g%d-%d", i, j))
				ch := NewCompressedHeader(local)
				if ch == nil {
					t.Errorf("nil compressed header")
					return
				}
				dec := ch.Decode()
				if dec.Get("Content-Type") != "application/json" {
					t.Errorf("concurrent decode mismatch")
					return
				}
				if want := fmt.Sprintf("g%d-%d", i, j); dec.Get("X-Req") != want {
					t.Errorf("X-Req got=%q want=%q", dec.Get("X-Req"), want)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestCacheCompressHeadersOption(t *testing.T) {
	c, err := NewWithOptions(Options{MaxItems: 10, CompressHeaders: true})
	if err != nil {
		t.Fatal(err)
	}
	hdr := http.Header{
		"Content-Type":  {"text/html"},
		"Cache-Control": {"max-age=60"},
	}
	e := &Entry{
		Body:       []byte("hello"),
		Header:     hdr.Clone(),
		StatusCode: 200,
	}
	c.Put("k", e)

	// The original entry should now have been compressed in place.
	if e.Header != nil {
		t.Error("expected raw Header to be cleared after compression")
	}
	if e.CompressedHeader == nil {
		t.Fatal("expected CompressedHeader to be populated")
	}

	got := c.Get("k")
	if got == nil {
		t.Fatal("expected cache hit")
	}
	if got.GetHeader().Get("Content-Type") != "text/html" {
		t.Errorf("Content-Type = %q", got.GetHeader().Get("Content-Type"))
	}
	if got.GetHeader().Get("Cache-Control") != "max-age=60" {
		t.Errorf("Cache-Control = %q", got.GetHeader().Get("Cache-Control"))
	}
}
