// Control plane server for the CDN.
// Provides REST API, dashboard, analytics, certificate management,
// and config push/purge fanout to registered edge servers.
//
// Usage:
//   go run ./controlplane/cmd -admin-key YOUR_SECRET
//
// Environment variables (override flags):
//   CP_LISTEN        — listen address (default ":9090")
//   CP_ADMIN_KEY     — admin API key (required)
//   CP_DATA_DIR      — persistent data directory (default "./cp-data")
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rzawadzk/cdn-edge/controlplane/analytics"
	"github.com/rzawadzk/cdn-edge/controlplane/api"
	"github.com/rzawadzk/cdn-edge/controlplane/certs"
	"github.com/rzawadzk/cdn-edge/controlplane/dashboard"
	"github.com/rzawadzk/cdn-edge/controlplane/store"
	cpSync "github.com/rzawadzk/cdn-edge/controlplane/sync"
)

func main() {
	listen := flag.String("listen", ":9090", "control plane listen address")
	adminKey := flag.String("admin-key", "", "admin API key (required)")
	dataDir := flag.String("data-dir", "./cp-data", "persistent data directory")
	flag.Parse()

	// Env overrides.
	if v := os.Getenv("CP_LISTEN"); v != "" {
		*listen = v
	}
	if v := os.Getenv("CP_ADMIN_KEY"); v != "" {
		*adminKey = v
	}
	if v := os.Getenv("CP_DATA_DIR"); v != "" {
		*dataDir = v
	}

	if *adminKey == "" {
		log.Fatal("admin API key is required (--admin-key or CP_ADMIN_KEY)")
	}

	// Initialize store.
	st, err := store.New(*dataDir)
	if err != nil {
		log.Fatalf("store init: %v", err)
	}

	// Initialize syncer for pushing config/purge to edges.
	syncer := cpSync.New(st, *adminKey)

	// Initialize analytics collector.
	collector, err := analytics.NewCollector(*dataDir)
	if err != nil {
		log.Fatalf("analytics init: %v", err)
	}

	// Initialize certificate manager.
	certMgr, err := certs.NewManager(*dataDir)
	if err != nil {
		log.Fatalf("certs init: %v", err)
	}

	// Initialize API server.
	apiServer := api.New(st, *adminKey, syncer.PushConfig)

	// Wire real handlers into API.
	apiServer.SetAnalyticsHandlers(collector.HandleIngest, collector.HandleQuery)
	apiServer.SetPurgeHandler(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprintf(w, `{"error":"method not allowed"}`)
			return
		}
		if err := syncer.PurgeAll(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "purged"})
	})
	apiServer.SetCertsHandlers(certMgr.HandleCerts, certMgr.HandleCert)

	// Assemble the top-level mux.
	mux := http.NewServeMux()

	// Health endpoints (unauthenticated).
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	// API routes.
	mux.Handle("/api/", apiServer.Handler())

	// Dashboard (serves at root).
	mux.Handle("/", dashboard.Handler())

	srv := &http.Server{
		Addr:         *listen,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown.
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("CDN control plane listening on %s", *listen)
		log.Printf("  Dashboard: http://localhost%s/", *listen)
		log.Printf("  API:       http://localhost%s/api/v1/", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	_ = certMgr // keep reference alive for renewal goroutine

	<-done
	log.Println("shutting down control plane...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	log.Println("control plane stopped")
}
