package certs

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func tmpManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestProvisionSelfSigned(t *testing.T) {
	m := tmpManager(t)
	if err := m.ProvisionSelfSigned("test.example.com"); err != nil {
		t.Fatalf("ProvisionSelfSigned: %v", err)
	}

	if !m.HasCert("test.example.com") {
		t.Fatal("expected cert to exist")
	}

	info, ok := m.GetCertInfo("test.example.com")
	if !ok {
		t.Fatal("GetCertInfo returned false")
	}
	if info.Hostname != "test.example.com" {
		t.Fatalf("hostname = %q", info.Hostname)
	}
	if info.Source != "self-signed" {
		t.Fatalf("source = %q, want self-signed", info.Source)
	}
	if info.Issuer != "self-signed" {
		t.Fatalf("issuer = %q, want self-signed", info.Issuer)
	}
}

func TestProvisionACME(t *testing.T) {
	m := tmpManager(t)
	if err := m.ProvisionACME("acme.example.com"); err != nil {
		t.Fatalf("ProvisionACME: %v", err)
	}
	info, ok := m.GetCertInfo("acme.example.com")
	if !ok {
		t.Fatal("cert not found")
	}
	if info.Source != "acme" {
		t.Fatalf("source = %q, want acme", info.Source)
	}
	if !info.AutoRenew {
		t.Fatal("ACME cert should have auto-renew")
	}
}

func TestProvisionACMESkipsWhenValid(t *testing.T) {
	m := tmpManager(t)
	m.ProvisionACME("test.com")

	// Second call should be a no-op (cert is valid for >30 days).
	if err := m.ProvisionACME("test.com"); err != nil {
		t.Fatalf("second ProvisionACME: %v", err)
	}
}

func TestGetCertificateSNI(t *testing.T) {
	m := tmpManager(t)
	m.ProvisionSelfSigned("sni.example.com")

	// Simulate TLS handshake.
	cert, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: "sni.example.com"})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Fatal("expected non-nil certificate")
	}

	_, err = m.GetCertificate(&tls.ClientHelloInfo{ServerName: "unknown.com"})
	if err == nil {
		t.Fatal("expected error for unknown hostname")
	}
}

func TestDeleteCert(t *testing.T) {
	m := tmpManager(t)
	m.ProvisionSelfSigned("del.example.com")
	m.DeleteCert("del.example.com")

	if m.HasCert("del.example.com") {
		t.Fatal("cert should be deleted")
	}
	_, ok := m.GetCertInfo("del.example.com")
	if ok {
		t.Fatal("GetCertInfo should return false after delete")
	}
}

func TestListCerts(t *testing.T) {
	m := tmpManager(t)
	m.ProvisionSelfSigned("a.com")
	m.ProvisionSelfSigned("b.com")

	list := m.ListCerts()
	if len(list) != 2 {
		t.Fatalf("ListCerts = %d, want 2", len(list))
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	m1, _ := NewManager(dir)
	m1.ProvisionSelfSigned("persist.com")

	m2, err := NewManager(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !m2.HasCert("persist.com") {
		t.Fatal("cert not persisted")
	}

	// TLS cert should also be reloaded.
	cert, err := m2.GetCertificate(&tls.ClientHelloInfo{ServerName: "persist.com"})
	if err != nil {
		t.Fatalf("GetCertificate after reopen: %v", err)
	}
	if cert == nil {
		t.Fatal("TLS cert not reloaded from disk")
	}
}

// --- HTTP Handler tests ---

func TestHandleCertsListAndProvision(t *testing.T) {
	m := tmpManager(t)

	// POST: provision self-signed.
	body, _ := json.Marshal(map[string]string{"hostname": "test.com", "mode": "self-signed"})
	req := httptest.NewRequest("POST", "/api/v1/certs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	m.HandleCerts(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("provision: got %d, body=%s", w.Code, w.Body.String())
	}

	// GET: list.
	req = httptest.NewRequest("GET", "/api/v1/certs", nil)
	w = httptest.NewRecorder()
	m.HandleCerts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: got %d", w.Code)
	}
	var list []*CertInfo
	json.NewDecoder(w.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("list = %d, want 1", len(list))
	}
}

func TestHandleCertsMissingHostname(t *testing.T) {
	m := tmpManager(t)
	body, _ := json.Marshal(map[string]string{"mode": "self-signed"})
	req := httptest.NewRequest("POST", "/api/v1/certs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	m.HandleCerts(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCertsInvalidMode(t *testing.T) {
	m := tmpManager(t)
	body, _ := json.Marshal(map[string]string{"hostname": "test.com", "mode": "invalid"})
	req := httptest.NewRequest("POST", "/api/v1/certs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	m.HandleCerts(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", w.Code)
	}
}

func TestHandleCertGetAndDelete(t *testing.T) {
	m := tmpManager(t)
	m.ProvisionSelfSigned("test.com")

	// GET.
	req := httptest.NewRequest("GET", "/api/v1/certs/test.com", nil)
	w := httptest.NewRecorder()
	m.HandleCert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get: got %d", w.Code)
	}

	// DELETE.
	req = httptest.NewRequest("DELETE", "/api/v1/certs/test.com", nil)
	w = httptest.NewRecorder()
	m.HandleCert(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: got %d", w.Code)
	}

	// GET after delete.
	req = httptest.NewRequest("GET", "/api/v1/certs/test.com", nil)
	w = httptest.NewRecorder()
	m.HandleCert(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404", w.Code)
	}
}

func TestHandleCertsMethodNotAllowed(t *testing.T) {
	m := tmpManager(t)
	req := httptest.NewRequest("PUT", "/api/v1/certs", nil)
	w := httptest.NewRecorder()
	m.HandleCerts(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

