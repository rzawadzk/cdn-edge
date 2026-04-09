package purge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rzawadzk/cdn-edge/cache"
)

func TestLocalPurge(t *testing.T) {
	c, _ := cache.New(100, 0, "", 0)
	c.Put("key", &cache.Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})

	d := New(c, "")

	body := strings.NewReader(`{"distributed":false}`)
	w := httptest.NewRecorder()
	d.ServePurge(w, httptest.NewRequest("POST", "/purge", body))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if c.Len() != 0 {
		t.Errorf("cache len = %d after purge, want 0", c.Len())
	}
}

func TestDistributedPurge(t *testing.T) {
	// Set up a fake peer that tracks if it was called.
	peerCalled := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalled = true
		if r.Method != http.MethodPost {
			t.Errorf("peer got method %s, want POST", r.Method)
		}
		// Verify it's a local-only purge (no infinite fanout).
		var req struct {
			Distributed bool `json:"distributed"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Distributed {
			t.Error("peer should receive distributed=false")
		}
		w.WriteHeader(200)
	}))
	defer peer.Close()

	c, _ := cache.New(100, 0, "", 0)
	c.Put("key", &cache.Entry{Body: []byte("x"), StoredAt: time.Now(), TTL: time.Hour})

	d := New(c, "")
	d.SetPeers([]string{peer.URL})

	w := httptest.NewRecorder()
	d.ServePurge(w, httptest.NewRequest("POST", "/purge", nil))

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !peerCalled {
		t.Error("peer was not called during distributed purge")
	}
	if c.Len() != 0 {
		t.Error("local cache not purged")
	}
}

func TestPurgeMethodNotAllowed(t *testing.T) {
	c, _ := cache.New(100, 0, "", 0)
	d := New(c, "")

	w := httptest.NewRecorder()
	d.ServePurge(w, httptest.NewRequest("GET", "/purge", nil))

	if w.Code != 405 {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestSetPeers(t *testing.T) {
	c, _ := cache.New(100, 0, "", 0)
	d := New(c, "")

	d.SetPeers([]string{"http://a", "http://b"})
	peers := d.Peers()
	if len(peers) != 2 {
		t.Errorf("peers = %d, want 2", len(peers))
	}
}

func TestPurgeWithAPIKey(t *testing.T) {
	peerCalled := false
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerCalled = true
		if key := r.Header.Get("X-API-Key"); key != "secret" {
			t.Errorf("peer got API key %q, want %q", key, "secret")
		}
		w.WriteHeader(200)
	}))
	defer peer.Close()

	c, _ := cache.New(100, 0, "", 0)
	d := New(c, "secret")
	d.SetPeers([]string{peer.URL})

	w := httptest.NewRecorder()
	d.ServePurge(w, httptest.NewRequest("POST", "/purge", nil))

	if !peerCalled {
		t.Error("peer not called")
	}
}
