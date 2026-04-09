package analytics

import (
	"testing"
)

// TestCollectorSatisfiesBackend verifies at compile time that *Collector
// implements the Backend interface.
func TestCollectorSatisfiesBackend(t *testing.T) {
	var _ Backend = (*Collector)(nil)
}

// TestBackendIngestAndQuery exercises the Backend interface methods on a Collector.
func TestBackendIngestAndQuery(t *testing.T) {
	c, err := NewCollector("")
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	var b Backend = c

	if err := b.Ingest([]LogEntry{
		{Host: "b.com", Status: 200, BytesSent: 500, DurationMs: 2.0, CacheStatus: "HIT"},
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	stats, err := b.Query("b.com")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 result, got %d", len(stats))
	}
	if stats[0].Requests != 1 {
		t.Fatalf("requests = %d, want 1", stats[0].Requests)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestBackendQueryAll verifies that Query("") returns all domains.
func TestBackendQueryAll(t *testing.T) {
	c, _ := NewCollector("")
	var b Backend = c

	b.Ingest([]LogEntry{
		{Host: "x.com", Status: 200},
		{Host: "y.com", Status: 200},
	})

	stats, err := b.Query("")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 results, got %d", len(stats))
	}
}
