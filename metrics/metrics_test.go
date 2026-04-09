package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/proxy"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	c, err := cache.New(100, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	o := proxy.New(proxy.Options{Timeout: 5 * time.Second, CoalesceTimeout: 5 * time.Second})
	return New(c, o)
}

func TestLivenessEndpoint(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ServeLiveness(w, httptest.NewRequest("GET", "/livez", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "alive" {
		t.Errorf("status = %q, want alive", resp["status"])
	}
}

func TestReadinessNotReady(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ServeReadiness(w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 503 {
		t.Errorf("status = %d, want 503 (not ready by default)", w.Code)
	}
}

func TestReadinessReady(t *testing.T) {
	h := newTestHandler(t)
	h.SetReady(true)
	w := httptest.NewRecorder()
	h.ServeReadiness(w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestPrometheusMetrics(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ServePrometheus(w, httptest.NewRequest("GET", "/metrics", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := w.Body.String()
	expected := []string{
		"cdn_cache_hits_total",
		"cdn_cache_misses_total",
		"cdn_origin_circuit_open",
		"cdn_goroutines",
	}
	for _, m := range expected {
		if !strings.Contains(body, m) {
			t.Errorf("metrics output missing %q", m)
		}
	}
}

func TestJSONMetricsEndpoint(t *testing.T) {
	h := newTestHandler(t)
	w := httptest.NewRecorder()
	h.ServeJSONMetrics(w, httptest.NewRequest("GET", "/metrics.json", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp metricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse metrics: %v", err)
	}
	if resp.Origin.CircuitState != "closed" {
		t.Errorf("circuit state = %q, want closed", resp.Origin.CircuitState)
	}
}

func TestPurgeEndpoint(t *testing.T) {
	h := newTestHandler(t)

	// GET should fail.
	w := httptest.NewRecorder()
	h.ServePurge(w, httptest.NewRequest("GET", "/purge", nil))
	if w.Code != 405 {
		t.Errorf("GET purge status = %d, want 405", w.Code)
	}

	// POST should succeed.
	w2 := httptest.NewRecorder()
	h.ServePurge(w2, httptest.NewRequest("POST", "/purge", nil))
	if w2.Code != 200 {
		t.Errorf("POST purge status = %d, want 200", w2.Code)
	}
}

func TestAdminAuth(t *testing.T) {
	called := false
	inner := func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}

	protected := AdminAuth("secret-key", inner)

	// Without key.
	w := httptest.NewRecorder()
	protected(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 401 {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if called {
		t.Error("handler should not be called without auth")
	}

	// With correct key in header.
	called = false
	w2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-API-Key", "secret-key")
	protected(w2, req)
	if w2.Code != 200 {
		t.Errorf("status = %d, want 200", w2.Code)
	}
	if !called {
		t.Error("handler should be called with correct auth")
	}

	// With correct key in query.
	called = false
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/metrics?api_key=secret-key", nil)
	protected(w3, req3)
	if !called {
		t.Error("handler should be called with query param auth")
	}
}

func TestAdminAuthDisabled(t *testing.T) {
	called := false
	inner := func(w http.ResponseWriter, r *http.Request) {
		called = true
	}

	// Empty key means no auth.
	unprotected := AdminAuth("", inner)
	unprotected(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("handler should be called when auth is disabled")
	}
}
