package analytics

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIngestAndQuery(t *testing.T) {
	c, err := NewCollector("")
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}

	entries := []LogEntry{
		{Host: "acme.com", Status: 200, BytesSent: 1000, DurationMs: 5.0, CacheStatus: "HIT"},
		{Host: "acme.com", Status: 200, BytesSent: 2000, DurationMs: 10.0, CacheStatus: "MISS"},
		{Host: "acme.com", Status: 404, BytesSent: 100, DurationMs: 1.0, CacheStatus: "MISS"},
		{Host: "other.com", Status: 500, BytesSent: 50, DurationMs: 100.0, CacheStatus: "BYPASS"},
	}
	c.Ingest(entries)

	stats := c.Query("acme.com")
	if len(stats) != 1 {
		t.Fatalf("query acme.com: got %d results", len(stats))
	}
	s := stats[0]
	if s.Requests != 3 {
		t.Fatalf("requests = %d, want 3", s.Requests)
	}
	if s.BytesServed != 3100 {
		t.Fatalf("bytes = %d, want 3100", s.BytesServed)
	}
	if s.CacheHits != 1 {
		t.Fatalf("cache hits = %d, want 1", s.CacheHits)
	}
	if s.CacheMisses != 2 {
		t.Fatalf("cache misses = %d, want 2", s.CacheMisses)
	}
	if s.Status2xx != 2 {
		t.Fatalf("2xx = %d, want 2", s.Status2xx)
	}
	if s.Status4xx != 1 {
		t.Fatalf("4xx = %d, want 1", s.Status4xx)
	}

	all := c.Query("")
	if len(all) != 2 {
		t.Fatalf("query all: got %d results, want 2", len(all))
	}
}

func TestAvgLatency(t *testing.T) {
	c, _ := NewCollector("")
	c.Ingest([]LogEntry{
		{Host: "a.com", Status: 200, DurationMs: 10.0},
		{Host: "a.com", Status: 200, DurationMs: 20.0},
	})
	stats := c.Query("a.com")
	if len(stats) != 1 {
		t.Fatal("expected 1 result")
	}
	if stats[0].AvgLatencyMs != 15.0 {
		t.Fatalf("avg latency = %f, want 15.0", stats[0].AvgLatencyMs)
	}
}

func TestStatusCodeBuckets(t *testing.T) {
	c, _ := NewCollector("")
	c.Ingest([]LogEntry{
		{Host: "a.com", Status: 301},
		{Host: "a.com", Status: 302},
		{Host: "a.com", Status: 503},
	})
	stats := c.Query("a.com")
	s := stats[0]
	if s.Status3xx != 2 {
		t.Fatalf("3xx = %d, want 2", s.Status3xx)
	}
	if s.Status5xx != 1 {
		t.Fatalf("5xx = %d, want 1", s.Status5xx)
	}
}

func TestCacheStatusSTALE(t *testing.T) {
	c, _ := NewCollector("")
	c.Ingest([]LogEntry{
		{Host: "a.com", Status: 200, CacheStatus: "STALE"},
	})
	stats := c.Query("a.com")
	if stats[0].CacheHits != 1 {
		t.Fatal("STALE should count as cache hit")
	}
}

func TestHandleIngest(t *testing.T) {
	c, _ := NewCollector("")

	entries := []LogEntry{{Host: "a.com", Status: 200}}
	body, _ := json.Marshal(entries)
	req := httptest.NewRequest("POST", "/api/v1/analytics/ingest", bytes.NewReader(body))
	w := httptest.NewRecorder()
	c.HandleIngest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	if c.Query("a.com")[0].Requests != 1 {
		t.Fatal("entry not ingested")
	}
}

func TestHandleIngestBadMethod(t *testing.T) {
	c, _ := NewCollector("")
	req := httptest.NewRequest("GET", "/api/v1/analytics/ingest", nil)
	w := httptest.NewRecorder()
	c.HandleIngest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestHandleIngestBadJSON(t *testing.T) {
	c, _ := NewCollector("")
	req := httptest.NewRequest("POST", "/api/v1/analytics/ingest", bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	c.HandleIngest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleQuery(t *testing.T) {
	c, _ := NewCollector("")
	c.Ingest([]LogEntry{{Host: "a.com", Status: 200}})

	req := httptest.NewRequest("GET", "/api/v1/analytics/query?hostname=a.com", nil)
	w := httptest.NewRecorder()
	c.HandleQuery(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var stats []*DomainStats
	json.NewDecoder(w.Body).Decode(&stats)
	if len(stats) != 1 {
		t.Fatalf("results = %d", len(stats))
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	c1, _ := NewCollector(dir)
	c1.Ingest([]LogEntry{{Host: "a.com", Status: 200, BytesSent: 100}})
	c1.flushStats()

	c2, _ := NewCollector(dir)
	stats := c2.Query("a.com")
	if len(stats) != 1 || stats[0].Requests != 1 {
		t.Fatal("stats not persisted")
	}
}
