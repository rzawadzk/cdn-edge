// Package store provides persistent storage for the control plane.
// Uses a file-backed JSON store for zero external dependencies.
// Can be swapped for PostgreSQL/MySQL by implementing the same interface.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rzawadzk/cdn-edge/tenant"
)

// Store manages tenants, domains, cache rules, and edge nodes.
type Store struct {
	mu   sync.RWMutex
	dir  string
	data *storeData
}

type storeData struct {
	Version    int64                        `json:"version"`
	Tenants    map[string]*tenant.Tenant    `json:"tenants"`
	Domains    map[string]*tenant.Domain    `json:"domains"`
	CacheRules map[string]*tenant.CacheRule `json:"cache_rules"`
	EdgeNodes  map[string]*tenant.EdgeNode  `json:"edge_nodes"`
}

// New opens or creates a store at the given directory.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	s := &Store{dir: dir}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path() string {
	return filepath.Join(s.dir, "store.json")
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path())
	if os.IsNotExist(err) {
		s.data = &storeData{
			Version:    0,
			Tenants:    make(map[string]*tenant.Tenant),
			Domains:    make(map[string]*tenant.Domain),
			CacheRules: make(map[string]*tenant.CacheRule),
			EdgeNodes:  make(map[string]*tenant.EdgeNode),
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read: %w", err)
	}
	var sd storeData
	if err := json.Unmarshal(data, &sd); err != nil {
		return fmt.Errorf("store: parse: %w", err)
	}
	s.data = &sd
	return nil
}

func (s *Store) save() error {
	s.data.Version++
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o644)
}

func genID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func genAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "cdnk_" + hex.EncodeToString(b)
}

// --- Tenants ---

func (s *Store) CreateTenant(name string) (*tenant.Tenant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := &tenant.Tenant{
		ID:        genID(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Active:    true,
		APIKey:    genAPIKey(),
	}
	s.data.Tenants[t.ID] = t
	return t, s.save()
}

func (s *Store) GetTenant(id string) (*tenant.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data.Tenants[id]
	if !ok {
		return nil, fmt.Errorf("tenant not found: %s", id)
	}
	return t, nil
}

func (s *Store) ListTenants() []*tenant.Tenant {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*tenant.Tenant
	for _, t := range s.data.Tenants {
		list = append(list, t)
	}
	return list
}

func (s *Store) DeleteTenant(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Tenants[id]; !ok {
		return fmt.Errorf("tenant not found: %s", id)
	}
	// Also delete all domains and rules belonging to this tenant.
	for did, d := range s.data.Domains {
		if d.TenantID == id {
			for rid, r := range s.data.CacheRules {
				if r.DomainID == did {
					delete(s.data.CacheRules, rid)
				}
			}
			delete(s.data.Domains, did)
		}
	}
	delete(s.data.Tenants, id)
	return s.save()
}

// GetTenantByAPIKey finds a tenant by their API key.
func (s *Store) GetTenantByAPIKey(key string) (*tenant.Tenant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.data.Tenants {
		if t.APIKey == key {
			return t, nil
		}
	}
	return nil, fmt.Errorf("invalid API key")
}

// --- Domains ---

func (s *Store) CreateDomain(d *tenant.Domain) (*tenant.Domain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify tenant exists.
	if _, ok := s.data.Tenants[d.TenantID]; !ok {
		return nil, fmt.Errorf("tenant not found: %s", d.TenantID)
	}
	// Check hostname uniqueness.
	for _, existing := range s.data.Domains {
		if existing.Hostname == d.Hostname {
			return nil, fmt.Errorf("hostname already registered: %s", d.Hostname)
		}
	}

	d.ID = genID()
	d.CreatedAt = time.Now().UTC()
	d.UpdatedAt = time.Now().UTC()
	if d.DefaultTTLSec == 0 {
		d.DefaultTTLSec = 300
	}
	if d.TLSMode == "" {
		d.TLSMode = "managed"
	}
	d.Active = true

	s.data.Domains[d.ID] = d
	return d, s.save()
}

func (s *Store) GetDomain(id string) (*tenant.Domain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.data.Domains[id]
	if !ok {
		return nil, fmt.Errorf("domain not found: %s", id)
	}
	return d, nil
}

func (s *Store) GetDomainByHostname(hostname string) (*tenant.Domain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.data.Domains {
		if d.Hostname == hostname {
			return d, nil
		}
	}
	return nil, fmt.Errorf("domain not found: %s", hostname)
}

func (s *Store) ListDomains(tenantID string) []*tenant.Domain {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*tenant.Domain
	for _, d := range s.data.Domains {
		if tenantID == "" || d.TenantID == tenantID {
			list = append(list, d)
		}
	}
	return list
}

func (s *Store) UpdateDomain(d *tenant.Domain) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Domains[d.ID]; !ok {
		return fmt.Errorf("domain not found: %s", d.ID)
	}
	d.UpdatedAt = time.Now().UTC()
	s.data.Domains[d.ID] = d
	return s.save()
}

func (s *Store) DeleteDomain(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Domains[id]; !ok {
		return fmt.Errorf("domain not found: %s", id)
	}
	// Delete associated cache rules.
	for rid, r := range s.data.CacheRules {
		if r.DomainID == id {
			delete(s.data.CacheRules, rid)
		}
	}
	delete(s.data.Domains, id)
	return s.save()
}

// --- Cache Rules ---

func (s *Store) CreateCacheRule(r *tenant.CacheRule) (*tenant.CacheRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Domains[r.DomainID]; !ok {
		return nil, fmt.Errorf("domain not found: %s", r.DomainID)
	}
	r.ID = genID()
	s.data.CacheRules[r.ID] = r
	return r, s.save()
}

func (s *Store) ListCacheRules(domainID string) []*tenant.CacheRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*tenant.CacheRule
	for _, r := range s.data.CacheRules {
		if r.DomainID == domainID {
			list = append(list, r)
		}
	}
	return list
}

func (s *Store) DeleteCacheRule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.CacheRules[id]; !ok {
		return fmt.Errorf("cache rule not found: %s", id)
	}
	delete(s.data.CacheRules, id)
	return s.save()
}

// --- Edge Nodes ---

func (s *Store) RegisterEdge(name, addr, region string) (*tenant.EdgeNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := &tenant.EdgeNode{
		ID:       genID(),
		Name:     name,
		Addr:     addr,
		Region:   region,
		LastSeen: time.Now().UTC(),
		Active:   true,
	}
	s.data.EdgeNodes[node.ID] = node
	return node, s.save()
}

func (s *Store) ListEdges() []*tenant.EdgeNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*tenant.EdgeNode
	for _, n := range s.data.EdgeNodes {
		list = append(list, n)
	}
	return list
}

func (s *Store) HeartbeatEdge(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.data.EdgeNodes[id]
	if !ok {
		return fmt.Errorf("edge not found: %s", id)
	}
	n.LastSeen = time.Now().UTC()
	return s.save()
}

func (s *Store) DeleteEdge(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.EdgeNodes, id)
	return s.save()
}

// --- Edge Config Snapshot ---

// BuildEdgeConfig generates the full configuration snapshot for edge servers.
func (s *Store) BuildEdgeConfig() *tenant.EdgeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	domains := make(map[string]*tenant.Domain)
	rules := make(map[string][]*tenant.CacheRule)

	for _, d := range s.data.Domains {
		if !d.Active {
			continue
		}
		// Verify tenant is active.
		t, ok := s.data.Tenants[d.TenantID]
		if !ok || !t.Active {
			continue
		}
		domains[d.Hostname] = d
	}

	for _, r := range s.data.CacheRules {
		rules[r.DomainID] = append(rules[r.DomainID], r)
	}

	return &tenant.EdgeConfig{
		Version:   s.data.Version,
		Timestamp: time.Now().UTC(),
		Domains:   domains,
		Rules:     rules,
	}
}

// Version returns the current store version.
func (s *Store) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Version
}
