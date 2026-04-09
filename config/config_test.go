package config

import (
	"testing"
	"time"
)

func validConfig() *Config {
	return &Config{
		ListenAddr:      ":8080",
		OriginURL:       "https://origin.example.com",
		DefaultTTL:      5 * time.Minute,
		MaxCacheItems:   1000,
		OriginTimeout:   30 * time.Second,
		CoalesceTimeout: 30 * time.Second,
		LogLevel:        "info",
	}
}

func TestValidateOK(t *testing.T) {
	cfg := validConfig()
	if err := cfg.validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestValidateMissingOrigin(t *testing.T) {
	cfg := validConfig()
	cfg.OriginURL = ""
	if err := cfg.validate(); err == nil {
		t.Error("expected error for missing origin")
	}
}

func TestValidateBadOriginScheme(t *testing.T) {
	cfg := validConfig()
	cfg.OriginURL = "ftp://origin.example.com"
	if err := cfg.validate(); err == nil {
		t.Error("expected error for non-http origin scheme")
	}
}

func TestValidateTLSWithoutCert(t *testing.T) {
	cfg := validConfig()
	cfg.TLSEnabled = true
	if err := cfg.validate(); err == nil {
		t.Error("expected error when TLS enabled without cert")
	}
}

func TestValidateTLSAutoWithoutDomain(t *testing.T) {
	cfg := validConfig()
	cfg.TLSEnabled = true
	cfg.TLSAuto = true
	if err := cfg.validate(); err == nil {
		t.Error("expected error when TLS auto enabled without domain")
	}
}

func TestValidateBadLogLevel(t *testing.T) {
	cfg := validConfig()
	cfg.LogLevel = "verbose"
	if err := cfg.validate(); err == nil {
		t.Error("expected error for invalid log level")
	}
}

func TestValidateNegativeRateLimit(t *testing.T) {
	cfg := validConfig()
	cfg.RateLimitRPS = -1
	if err := cfg.validate(); err == nil {
		t.Error("expected error for negative rate limit")
	}
}

func TestValidateRateLimitWithoutBurst(t *testing.T) {
	cfg := validConfig()
	cfg.RateLimitRPS = 100
	cfg.RateLimitBurst = 0
	if err := cfg.validate(); err == nil {
		t.Error("expected error for rate limit without burst")
	}
}

func TestValidateTrimsTrailingSlash(t *testing.T) {
	cfg := validConfig()
	cfg.OriginURL = "https://origin.example.com/"
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.OriginURL != "https://origin.example.com" {
		t.Errorf("trailing slash not trimmed: %q", cfg.OriginURL)
	}
}
