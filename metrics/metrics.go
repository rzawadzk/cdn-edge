package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync/atomic"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/proxy"
)

// Handler serves /metrics, /health, /ready, /purge endpoints.
type Handler struct {
	cache   *cache.Cache
	origin  *proxy.Origin
	ready   atomic.Bool
	ReqMet  *RequestMetrics
}

// New creates a metrics handler. The handler starts as not-ready.
func New(c *cache.Cache, o *proxy.Origin) *Handler {
	return &Handler{cache: c, origin: o, ReqMet: NewRequestMetrics()}
}

// SetReady marks the handler as ready (or not) to serve traffic.
func (h *Handler) SetReady(ready bool) {
	h.ready.Store(ready)
}

type metricsResponse struct {
	Cache   cacheMetrics   `json:"cache"`
	Runtime runtimeMetrics `json:"runtime"`
	Origin  originMetrics  `json:"origin"`
}

type cacheMetrics struct {
	Hits       int64   `json:"hits"`
	Misses     int64   `json:"misses"`
	Evictions  int64   `json:"evictions"`
	StaleHits  int64   `json:"stale_hits"`
	ItemsInMem int     `json:"items_in_memory"`
	HitRate    float64 `json:"hit_rate_pct"`
}

type runtimeMetrics struct {
	Goroutines  int     `json:"goroutines"`
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	NumGC       uint32  `json:"num_gc"`
}

type originMetrics struct {
	CircuitState string `json:"circuit_state"`
}

// ServeJSONMetrics writes cache and runtime metrics as JSON (legacy format).
func (h *Handler) ServeJSONMetrics(w http.ResponseWriter, r *http.Request) {
	hits, misses, evicts, staleHits := h.cache.GetStats()
	total := hits + misses
	hitRate := 0.0
	if total > 0 {
		hitRate = float64(hits) / float64(total) * 100
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	resp := metricsResponse{
		Cache: cacheMetrics{
			Hits:       hits,
			Misses:     misses,
			Evictions:  evicts,
			StaleHits:  staleHits,
			ItemsInMem: h.cache.Len(),
			HitRate:    hitRate,
		},
		Runtime: runtimeMetrics{
			Goroutines:  runtime.NumGoroutine(),
			HeapAllocMB: float64(mem.HeapAlloc) / 1024 / 1024,
			NumGC:       mem.NumGC,
		},
		Origin: originMetrics{
			CircuitState: h.origin.CircuitState(),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ServePrometheus writes metrics in Prometheus text exposition format.
func (h *Handler) ServePrometheus(w http.ResponseWriter, r *http.Request) {
	hits, misses, evicts, staleHits := h.cache.GetStats()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	circuitState := h.origin.CircuitState()
	circuitOpen := 0
	if circuitState == "open" {
		circuitOpen = 1
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Cache metrics.
	fmt.Fprintln(w, "# HELP cdn_cache_hits_total Total number of cache hits.")
	fmt.Fprintln(w, "# TYPE cdn_cache_hits_total counter")
	fmt.Fprintf(w, "cdn_cache_hits_total %d\n", hits)

	fmt.Fprintln(w, "# HELP cdn_cache_misses_total Total number of cache misses.")
	fmt.Fprintln(w, "# TYPE cdn_cache_misses_total counter")
	fmt.Fprintf(w, "cdn_cache_misses_total %d\n", misses)

	fmt.Fprintln(w, "# HELP cdn_cache_evictions_total Total number of cache evictions.")
	fmt.Fprintln(w, "# TYPE cdn_cache_evictions_total counter")
	fmt.Fprintf(w, "cdn_cache_evictions_total %d\n", evicts)

	fmt.Fprintln(w, "# HELP cdn_cache_stale_hits_total Total number of stale-while-revalidate hits.")
	fmt.Fprintln(w, "# TYPE cdn_cache_stale_hits_total counter")
	fmt.Fprintf(w, "cdn_cache_stale_hits_total %d\n", staleHits)

	fmt.Fprintln(w, "# HELP cdn_cache_items_in_memory Current number of items in the in-memory cache.")
	fmt.Fprintln(w, "# TYPE cdn_cache_items_in_memory gauge")
	fmt.Fprintf(w, "cdn_cache_items_in_memory %d\n", h.cache.Len())

	// Origin circuit breaker.
	fmt.Fprintln(w, "# HELP cdn_origin_circuit_open Whether the origin circuit breaker is open (1) or closed (0).")
	fmt.Fprintln(w, "# TYPE cdn_origin_circuit_open gauge")
	fmt.Fprintf(w, "cdn_origin_circuit_open %d\n", circuitOpen)

	// Runtime metrics.
	fmt.Fprintln(w, "# HELP cdn_goroutines Current number of goroutines.")
	fmt.Fprintln(w, "# TYPE cdn_goroutines gauge")
	fmt.Fprintf(w, "cdn_goroutines %d\n", runtime.NumGoroutine())

	fmt.Fprintln(w, "# HELP cdn_heap_alloc_bytes Current heap allocation in bytes.")
	fmt.Fprintln(w, "# TYPE cdn_heap_alloc_bytes gauge")
	fmt.Fprintf(w, "cdn_heap_alloc_bytes %d\n", mem.HeapAlloc)

	fmt.Fprintln(w, "# HELP cdn_gc_total Total number of completed GC cycles.")
	fmt.Fprintln(w, "# TYPE cdn_gc_total counter")
	fmt.Fprintf(w, "cdn_gc_total %d\n", mem.NumGC)

	// Request latency histogram and status code counters.
	h.ReqMet.WritePrometheus(w)
}

// ServeLiveness always returns 200 OK if the process is alive.
// Kubernetes uses this to decide whether to restart the pod.
func (h *Handler) ServeLiveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"alive"}`))
}

// ServeReadiness returns 200 if the server is ready to accept traffic,
// 503 otherwise. Kubernetes uses this to decide whether to route traffic.
func (h *Handler) ServeReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if !h.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"not ready"}`))
		return
	}
	// Also report not-ready if the origin circuit is open — we can still serve
	// cached content, but downstream LBs may want to route around us.
	if h.origin.CircuitState() == "open" {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"status":"origin unavailable"}`))
		return
	}
	w.Write([]byte(`{"status":"ready"}`))
}

// ServePurge handles cache purge requests (POST only).
func (h *Handler) ServePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	h.cache.Purge()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"purged"}`))
}

// ServeLogLevel handles GET to return and POST to change log level at runtime.
// Expected POST body: {"level":"debug|info|error"}
type logLevelSetter interface {
	Level() string
	SetLevel(string)
}

func LogLevelHandler(ll logLevelSetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			fmt.Fprintf(w, `{"level":%q}`, ll.Level())
		case http.MethodPost:
			var body struct {
				Level string `json:"level"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			switch body.Level {
			case "debug", "info", "error":
				ll.SetLevel(body.Level)
				fmt.Fprintf(w, `{"level":%q}`, body.Level)
			default:
				http.Error(w, "invalid level", http.StatusBadRequest)
			}
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	}
}

// AdminAuth wraps a handler with API key authentication.
func AdminAuth(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	if apiKey == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}
		if key != apiKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
