package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// HealthChecker periodically probes backend health endpoints and updates the
// balancer accordingly.
type HealthChecker struct {
	balancer  *Balancer
	client    *http.Client
	interval  time.Duration
	timeout   time.Duration
	path      string
	threshold int

	mu             sync.Mutex
	failureCounts  map[*Backend]int
	successCounts  map[*Backend]int
	recovThreshold int // consecutive successes to mark up (defaults to 1)
}

// NewHealthChecker creates a health checker.
//   - interval: how often to check (e.g., 10s)
//   - timeout: per-check HTTP timeout (e.g., 5s)
//   - path: health check URL path appended to each backend URL (e.g., "/healthz")
//   - threshold: consecutive failures before marking a backend down
func NewHealthChecker(balancer *Balancer, interval, timeout time.Duration, path string, threshold int) *HealthChecker {
	if threshold <= 0 {
		threshold = 3
	}
	return &HealthChecker{
		balancer:       balancer,
		client:         &http.Client{Timeout: timeout},
		interval:       interval,
		timeout:        timeout,
		path:           path,
		threshold:      threshold,
		failureCounts:  make(map[*Backend]int),
		successCounts:  make(map[*Backend]int),
		recovThreshold: 1,
	}
}

// Start begins periodic health checking. It blocks until ctx is cancelled.
func (hc *HealthChecker) Start(ctx context.Context) {
	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	// Run an initial check immediately.
	hc.checkAll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hc.checkAll(ctx)
		}
	}
}

func (hc *HealthChecker) checkAll(ctx context.Context) {
	backends := hc.balancer.Backends()
	var wg sync.WaitGroup
	for _, b := range backends {
		wg.Add(1)
		go func(be *Backend) {
			defer wg.Done()
			hc.check(ctx, be)
		}(b)
	}
	wg.Wait()
}

func (hc *HealthChecker) check(ctx context.Context, be *Backend) {
	url := be.URL + hc.path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		hc.recordFailure(be)
		return
	}
	req.Header.Set("User-Agent", "CDN-Edge-HealthCheck/1.0")

	resp, err := hc.client.Do(req)
	if err != nil {
		hc.recordFailure(be)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		hc.recordSuccess(be)
	} else {
		hc.recordFailure(be)
	}
}

func (hc *HealthChecker) recordFailure(be *Backend) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	hc.successCounts[be] = 0
	hc.failureCounts[be]++

	if hc.failureCounts[be] >= hc.threshold {
		hc.balancer.MarkDown(be)
	}
}

func (hc *HealthChecker) recordSuccess(be *Backend) {
	hc.mu.Lock()
	defer hc.mu.Unlock()

	hc.failureCounts[be] = 0
	hc.successCounts[be]++

	if hc.successCounts[be] >= hc.recovThreshold {
		hc.balancer.MarkUp(be)
	}
}

// CheckResult holds the outcome of a single health check (useful for diagnostics).
type CheckResult struct {
	Backend *Backend
	Healthy bool
	Latency time.Duration
	Error   error
}

// CheckOnce performs a single health check round and returns results.
func (hc *HealthChecker) CheckOnce(ctx context.Context) []CheckResult {
	backends := hc.balancer.Backends()
	results := make([]CheckResult, len(backends))
	var wg sync.WaitGroup

	for i, b := range backends {
		wg.Add(1)
		go func(idx int, be *Backend) {
			defer wg.Done()
			start := time.Now()
			url := be.URL + hc.path
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				results[idx] = CheckResult{Backend: be, Healthy: false, Error: err}
				return
			}
			resp, err := hc.client.Do(req)
			latency := time.Since(start)
			if err != nil {
				results[idx] = CheckResult{Backend: be, Healthy: false, Latency: latency, Error: err}
				return
			}
			resp.Body.Close()
			healthy := resp.StatusCode >= 200 && resp.StatusCode < 400
			if !healthy {
				err = fmt.Errorf("unhealthy status: %d", resp.StatusCode)
			}
			results[idx] = CheckResult{Backend: be, Healthy: healthy, Latency: latency, Error: err}
		}(i, b)
	}
	wg.Wait()
	return results
}
