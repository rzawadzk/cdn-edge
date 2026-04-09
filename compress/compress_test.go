package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

const testBody = "Hello, this is a test body that should be compressed by the middleware."

// handler returns a simple text handler for testing.
func handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(testBody))
	})
}

func TestBrotliResponse(t *testing.T) {
	srv := httptest.NewServer(Middleware(handler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "br")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "br" {
		t.Fatalf("expected Content-Encoding br, got %q", got)
	}

	reader := brotli.NewReader(resp.Body)
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != testBody {
		t.Fatalf("expected body %q, got %q", testBody, string(body))
	}
}

func TestGzipResponse(t *testing.T) {
	srv := httptest.NewServer(Middleware(handler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("expected Content-Encoding gzip, got %q", got)
	}

	reader, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != testBody {
		t.Fatalf("expected body %q, got %q", testBody, string(body))
	}
}

func TestBrotliPreferredOverGzip(t *testing.T) {
	srv := httptest.NewServer(Middleware(handler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip, br")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "br" {
		t.Fatalf("expected Content-Encoding br when both accepted, got %q", got)
	}

	reader := brotli.NewReader(resp.Body)
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != testBody {
		t.Fatalf("expected body %q, got %q", testBody, string(body))
	}
}

func TestNoCompressionWhenNotAccepted(t *testing.T) {
	srv := httptest.NewServer(Middleware(handler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("expected no Content-Encoding, got %q", ce)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != testBody {
		t.Fatalf("expected body %q, got %q", testBody, string(body))
	}
}

func TestNonCompressibleContentType(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte(strings.Repeat("x", 200)))
	})

	srv := httptest.NewServer(Middleware(h))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "br, gzip")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("expected no Content-Encoding for image/png, got %q", ce)
	}
}
