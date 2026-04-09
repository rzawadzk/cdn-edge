package purge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
)

// Distributor fans out purge requests to a list of peer edge nodes.
type Distributor struct {
	mu     sync.RWMutex
	peers  []string // base URLs of peer nodes, e.g. "http://edge-2:9090"
	cache  *cache.Cache
	apiKey string
	client *http.Client
}

// New creates a purge distributor.
func New(c *cache.Cache, apiKey string) *Distributor {
	return &Distributor{
		cache:  c,
		apiKey: apiKey,
		client: &http.Client{Timeout: 5 * time.Second},
	}
}

// SetPeers updates the list of peer nodes.
func (d *Distributor) SetPeers(peers []string) {
	d.mu.Lock()
	d.peers = peers
	d.mu.Unlock()
}

// Peers returns the current peer list.
func (d *Distributor) Peers() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, len(d.peers))
	copy(out, d.peers)
	return out
}

type purgeRequest struct {
	Distributed bool `json:"distributed"` // false = local-only, prevents infinite fanout
}

type purgeResult struct {
	Peer   string `json:"peer"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ServePurge handles both local and distributed purge.
// POST /purge — purges locally and fans out to peers.
// POST /purge with {"distributed":false} — local only (used by peers to avoid loops).
func (d *Distributor) ServePurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse body to check if this is a distributed or local-only request.
	var req purgeRequest
	req.Distributed = true // default: fan out
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&req) // ignore errors — default is distributed
	}

	// Always purge locally.
	d.cache.Purge()

	if !req.Distributed {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"purged","scope":"local"}`))
		return
	}

	// Fan out to peers.
	peers := d.Peers()
	results := make([]purgeResult, len(peers))
	var wg sync.WaitGroup

	for i, peer := range peers {
		wg.Add(1)
		go func(idx int, peerURL string) {
			defer wg.Done()
			results[idx] = d.purgePeer(peerURL)
		}(i, peer)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "purged",
		"scope":   "distributed",
		"peers":   len(peers),
		"results": results,
	})
}

func (d *Distributor) purgePeer(peerURL string) purgeResult {
	body, _ := json.Marshal(purgeRequest{Distributed: false})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, peerURL+"/purge", bytes.NewReader(body))
	if err != nil {
		return purgeResult{Peer: peerURL, Status: "error", Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if d.apiKey != "" {
		req.Header.Set("X-API-Key", d.apiKey)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return purgeResult{Peer: peerURL, Status: "error", Error: err.Error()}
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return purgeResult{Peer: peerURL, Status: "error", Error: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return purgeResult{Peer: peerURL, Status: "ok"}
}
