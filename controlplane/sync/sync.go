// Package sync pushes configuration and purge commands to edge servers.
package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/rzawadzk/cdn-edge/controlplane/store"
	"github.com/rzawadzk/cdn-edge/tenant"
)

// Syncer pushes config updates and purge requests to all registered edges.
type Syncer struct {
	store    *store.Store
	adminKey string
	client   *http.Client
}

// New creates a Syncer.
func New(s *store.Store, adminKey string) *Syncer {
	return &Syncer{
		store:    s,
		adminKey: adminKey,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// PushConfig sends the current config snapshot to all active edges.
func (s *Syncer) PushConfig() {
	edges := s.store.ListEdges()
	config := s.store.BuildEdgeConfig()

	data, err := json.Marshal(config)
	if err != nil {
		log.Printf("sync: marshal config: %v", err)
		return
	}

	var wg sync.WaitGroup
	for _, edge := range edges {
		if !edge.Active {
			continue
		}
		wg.Add(1)
		go func(e *tenant.EdgeNode) {
			defer wg.Done()
			if err := s.pushToEdge(e.Addr, "/edge/config", data); err != nil {
				log.Printf("sync: push config to %s: %v", e.Name, err)
			}
		}(edge)
	}
	wg.Wait()
}

// PurgeAll sends a purge command to all active edges.
func (s *Syncer) PurgeAll() error {
	edges := s.store.ListEdges()
	body, _ := json.Marshal(map[string]bool{"distributed": false})

	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errors []string

	for _, edge := range edges {
		if !edge.Active {
			continue
		}
		wg.Add(1)
		go func(e *tenant.EdgeNode) {
			defer wg.Done()
			if err := s.pushToEdge(e.Addr, "/purge", body); err != nil {
				errMu.Lock()
				errors = append(errors, fmt.Sprintf("%s: %v", e.Name, err))
				errMu.Unlock()
			}
		}(edge)
	}
	wg.Wait()

	if len(errors) > 0 {
		return fmt.Errorf("purge failed on: %v", errors)
	}
	return nil
}

func (s *Syncer) pushToEdge(baseAddr, path string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseAddr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", s.adminKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
