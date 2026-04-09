package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestHTTPSRedirect(t *testing.T) {
	h := HTTPSRedirect("")
	req := httptest.NewRequest("GET", "http://example.com:8080/path?q=1", nil)
	req.Host = "example.com:8080"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
	loc := w.Header().Get("Location")
	want := "https://example.com/path?q=1"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}

func TestHTTPSRedirectExplicitHost(t *testing.T) {
	h := HTTPSRedirect("cdn.example.com:8443")
	req := httptest.NewRequest("GET", "http://localhost/foo", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	want := "https://cdn.example.com:8443/foo"
	if loc != want {
		t.Errorf("Location = %q, want %q", loc, want)
	}
}
