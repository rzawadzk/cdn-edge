package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/config"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
)

// WarmHandler pre-fetches URLs into the cache.
// POST /warm with body: {"urls":["https://cdn.example.com/foo","..."]}
type WarmHandler struct {
	cache  *cache.Cache
	origin *proxy.Origin
	cfg    *config.Config
	log    *logging.Logger
}

// NewWarmHandler creates a cache warming handler.
func NewWarmHandler(c *cache.Cache, o *proxy.Origin, cfg *config.Config, log *logging.Logger) *WarmHandler {
	return &WarmHandler{cache: c, origin: o, cfg: cfg, log: log}
}

type warmRequest struct {
	URLs []string `json:"urls"`
}

type warmResult struct {
	URL    string `json:"url"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (wh *WarmHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req warmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		http.Error(w, `{"error":"no urls provided"}`, http.StatusBadRequest)
		return
	}
	if len(req.URLs) > 1000 {
		http.Error(w, `{"error":"max 1000 urls per request"}`, http.StatusBadRequest)
		return
	}

	// Warm concurrently with bounded parallelism.
	results := make([]warmResult, len(req.URLs))
	sem := make(chan struct{}, 10) // max 10 concurrent origin fetches
	var wg sync.WaitGroup

	for i, u := range req.URLs {
		wg.Add(1)
		go func(idx int, urlPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[idx] = wh.warmURL(urlPath)
		}(i, u)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"warmed":  len(req.URLs),
		"results": results,
	})
}

func (wh *WarmHandler) warmURL(urlPath string) warmResult {
	ctx, cancel := context.WithTimeout(context.Background(), wh.cfg.OriginTimeout)
	defer cancel()

	originURL := wh.cfg.OriginURL + urlPath
	cacheKey := cache.NormalizeKey("", urlPath, "", "", nil)

	resp, err := wh.origin.Fetch(ctx, "warm:"+cacheKey, originURL, http.Header{})
	if err != nil {
		return warmResult{URL: urlPath, Status: "error", Error: err.Error()}
	}
	if resp.StatusCode >= 400 {
		return warmResult{URL: urlPath, Status: "error", Error: fmt.Sprintf("status %d", resp.StatusCode)}
	}

	cc := resp.Header.Get("Cache-Control")
	if hasCacheDirective(cc, "no-store") || hasCacheDirective(cc, "private") {
		return warmResult{URL: urlPath, Status: "skipped"}
	}

	ttl := getCacheDirectiveValue(cc, "s-maxage")
	if ttl == 0 {
		ttl = getCacheDirectiveValue(cc, "max-age")
	}
	if ttl == 0 {
		ttl = wh.cfg.DefaultTTL
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		etag = fmt.Sprintf(`"%x"`, time.Now().UnixNano())
	}

	entry := &cache.Entry{
		Body:       resp.Body,
		Header:     resp.Header.Clone(),
		StatusCode: resp.StatusCode,
		StoredAt:   time.Now(),
		TTL:        ttl,
		ETag:       etag,
		LastMod:    resp.Header.Get("Last-Modified"),
	}
	wh.cache.Put(cacheKey, entry)
	return warmResult{URL: urlPath, Status: "ok"}
}
