// Package tenant defines the shared data model for CDN tenants.
// Both the control plane and edge servers import this package.
package tenant

import "time"

// Tenant represents a customer and their CDN configuration.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Active    bool      `json:"active"`
	APIKey    string    `json:"api_key"` // tenant's API key for purge/config
}

// Domain maps a hostname to an origin and cache rules.
type Domain struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Hostname string `json:"hostname"` // e.g. "www.customer.com"

	// Origin
	OriginURL      string `json:"origin_url"`       // e.g. "https://origin.customer.com"
	OriginHost     string `json:"origin_host"`       // Host header override; empty = use hostname
	OriginTimeout  int    `json:"origin_timeout_ms"` // milliseconds; 0 = default

	// Cache rules
	DefaultTTLSec        int  `json:"default_ttl_sec"`
	MaxCacheEntryBytes   int  `json:"max_cache_entry_bytes"`
	RespectOriginHeaders bool `json:"respect_origin_headers"` // honor Cache-Control from origin

	// CORS
	CORSOrigins string `json:"cors_origins"` // comma-separated; "*" for all
	CORSMaxAge  int    `json:"cors_max_age"`

	// TLS
	TLSMode    string `json:"tls_mode"`     // "managed" (ACME), "custom", "none"
	TLSCert    string `json:"tls_cert"`     // PEM cert (for "custom" mode)
	TLSKey     string `json:"tls_key"`      // PEM key (for "custom" mode)
	TLSAcmeDNS bool   `json:"tls_acme_dns"` // use DNS-01 challenge

	// Rate limiting
	RateLimitRPS   float64 `json:"rate_limit_rps"`
	RateLimitBurst int     `json:"rate_limit_burst"`

	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CacheRule is a path-specific override for cache behavior.
type CacheRule struct {
	ID       string `json:"id"`
	DomainID string `json:"domain_id"`
	PathGlob string `json:"path_glob"` // e.g. "/static/*", "/api/*"
	TTLSec   int    `json:"ttl_sec"`   // 0 = don't cache; -1 = use domain default
	Bypass   bool   `json:"bypass"`    // skip cache entirely for this path
}

// EdgeConfig is the full configuration snapshot pushed to an edge server.
// It contains all active domains and their rules.
type EdgeConfig struct {
	Version   int64              `json:"version"`    // monotonically increasing
	Timestamp time.Time          `json:"timestamp"`
	Domains   map[string]*Domain `json:"domains"`    // keyed by hostname
	Rules     map[string][]*CacheRule `json:"rules"` // keyed by domain ID
}

// EdgeNode represents a registered edge server.
type EdgeNode struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Addr     string    `json:"addr"`      // e.g. "https://edge-fra.internal:9090"
	Region   string    `json:"region"`    // e.g. "eu-west", "us-east"
	LastSeen time.Time `json:"last_seen"`
	Active   bool      `json:"active"`
}
