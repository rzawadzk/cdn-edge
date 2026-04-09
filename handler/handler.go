package handler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/config"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
)

// CDN is the main HTTP handler that serves cached content or fetches from origin.
type CDN struct {
	cache  *cache.Cache
	origin *proxy.Origin
	cfg    *config.Config
	log    *logging.Logger
}

// New creates a CDN handler.
func New(c *cache.Cache, o *proxy.Origin, cfg *config.Config, log *logging.Logger) *CDN {
	return &CDN{cache: c, origin: o, cfg: cfg, log: log}
}

func (h *CDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Request validation.
	if !isValidPath(r.URL.Path) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if r.Host == "" {
		http.Error(w, "Bad Request: missing Host header", http.StatusBadRequest)
		return
	}

	// Byte-range requests bypass cache and pass straight to origin.
	// Full byte-range-aware caching (storing slices) is complex; for now we
	// ensure Range requests work correctly by proxying them directly.
	if r.Header.Get("Range") != "" {
		h.proxyPassthrough(w, r)
		return
	}

	// Build primary cache key (Vary-unaware for initial lookup).
	primaryKey := cache.NormalizeKey(r.Host, r.URL.Path, r.URL.RawQuery, "", nil)

	// Check if the client explicitly bypasses cache.
	if hasCacheDirective(r.Header.Get("Cache-Control"), "no-cache") {
		h.fetchAndServe(w, r, primaryKey)
		return
	}

	// Try cache — first try without Vary, then with.
	entry := h.cache.Get(primaryKey)

	// If the cached entry has a Vary header, re-key with request headers.
	if entry != nil {
		if vary := entry.Header.Get("Vary"); vary != "" {
			if vary == "*" {
				// Vary: * means never serve from cache.
				h.fetchAndServe(w, r, primaryKey)
				return
			}
			varyKey := cache.NormalizeKey(r.Host, r.URL.Path, r.URL.RawQuery, vary, r.Header)
			if varyKey != primaryKey {
				entry = h.cache.Get(varyKey)
			}
		}
	}

	if entry != nil {
		// If stale but servable, serve stale and revalidate async.
		if entry.IsExpired() && entry.IsStaleServable() {
			// Snapshot request details before goroutine since r is tied to client context.
			host := r.Host
			requestURI := r.URL.RequestURI()
			urlPath := r.URL.Path
			rawQuery := r.URL.RawQuery
			reqHeader := r.Header.Clone()
			reqID := logging.GetRequestID(r.Context())
			go h.revalidateAsync(host, requestURI, urlPath, rawQuery, reqHeader, primaryKey, reqID)
			serveEntry(w, entry, "STALE")
			return
		}

		// Handle conditional requests.
		if etag := r.Header.Get("If-None-Match"); etag != "" && etag == entry.ETag {
			w.Header().Set("ETag", entry.ETag)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if ims := r.Header.Get("If-Modified-Since"); ims != "" && entry.LastMod != "" {
			imsTime, err := http.ParseTime(ims)
			lastMod, err2 := http.ParseTime(entry.LastMod)
			if err == nil && err2 == nil && !lastMod.After(imsTime) {
				w.Header().Set("Last-Modified", entry.LastMod)
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		serveEntry(w, entry, "HIT")
		return
	}

	h.fetchAndServe(w, r, primaryKey)
}

func (h *CDN) fetchAndServe(w http.ResponseWriter, r *http.Request, primaryKey string) {
	originURL := h.cfg.OriginURL + r.URL.RequestURI()

	resp, err := h.origin.Fetch(r.Context(), primaryKey, originURL, r.Header)
	if err != nil {
		reqID := logging.GetRequestID(r.Context())
		h.log.Error("origin fetch failed", err, reqID)

		// If circuit is open and we have stale data, serve it.
		if stale := h.cache.Get(primaryKey); stale != nil {
			serveEntry(w, stale, "STALE-ERROR")
			return
		}

		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	ttl, swr := h.determineTTL(resp)

	if ttl > 0 && isCacheableStatus(resp.StatusCode) {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			hash := sha256.Sum256(resp.Body)
			etag = fmt.Sprintf(`"%x"`, hash[:8])
		}

		// Determine the correct cache key (may include Vary).
		vary := resp.Header.Get("Vary")
		cacheKey := cache.NormalizeKey(r.Host, r.URL.Path, r.URL.RawQuery, vary, r.Header)

		entry := &cache.Entry{
			Body:                 resp.Body,
			Header:               resp.Header.Clone(),
			StatusCode:           resp.StatusCode,
			StoredAt:             time.Now(),
			TTL:                  ttl,
			ETag:                 etag,
			LastMod:              resp.Header.Get("Last-Modified"),
			VaryKey:              cacheKey,
			StaleWhileRevalidate: swr,
		}

		// Store under both primary key (for initial lookup) and vary key.
		h.cache.Put(primaryKey, entry)
		if cacheKey != primaryKey {
			h.cache.Put(cacheKey, entry)
		}

		serveEntry(w, entry, "MISS")
		return
	}

	// Not cacheable — pass through.
	serveResponse(w, resp, "BYPASS")
}

func (h *CDN) revalidateAsync(host, requestURI, urlPath, rawQuery string, reqHeader http.Header, primaryKey, reqID string) {
	// Bounded context to prevent leaked goroutines if origin hangs.
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.OriginTimeout)
	defer cancel()

	originURL := h.cfg.OriginURL + requestURI
	resp, err := h.origin.Fetch(ctx, "revalidate:"+primaryKey, originURL, reqHeader)
	if err != nil || resp.StatusCode >= 500 {
		h.log.Debug("async revalidation failed or skipped", reqID)
		return
	}

	ttl, swr := h.determineTTL(resp)
	if ttl <= 0 || !isCacheableStatus(resp.StatusCode) {
		return
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		hash := sha256.Sum256(resp.Body)
		etag = fmt.Sprintf(`"%x"`, hash[:8])
	}

	vary := resp.Header.Get("Vary")
	cacheKey := cache.NormalizeKey(host, urlPath, rawQuery, vary, reqHeader)

	entry := &cache.Entry{
		Body:                 resp.Body,
		Header:               resp.Header.Clone(),
		StatusCode:           resp.StatusCode,
		StoredAt:             time.Now(),
		TTL:                  ttl,
		ETag:                 etag,
		LastMod:              resp.Header.Get("Last-Modified"),
		VaryKey:              cacheKey,
		StaleWhileRevalidate: swr,
	}
	h.cache.Put(primaryKey, entry)
	if cacheKey != primaryKey {
		h.cache.Put(cacheKey, entry)
	}
}

func (h *CDN) determineTTL(resp *proxy.Response) (ttl, swr time.Duration) {
	cc := resp.Header.Get("Cache-Control")

	if hasCacheDirective(cc, "no-store") || hasCacheDirective(cc, "private") {
		return 0, 0
	}

	swr = getCacheDirectiveValue(cc, "stale-while-revalidate")

	if sma := getCacheDirectiveValue(cc, "s-maxage"); sma > 0 {
		return sma, swr
	}
	if ma := getCacheDirectiveValue(cc, "max-age"); ma > 0 {
		return ma, swr
	}

	return h.cfg.DefaultTTL, swr
}

func serveEntry(w http.ResponseWriter, entry *cache.Entry, cacheStatus string) {
	for k, vals := range entry.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", cacheStatus)
	if entry.ETag != "" {
		w.Header().Set("ETag", entry.ETag)
	}
	w.Header().Set("Age", strconv.Itoa(int(time.Since(entry.StoredAt).Seconds())))
	w.WriteHeader(entry.StatusCode)
	w.Write(entry.Body)
}

func serveResponse(w http.ResponseWriter, resp *proxy.Response, cacheStatus string) {
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", cacheStatus)
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.Body)
}

func isCacheableStatus(code int) bool {
	switch code {
	case 200, 203, 204, 206, 300, 301, 308, 404, 410:
		return true
	}
	return false
}

func hasCacheDirective(cc, directive string) bool {
	for _, part := range strings.Split(cc, ",") {
		key, _, _ := strings.Cut(strings.TrimSpace(strings.ToLower(part)), "=")
		if strings.TrimSpace(key) == directive {
			return true
		}
	}
	return false
}

func getCacheDirectiveValue(cc, directive string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(strings.ToLower(part)), "=")
		if ok && strings.TrimSpace(key) == directive {
			secs, err := strconv.Atoi(strings.TrimSpace(val))
			if err == nil {
				return time.Duration(secs) * time.Second
			}
		}
	}
	return 0
}

// proxyPassthrough sends the request directly to origin without caching.
// Used for Range requests and other non-cacheable request patterns.
func (h *CDN) proxyPassthrough(w http.ResponseWriter, r *http.Request) {
	originURL := h.cfg.OriginURL + r.URL.RequestURI()
	resp, err := h.origin.Fetch(r.Context(), "passthrough:"+r.URL.RequestURI(), originURL, r.Header)
	if err != nil {
		reqID := logging.GetRequestID(r.Context())
		h.log.Error("origin passthrough failed", err, reqID)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	serveResponse(w, resp, "BYPASS")
}

// isValidPath rejects path traversal attempts.
func isValidPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	// Check raw path for traversal before cleaning.
	if strings.Contains(p, "..") {
		return false
	}
	cleaned := path.Clean(p)
	if strings.Contains(cleaned, "..") {
		return false
	}
	return true
}
