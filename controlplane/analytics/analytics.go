// Package analytics aggregates access logs from edge servers.
// Stores aggregated metrics in memory with periodic disk flush.
// For production, swap for ClickHouse or BigQuery.
package analytics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogEntry is one access log line received from an edge.
type LogEntry struct {
	Timestamp   string  `json:"ts"`
	EdgeID      string  `json:"edge_id"`
	RequestID   string  `json:"request_id"`
	Method      string  `json:"method"`
	Host        string  `json:"host"`
	Path        string  `json:"path"`
	Status      int     `json:"status"`
	BytesSent   int     `json:"bytes_sent"`
	DurationMs  float64 `json:"duration_ms"`
	CacheStatus string  `json:"cache_status"`
	ClientIP    string  `json:"client_ip"`
}

// DomainStats holds aggregated stats for a domain.
type DomainStats struct {
	Hostname     string  `json:"hostname"`
	Requests     int64   `json:"requests"`
	BytesServed  int64   `json:"bytes_served"`
	CacheHits    int64   `json:"cache_hits"`
	CacheMisses  int64   `json:"cache_misses"`
	Status2xx    int64   `json:"status_2xx"`
	Status3xx    int64   `json:"status_3xx"`
	Status4xx    int64   `json:"status_4xx"`
	Status5xx    int64   `json:"status_5xx"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
	totalLatency float64
}

// Collector receives log entries from edges and aggregates them.
type Collector struct {
	mu    sync.Mutex
	stats map[string]*DomainStats // keyed by hostname
	dir   string
}

// NewCollector creates an analytics collector.
func NewCollector(dataDir string) (*Collector, error) {
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, err
		}
	}
	c := &Collector{
		stats: make(map[string]*DomainStats),
		dir:   dataDir,
	}
	c.loadStats()
	// Periodic flush.
	go func() {
		for range time.Tick(1 * time.Minute) {
			c.flushStats()
		}
	}()
	return c, nil
}

// Ingest processes a batch of log entries from an edge.
func (c *Collector) Ingest(entries []LogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, e := range entries {
		ds, ok := c.stats[e.Host]
		if !ok {
			ds = &DomainStats{Hostname: e.Host}
			c.stats[e.Host] = ds
		}
		ds.Requests++
		ds.BytesServed += int64(e.BytesSent)
		ds.totalLatency += e.DurationMs

		switch e.CacheStatus {
		case "HIT", "STALE":
			ds.CacheHits++
		case "MISS", "BYPASS":
			ds.CacheMisses++
		}

		switch {
		case e.Status >= 200 && e.Status < 300:
			ds.Status2xx++
		case e.Status >= 300 && e.Status < 400:
			ds.Status3xx++
		case e.Status >= 400 && e.Status < 500:
			ds.Status4xx++
		case e.Status >= 500:
			ds.Status5xx++
		}

		if ds.Requests > 0 {
			ds.AvgLatencyMs = ds.totalLatency / float64(ds.Requests)
		}
	}
}

// Query returns stats for a domain, or all domains if hostname is empty.
func (c *Collector) Query(hostname string) []*DomainStats {
	c.mu.Lock()
	defer c.mu.Unlock()

	var result []*DomainStats
	for _, ds := range c.stats {
		if hostname == "" || ds.Hostname == hostname {
			cp := *ds
			result = append(result, &cp)
		}
	}
	return result
}

// HandleIngest is an HTTP handler for POST /api/v1/analytics/ingest.
func (c *Collector) HandleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var entries []LogEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	c.Ingest(entries)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"accepted":%d}`, len(entries))
}

// HandleQuery is an HTTP handler for GET /api/v1/analytics/query.
func (c *Collector) HandleQuery(w http.ResponseWriter, r *http.Request) {
	hostname := r.URL.Query().Get("hostname")
	stats := c.Query(hostname)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (c *Collector) flushStats() {
	if c.dir == "" {
		return
	}
	c.mu.Lock()
	data, err := json.MarshalIndent(c.stats, "", "  ")
	c.mu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(c.dir, "stats.json"), data, 0o644)
}

func (c *Collector) loadStats() {
	if c.dir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(c.dir, "stats.json"))
	if err != nil {
		return
	}
	json.Unmarshal(data, &c.stats)
}
