// Package certs manages TLS certificates for CDN domains.
// Supports auto-provisioning via ACME (Let's Encrypt) and custom cert upload.
// Stores certificates on disk in PEM format with file-backed metadata.
package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// CertInfo holds metadata about a stored certificate.
type CertInfo struct {
	Hostname  string    `json:"hostname"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Issuer    string    `json:"issuer"`
	AutoRenew bool      `json:"auto_renew"`
	Source    string    `json:"source"` // "acme", "custom", "self-signed"
}

// ManagerConfig holds configuration for the certificate Manager.
type ManagerConfig struct {
	// DataDir is the base directory for storing certificates and metadata.
	DataDir string

	// ACMEEmail is the contact email for the ACME account (e.g. admin@example.com).
	ACMEEmail string

	// ACMEDirectory is the ACME directory URL. Defaults to Let's Encrypt production.
	ACMEDirectory string

	// SelfSignedOnly disables real ACME and always falls back to self-signed certs.
	// Useful for development/testing.
	SelfSignedOnly bool
}

// Manager handles TLS certificate lifecycle.
type Manager struct {
	mu       sync.RWMutex
	dir      string
	certs    map[string]*CertInfo // keyed by hostname
	tlsCerts map[string]*tls.Certificate

	acmeManager    *autocert.Manager
	selfSignedOnly bool
}

// NewManager creates a certificate manager with default settings (self-signed only).
func NewManager(dataDir string) (*Manager, error) {
	return NewManagerWithConfig(ManagerConfig{
		DataDir:        dataDir,
		SelfSignedOnly: true,
	})
}

// NewManagerWithConfig creates a certificate manager with the given configuration.
func NewManagerWithConfig(cfg ManagerConfig) (*Manager, error) {
	certsDir := filepath.Join(cfg.DataDir, "certs")
	if err := os.MkdirAll(certsDir, 0o700); err != nil {
		return nil, fmt.Errorf("certs: create dir: %w", err)
	}
	m := &Manager{
		dir:            certsDir,
		certs:          make(map[string]*CertInfo),
		tlsCerts:       make(map[string]*tls.Certificate),
		selfSignedOnly: cfg.SelfSignedOnly,
	}

	if !cfg.SelfSignedOnly {
		am := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Email:      cfg.ACMEEmail,
			Cache:      autocert.DirCache(filepath.Join(certsDir, "acme-cache")),
			HostPolicy: m.acmeHostPolicy,
		}
		if cfg.ACMEDirectory != "" {
			am.Client = &acme.Client{
				DirectoryURL: cfg.ACMEDirectory,
			}
		}
		m.acmeManager = am
	}

	m.loadMetadata()
	m.loadCertsFromDisk()

	// Start renewal checker.
	go m.renewalLoop()

	return m, nil
}

// GetCertificate implements the tls.Config.GetCertificate callback.
// This enables per-hostname TLS (SNI-based certificate selection).
// For ACME-managed domains, it delegates to the autocert.Manager.
func (m *Manager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	cert, ok := m.tlsCerts[hello.ServerName]
	info := m.certs[hello.ServerName]
	m.mu.RUnlock()

	if ok {
		return cert, nil
	}

	// Delegate to autocert for ACME-managed domains.
	if m.acmeManager != nil && info != nil && info.Source == "acme" {
		return m.acmeManager.GetCertificate(hello)
	}

	return nil, fmt.Errorf("no certificate for %s", hello.ServerName)
}

// acmeHostPolicy restricts which hosts the autocert.Manager will provision.
// Only domains that have been explicitly requested via ProvisionACME are allowed.
func (m *Manager) acmeHostPolicy(_ context.Context, host string) error {
	m.mu.RLock()
	info, ok := m.certs[host]
	m.mu.RUnlock()
	if ok && info.Source == "acme" {
		return nil
	}
	return fmt.Errorf("host %q not allowed for ACME", host)
}

// StoreCert stores a custom PEM certificate and key for a hostname.
func (m *Manager) StoreCert(hostname string, certPEM, keyPEM []byte) error {
	// Validate the cert/key pair.
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("invalid cert/key pair: %w", err)
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}

	// Write to disk.
	certPath := filepath.Join(m.dir, hostname+".crt")
	keyPath := filepath.Join(m.dir, hostname+".key")

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	info := &CertInfo{
		Hostname:  hostname,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Issuer:    leaf.Issuer.CommonName,
		AutoRenew: false,
		Source:    "custom",
	}

	m.mu.Lock()
	m.certs[hostname] = info
	m.tlsCerts[hostname] = &tlsCert
	m.mu.Unlock()

	m.saveMetadata()
	log.Printf("certs: stored custom cert for %s (expires %s)", hostname, leaf.NotAfter.Format(time.RFC3339))
	return nil
}

// ProvisionSelfSigned creates a self-signed certificate for development/testing.
func (m *Manager) ProvisionSelfSigned(hostname string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	// Write to disk.
	certPath := filepath.Join(m.dir, hostname+".crt")
	keyPath := filepath.Join(m.dir, hostname+".key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}

	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)

	info := &CertInfo{
		Hostname:  hostname,
		NotBefore: template.NotBefore,
		NotAfter:  template.NotAfter,
		Issuer:    "self-signed",
		AutoRenew: false,
		Source:    "self-signed",
	}

	m.mu.Lock()
	m.certs[hostname] = info
	m.tlsCerts[hostname] = &tlsCert
	m.mu.Unlock()

	m.saveMetadata()
	log.Printf("certs: provisioned self-signed cert for %s", hostname)
	return nil
}

// ProvisionACME initiates ACME certificate provisioning for a hostname.
// When configured with a real ACME directory, it uses autocert to obtain
// a certificate via Let's Encrypt (or the configured directory).
// In self-signed-only mode (default for development), falls back to self-signed certs.
func (m *Manager) ProvisionACME(hostname string) error {
	// Check if we already have a valid cert.
	m.mu.RLock()
	info, exists := m.certs[hostname]
	m.mu.RUnlock()
	if exists && info.NotAfter.After(time.Now().Add(30*24*time.Hour)) {
		return nil // Still valid for >30 days.
	}

	if m.selfSignedOnly || m.acmeManager == nil {
		log.Printf("certs: ACME provisioning requested for %s (using self-signed fallback)", hostname)
		if err := m.ProvisionSelfSigned(hostname); err != nil {
			return err
		}
		// Mark as auto-renew so the renewal loop picks it up.
		m.mu.Lock()
		if ci, ok := m.certs[hostname]; ok {
			ci.AutoRenew = true
			ci.Source = "acme"
		}
		m.mu.Unlock()
		m.saveMetadata()
		return nil
	}

	log.Printf("certs: ACME provisioning for %s via Let's Encrypt", hostname)

	// Register the hostname in metadata first so the host policy allows it.
	m.mu.Lock()
	m.certs[hostname] = &CertInfo{
		Hostname:  hostname,
		AutoRenew: true,
		Source:    "acme",
	}
	m.mu.Unlock()
	m.saveMetadata()

	// Trigger certificate fetch via autocert. The autocert.Manager handles
	// the full ACME flow: account registration, authorization, challenge
	// response, order finalization, and certificate download.
	hello := &tls.ClientHelloInfo{ServerName: hostname}
	cert, err := m.acmeManager.GetCertificate(hello)
	if err != nil {
		// Roll back metadata on failure.
		m.mu.Lock()
		delete(m.certs, hostname)
		m.mu.Unlock()
		m.saveMetadata()
		return fmt.Errorf("certs: ACME provision for %s: %w", hostname, err)
	}

	// Parse the leaf to extract metadata.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("certs: parse ACME cert for %s: %w", hostname, err)
	}

	m.mu.Lock()
	m.certs[hostname] = &CertInfo{
		Hostname:  hostname,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Issuer:    leaf.Issuer.CommonName,
		AutoRenew: true,
		Source:    "acme",
	}
	m.tlsCerts[hostname] = cert
	m.mu.Unlock()
	m.saveMetadata()

	log.Printf("certs: ACME cert provisioned for %s (expires %s)", hostname, leaf.NotAfter.Format(time.RFC3339))
	return nil
}

// DeleteCert removes a certificate for a hostname.
func (m *Manager) DeleteCert(hostname string) {
	m.mu.Lock()
	delete(m.certs, hostname)
	delete(m.tlsCerts, hostname)
	m.mu.Unlock()

	os.Remove(filepath.Join(m.dir, hostname+".crt"))
	os.Remove(filepath.Join(m.dir, hostname+".key"))
	m.saveMetadata()
}

// ListCerts returns metadata for all stored certificates.
func (m *Manager) ListCerts() []*CertInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var list []*CertInfo
	for _, ci := range m.certs {
		cp := *ci
		list = append(list, &cp)
	}
	return list
}

// GetCertInfo returns metadata for a specific hostname's certificate.
func (m *Manager) GetCertInfo(hostname string) (*CertInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ci, ok := m.certs[hostname]
	if !ok {
		return nil, false
	}
	cp := *ci
	return &cp, true
}

// HasCert checks if a valid certificate exists for the hostname.
func (m *Manager) HasCert(hostname string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ci, ok := m.certs[hostname]
	return ok && ci.NotAfter.After(time.Now())
}

// renewalLoop checks for certificates that need renewal every 12 hours.
func (m *Manager) renewalLoop() {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		m.checkRenewals()
	}
}

func (m *Manager) checkRenewals() {
	m.mu.RLock()
	var toRenew []string
	for hostname, ci := range m.certs {
		if !ci.AutoRenew {
			continue
		}
		// Renew if expiring within 30 days.
		if ci.NotAfter.Before(time.Now().Add(30 * 24 * time.Hour)) {
			toRenew = append(toRenew, hostname)
		}
	}
	m.mu.RUnlock()

	for _, hostname := range toRenew {
		log.Printf("certs: auto-renewing certificate for %s", hostname)
		if err := m.ProvisionACME(hostname); err != nil {
			log.Printf("certs: renewal failed for %s: %v", hostname, err)
		}
	}
}

func (m *Manager) metadataPath() string {
	return filepath.Join(m.dir, "metadata.json")
}

func (m *Manager) saveMetadata() {
	m.mu.RLock()
	data, err := json.MarshalIndent(m.certs, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return
	}
	os.WriteFile(m.metadataPath(), data, 0o644)
}

func (m *Manager) loadMetadata() {
	data, err := os.ReadFile(m.metadataPath())
	if err != nil {
		return
	}
	json.Unmarshal(data, &m.certs)
}

func (m *Manager) loadCertsFromDisk() {
	for hostname := range m.certs {
		certPath := filepath.Join(m.dir, hostname+".crt")
		keyPath := filepath.Join(m.dir, hostname+".key")

		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			continue
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			continue
		}

		tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			log.Printf("certs: failed to load %s: %v", hostname, err)
			continue
		}

		m.tlsCerts[hostname] = &tlsCert
	}
}
