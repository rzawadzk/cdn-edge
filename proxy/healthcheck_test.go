package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthCheckMarksDownAfterThreshold(t *testing.T) {
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := &Backend{URL: srv.URL}
	bal := NewBalancer([]*Backend{b}, RoundRobin)
	hc := NewHealthChecker(bal, 50*time.Millisecond, 2*time.Second, "/healthz", 3)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	hc.Start(ctx)

	// After several checks with 500s, backend should be marked down.
	if b.IsHealthy() {
		t.Error("backend should be marked down after threshold failures")
	}
}

func TestHealthCheckMarksUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := &Backend{URL: srv.URL}
	bal := NewBalancer([]*Backend{b}, RoundRobin)

	// Start with backend down.
	bal.MarkDown(b)
	if b.IsHealthy() {
		t.Fatal("precondition: backend should start down")
	}

	hc := NewHealthChecker(bal, 50*time.Millisecond, 2*time.Second, "/healthz", 3)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hc.Start(ctx)

	// After health checks succeed, backend should be back up.
	if !b.IsHealthy() {
		t.Error("backend should be marked up after successful checks")
	}
}

func TestHealthCheckThresholdNotReachedKeepsHealthy(t *testing.T) {
	// Fail only the first request, then succeed.
	var reqCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := &Backend{URL: srv.URL}
	bal := NewBalancer([]*Backend{b}, RoundRobin)
	hc := NewHealthChecker(bal, 50*time.Millisecond, 2*time.Second, "/healthz", 3)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hc.Start(ctx)

	// Should still be healthy since threshold (3) was not reached consecutively.
	if !b.IsHealthy() {
		t.Error("backend should remain healthy when failures are below threshold")
	}
}

func TestHealthCheckMultipleBackends(t *testing.T) {
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srvOK.Close()

	srvBad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srvBad.Close()

	bOK := &Backend{URL: srvOK.URL}
	bBad := &Backend{URL: srvBad.URL}
	bal := NewBalancer([]*Backend{bOK, bBad}, RoundRobin)
	hc := NewHealthChecker(bal, 50*time.Millisecond, 2*time.Second, "/healthz", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	hc.Start(ctx)

	if !bOK.IsHealthy() {
		t.Error("healthy backend should remain up")
	}
	if bBad.IsHealthy() {
		t.Error("unhealthy backend should be marked down")
	}

	// Balancer should only return the healthy one.
	be, err := bal.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if be.URL != srvOK.URL {
		t.Errorf("got %s, want %s", be.URL, srvOK.URL)
	}
}

func TestCheckOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b := &Backend{URL: srv.URL}
	bal := NewBalancer([]*Backend{b}, RoundRobin)
	hc := NewHealthChecker(bal, time.Second, 2*time.Second, "/healthz", 3)

	results := hc.CheckOnce(context.Background())
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].Healthy {
		t.Errorf("expected healthy result, got error: %v", results[0].Error)
	}
	if results[0].Latency <= 0 {
		t.Error("expected positive latency")
	}
}
