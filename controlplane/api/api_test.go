package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rzawadzk/cdn-edge/controlplane/store"
	"github.com/rzawadzk/cdn-edge/tenant"
)

const testAdminKey = "test-admin-key"

func setup(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	configChanged := false
	srv := New(st, testAdminKey, func() { configChanged = true })
	_ = configChanged
	return srv, st
}

func doReq(handler http.Handler, method, path string, body any, apiKey string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(w.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
	}
	return v
}

// --- Auth tests ---

func TestAdminAuthRequired(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	// No key.
	w := doReq(h, "GET", "/api/v1/tenants", nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: got %d, want 401", w.Code)
	}

	// Wrong key.
	w = doReq(h, "GET", "/api/v1/tenants", nil, "wrong-key")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: got %d, want 401", w.Code)
	}

	// Correct key.
	w = doReq(h, "GET", "/api/v1/tenants", nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("correct key: got %d, want 200", w.Code)
	}
}

func TestTenantKeyAuth(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()

	ten, _ := st.CreateTenant("Acme")

	// Tenant key should work for domain endpoints.
	w := doReq(h, "GET", "/api/v1/domains", nil, ten.APIKey)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant key: got %d, want 200", w.Code)
	}

	// Tenant key should NOT work for admin endpoints.
	w = doReq(h, "GET", "/api/v1/tenants", nil, ten.APIKey)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("tenant key on admin: got %d, want 401", w.Code)
	}
}

func TestBearerTokenAuth(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/api/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("bearer auth: got %d, want 200", w.Code)
	}
}

// --- Tenant CRUD ---

func TestCreateTenant(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	w := doReq(h, "POST", "/api/v1/tenants", map[string]string{"name": "Acme"}, testAdminKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201, body=%s", w.Code, w.Body.String())
	}
	var ten tenant.Tenant
	json.NewDecoder(w.Body).Decode(&ten)
	if ten.Name != "Acme" || ten.ID == "" || ten.APIKey == "" {
		t.Fatalf("unexpected tenant: %+v", ten)
	}
}

func TestCreateTenantMissingName(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	w := doReq(h, "POST", "/api/v1/tenants", map[string]string{}, testAdminKey)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestGetTenant(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")

	w := doReq(h, "GET", "/api/v1/tenants/"+ten.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
	var got tenant.Tenant
	json.NewDecoder(w.Body).Decode(&got)
	if got.Name != "Acme" {
		t.Fatalf("name = %q", got.Name)
	}
}

func TestGetTenantNotFound(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()
	w := doReq(h, "GET", "/api/v1/tenants/nonexistent", nil, testAdminKey)
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", w.Code)
	}
}

func TestDeleteTenant(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")

	w := doReq(h, "DELETE", "/api/v1/tenants/"+ten.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	if len(st.ListTenants()) != 0 {
		t.Fatal("tenant not deleted")
	}
}

func TestTenantsMethodNotAllowed(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()
	w := doReq(h, "PUT", "/api/v1/tenants", nil, testAdminKey)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

// --- Domain CRUD ---

func TestCreateDomain(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")

	body := map[string]string{
		"tenant_id":  ten.ID,
		"hostname":   "www.acme.com",
		"origin_url": "https://origin.acme.com",
	}
	w := doReq(h, "POST", "/api/v1/domains", body, testAdminKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201, body=%s", w.Code, w.Body.String())
	}
}

func TestCreateDomainMissingFields(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()
	w := doReq(h, "POST", "/api/v1/domains", map[string]string{"hostname": "x.com"}, testAdminKey)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestListDomains(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "a.com", OriginURL: "https://a.com"})
	st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "b.com", OriginURL: "https://b.com"})

	w := doReq(h, "GET", "/api/v1/domains", nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var domains []*tenant.Domain
	json.NewDecoder(w.Body).Decode(&domains)
	if len(domains) != 2 {
		t.Fatalf("domains = %d, want 2", len(domains))
	}
}

func TestListDomainsFilterByTenant(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	t1, _ := st.CreateTenant("A")
	t2, _ := st.CreateTenant("B")
	st.CreateDomain(&tenant.Domain{TenantID: t1.ID, Hostname: "a.com", OriginURL: "https://a.com"})
	st.CreateDomain(&tenant.Domain{TenantID: t2.ID, Hostname: "b.com", OriginURL: "https://b.com"})

	w := doReq(h, "GET", "/api/v1/domains?tenant_id="+t1.ID, nil, testAdminKey)
	var domains []*tenant.Domain
	json.NewDecoder(w.Body).Decode(&domains)
	if len(domains) != 1 {
		t.Fatalf("filtered domains = %d, want 1", len(domains))
	}
}

func TestUpdateDomain(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	dom, _ := st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://old.acme.com"})

	body := map[string]any{
		"origin_url": "https://new.acme.com",
		"hostname":   "acme.com",
		"tenant_id":  ten.ID,
	}
	w := doReq(h, "PUT", "/api/v1/domains/"+dom.ID, body, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200, body=%s", w.Code, w.Body.String())
	}

	got, _ := st.GetDomain(dom.ID)
	if got.OriginURL != "https://new.acme.com" {
		t.Fatalf("origin = %q", got.OriginURL)
	}
}

func TestDeleteDomain(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	dom, _ := st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})

	w := doReq(h, "DELETE", "/api/v1/domains/"+dom.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if len(st.ListDomains("")) != 0 {
		t.Fatal("domain not deleted")
	}
}

// --- Cache Rule CRUD ---

func TestCreateAndListCacheRules(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	dom, _ := st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})

	body := map[string]any{
		"domain_id": dom.ID,
		"path_glob": "/static/*",
		"ttl_sec":   3600,
	}
	w := doReq(h, "POST", "/api/v1/rules", body, testAdminKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("create rule: got %d, body=%s", w.Code, w.Body.String())
	}

	w = doReq(h, "GET", "/api/v1/rules?domain_id="+dom.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("list rules: got %d", w.Code)
	}
	var rules []*tenant.CacheRule
	json.NewDecoder(w.Body).Decode(&rules)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
}

func TestDeleteCacheRule(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	dom, _ := st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})
	rule, _ := st.CreateCacheRule(&tenant.CacheRule{DomainID: dom.ID, PathGlob: "/*"})

	w := doReq(h, "DELETE", "/api/v1/rules/"+rule.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
}

// --- Edge CRUD ---

func TestCreateAndListEdges(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	body := map[string]string{"name": "edge-1", "addr": "https://e1:9090", "region": "us-east"}
	w := doReq(h, "POST", "/api/v1/edges", body, testAdminKey)
	if w.Code != http.StatusCreated {
		t.Fatalf("create edge: got %d, body=%s", w.Code, w.Body.String())
	}

	w = doReq(h, "GET", "/api/v1/edges", nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("list edges: got %d", w.Code)
	}
	var edges []*tenant.EdgeNode
	json.NewDecoder(w.Body).Decode(&edges)
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
}

func TestDeleteEdge(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	node, _ := st.RegisterEdge("e1", "https://e1:9090", "us-east")

	w := doReq(h, "DELETE", "/api/v1/edges/"+node.ID, nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
}

// --- Config endpoint ---

func TestGetConfig(t *testing.T) {
	srv, st := setup(t)
	h := srv.Handler()
	ten, _ := st.CreateTenant("Acme")
	st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})

	// Config endpoint is unauthenticated.
	w := doReq(h, "GET", "/api/v1/config", nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var cfg tenant.EdgeConfig
	json.NewDecoder(w.Body).Decode(&cfg)
	if len(cfg.Domains) != 1 {
		t.Fatalf("config domains = %d, want 1", len(cfg.Domains))
	}
}

func TestConfigMethodNotAllowed(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()
	w := doReq(h, "POST", "/api/v1/config", nil, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

// --- Wired handler delegation ---

func TestAnalyticsHandlerDelegation(t *testing.T) {
	srv, _ := setup(t)
	ingestCalled := false
	queryCalled := false
	srv.SetAnalyticsHandlers(
		func(w http.ResponseWriter, r *http.Request) { ingestCalled = true; w.WriteHeader(200) },
		func(w http.ResponseWriter, r *http.Request) { queryCalled = true; w.WriteHeader(200) },
	)
	h := srv.Handler()

	doReq(h, "POST", "/api/v1/analytics/ingest", nil, "")
	if !ingestCalled {
		t.Fatal("ingest handler not called")
	}

	doReq(h, "GET", "/api/v1/analytics/query", nil, testAdminKey)
	if !queryCalled {
		t.Fatal("query handler not called")
	}
}

func TestPurgeHandlerDelegation(t *testing.T) {
	srv, _ := setup(t)
	purgeCalled := false
	srv.SetPurgeHandler(func(w http.ResponseWriter, r *http.Request) {
		purgeCalled = true
		w.WriteHeader(200)
	})
	h := srv.Handler()

	doReq(h, "POST", "/api/v1/purge", nil, testAdminKey)
	if !purgeCalled {
		t.Fatal("purge handler not called")
	}
}

func TestCertsHandlerDelegation(t *testing.T) {
	srv, _ := setup(t)
	listCalled := false
	singleCalled := false
	srv.SetCertsHandlers(
		func(w http.ResponseWriter, r *http.Request) { listCalled = true; w.WriteHeader(200) },
		func(w http.ResponseWriter, r *http.Request) { singleCalled = true; w.WriteHeader(200) },
	)
	h := srv.Handler()

	doReq(h, "GET", "/api/v1/certs", nil, testAdminKey)
	if !listCalled {
		t.Fatal("certs list handler not called")
	}

	doReq(h, "GET", "/api/v1/certs/example.com", nil, testAdminKey)
	if !singleCalled {
		t.Fatal("cert single handler not called")
	}
}

// --- Fallback when handlers not wired ---

func TestAnalyticsFallbackWhenNotWired(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	w := doReq(h, "POST", "/api/v1/analytics/ingest", nil, "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("ingest fallback: got %d, want 202", w.Code)
	}

	w = doReq(h, "GET", "/api/v1/analytics/query", nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("query fallback: got %d, want 200", w.Code)
	}
}

func TestPurgeFallbackWhenNotWired(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	w := doReq(h, "POST", "/api/v1/purge", nil, testAdminKey)
	if w.Code != http.StatusOK {
		t.Fatalf("purge fallback: got %d, want 200", w.Code)
	}
}

func TestCertsFallbackWhenNotWired(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	w := doReq(h, "GET", "/api/v1/certs", nil, testAdminKey)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("certs fallback: got %d, want 501", w.Code)
	}
}

// --- extractAPIKey helper ---

func TestExtractAPIKeyFromQueryParam(t *testing.T) {
	srv, _ := setup(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/api/v1/tenants?api_key="+testAdminKey, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("query param auth: got %d, want 200", w.Code)
	}
}
