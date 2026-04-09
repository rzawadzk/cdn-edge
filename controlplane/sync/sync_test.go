package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rzawadzk/cdn-edge/controlplane/store"
	"github.com/rzawadzk/cdn-edge/tenant"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

func TestPushConfig(t *testing.T) {
	st := testStore(t)
	ten, _ := st.CreateTenant("Acme")
	st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})

	var received atomic.Bool
	var configVersion int64
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edge/config" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			return
		}
		if r.Header.Get("X-API-Key") != "admin-key" {
			t.Error("missing API key")
		}
		body, _ := io.ReadAll(r.Body)
		var cfg tenant.EdgeConfig
		json.Unmarshal(body, &cfg)
		configVersion = cfg.Version
		received.Store(true)
		w.WriteHeader(200)
	}))
	defer edge.Close()

	st.RegisterEdge("e1", edge.URL, "us-east")

	syncer := New(st, "admin-key")
	syncer.PushConfig()

	if !received.Load() {
		t.Fatal("config not pushed to edge")
	}
	if configVersion == 0 {
		t.Fatal("config version should be > 0")
	}
}

func TestPurgeAll(t *testing.T) {
	st := testStore(t)
	var purgeCount atomic.Int32

	edge1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/purge" {
			purgeCount.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer edge1.Close()

	edge2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/purge" {
			purgeCount.Add(1)
		}
		w.WriteHeader(200)
	}))
	defer edge2.Close()

	st.RegisterEdge("e1", edge1.URL, "us-east")
	st.RegisterEdge("e2", edge2.URL, "eu-west")

	syncer := New(st, "admin-key")
	err := syncer.PurgeAll()
	if err != nil {
		t.Fatalf("PurgeAll: %v", err)
	}
	if purgeCount.Load() != 2 {
		t.Fatalf("purge count = %d, want 2", purgeCount.Load())
	}
}

func TestPurgeAllPartialFailure(t *testing.T) {
	st := testStore(t)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()

	st.RegisterEdge("good", good.URL, "us-east")
	st.RegisterEdge("bad", bad.URL, "eu-west")

	syncer := New(st, "admin-key")
	err := syncer.PurgeAll()
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
}

func TestPurgeAllNoEdges(t *testing.T) {
	st := testStore(t)
	syncer := New(st, "admin-key")
	err := syncer.PurgeAll()
	if err != nil {
		t.Fatalf("PurgeAll with no edges should succeed: %v", err)
	}
}

func TestPushConfigUnreachableEdge(t *testing.T) {
	st := testStore(t)
	ten, _ := st.CreateTenant("Acme")
	st.CreateDomain(&tenant.Domain{TenantID: ten.ID, Hostname: "acme.com", OriginURL: "https://acme.com"})
	st.RegisterEdge("bad", "http://127.0.0.1:1", "us-east") // unreachable

	syncer := New(st, "admin-key")
	// Should not panic.
	syncer.PushConfig()
}

func TestPushConfigSendsDistributedFalse(t *testing.T) {
	st := testStore(t)

	var receivedBody map[string]any
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/purge" {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &receivedBody)
		}
		w.WriteHeader(200)
	}))
	defer edge.Close()

	st.RegisterEdge("e1", edge.URL, "us-east")

	syncer := New(st, "admin-key")
	syncer.PurgeAll()

	if receivedBody == nil {
		t.Fatal("no body received")
	}
	distributed, ok := receivedBody["distributed"]
	if !ok {
		t.Fatal("missing distributed field")
	}
	if distributed != false {
		t.Fatalf("distributed = %v, want false", distributed)
	}
}
