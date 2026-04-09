package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
	"github.com/rzawadzk/cdn-edge/compress"
	"github.com/rzawadzk/cdn-edge/config"
	"github.com/rzawadzk/cdn-edge/handler"
	"github.com/rzawadzk/cdn-edge/logging"
	"github.com/rzawadzk/cdn-edge/metrics"
	"github.com/rzawadzk/cdn-edge/middleware"
	"github.com/rzawadzk/cdn-edge/proxy"
	"github.com/rzawadzk/cdn-edge/purge"
	"github.com/rzawadzk/cdn-edge/ratelimit"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	logger := logging.New()
	logger.SetLevel(cfg.LogLevel)

	// Initialize cache.
	c, err := cache.NewWithOptions(cache.Options{
		MaxItems:      cfg.MaxCacheItems,
		MaxEntryBytes: cfg.MaxCacheEntryBytes,
		MaxKeyLen:     cfg.MaxCacheKeyLen,
		DiskDir:       cfg.DiskCacheDir,
		DiskMaxBytes:  cfg.DiskCacheMaxMB << 20,
	})
	if err != nil {
		log.Fatalf("failed to initialize cache: %v", err)
	}

	// Initialize origin proxy.
	origin := proxy.New(proxy.Options{
		Timeout:             cfg.OriginTimeout,
		CoalesceTimeout:     cfg.CoalesceTimeout,
		MaxBodyBytes:        cfg.MaxCacheEntryBytes,
		HostOverride:        cfg.OriginHostOverride,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
	})

	// Initialize handlers.
	met := metrics.New(c, origin)
	warmer := handler.NewWarmHandler(c, origin, cfg, logger)

	// Distributed purge.
	dist := purge.New(c, cfg.AdminAPIKey)
	if peers := os.Getenv("CDN_PURGE_PEERS"); peers != "" {
		dist.SetPeers(strings.Split(peers, ","))
	}

	// Build the data-plane handler: single-tenant or multi-tenant.
	var appHandler http.Handler

	var mtHandler *handler.MultiTenantCDN // nil unless multi-tenant
	if cfg.ControlPlaneURL != "" {
		// Multi-tenant mode: route by Host header, config from control plane.
		mtHandler = handler.NewMultiTenant(c, origin, logger, cfg.DefaultTTL)
		appHandler = mtHandler
		logger.Info("multi-tenant mode enabled, control plane: " + cfg.ControlPlaneURL)
	} else {
		// Single-tenant mode: original behavior.
		cdn := handler.New(c, origin, cfg, logger)
		appHandler = cdn
	}

	if cfg.CompressionEnabled {
		appHandler = compress.Middleware(appHandler)
	}
	if cfg.CORSOrigins != "" {
		appHandler = middleware.CORS(cfg.CORSOrigins, cfg.CORSMaxAge)(appHandler)
	}
	if cfg.RateLimitRPS > 0 {
		rl := ratelimit.New(cfg.RateLimitRPS, cfg.RateLimitBurst)
		appHandler = rl.Middleware(appHandler)
	}

	// Request latency + status code metrics (wraps entire data plane).
	appHandler = metrics.NewRequestMetricsMiddleware(met.ReqMet, appHandler)

	// Assemble main mux.
	mainMux := http.NewServeMux()
	if cfg.AdminAddr == "" || cfg.AdminAddr == cfg.ListenAddr {
		mountAdmin(mainMux, met, dist, warmer, logger, cfg)
	}
	mainMux.Handle("/", appHandler)

	// Outermost middleware: structured access logging.
	mainHandler := logger.Middleware(mainMux)

	// Main server.
	srv := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mainHandler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if cfg.TLSEnabled && !cfg.TLSAuto {
		srv.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			},
		}
	}

	// Separate admin server if configured.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" && cfg.AdminAddr != cfg.ListenAddr {
		adminMux := http.NewServeMux()
		mountAdmin(adminMux, met, dist, warmer, logger, cfg)
		adminSrv = &http.Server{
			Addr:         cfg.AdminAddr,
			Handler:      adminMux,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
	}

	// HTTPS redirect server.
	var redirectSrv *http.Server
	if cfg.TLSEnabled && cfg.HTTPSRedirect {
		redirectSrv = &http.Server{
			Addr:         ":80",
			Handler:      middleware.HTTPSRedirect(""),
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 5 * time.Second,
		}
	}

	// Multi-tenant: start config poller and mount config push endpoint.
	var pollerCancel context.CancelFunc
	if mtHandler != nil {
		// Mount config push endpoint (control plane pushes to this).
		mainMux.HandleFunc("/edge/config", metrics.AdminAuth(cfg.AdminAPIKey, mtHandler.HandleConfigPush))

		// Start config poller.
		pollerCtx, cancel := context.WithCancel(context.Background())
		pollerCancel = cancel
		poller := handler.NewConfigPoller(cfg.ControlPlaneURL, cfg.AdminAPIKey, mtHandler, logger)
		go poller.Start(pollerCtx, cfg.ConfigPollInterval)
	}

	// Signal handling: SIGTERM/SIGINT = shutdown, SIGHUP = config reload.
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)

	// Start servers.
	if adminSrv != nil {
		go func() {
			logger.Info("admin server listening on " + cfg.AdminAddr)
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("admin server error: %v", err)
			}
		}()
	}

	if redirectSrv != nil {
		go func() {
			logger.Info("HTTPS redirect server listening on :80")
			if err := redirectSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("redirect server error", err)
			}
		}()
	}

	go func() {
		if cfg.ControlPlaneURL != "" {
			logger.Info("CDN edge server listening on " + cfg.ListenAddr + " (multi-tenant, control plane: " + cfg.ControlPlaneURL + ")")
		} else {
			logger.Info("CDN edge server listening on " + cfg.ListenAddr + ", origin: " + cfg.OriginURL)
		}
		met.SetReady(true)
		var err error
		if cfg.TLSEnabled && !cfg.TLSAuto {
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// SIGHUP handler for config reload (reloads log level, rate limits, peers).
	go func() {
		for range sighup {
			logger.Info("received SIGHUP, reloading configuration")
			// Re-read env vars for hot-reloadable settings.
			if lvl := os.Getenv("CDN_LOG_LEVEL"); lvl != "" {
				logger.SetLevel(lvl)
				logger.Info("log level changed to " + lvl)
			}
			if peers := os.Getenv("CDN_PURGE_PEERS"); peers != "" {
				dist.SetPeers(strings.Split(peers, ","))
				logger.Info("purge peers updated")
			}
		}
	}()

	<-done
	logger.Info("shutting down...")

	// Stop config poller if running.
	if pollerCancel != nil {
		pollerCancel()
	}

	// Mark not-ready first so load balancers stop sending traffic.
	met.SetReady(false)

	// Drain delay: wait for LB health checks to detect not-ready.
	if cfg.DrainDelay > 0 {
		logger.Info("drain delay: waiting for load balancers to stop routing")
		time.Sleep(cfg.DrainDelay)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if redirectSrv != nil {
		redirectSrv.Shutdown(ctx)
	}
	if adminSrv != nil {
		adminSrv.Shutdown(ctx)
	}
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	logger.Info("server stopped")
}

// mountAdmin registers admin endpoints onto the given mux.
func mountAdmin(mux *http.ServeMux, met *metrics.Handler, dist *purge.Distributor, warmer *handler.WarmHandler, logger *logging.Logger, cfg *config.Config) {
	// Liveness and readiness are unauthenticated.
	mux.HandleFunc("/livez", met.ServeLiveness)
	mux.HandleFunc("/readyz", met.ServeReadiness)
	mux.HandleFunc("/health", met.ServeLiveness)

	// Metrics (authenticated).
	mux.HandleFunc("/metrics", metrics.AdminAuth(cfg.AdminAPIKey, met.ServePrometheus))
	mux.HandleFunc("/metrics.json", metrics.AdminAuth(cfg.AdminAPIKey, met.ServeJSONMetrics))

	// Operational endpoints (authenticated).
	mux.HandleFunc("/purge", metrics.AdminAuth(cfg.AdminAPIKey, dist.ServePurge))
	mux.Handle("/warm", metrics.AdminAuth(cfg.AdminAPIKey, warmer.ServeHTTP))
	mux.HandleFunc("/loglevel", metrics.AdminAuth(cfg.AdminAPIKey, metrics.LogLevelHandler(logger)))
}
