package handshake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// generateTestTLSConfig creates a self-signed TLS config for tests.
func generateTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// newLoopbackListener returns a new TCP listener on 127.0.0.1:0.
func newLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// TestBasicAcceptClose verifies the pass-through (no TLS) path and clean close.
func TestBasicAcceptClose(t *testing.T) {
	inner := newLoopbackListener(t)
	l := NewOffloadListener(inner, nil, Config{Workers: 2, QueueSize: 16})

	addr := l.Addr().String()

	// Dial and verify Accept returns a conn.
	clientConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientConn.Close()

	srvConn, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if srvConn == nil {
		t.Fatal("nil conn")
	}
	srvConn.Close()

	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Accept after close should return an error.
	_, err = l.Accept()
	if err == nil {
		t.Fatal("expected error from Accept after Close")
	}
}

// TestTLSHandshakeCompletes drives a real TLS client against the offload
// listener and ensures the handshake completes and data flows.
func TestTLSHandshakeCompletes(t *testing.T) {
	inner := newLoopbackListener(t)
	tlsCfg := generateTestTLSConfig(t)
	l := NewOffloadListener(inner, tlsCfg, Config{Workers: 2, QueueSize: 16, HandshakeTimeout: 3 * time.Second})
	defer l.Close()

	serverDone := make(chan error, 1)
	go func() {
		srvConn, err := l.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer srvConn.Close()
		_, _ = srvConn.Write([]byte("hello"))
		serverDone <- nil
	}()

	clientCfg := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}
	clientConn, err := tls.Dial("tcp", l.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	defer clientConn.Close()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(clientConn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("got %q, want %q", buf, "hello")
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("server error: %v", err)
	}

	q, c, f := l.Stats()
	if c < 1 {
		t.Errorf("expected completed >= 1, got %d", c)
	}
	if f != 0 {
		t.Errorf("expected failed = 0, got %d", f)
	}
	if q != 0 {
		t.Errorf("expected queued = 0, got %d", q)
	}
}

// TestConcurrentHandshakes ensures many handshakes can complete in parallel.
func TestConcurrentHandshakes(t *testing.T) {
	inner := newLoopbackListener(t)
	tlsCfg := generateTestTLSConfig(t)
	l := NewOffloadListener(inner, tlsCfg, Config{Workers: 8, QueueSize: 64, HandshakeTimeout: 5 * time.Second})
	defer l.Close()

	const N = 20

	// Drain Accept in a goroutine.
	var acceptedCount atomic.Int64
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			acceptedCount.Add(1)
			// Echo one byte per connection, then close.
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				_, _ = c.Write(buf)
			}(c)
		}
	}()

	clientCfg := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}

	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := tls.Dial("tcp", l.Addr().String(), clientCfg)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("x")); err != nil {
				errCh <- err
				return
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(conn, buf); err != nil {
				errCh <- err
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("client error: %v", err)
	}

	// Give the accept goroutine a moment to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && acceptedCount.Load() < N {
		time.Sleep(10 * time.Millisecond)
	}

	if got := acceptedCount.Load(); got < N {
		t.Errorf("accepted only %d of %d conns", got, N)
	}

	_, completed, failed := l.Stats()
	if completed < N {
		t.Errorf("completed = %d, want >= %d", completed, N)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
}

// TestSlowHandshakeDoesNotBlockAccept verifies that a stuck handshake (a TCP
// client that never sends a ClientHello) does not block handshakes for other
// clients, and that Accept() returns the second client promptly.
func TestSlowHandshakeDoesNotBlockAccept(t *testing.T) {
	inner := newLoopbackListener(t)
	tlsCfg := generateTestTLSConfig(t)
	l := NewOffloadListener(inner, tlsCfg, Config{Workers: 4, QueueSize: 16, HandshakeTimeout: 500 * time.Millisecond})
	defer l.Close()

	// Dial a raw TCP connection but never complete a handshake.
	slow, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("slow dial: %v", err)
	}
	defer slow.Close()

	// Now dial a real TLS client — it should be handshaken on another worker.
	acceptCh := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		acceptCh <- c
	}()

	clientCfg := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}
	done := make(chan error, 1)
	go func() {
		conn, err := tls.Dial("tcp", l.Addr().String(), clientCfg)
		if err != nil {
			done <- err
			return
		}
		conn.Close()
		done <- nil
	}()

	select {
	case c := <-acceptCh:
		c.Close()
	case err := <-acceptErr:
		t.Fatalf("accept err: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Accept() was blocked by slow client")
	}

	if err := <-done; err != nil {
		t.Fatalf("fast client failed: %v", err)
	}
}

// TestHandshakeTimeoutDropsBadClient ensures that a client that never sends a
// ClientHello is eventually dropped and counted as failed.
func TestHandshakeTimeoutDropsBadClient(t *testing.T) {
	inner := newLoopbackListener(t)
	tlsCfg := generateTestTLSConfig(t)
	l := NewOffloadListener(inner, tlsCfg, Config{Workers: 2, QueueSize: 4, HandshakeTimeout: 150 * time.Millisecond})
	defer l.Close()

	slow, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatalf("slow dial: %v", err)
	}
	defer slow.Close()

	// Wait for the failed counter to tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, failed := l.Stats()
		if failed >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, failed := l.Stats()
	t.Fatalf("expected failed >= 1, got %d", failed)
}

// TestCloseStopsWorkers verifies Close cleanly shuts everything down.
func TestCloseStopsWorkers(t *testing.T) {
	inner := newLoopbackListener(t)
	l := NewOffloadListener(inner, nil, Config{Workers: 4, QueueSize: 8})

	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Second close should be a no-op.
	_ = l.Close()

	// Accept should return quickly.
	doneCh := make(chan struct{})
	go func() {
		_, _ = l.Accept()
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not return after Close")
	}

	// Give workers a moment to fully exit.
	time.Sleep(50 * time.Millisecond)
}

// TestStatsTrackCounts sanity-checks that Stats() returns sane values.
func TestStatsTrackCounts(t *testing.T) {
	inner := newLoopbackListener(t)
	tlsCfg := generateTestTLSConfig(t)
	l := NewOffloadListener(inner, tlsCfg, Config{Workers: 2, QueueSize: 8, HandshakeTimeout: 2 * time.Second})
	defer l.Close()

	// Drain accepts.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	clientCfg := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}
	for i := 0; i < 3; i++ {
		conn, err := tls.Dial("tcp", l.Addr().String(), clientCfg)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.Close()
	}

	// Wait for completion count to reach 3.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, c, _ := l.Stats()
		if c >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	_, completed, _ := l.Stats()
	if completed < 3 {
		t.Errorf("completed = %d, want >= 3", completed)
	}
}
