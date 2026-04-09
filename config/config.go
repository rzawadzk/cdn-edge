package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all edge server configuration.
type Config struct {
	// ListenAddr is the address to listen on (e.g., ":8080").
	ListenAddr string

	// OriginURL is the base URL of the origin server (e.g., "https://origin.example.com").
	OriginURL string

	// OriginHostOverride overrides the Host header sent to origin. Empty = use client's Host.
	OriginHostOverride string

	// DefaultTTL is the cache duration when no Cache-Control header is present.
	DefaultTTL time.Duration

	// MaxCacheItems is the maximum number of items in the in-memory LRU cache.
	MaxCacheItems int

	// DiskCacheDir is the directory for disk-backed cache overflow. Empty disables disk cache.
	DiskCacheDir string

	// DiskCacheMaxMB is the maximum disk cache size in megabytes. 0 means unlimited.
	DiskCacheMaxMB int64

	// OriginTimeout is the max time to wait for an origin response.
	OriginTimeout time.Duration

	// TLS
	TLSEnabled    bool
	TLSCertFile   string
	TLSKeyFile    string
	TLSAuto       bool   // enable automatic ACME (Let's Encrypt) certificates
	TLSAutoDomain string // domain for ACME
	HTTPSRedirect bool   // redirect HTTP to HTTPS when TLS is enabled

	// Compression
	CompressionEnabled bool

	// Admin
	AdminAddr   string // separate listen address for admin endpoints (metrics, purge, health)
	AdminAPIKey string // API key for admin endpoints; empty = no auth

	// Rate limiting
	RateLimitRPS   float64 // per-IP requests per second; 0 = disabled
	RateLimitBurst int     // burst bucket size

	// Cache limits
	MaxCacheEntryBytes int64 // max size of a single cache entry in bytes; 0 = no limit

	// Origin fetch
	CoalesceTimeout time.Duration // max time waiters block on a coalesced request

	// CORS
	CORSOrigins string // comma-separated allowed origins; "*" for all; empty = disabled
	CORSMaxAge  int    // Access-Control-Max-Age in seconds

	// Logging
	LogLevel string // "debug", "info", "error"

	// Connection pool
	MaxIdleConnsPerHost int // max idle connections to origin per host

	// Graceful shutdown
	DrainDelay time.Duration // delay between readiness=false and stopping listener

	// Cache key
	MaxCacheKeyLen int // max cache key length; 0 = no limit

	// Multi-tenant mode
	ControlPlaneURL    string        // control plane URL; empty = single-tenant mode
	ConfigPollInterval time.Duration // how often to poll for config updates

	// Tracing (OpenTelemetry)
	TracingEnabled    bool    // enable distributed tracing
	TracingEndpoint   string  // OTLP collector gRPC endpoint
	TracingSampleRate float64 // trace sample rate 0.0-1.0
	TracingInsecure   bool    // use insecure gRPC connection
}

// Load parses config from flags, then overlays environment variables.
// Env vars take precedence (12-factor pattern for container deployments).
func Load() (*Config, error) {
	cfg := &Config{}

	flag.StringVar(&cfg.ListenAddr, "listen", ":8080", "address to listen on")
	flag.StringVar(&cfg.OriginURL, "origin", "", "origin server base URL (required)")
	flag.StringVar(&cfg.OriginHostOverride, "origin-host", "", "override Host header sent to origin")
	flag.DurationVar(&cfg.DefaultTTL, "default-ttl", 5*time.Minute, "default cache TTL")
	flag.IntVar(&cfg.MaxCacheItems, "max-cache-items", 10000, "max in-memory cache items")
	flag.StringVar(&cfg.DiskCacheDir, "disk-cache-dir", "", "disk cache directory (optional)")
	flag.Int64Var(&cfg.DiskCacheMaxMB, "disk-cache-max-mb", 1024, "max disk cache size in MB (0=unlimited)")
	flag.DurationVar(&cfg.OriginTimeout, "origin-timeout", 30*time.Second, "origin request timeout")

	// TLS
	flag.BoolVar(&cfg.TLSEnabled, "tls", false, "enable TLS")
	flag.StringVar(&cfg.TLSCertFile, "tls-cert", "", "TLS certificate file")
	flag.StringVar(&cfg.TLSKeyFile, "tls-key", "", "TLS key file")
	flag.BoolVar(&cfg.TLSAuto, "tls-auto", false, "enable automatic ACME certificates")
	flag.StringVar(&cfg.TLSAutoDomain, "tls-auto-domain", "", "domain for ACME")
	flag.BoolVar(&cfg.HTTPSRedirect, "https-redirect", true, "redirect HTTP to HTTPS when TLS enabled")

	// Compression
	flag.BoolVar(&cfg.CompressionEnabled, "compress", true, "enable gzip compression")

	// Admin
	flag.StringVar(&cfg.AdminAddr, "admin-listen", "", "separate admin listen address")
	flag.StringVar(&cfg.AdminAPIKey, "admin-api-key", "", "API key for admin endpoints")

	// Rate limiting
	flag.Float64Var(&cfg.RateLimitRPS, "rate-limit-rps", 0, "per-IP rate limit requests/sec (0=disabled)")
	flag.IntVar(&cfg.RateLimitBurst, "rate-limit-burst", 50, "rate limit burst size")

	// Cache limits
	flag.Int64Var(&cfg.MaxCacheEntryBytes, "max-cache-entry-bytes", 10<<20, "max cache entry size (default 10MB)")

	// Origin fetch
	flag.DurationVar(&cfg.CoalesceTimeout, "coalesce-timeout", 30*time.Second, "max wait for coalesced request")

	// CORS
	flag.StringVar(&cfg.CORSOrigins, "cors-origins", "", "allowed CORS origins (comma-separated, * for all)")
	flag.IntVar(&cfg.CORSMaxAge, "cors-max-age", 86400, "CORS preflight max-age in seconds")

	// Logging
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "log level: debug, info, error")

	// Connection pool
	flag.IntVar(&cfg.MaxIdleConnsPerHost, "max-idle-conns-per-host", 100, "max idle connections to origin per host")

	// Graceful shutdown
	flag.DurationVar(&cfg.DrainDelay, "drain-delay", 5*time.Second, "delay between readiness=false and listener stop")

	// Cache key
	flag.IntVar(&cfg.MaxCacheKeyLen, "max-cache-key-len", 8192, "max cache key length in bytes")

	// Multi-tenant mode
	flag.StringVar(&cfg.ControlPlaneURL, "control-plane", "", "control plane URL (enables multi-tenant mode)")
	flag.DurationVar(&cfg.ConfigPollInterval, "config-poll-interval", 30*time.Second, "config poll interval in multi-tenant mode")

	// Tracing
	flag.BoolVar(&cfg.TracingEnabled, "tracing", false, "enable OpenTelemetry distributed tracing")
	flag.StringVar(&cfg.TracingEndpoint, "tracing-endpoint", "localhost:4317", "OTLP collector gRPC endpoint")
	flag.Float64Var(&cfg.TracingSampleRate, "tracing-sample-rate", 1.0, "trace sample rate (0.0-1.0)")
	flag.BoolVar(&cfg.TracingInsecure, "tracing-insecure", true, "use insecure gRPC for OTLP")

	flag.Parse()

	// Overlay environment variables (env takes precedence over flags).
	cfg.applyEnv()

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (cfg *Config) applyEnv() {
	envStr(&cfg.ListenAddr, "CDN_LISTEN")
	envStr(&cfg.OriginURL, "CDN_ORIGIN")
	envStr(&cfg.OriginHostOverride, "CDN_ORIGIN_HOST")
	envDuration(&cfg.DefaultTTL, "CDN_DEFAULT_TTL")
	envInt(&cfg.MaxCacheItems, "CDN_MAX_CACHE_ITEMS")
	envStr(&cfg.DiskCacheDir, "CDN_DISK_CACHE_DIR")
	envInt64(&cfg.DiskCacheMaxMB, "CDN_DISK_CACHE_MAX_MB")
	envDuration(&cfg.OriginTimeout, "CDN_ORIGIN_TIMEOUT")

	envBool(&cfg.TLSEnabled, "CDN_TLS")
	envStr(&cfg.TLSCertFile, "CDN_TLS_CERT")
	envStr(&cfg.TLSKeyFile, "CDN_TLS_KEY")
	envBool(&cfg.TLSAuto, "CDN_TLS_AUTO")
	envStr(&cfg.TLSAutoDomain, "CDN_TLS_AUTO_DOMAIN")
	envBool(&cfg.HTTPSRedirect, "CDN_HTTPS_REDIRECT")

	envBool(&cfg.CompressionEnabled, "CDN_COMPRESS")

	envStr(&cfg.AdminAddr, "CDN_ADMIN_LISTEN")
	envStr(&cfg.AdminAPIKey, "CDN_ADMIN_API_KEY")

	envFloat64(&cfg.RateLimitRPS, "CDN_RATE_LIMIT_RPS")
	envInt(&cfg.RateLimitBurst, "CDN_RATE_LIMIT_BURST")

	envInt64(&cfg.MaxCacheEntryBytes, "CDN_MAX_CACHE_ENTRY_BYTES")
	envDuration(&cfg.CoalesceTimeout, "CDN_COALESCE_TIMEOUT")

	envStr(&cfg.CORSOrigins, "CDN_CORS_ORIGINS")
	envInt(&cfg.CORSMaxAge, "CDN_CORS_MAX_AGE")

	envStr(&cfg.LogLevel, "CDN_LOG_LEVEL")
	envInt(&cfg.MaxIdleConnsPerHost, "CDN_MAX_IDLE_CONNS_PER_HOST")
	envDuration(&cfg.DrainDelay, "CDN_DRAIN_DELAY")
	envInt(&cfg.MaxCacheKeyLen, "CDN_MAX_CACHE_KEY_LEN")
	envStr(&cfg.ControlPlaneURL, "CDN_CONTROL_PLANE")
	envDuration(&cfg.ConfigPollInterval, "CDN_CONFIG_POLL_INTERVAL")

	envBool(&cfg.TracingEnabled, "CDN_TRACING")
	envStr(&cfg.TracingEndpoint, "CDN_TRACING_ENDPOINT")
	envFloat64(&cfg.TracingSampleRate, "CDN_TRACING_SAMPLE_RATE")
	envBool(&cfg.TracingInsecure, "CDN_TRACING_INSECURE")
}

func (cfg *Config) validate() error {
	// In multi-tenant mode, origin is not required (comes from config).
	if cfg.ControlPlaneURL != "" {
		// Multi-tenant mode — origin is optional.
		if cfg.OriginURL == "" {
			cfg.OriginURL = "http://localhost" // placeholder for proxy init
		}
	}

	if cfg.OriginURL == "" {
		return fmt.Errorf("origin URL is required (--origin or CDN_ORIGIN), or use --control-plane for multi-tenant mode")
	}
	u, err := url.Parse(cfg.OriginURL)
	if err != nil {
		return fmt.Errorf("invalid origin URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("origin URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("origin URL must have a host")
	}
	// Strip trailing slash for consistency.
	cfg.OriginURL = strings.TrimRight(cfg.OriginURL, "/")

	if cfg.TLSEnabled {
		if cfg.TLSAuto {
			if cfg.TLSAutoDomain == "" {
				return fmt.Errorf("--tls-auto-domain is required when --tls-auto is enabled")
			}
		} else {
			if cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" {
				return fmt.Errorf("--tls-cert and --tls-key are required when --tls is enabled (or use --tls-auto)")
			}
		}
	}

	if cfg.MaxCacheItems <= 0 {
		return fmt.Errorf("--max-cache-items must be positive")
	}
	if cfg.OriginTimeout <= 0 {
		return fmt.Errorf("--origin-timeout must be positive")
	}
	if cfg.CoalesceTimeout <= 0 {
		return fmt.Errorf("--coalesce-timeout must be positive")
	}
	if cfg.RateLimitRPS < 0 {
		return fmt.Errorf("--rate-limit-rps cannot be negative")
	}
	if cfg.RateLimitBurst <= 0 && cfg.RateLimitRPS > 0 {
		return fmt.Errorf("--rate-limit-burst must be positive when rate limiting is enabled")
	}

	switch cfg.LogLevel {
	case "debug", "info", "error":
	default:
		return fmt.Errorf("--log-level must be debug, info, or error; got %q", cfg.LogLevel)
	}

	return nil
}

// Env helpers — only override if the env var is set and non-empty.

func envStr(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func envBool(dst *bool, key string) {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			*dst = b
		}
	}
}

func envInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			*dst = n
		}
	}
}

func envInt64(dst *int64, key string) {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			*dst = n
		}
	}
}

func envFloat64(dst *float64, key string) {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			*dst = f
		}
	}
}

func envDuration(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			*dst = d
		}
	}
}
