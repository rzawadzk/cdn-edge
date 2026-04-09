package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/proxy"
	"github.com/rzawadzk/cdn-edge/tenant"
)

// MultiTenantCDN routes requests based on the Host header to per-tenant
// origins and cache configurations.
type MultiTenantCDN struct {
	mu     sync.RWMutex
	config *tenant.EdgeConfig
	cache  *cache.Cache
	origin *proxy.Origin
	log    *logging.Logger

	defaultTTL time.Duration
}

// NewMultiTenant creates a multi-tenant CDN handler.
func NewMultiTenant(c *cache.Cache, o *proxy.Origin, log *logging.Logger, defaultTTL time.Duration) *MultiTenantCDN {
	return &MultiTenantCDN{
		cache:      c,
		origin:     o,
		log:        log,
		defaultTTL: defaultTTL,
	}
}

// UpdateConfig replaces the routing configuration atomically.
func (h *MultiTenantCDN) UpdateConfig(cfg *tenant.EdgeConfig) {
	h.mu.Lock()
	h.config = cfg
	h.mu.Unlock()
	h.log.Info(fmt.Sprintf("config updated: version=%d, domains=%d", cfg.Version, len(cfg.Domains)))
}

// GetConfig returns the current config version.
func (h *MultiTenantCDN) GetConfig() *tenant.EdgeConfig {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.config
}

// HandleConfigPush receives a config push from the control plane (POST /edge/config).
func (h *MultiTenantCDN) HandleConfigPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var cfg tenant.EdgeConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	h.UpdateConfig(&cfg)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%d}`, cfg.Version)
}

func (h *MultiTenantCDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if !mtIsValidPath(r.URL.Path) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	hostname := stripPort(r.Host)
	if hostname == "" {
		http.Error(w, "Bad Request: missing Host", http.StatusBadRequest)
		return
	}

	// Look up domain config.
	domain := h.lookupDomain(hostname)
	if domain == nil {
		http.Error(w, "Unknown domain", http.StatusForbidden)
		return
	}
	if !domain.Active {
		http.Error(w, "Domain inactive", http.StatusForbidden)
		return
	}

	// Byte-range requests: serve from cache or fetch full + cache + slice.
	if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
		h.mtServeByteRange(w, r, domain, rangeHeader)
		return
	}

	primaryKey := cache.NormalizeKey(hostname, r.URL.Path, r.URL.RawQuery, "", nil)

	// Check path-specific cache rules.
	rules := h.lookupRules(domain.ID)
	for _, rule := range rules {
		if matchGlob(r.URL.Path, rule.PathGlob) {
			if rule.Bypass {
				h.mtProxyPassthrough(w, r, domain)
				return
			}
		}
	}

	if hasCacheDirective(r.Header.Get("Cache-Control"), "no-cache") {
		h.mtFetchAndServe(w, r, domain, primaryKey, rules)
		return
	}

	entry := h.cache.Get(primaryKey)
	if entry != nil {
		if vary := entry.Header.Get("Vary"); vary != "" && vary != "*" {
			varyKey := cache.NormalizeKey(hostname, r.URL.Path, r.URL.RawQuery, vary, r.Header)
			if varyKey != primaryKey {
				entry = h.cache.Get(varyKey)
			}
		}
	}

	if entry != nil {
		if entry.IsExpired() && entry.IsStaleServable() {
			serveEntry(w, entry, "STALE")
			return
		}
		if etag := r.Header.Get("If-None-Match"); etag != "" && etag == entry.ETag {
			w.Header().Set("ETag", entry.ETag)
			w.Header().Set("X-Cache", "HIT")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		serveEntry(w, entry, "HIT")
		return
	}

	h.mtFetchAndServe(w, r, domain, primaryKey, rules)
}

func (h *MultiTenantCDN) mtFetchAndServe(w http.ResponseWriter, r *http.Request, domain *tenant.Domain, primaryKey string, rules []*tenant.CacheRule) {
	originURL := strings.TrimRight(domain.OriginURL, "/") + r.URL.RequestURI()

	reqHeader := r.Header.Clone()
	if domain.OriginHost != "" {
		reqHeader.Set("Host", domain.OriginHost)
	}

	resp, err := h.origin.Fetch(r.Context(), primaryKey, originURL, reqHeader)
	if err != nil {
		reqID := logging.GetRequestID(r.Context())
		h.log.Error("origin fetch failed for "+domain.Hostname, err, reqID)

		if stale := h.cache.Get(primaryKey); stale != nil {
			serveEntry(w, stale, "STALE-ERROR")
			return
		}
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	ttl, swr := h.mtDetermineTTL(resp, domain, rules, r.URL.Path)

	if ttl > 0 && isCacheableStatus(resp.StatusCode) {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			hash := sha256.Sum256(resp.Body)
			etag = fmt.Sprintf(`"%x"`, hash[:8])
		}

		vary := resp.Header.Get("Vary")
		cacheKey := cache.NormalizeKey(stripPort(r.Host), r.URL.Path, r.URL.RawQuery, vary, r.Header)

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
		serveEntry(w, entry, "MISS")
		return
	}
	serveResponse(w, resp, "BYPASS")
}

func (h *MultiTenantCDN) mtDetermineTTL(resp *proxy.Response, domain *tenant.Domain, rules []*tenant.CacheRule, urlPath string) (ttl, swr time.Duration) {
	cc := resp.Header.Get("Cache-Control")
	if hasCacheDirective(cc, "no-store") || hasCacheDirective(cc, "private") {
		return 0, 0
	}

	swr = getCacheDirectiveValue(cc, "stale-while-revalidate")

	if domain.RespectOriginHeaders {
		if sma := getCacheDirectiveValue(cc, "s-maxage"); sma > 0 {
			return sma, swr
		}
		if ma := getCacheDirectiveValue(cc, "max-age"); ma > 0 {
			return ma, swr
		}
	}

	// Check path-specific TTL.
	for _, rule := range rules {
		if matchGlob(urlPath, rule.PathGlob) && rule.TTLSec > 0 {
			return time.Duration(rule.TTLSec) * time.Second, swr
		}
	}

	// Domain default.
	if domain.DefaultTTLSec > 0 {
		return time.Duration(domain.DefaultTTLSec) * time.Second, swr
	}
	return h.defaultTTL, swr
}

func (h *MultiTenantCDN) mtServeByteRange(w http.ResponseWriter, r *http.Request, domain *tenant.Domain, rangeHeader string) {
	hostname := stripPort(r.Host)
	primaryKey := cache.NormalizeKey(hostname, r.URL.Path, r.URL.RawQuery, "", nil)

	// Try to serve from existing cache entry.
	if entry := h.cache.Get(primaryKey); entry != nil {
		start, end, ok := parseRange(rangeHeader, len(entry.Body))
		if !ok {
			h.mtProxyPassthrough(w, r, domain)
			return
		}
		serveRange(w, entry, start, end)
		return
	}

	// Not cached: fetch full response from origin (strip Range so we get complete body).
	originURL := strings.TrimRight(domain.OriginURL, "/") + r.URL.RequestURI()
	cleanHeader := r.Header.Clone()
	cleanHeader.Del("Range")
	if domain.OriginHost != "" {
		cleanHeader.Set("Host", domain.OriginHost)
	}

	resp, err := h.origin.Fetch(r.Context(), primaryKey, originURL, cleanHeader)
	if err != nil {
		reqID := logging.GetRequestID(r.Context())
		h.log.Error("origin fetch failed for "+domain.Hostname, err, reqID)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	rules := h.lookupRules(domain.ID)
	ttl, swr := h.mtDetermineTTL(resp, domain, rules, r.URL.Path)

	if ttl > 0 && isCacheableStatus(resp.StatusCode) {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			hash := sha256.Sum256(resp.Body)
			etag = fmt.Sprintf(`"%x"`, hash[:8])
		}
		vary := resp.Header.Get("Vary")
		cacheKey := cache.NormalizeKey(hostname, r.URL.Path, r.URL.RawQuery, vary, r.Header)
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

		start, end, ok := parseRange(rangeHeader, len(entry.Body))
		if !ok {
			serveEntry(w, entry, "MISS")
			return
		}
		serveRange(w, entry, start, end)
		return
	}

	serveResponse(w, resp, "BYPASS")
}

func (h *MultiTenantCDN) mtProxyPassthrough(w http.ResponseWriter, r *http.Request, domain *tenant.Domain) {
	originURL := strings.TrimRight(domain.OriginURL, "/") + r.URL.RequestURI()
	resp, err := h.origin.Fetch(r.Context(), "passthrough:"+r.URL.RequestURI(), originURL, r.Header)
	if err != nil {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	serveResponse(w, resp, "BYPASS")
}

func (h *MultiTenantCDN) lookupDomain(hostname string) *tenant.Domain {
	h.mu.RLock()
	cfg := h.config
	h.mu.RUnlock()

	if cfg == nil {
		return nil
	}
	return cfg.Domains[hostname]
}

func (h *MultiTenantCDN) lookupRules(domainID string) []*tenant.CacheRule {
	h.mu.RLock()
	cfg := h.config
	h.mu.RUnlock()

	if cfg == nil {
		return nil
	}
	return cfg.Rules[domainID]
}

// matchGlob does simple path glob matching (* matches any segment).
func matchGlob(urlPath, pattern string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" || pattern == "/*" {
		return true
	}
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(urlPath, prefix+"/") || urlPath == prefix
	}
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		return strings.HasPrefix(urlPath, prefix)
	}
	return urlPath == pattern
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

func mtIsValidPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	if strings.Contains(p, "..") {
		return false
	}
	cleaned := path.Clean(p)
	if strings.Contains(cleaned, "..") {
		return false
	}
	return true
}

// ConfigPoller periodically fetches config from the control plane.
type ConfigPoller struct {
	controlPlaneURL string
	adminKey        string
	handler         *MultiTenantCDN
	log             *logging.Logger
	client          *http.Client
	lastVersion     int64
}

// NewConfigPoller creates a config poller.
func NewConfigPoller(cpURL, adminKey string, handler *MultiTenantCDN, log *logging.Logger) *ConfigPoller {
	return &ConfigPoller{
		controlPlaneURL: strings.TrimRight(cpURL, "/"),
		adminKey:        adminKey,
		handler:         handler,
		log:             log,
		client:          &http.Client{Timeout: 10 * time.Second},
	}
}

// Start begins polling for config updates. Blocks until ctx is cancelled.
func (p *ConfigPoller) Start(ctx context.Context, interval time.Duration) {
	// Initial fetch.
	p.poll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *ConfigPoller) poll() {
	req, err := http.NewRequest(http.MethodGet, p.controlPlaneURL+"/api/v1/config", nil)
	if err != nil {
		p.log.Error("config poll: build request", err)
		return
	}
	req.Header.Set("X-API-Key", p.adminKey)

	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Error("config poll: fetch", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		p.log.Error("config poll: bad status", fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		p.log.Error("config poll: read", err)
		return
	}

	var cfg tenant.EdgeConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		p.log.Error("config poll: parse", err)
		return
	}

	if cfg.Version > p.lastVersion {
		p.handler.UpdateConfig(&cfg)
		p.lastVersion = cfg.Version
		p.log.Info(fmt.Sprintf("config poll: updated to version %d (%d domains)", cfg.Version, len(cfg.Domains)))
	}
}

