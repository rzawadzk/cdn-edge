package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// RequestMetrics tracks per-request latency and status code distributions.
// It implements a lock-free histogram for latency and atomic counters for status codes.
type RequestMetrics struct {
	// Latency histogram buckets (cumulative, in milliseconds).
	bucketBounds []float64
	bucketCounts []atomic.Int64
	totalCount   atomic.Int64
	totalSumUs   atomic.Int64 // sum of latencies in microseconds

	// Status code family counters.
	status2xx atomic.Int64
	status3xx atomic.Int64
	status4xx atomic.Int64
	status5xx atomic.Int64
}

// NewRequestMetrics creates a latency histogram with CDN-relevant bucket boundaries.
func NewRequestMetrics() *RequestMetrics {
	bounds := []float64{0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}
	return &RequestMetrics{
		bucketBounds: bounds,
		bucketCounts: make([]atomic.Int64, len(bounds)+1), // +1 for +Inf
	}
}

// Record records one request's latency and status code.
func (rm *RequestMetrics) Record(duration time.Duration, statusCode int) {
	ms := float64(duration.Microseconds()) / 1000.0
	rm.totalCount.Add(1)
	rm.totalSumUs.Add(int64(duration.Microseconds()))

	// Find the first bucket where bound >= ms (i.e. this request fits in that bucket).
	// Each bucket stores its own (non-cumulative) count; we compute cumulative at read time.
	idx := sort.SearchFloat64s(rm.bucketBounds, ms)
	// idx is the index of the first bound >= ms. If ms exactly equals a bound,
	// it belongs in that bucket (<=bound). SearchFloat64s returns the position
	// where ms would be inserted, so if bounds[idx] == ms, idx is correct.
	if idx > len(rm.bucketBounds) {
		idx = len(rm.bucketBounds)
	}
	rm.bucketCounts[idx].Add(1)

	// Status code family.
	switch {
	case statusCode >= 200 && statusCode < 300:
		rm.status2xx.Add(1)
	case statusCode >= 300 && statusCode < 400:
		rm.status3xx.Add(1)
	case statusCode >= 400 && statusCode < 500:
		rm.status4xx.Add(1)
	case statusCode >= 500:
		rm.status5xx.Add(1)
	}
}

// WritePrometheus writes the histogram and status counters in Prometheus exposition format.
func (rm *RequestMetrics) WritePrometheus(w http.ResponseWriter) {
	fmt.Fprintln(w, "# HELP cdn_request_duration_ms Request latency histogram in milliseconds.")
	fmt.Fprintln(w, "# TYPE cdn_request_duration_ms histogram")

	// Buckets are stored non-cumulatively; compute cumulative at read time.
	var cumulative int64
	for i, bound := range rm.bucketBounds {
		cumulative += rm.bucketCounts[i].Load()
		fmt.Fprintf(w, "cdn_request_duration_ms_bucket{le=\"%s\"} %d\n", formatFloat(bound), cumulative)
	}
	cumulative += rm.bucketCounts[len(rm.bucketBounds)].Load()
	fmt.Fprintf(w, "cdn_request_duration_ms_bucket{le=\"+Inf\"} %d\n", cumulative)

	totalCount := rm.totalCount.Load()
	totalSumMs := float64(rm.totalSumUs.Load()) / 1000.0
	fmt.Fprintf(w, "cdn_request_duration_ms_sum %s\n", formatFloat(totalSumMs))
	fmt.Fprintf(w, "cdn_request_duration_ms_count %d\n", totalCount)

	fmt.Fprintln(w, "# HELP cdn_responses_total Total responses by status code class.")
	fmt.Fprintln(w, "# TYPE cdn_responses_total counter")
	fmt.Fprintf(w, "cdn_responses_total{code=\"2xx\"} %d\n", rm.status2xx.Load())
	fmt.Fprintf(w, "cdn_responses_total{code=\"3xx\"} %d\n", rm.status3xx.Load())
	fmt.Fprintf(w, "cdn_responses_total{code=\"4xx\"} %d\n", rm.status4xx.Load())
	fmt.Fprintf(w, "cdn_responses_total{code=\"5xx\"} %d\n", rm.status5xx.Load())
}

// Percentile computes an approximate percentile (0-100) from the histogram.
// Returns the upper bound of the bucket that contains the target rank.
func (rm *RequestMetrics) Percentile(p float64) float64 {
	total := rm.totalCount.Load()
	if total == 0 {
		return 0
	}
	target := int64(math.Ceil(float64(total) * p / 100.0))

	var cumulative int64
	for i, bound := range rm.bucketBounds {
		cumulative += rm.bucketCounts[i].Load()
		if cumulative >= target {
			return bound
		}
	}
	// Overflow bucket (+Inf) — return the highest finite bound.
	if len(rm.bucketBounds) > 0 {
		return rm.bucketBounds[len(rm.bucketBounds)-1]
	}
	return 0
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprintf("%g", f)
}

// RequestMetricsMiddleware wraps an http.Handler and records request metrics.
type RequestMetricsMiddleware struct {
	next    http.Handler
	metrics *RequestMetrics
}

type metricsRW struct {
	http.ResponseWriter
	status int
	mu     sync.Once
}

func (rw *metricsRW) WriteHeader(code int) {
	rw.mu.Do(func() {
		rw.status = code
	})
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *metricsRW) Write(b []byte) (int, error) {
	rw.mu.Do(func() {
		rw.status = 200
	})
	return rw.ResponseWriter.Write(b)
}

func (rw *metricsRW) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// NewRequestMetricsMiddleware wraps a handler with latency+status recording.
func NewRequestMetricsMiddleware(rm *RequestMetrics, next http.Handler) http.Handler {
	return &RequestMetricsMiddleware{next: next, metrics: rm}
}

func (m *RequestMetricsMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &metricsRW{ResponseWriter: w, status: 200}
	start := time.Now()
	m.next.ServeHTTP(rw, r)
	m.metrics.Record(time.Since(start), rw.status)
}
