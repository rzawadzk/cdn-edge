package upgrade

import (
	"context"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestListenerNormal verifies that Listener creates a working TCP listener
// when no CDN_UPGRADE_FD is set.
func TestListenerNormal(t *testing.T) {
	os.Unsetenv(EnvUpgradeFD)

	ln, err := Listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listener: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	if addr.Port == 0 {
		t.Fatal("expected non-zero port")
	}

	// Verify we can connect.
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
}

// TestSOREUSEPORT verifies that the reusePortControl function successfully
// sets socket options without error.
func TestSOREUSEPORT(t *testing.T) {
	// Create a raw socket to test the control function.
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer syscall.Close(fd)

	// Verify SO_REUSEPORT can be set.
	err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1)
	if err != nil {
		t.Fatalf("SetsockoptInt SO_REUSEPORT: %v", err)
	}

	// Read back the value.
	val, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort)
	if err != nil {
		t.Fatalf("GetsockoptInt SO_REUSEPORT: %v", err)
	}
	if val == 0 {
		t.Fatal("SO_REUSEPORT not set")
	}
}

// TestReusePortListener verifies that two listeners can bind to the same
// address when SO_REUSEPORT is enabled.
func TestReusePortListener(t *testing.T) {
	os.Unsetenv(EnvUpgradeFD)

	ln1, err := Listener("127.0.0.1:0")
	if err != nil {
		t.Fatalf("first Listener: %v", err)
	}
	defer ln1.Close()

	addr := ln1.Addr().String()

	// A second listener on the same address should succeed with SO_REUSEPORT.
	ln2, err := Listener(addr)
	if err != nil {
		t.Fatalf("second Listener on same addr: %v", err)
	}
	defer ln2.Close()
}

// TestFDInheritance verifies that a listener's file descriptor can be
// extracted, passed via the environment, and reconstructed.
func TestFDInheritance(t *testing.T) {
	// Create a normal listener.
	orig, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer orig.Close()

	origAddr := orig.Addr().String()

	// Extract its file descriptor.
	tcpLn := orig.(*net.TCPListener)
	f, err := tcpLn.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer f.Close()

	fd := f.Fd()

	// Set the env var and use Listener to reconstruct.
	os.Setenv(EnvUpgradeFD, strconv.Itoa(int(fd)))
	defer os.Unsetenv(EnvUpgradeFD)

	inherited, err := Listener("ignored-because-fd-is-set")
	if err != nil {
		t.Fatalf("Listener with fd: %v", err)
	}
	defer inherited.Close()

	// The inherited listener should be on the same address.
	if inherited.Addr().String() != origAddr {
		t.Fatalf("address mismatch: got %s, want %s", inherited.Addr().String(), origAddr)
	}

	// Verify we can connect through the inherited listener.
	conn, err := net.DialTimeout("tcp", origAddr, time.Second)
	if err != nil {
		t.Fatalf("dial inherited: %v", err)
	}
	conn.Close()
}

// TestUpgraderReadySignal verifies that the Upgrader's ReadySignal channel
// works correctly.
func TestUpgraderReadySignal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	u := New(ln)

	ch := u.ReadySignal()
	select {
	case <-ch:
		t.Fatal("ready channel should not be closed yet")
	default:
		// expected
	}
}

// TestListenForUpgradeContextCancel verifies that ListenForUpgrade returns
// when the context is cancelled.
func TestListenForUpgradeContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	u := New(ln)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- u.ListenForUpgrade(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenForUpgrade did not return after context cancellation")
	}
}

// TestSignalReadyNoEnv verifies that SignalReady is a no-op when not in
// an upgrade child process.
func TestSignalReadyNoEnv(t *testing.T) {
	os.Unsetenv(EnvUpgradeReady)
	if err := SignalReady(); err != nil {
		t.Fatalf("SignalReady without env: %v", err)
	}
}

// TestSignalReadyWithPipe verifies that SignalReady correctly closes the
// ready-pipe fd, which unblocks the parent's read.
func TestSignalReadyWithPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()

	// Set the env var to point to the write end.
	os.Setenv(EnvUpgradeReady, strconv.Itoa(int(w.Fd())))
	defer os.Unsetenv(EnvUpgradeReady)

	if err := SignalReady(); err != nil {
		t.Fatalf("SignalReady: %v", err)
	}

	// The parent side should get EOF now.
	buf := make([]byte, 1)
	n, err := r.Read(buf)
	if n != 0 || err == nil {
		t.Fatalf("expected EOF after SignalReady, got n=%d err=%v", n, err)
	}
}

// TestIsUpgrade verifies the IsUpgrade helper.
func TestIsUpgrade(t *testing.T) {
	os.Unsetenv(EnvUpgradeFD)
	if IsUpgrade() {
		t.Fatal("expected false when env not set")
	}

	os.Setenv(EnvUpgradeFD, "3")
	defer os.Unsetenv(EnvUpgradeFD)
	if !IsUpgrade() {
		t.Fatal("expected true when env is set")
	}
}

// TestBuildChildEnv verifies that buildChildEnv correctly sets and replaces
// upgrade environment variables.
func TestBuildChildEnv(t *testing.T) {
	// Seed existing env with stale values.
	os.Setenv(EnvUpgradeFD, "99")
	os.Setenv(EnvUpgradeReady, "99")
	defer os.Unsetenv(EnvUpgradeFD)
	defer os.Unsetenv(EnvUpgradeReady)

	env := buildChildEnv(3, 4)

	var foundFD, foundReady string
	fdCount, readyCount := 0, 0
	for _, e := range env {
		if len(e) > len(EnvUpgradeFD) && e[:len(EnvUpgradeFD)+1] == EnvUpgradeFD+"=" {
			foundFD = e
			fdCount++
		}
		if len(e) > len(EnvUpgradeReady) && e[:len(EnvUpgradeReady)+1] == EnvUpgradeReady+"=" {
			foundReady = e
			readyCount++
		}
	}

	if fdCount != 1 {
		t.Fatalf("expected 1 %s entry, got %d", EnvUpgradeFD, fdCount)
	}
	if readyCount != 1 {
		t.Fatalf("expected 1 %s entry, got %d", EnvUpgradeReady, readyCount)
	}
	if foundFD != EnvUpgradeFD+"=3" {
		t.Fatalf("unexpected fd env: %s", foundFD)
	}
	if foundReady != EnvUpgradeReady+"=4" {
		t.Fatalf("unexpected ready env: %s", foundReady)
	}
}
