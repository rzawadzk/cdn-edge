package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func dummyHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
}

func TestCORSDisabled(t *testing.T) {
	h := CORS("", 3600)(dummyHandler())
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Origin", "https://foo.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS headers should not be set when disabled")
	}
}

func TestCORSWildcard(t *testing.T) {
	h := CORS("*", 3600)(dummyHandler())
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Origin", "https://foo.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORSAllowedOrigin(t *testing.T) {
	h := CORS("https://app.example.com,https://www.example.com", 3600)(dummyHandler())
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("got %q, want https://app.example.com", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	h := CORS("https://app.example.com", 3600)(dummyHandler())
	req := httptest.NewRequest("GET", "http://example.com/", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin should not get CORS header, got %q", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	h := CORS("*", 3600)(dummyHandler())
	req := httptest.NewRequest("OPTIONS", "http://example.com/", nil)
	req.Header.Set("Origin", "https://foo.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 204 {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("missing Access-Control-Allow-Methods")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom" {
		t.Errorf("Access-Control-Allow-Headers = %q, want X-Custom", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "3600" {
		t.Errorf("Access-Control-Max-Age = %q, want 3600", got)
	}
}
