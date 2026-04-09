package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
	"github.com/rzawadzk/cdn-edge/tenant"
)

func newTestMultiTenantCDN(t *testing.T) (*MultiTenantCDN, *cache.Cache) {
	t.Helper()
	c, err := cache.New(1000, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	log := logging.New()
	return NewMultiTenant(c, o, log, 5*time.Minute), c
}

func makeConfig(domains map[string]*tenant.Domain, rules map[string][]*tenant.CacheRule) *tenant.EdgeConfig {
	return &tenant.EdgeConfig{
		Version:   1,
		Timestamp: time.Now(),
		Domains:   domains,
		Rules:     rules,
	}
}

func TestMultiTenantUnknownHost(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(nil, nil))

	req := httptest.NewRequest("GET", "http://unknown.com/test", nil)
	req.Host = "unknown.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestMultiTenantNoConfig(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	// Don't set config.

	req := httptest.NewRequest("GET", "http://example.com/test", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestMultiTenantInactiveDomain(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"inactive.com": {
			ID: "d1", Hostname: "inactive.com", OriginURL: "https://origin.com",
			Active: false,
		},
	}, nil))

	req := httptest.NewRequest("GET", "http://inactive.com/test", nil)
	req.Host = "inactive.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", w.Code)
	}
}

func TestMultiTenantMethodNotAllowed(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	req := httptest.NewRequest("POST", "http://example.com/test", nil)
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestMultiTenantPathTraversal(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	req := httptest.NewRequest("GET", "http://example.com/../etc/passwd", nil)
	req.Host = "example.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestMultiTenantMissingHost(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(nil, nil))

	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = ""
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestMultiTenantCacheMissAndHit(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Content-Type":  "text/plain",
		"Cache-Control": "max-age=300",
	}, "multi-tenant hello")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {
			ID: "d1", Hostname: "acme.com", OriginURL: origin.URL,
			Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true,
		},
	}, nil))

	// First request — MISS.
	req := httptest.NewRequest("GET", "http://acme.com/hello", nil)
	req.Host = "acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", w.Header().Get("X-Cache"))
	}
	if w.Body.String() != "multi-tenant hello" {
		t.Fatalf("body = %q", w.Body.String())
	}

	// Second request — HIT.
	req2 := httptest.NewRequest("GET", "http://acme.com/hello", nil)
	req2.Host = "acme.com"
	w2 := httptest.NewRecorder()
	mt.ServeHTTP(w2, req2)

	if w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", w2.Header().Get("X-Cache"))
	}
}

func TestMultiTenantHostIsolation(t *testing.T) {
	origin1 := setupTestOrigin(t, 200, map[string]string{"Cache-Control": "max-age=300"}, "site-a")
	defer origin1.Close()
	origin2 := setupTestOrigin(t, 200, map[string]string{"Cache-Control": "max-age=300"}, "site-b")
	defer origin2.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"a.com": {ID: "d1", Hostname: "a.com", OriginURL: origin1.URL, Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true},
		"b.com": {ID: "d2", Hostname: "b.com", OriginURL: origin2.URL, Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true},
	}, nil))

	// Request to a.com.
	req := httptest.NewRequest("GET", "http://a.com/test", nil)
	req.Host = "a.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)
	if w.Body.String() != "site-a" {
		t.Fatalf("a.com body = %q", w.Body.String())
	}

	// Request to b.com.
	req = httptest.NewRequest("GET", "http://b.com/test", nil)
	req.Host = "b.com"
	w = httptest.NewRecorder()
	mt.ServeHTTP(w, req)
	if w.Body.String() != "site-b" {
		t.Fatalf("b.com body = %q", w.Body.String())
	}
}

func TestMultiTenantBypassRule(t *testing.T) {
	origin := setupTestOrigin(t, 200, nil, "api response")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	domID := "d1"
	mt.UpdateConfig(makeConfig(
		map[string]*tenant.Domain{
			"acme.com": {ID: domID, Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 300},
		},
		map[string][]*tenant.CacheRule{
			domID: {{ID: "r1", DomainID: domID, PathGlob: "/api/*", Bypass: true}},
		},
	))

	req := httptest.NewRequest("GET", "http://acme.com/api/users", nil)
	req.Host = "acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("X-Cache = %q, want BYPASS", w.Header().Get("X-Cache"))
	}
}

func TestMultiTenantNoStoreBypass(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "no-store",
	}, "private data")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {ID: "d1", Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true},
	}, nil))

	req := httptest.NewRequest("GET", "http://acme.com/secret", nil)
	req.Host = "acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("X-Cache = %q, want BYPASS", w.Header().Get("X-Cache"))
	}
}

func TestMultiTenantDomainDefaultTTL(t *testing.T) {
	origin := setupTestOrigin(t, 200, nil, "default ttl")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {ID: "d1", Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 600},
	}, nil))

	req := httptest.NewRequest("GET", "http://acme.com/page", nil)
	req.Host = "acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	// Should cache (MISS, not BYPASS) since domain has a default TTL.
	if w.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache = %q, want MISS", w.Header().Get("X-Cache"))
	}

	// Second request should hit cache.
	req2 := httptest.NewRequest("GET", "http://acme.com/page", nil)
	req2.Host = "acme.com"
	w2 := httptest.NewRecorder()
	mt.ServeHTTP(w2, req2)
	if w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", w2.Header().Get("X-Cache"))
	}
}

func TestMultiTenantConditionalRequest(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{
		"Cache-Control": "max-age=300",
	}, "etag content")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {ID: "d1", Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true},
	}, nil))

	// First request to populate cache.
	req := httptest.NewRequest("GET", "http://acme.com/etag", nil)
	req.Host = "acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header")
	}

	// Second request with If-None-Match.
	req2 := httptest.NewRequest("GET", "http://acme.com/etag", nil)
	req2.Host = "acme.com"
	req2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	mt.ServeHTTP(w2, req2)

	if w2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w2.Code)
	}
}

func TestMultiTenantHostWithPort(t *testing.T) {
	origin := setupTestOrigin(t, 200, map[string]string{"Cache-Control": "max-age=300"}, "with port")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {ID: "d1", Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 300, RespectOriginHeaders: true},
	}, nil))

	req := httptest.NewRequest("GET", "http://acme.com:8080/test", nil)
	req.Host = "acme.com:8080"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestMultiTenantRangeBypass(t *testing.T) {
	origin := setupTestOrigin(t, 200, nil, "full body")
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"acme.com": {ID: "d1", Hostname: "acme.com", OriginURL: origin.URL, Active: true, DefaultTTLSec: 300},
	}, nil))

	req := httptest.NewRequest("GET", "http://acme.com/file", nil)
	req.Host = "acme.com"
	req.Header.Set("Range", "bytes=0-10")
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if w.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("Range request should bypass cache, got X-Cache=%q", w.Header().Get("X-Cache"))
	}
}

func TestMultiTenantOriginHostOverride(t *testing.T) {
	var receivedHost string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer origin.Close()

	mt, _ := newTestMultiTenantCDN(t)
	mt.UpdateConfig(makeConfig(map[string]*tenant.Domain{
		"cdn.acme.com": {
			ID: "d1", Hostname: "cdn.acme.com", OriginURL: origin.URL,
			OriginHost: "real-origin.acme.com", Active: true, DefaultTTLSec: 300,
		},
	}, nil))

	req := httptest.NewRequest("GET", "http://cdn.acme.com/test", nil)
	req.Host = "cdn.acme.com"
	w := httptest.NewRecorder()
	mt.ServeHTTP(w, req)

	if receivedHost != "real-origin.acme.com" {
		t.Fatalf("origin received Host=%q, want real-origin.acme.com", receivedHost)
	}
}

// --- HandleConfigPush ---

func TestHandleConfigPush(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)

	cfg := &tenant.EdgeConfig{
		Version: 42,
		Domains: map[string]*tenant.Domain{
			"pushed.com": {ID: "d1", Hostname: "pushed.com", OriginURL: "https://pushed.com", Active: true},
		},
	}
	body, _ := json.Marshal(cfg)

	req := httptest.NewRequest("POST", "/edge/config", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mt.HandleConfigPush(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}

	got := mt.GetConfig()
	if got == nil || got.Version != 42 {
		t.Fatalf("config not updated, got %+v", got)
	}
	if _, ok := got.Domains["pushed.com"]; !ok {
		t.Fatal("pushed domain not in config")
	}
}

func TestHandleConfigPushBadMethod(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	req := httptest.NewRequest("GET", "/edge/config", nil)
	w := httptest.NewRecorder()
	mt.HandleConfigPush(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestHandleConfigPushBadJSON(t *testing.T) {
	mt, _ := newTestMultiTenantCDN(t)
	req := httptest.NewRequest("POST", "/edge/config", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	mt.HandleConfigPush(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

// --- matchGlob ---

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		path, pattern string
		want          bool
	}{
		{"/static/app.js", "/static/*", true},
		{"/static/css/style.css", "/static/*", true},
		{"/api/users", "/api/*", true},
		{"/api", "/api/*", true},
		{"/other/path", "/api/*", false},
		{"/anything", "*", true},
		{"/anything", "/*", true},
		{"/exact", "/exact", true},
		{"/exact/more", "/exact", false},
		{"/", "", false},
		{"/images/logo.png", "/images/", false},
		{"/prefix-match", "/prefix*", true},
		{"/prefix-match/deep", "/prefix*", true},
	}

	for _, tt := range tests {
		got := matchGlob(tt.path, tt.pattern)
		if got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
		}
	}
}

// --- mtIsValidPath ---

func TestMtIsValidPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/foo/bar", true},
		{"../etc/passwd", false},
		{"/foo/../bar", false},
		{"", false},
		{"/normal/path.html", true},
	}

	for _, tt := range tests {
		got := mtIsValidPath(tt.path)
		if got != tt.want {
			t.Errorf("mtIsValidPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// --- stripPort ---

func TestStripPort(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"example.com:8080", "example.com"},
		{"example.com", "example.com"},
		{"[::1]:8080", "[::1]"},
	}
	for _, tt := range tests {
		got := stripPort(tt.input)
		if got != tt.want {
			t.Errorf("stripPort(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
