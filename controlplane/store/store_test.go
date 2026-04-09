package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rzawadzk/cdn-edge/tenant"
)

func tmpStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// --- Tenant tests ---

func TestCreateAndGetTenant(t *testing.T) {
	s := tmpStore(t)
	ten, err := s.CreateTenant("Acme Inc")
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if ten.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if ten.Name != "Acme Inc" {
		t.Fatalf("name = %q, want %q", ten.Name, "Acme Inc")
	}
	if !ten.Active {
		t.Fatal("expected active tenant")
	}
	if ten.APIKey == "" || len(ten.APIKey) < 10 {
		t.Fatalf("expected valid API key, got %q", ten.APIKey)
	}

	got, err := s.GetTenant(ten.ID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Name != "Acme Inc" {
		t.Fatalf("got name %q", got.Name)
	}
}

func TestGetTenantNotFound(t *testing.T) {
	s := tmpStore(t)
	_, err := s.GetTenant("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent tenant")
	}
}

func TestListTenants(t *testing.T) {
	s := tmpStore(t)
	s.CreateTenant("A")
	s.CreateTenant("B")
	list := s.ListTenants()
	if len(list) != 2 {
		t.Fatalf("ListTenants = %d, want 2", len(list))
	}
}

func TestDeleteTenantCascadesDomainAndRules(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})
	s.CreateCacheRule(&tenant.CacheRule{DomainID: dom.ID, PathGlob: "/api/*", TTLSec: 60})

	if err := s.DeleteTenant(ten.ID); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}

	if len(s.ListTenants()) != 0 {
		t.Fatal("tenant not deleted")
	}
	if len(s.ListDomains("")) != 0 {
		t.Fatal("domain not cascade-deleted")
	}
	if len(s.ListCacheRules(dom.ID)) != 0 {
		t.Fatal("cache rule not cascade-deleted")
	}
}

func TestDeleteTenantNotFound(t *testing.T) {
	s := tmpStore(t)
	if err := s.DeleteTenant("nonexistent"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetTenantByAPIKey(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")

	got, err := s.GetTenantByAPIKey(ten.APIKey)
	if err != nil {
		t.Fatalf("GetTenantByAPIKey: %v", err)
	}
	if got.ID != ten.ID {
		t.Fatalf("got ID %q, want %q", got.ID, ten.ID)
	}

	_, err = s.GetTenantByAPIKey("invalid-key")
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

// --- Domain tests ---

func TestCreateDomainDefaults(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")

	dom, err := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "www.acme.com",
		OriginURL: "https://origin.acme.com",
	})
	if err != nil {
		t.Fatalf("CreateDomain: %v", err)
	}
	if dom.DefaultTTLSec != 300 {
		t.Fatalf("default TTL = %d, want 300", dom.DefaultTTLSec)
	}
	if dom.TLSMode != "managed" {
		t.Fatalf("TLS mode = %q, want %q", dom.TLSMode, "managed")
	}
	if !dom.Active {
		t.Fatal("expected active domain")
	}
}

func TestCreateDomainBadTenant(t *testing.T) {
	s := tmpStore(t)
	_, err := s.CreateDomain(&tenant.Domain{
		TenantID:  "nonexistent",
		Hostname:  "foo.com",
		OriginURL: "https://origin.foo.com",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent tenant")
	}
}

func TestCreateDomainDuplicateHostname(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "dup.com",
		OriginURL: "https://origin.dup.com",
	})
	_, err := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "dup.com",
		OriginURL: "https://origin2.dup.com",
	})
	if err == nil {
		t.Fatal("expected error for duplicate hostname")
	}
}

func TestGetDomain(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "www.acme.com",
		OriginURL: "https://origin.acme.com",
	})

	got, err := s.GetDomain(dom.ID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	if got.Hostname != "www.acme.com" {
		t.Fatalf("hostname = %q", got.Hostname)
	}

	_, err = s.GetDomain("nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGetDomainByHostname(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "www.acme.com",
		OriginURL: "https://origin.acme.com",
	})

	got, err := s.GetDomainByHostname("www.acme.com")
	if err != nil {
		t.Fatalf("GetDomainByHostname: %v", err)
	}
	if got.OriginURL != "https://origin.acme.com" {
		t.Fatalf("origin = %q", got.OriginURL)
	}

	_, err = s.GetDomainByHostname("nope.com")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListDomainsFilterByTenant(t *testing.T) {
	s := tmpStore(t)
	t1, _ := s.CreateTenant("A")
	t2, _ := s.CreateTenant("B")
	s.CreateDomain(&tenant.Domain{TenantID: t1.ID, Hostname: "a.com", OriginURL: "https://a.com"})
	s.CreateDomain(&tenant.Domain{TenantID: t2.ID, Hostname: "b.com", OriginURL: "https://b.com"})

	all := s.ListDomains("")
	if len(all) != 2 {
		t.Fatalf("all domains = %d, want 2", len(all))
	}

	t1Domains := s.ListDomains(t1.ID)
	if len(t1Domains) != 1 {
		t.Fatalf("t1 domains = %d, want 1", len(t1Domains))
	}
}

func TestUpdateDomain(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "www.acme.com",
		OriginURL: "https://origin.acme.com",
	})

	dom.OriginURL = "https://new-origin.acme.com"
	if err := s.UpdateDomain(dom); err != nil {
		t.Fatalf("UpdateDomain: %v", err)
	}

	got, _ := s.GetDomain(dom.ID)
	if got.OriginURL != "https://new-origin.acme.com" {
		t.Fatalf("origin not updated: %q", got.OriginURL)
	}
}

func TestUpdateDomainNotFound(t *testing.T) {
	s := tmpStore(t)
	err := s.UpdateDomain(&tenant.Domain{ID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteDomainCascadesRules(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})
	s.CreateCacheRule(&tenant.CacheRule{DomainID: dom.ID, PathGlob: "/api/*", TTLSec: 60})

	if err := s.DeleteDomain(dom.ID); err != nil {
		t.Fatalf("DeleteDomain: %v", err)
	}
	if len(s.ListCacheRules(dom.ID)) != 0 {
		t.Fatal("cache rules not cascade-deleted")
	}
}

func TestDeleteDomainNotFound(t *testing.T) {
	s := tmpStore(t)
	if err := s.DeleteDomain("nonexistent"); err == nil {
		t.Fatal("expected error")
	}
}

// --- Cache Rule tests ---

func TestCreateCacheRule(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})

	rule, err := s.CreateCacheRule(&tenant.CacheRule{
		DomainID: dom.ID,
		PathGlob: "/static/*",
		TTLSec:   3600,
	})
	if err != nil {
		t.Fatalf("CreateCacheRule: %v", err)
	}
	if rule.ID == "" {
		t.Fatal("expected non-empty ID")
	}

	rules := s.ListCacheRules(dom.ID)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
}

func TestCreateCacheRuleBadDomain(t *testing.T) {
	s := tmpStore(t)
	_, err := s.CreateCacheRule(&tenant.CacheRule{
		DomainID: "nonexistent",
		PathGlob: "/*",
		TTLSec:   60,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent domain")
	}
}

func TestDeleteCacheRule(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})
	rule, _ := s.CreateCacheRule(&tenant.CacheRule{DomainID: dom.ID, PathGlob: "/*"})

	if err := s.DeleteCacheRule(rule.ID); err != nil {
		t.Fatalf("DeleteCacheRule: %v", err)
	}
	if len(s.ListCacheRules(dom.ID)) != 0 {
		t.Fatal("rule not deleted")
	}
}

func TestDeleteCacheRuleNotFound(t *testing.T) {
	s := tmpStore(t)
	if err := s.DeleteCacheRule("nonexistent"); err == nil {
		t.Fatal("expected error")
	}
}

// --- Edge Node tests ---

func TestRegisterAndListEdge(t *testing.T) {
	s := tmpStore(t)
	node, err := s.RegisterEdge("edge-fra-1", "https://edge-fra.internal:9090", "eu-west")
	if err != nil {
		t.Fatalf("RegisterEdge: %v", err)
	}
	if node.ID == "" || node.Name != "edge-fra-1" || !node.Active {
		t.Fatalf("unexpected node: %+v", node)
	}

	edges := s.ListEdges()
	if len(edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(edges))
	}
}

func TestHeartbeatEdge(t *testing.T) {
	s := tmpStore(t)
	node, _ := s.RegisterEdge("e1", "https://e1:9090", "us-east")
	firstSeen := node.LastSeen

	if err := s.HeartbeatEdge(node.ID); err != nil {
		t.Fatalf("HeartbeatEdge: %v", err)
	}

	edges := s.ListEdges()
	for _, e := range edges {
		if e.ID == node.ID && !e.LastSeen.After(firstSeen) && e.LastSeen != firstSeen {
			t.Fatal("LastSeen not updated")
		}
	}
}

func TestHeartbeatEdgeNotFound(t *testing.T) {
	s := tmpStore(t)
	if err := s.HeartbeatEdge("nonexistent"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteEdge(t *testing.T) {
	s := tmpStore(t)
	node, _ := s.RegisterEdge("e1", "https://e1:9090", "us-east")
	if err := s.DeleteEdge(node.ID); err != nil {
		t.Fatalf("DeleteEdge: %v", err)
	}
	if len(s.ListEdges()) != 0 {
		t.Fatal("edge not deleted")
	}
}

// --- BuildEdgeConfig tests ---

func TestBuildEdgeConfigOnlyActiveDomainsWithActiveTenants(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Active")
	s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "active.com",
		OriginURL: "https://origin.active.com",
	})

	cfg := s.BuildEdgeConfig()
	if len(cfg.Domains) != 1 {
		t.Fatalf("domains = %d, want 1", len(cfg.Domains))
	}
	if _, ok := cfg.Domains["active.com"]; !ok {
		t.Fatal("expected active.com in config")
	}
}

func TestBuildEdgeConfigExcludesInactiveDomain(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})
	// Deactivate domain.
	dom.Active = false
	s.UpdateDomain(dom)

	cfg := s.BuildEdgeConfig()
	if len(cfg.Domains) != 0 {
		t.Fatalf("expected 0 domains, got %d", len(cfg.Domains))
	}
}

func TestBuildEdgeConfigIncludesRules(t *testing.T) {
	s := tmpStore(t)
	ten, _ := s.CreateTenant("Acme")
	dom, _ := s.CreateDomain(&tenant.Domain{
		TenantID:  ten.ID,
		Hostname:  "acme.com",
		OriginURL: "https://origin.acme.com",
	})
	s.CreateCacheRule(&tenant.CacheRule{DomainID: dom.ID, PathGlob: "/static/*", TTLSec: 3600})

	cfg := s.BuildEdgeConfig()
	rules := cfg.Rules[dom.ID]
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
}

// --- Version tests ---

func TestVersionIncrementsOnMutation(t *testing.T) {
	s := tmpStore(t)
	v0 := s.Version()
	s.CreateTenant("A")
	v1 := s.Version()
	if v1 <= v0 {
		t.Fatalf("version did not increment: %d -> %d", v0, v1)
	}
}

// --- Persistence tests ---

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.CreateTenant("Persistent")

	// Reopen store from same directory.
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := s2.ListTenants()
	if len(list) != 1 || list[0].Name != "Persistent" {
		t.Fatalf("data not persisted: %+v", list)
	}
}

func TestNewStoreCorruptedFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "store.json"), []byte("not json"), 0o644)
	_, err := New(dir)
	if err == nil {
		t.Fatal("expected error for corrupted file")
	}
}
