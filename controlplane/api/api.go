// Package api provides the REST API for the CDN control plane.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rzawadzk/cdn-edge/controlplane/store"
	"github.com/rzawadzk/cdn-edge/tenant"
)

// Server is the control plane API server.
type Server struct {
	store     *store.Store
	adminKey  string
	onConfigChange func() // called after any config mutation

	// Wired externally for real implementations.
	analyticsIngest http.HandlerFunc
	analyticsQuery  http.HandlerFunc
	purgeHandler    http.HandlerFunc
	certsHandler    http.HandlerFunc
	certHandler     http.HandlerFunc
}

// New creates an API server.
func New(s *store.Store, adminKey string, onConfigChange func()) *Server {
	return &Server{store: s, adminKey: adminKey, onConfigChange: onConfigChange}
}

// SetAnalyticsHandlers wires in the analytics collector's HTTP handlers.
func (s *Server) SetAnalyticsHandlers(ingest, query http.HandlerFunc) {
	s.analyticsIngest = ingest
	s.analyticsQuery = query
}

// SetPurgeHandler wires in the purge handler that fans out to edges.
func (s *Server) SetPurgeHandler(h http.HandlerFunc) {
	s.purgeHandler = h
}

// SetCertsHandlers wires in certificate management HTTP handlers.
func (s *Server) SetCertsHandlers(listCreate, single http.HandlerFunc) {
	s.certsHandler = listCreate
	s.certHandler = single
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Admin endpoints (protected by admin key).
	mux.HandleFunc("/api/v1/tenants", s.adminAuth(s.handleTenants))
	mux.HandleFunc("/api/v1/tenants/", s.adminAuth(s.handleTenant))

	// Domain management (admin or tenant key).
	mux.HandleFunc("/api/v1/domains", s.auth(s.handleDomains))
	mux.HandleFunc("/api/v1/domains/", s.auth(s.handleDomain))

	// Cache rules.
	mux.HandleFunc("/api/v1/rules", s.auth(s.handleRules))
	mux.HandleFunc("/api/v1/rules/", s.auth(s.handleRule))

	// Edge management (admin only).
	mux.HandleFunc("/api/v1/edges", s.adminAuth(s.handleEdges))
	mux.HandleFunc("/api/v1/edges/", s.adminAuth(s.handleEdge))

	// Config snapshot (edges poll this).
	mux.HandleFunc("/api/v1/config", s.handleConfig)

	// Customer-facing purge.
	mux.HandleFunc("/api/v1/purge", s.auth(s.handlePurge))

	// Analytics.
	mux.HandleFunc("/api/v1/analytics/ingest", s.handleAnalyticsIngest)
	mux.HandleFunc("/api/v1/analytics/query", s.auth(s.handleAnalyticsQuery))

	// Certificates.
	mux.HandleFunc("/api/v1/certs", s.adminAuth(s.handleCerts))
	mux.HandleFunc("/api/v1/certs/", s.adminAuth(s.handleCert))

	return mux
}

// --- Auth middleware ---

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := extractAPIKey(r)
		if key != s.adminKey {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := extractAPIKey(r)
		if key == s.adminKey {
			next(w, r)
			return
		}
		// Try tenant key.
		_, err := s.store.GetTenantByAPIKey(key)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func extractAPIKey(r *http.Request) string {
	if key := r.Header.Get("X-API-Key"); key != "" {
		return key
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return r.URL.Query().Get("api_key")
}

// --- Tenant handlers ---

func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.ListTenants())
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		t, err := s.store.CreateTenant(body.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, t)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleTenant(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	switch r.Method {
	case http.MethodGet:
		t, err := s.store.GetTenant(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, t)
	case http.MethodDelete:
		if err := s.store.DeleteTenant(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		methodNotAllowed(w)
	}
}

// --- Domain handlers ---

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tenantID := r.URL.Query().Get("tenant_id")
		writeJSON(w, http.StatusOK, s.store.ListDomains(tenantID))
	case http.MethodPost:
		var d tenant.Domain
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if d.Hostname == "" || d.OriginURL == "" || d.TenantID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "hostname, origin_url, and tenant_id are required"})
			return
		}
		created, err := s.store.CreateDomain(&d)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleDomain(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	switch r.Method {
	case http.MethodGet:
		d, err := s.store.GetDomain(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, d)
	case http.MethodPut:
		var d tenant.Domain
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		d.ID = id
		if err := s.store.UpdateDomain(&d); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusOK, d)
	case http.MethodDelete:
		if err := s.store.DeleteDomain(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		methodNotAllowed(w)
	}
}

// --- Cache rule handlers ---

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		domainID := r.URL.Query().Get("domain_id")
		writeJSON(w, http.StatusOK, s.store.ListCacheRules(domainID))
	case http.MethodPost:
		var rule tenant.CacheRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		created, err := s.store.CreateCacheRule(&rule)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleRule(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteCacheRule(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		s.notifyConfigChange()
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		methodNotAllowed(w)
	}
}

// --- Edge handlers ---

func (s *Server) handleEdges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.store.ListEdges())
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Addr   string `json:"addr"`
			Region string `json:"region"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		node, err := s.store.RegisterEdge(body.Name, body.Addr, body.Region)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, node)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleEdge(w http.ResponseWriter, r *http.Request) {
	id := lastPathSegment(r.URL.Path)
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteEdge(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	default:
		methodNotAllowed(w)
	}
}

// --- Config snapshot (edges poll this) ---

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.store.BuildEdgeConfig())
}

// --- Purge (customer-facing, fans out to edges) ---

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	if s.purgeHandler != nil {
		s.purgeHandler(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "purge_queued"})
}

// --- Analytics ---

func (s *Server) handleAnalyticsIngest(w http.ResponseWriter, r *http.Request) {
	if s.analyticsIngest != nil {
		s.analyticsIngest(w, r)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleAnalyticsQuery(w http.ResponseWriter, r *http.Request) {
	if s.analyticsQuery != nil {
		s.analyticsQuery(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "not_implemented"})
}

// --- Certificates ---

func (s *Server) handleCerts(w http.ResponseWriter, r *http.Request) {
	if s.certsHandler != nil {
		s.certsHandler(w, r)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "certificate manager not configured"})
}

func (s *Server) handleCert(w http.ResponseWriter, r *http.Request) {
	if s.certHandler != nil {
		s.certHandler(w, r)
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "certificate manager not configured"})
}

func (s *Server) notifyConfigChange() {
	if s.onConfigChange != nil {
		go s.onConfigChange()
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func lastPathSegment(path string) string {
	path = strings.TrimRight(path, "/")
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return path
	}
	return path[i+1:]
}
