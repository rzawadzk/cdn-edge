// Package upgrade implements graceful binary upgrades via file descriptor
// passing for the CDN edge server. When SIGUSR2 is received, the running
// process starts a new copy of itself, passes the listening socket's file
// descriptor to the child, waits for the child to signal readiness, then
// gracefully drains existing connections and exits.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
)

const (
	// EnvUpgradeFD is the environment variable that carries the inherited
	// file descriptor number from the parent process to the child.
	EnvUpgradeFD = "CDN_UPGRADE_FD"

	// EnvUpgradeReady is the environment variable set by the parent so the
	// child can find the pipe fd used to signal readiness back.
	EnvUpgradeReady = "CDN_UPGRADE_READY_FD"
)

// Listener creates or inherits a TCP listener. If CDN_UPGRADE_FD is set,
// the listener is reconstructed from the inherited file descriptor.
// Otherwise a new listener is created on addr with SO_REUSEPORT enabled.
func Listener(addr string) (net.Listener, error) {
	if fdStr := os.Getenv(EnvUpgradeFD); fdStr != "" {
		return inheritedListener(fdStr)
	}
	return newReusePortListener(addr)
}

// inheritedListener reconstructs a net.Listener from a file descriptor
// number passed via the environment.
func inheritedListener(fdStr string) (net.Listener, error) {
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("upgrade: invalid fd %q: %w", fdStr, err)
	}
	f := os.NewFile(uintptr(fd), "inherited-listener")
	if f == nil {
		return nil, fmt.Errorf("upgrade: cannot open fd %d", fd)
	}
	defer f.Close()

	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("upgrade: FileListener fd %d: %w", fd, err)
	}
	return ln, nil
}

// newReusePortListener creates a TCP listener with SO_REUSEPORT set via
// a net.ListenConfig control function.
func newReusePortListener(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: reusePortControl,
	}
	return lc.Listen(context.Background(), "tcp", addr)
}

// reusePortControl sets SO_REUSEPORT (and SO_REUSEADDR) on the socket
// before it is bound. This works on Linux and Darwin.
func reusePortControl(network, address string, c syscall.RawConn) error {
	var setErr error
	err := c.Control(func(fd uintptr) {
		// SO_REUSEADDR lets us bind immediately after a previous listener
		// on the same address has closed.
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			setErr = fmt.Errorf("upgrade: SO_REUSEADDR: %w", err)
			return
		}
		// SO_REUSEPORT allows multiple processes to bind to the same
		// address and port, which is key during a rolling upgrade.
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); err != nil {
			setErr = fmt.Errorf("upgrade: SO_REUSEPORT: %w", err)
			return
		}
	})
	if err != nil {
		return err
	}
	return setErr
}

// Upgrader manages graceful binary upgrades triggered by SIGUSR2.
type Upgrader struct {
	ln    net.Listener
	ready chan struct{}
	mu    sync.Mutex
}

// New creates an Upgrader that will pass ln's file descriptor to the
// child process during an upgrade.
func New(ln net.Listener) *Upgrader {
	return &Upgrader{
		ln:    ln,
		ready: make(chan struct{}),
	}
}

// ReadySignal returns a channel that is closed when the new (child) process
// signals that it is ready to accept traffic. The old process should drain
// connections after this channel closes.
func (u *Upgrader) ReadySignal() <-chan struct{} {
	return u.ready
}

// ListenForUpgrade installs a SIGUSR2 handler and blocks until either an
// upgrade is triggered or ctx is cancelled. On SIGUSR2 it forks a new
// process with the listener fd inherited, waits for the child to signal
// readiness (by closing a pipe), then closes the ready channel and returns
// nil. If ctx is cancelled before an upgrade, it returns ctx.Err().
func (u *Upgrader) ListenForUpgrade(ctx context.Context) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR2)
	defer signal.Stop(sig)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sig:
		return u.doUpgrade()
	}
}

// doUpgrade performs the actual fork-exec with fd passing.
func (u *Upgrader) doUpgrade() error {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Extract the underlying file from the listener so we can pass it
	// to the child process.
	lnFile, err := listenerFile(u.ln)
	if err != nil {
		return fmt.Errorf("upgrade: extract listener fd: %w", err)
	}
	defer lnFile.Close()

	// Create a pipe for the child to signal readiness. The child inherits
	// the write end and closes it when ready; the parent reads from the
	// read end -- an EOF means the child is ready.
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("upgrade: create ready pipe: %w", err)
	}
	defer readyR.Close()
	// readyW is passed to the child; we close our copy after starting it.

	// The child's ExtraFiles are [lnFile, readyW]. ExtraFiles start at fd 3
	// in the child, so lnFile = fd 3, readyW = fd 4.
	const childListenerFD = 3
	const childReadyFD = 4

	exe, err := os.Executable()
	if err != nil {
		readyW.Close()
		return fmt.Errorf("upgrade: resolve executable: %w", err)
	}

	env := buildChildEnv(childListenerFD, childReadyFD)

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{lnFile, readyW}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		readyW.Close()
		return fmt.Errorf("upgrade: start child: %w", err)
	}

	// Close the write end in the parent so we get EOF when the child
	// closes its copy.
	readyW.Close()

	// Wait for the child to signal readiness (close the pipe) or die.
	buf := make([]byte, 1)
	_, _ = readyR.Read(buf) // blocks until EOF or error

	// Signal to the caller that the new process is ready.
	close(u.ready)
	return nil
}

// SignalReady should be called by a child process (one started via an
// upgrade) once it is ready to accept traffic. It closes the ready-pipe
// fd inherited from the parent.
func SignalReady() error {
	fdStr := os.Getenv(EnvUpgradeReady)
	if fdStr == "" {
		// Not an upgraded child; nothing to do.
		return nil
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return fmt.Errorf("upgrade: invalid ready fd %q: %w", fdStr, err)
	}
	f := os.NewFile(uintptr(fd), "ready-signal")
	if f == nil {
		return fmt.Errorf("upgrade: cannot open ready fd %d", fd)
	}
	return f.Close()
}

// IsUpgrade reports whether the current process was started as part of a
// graceful upgrade (i.e. CDN_UPGRADE_FD is set).
func IsUpgrade() bool {
	return os.Getenv(EnvUpgradeFD) != ""
}

// buildChildEnv returns a copy of the current environment with the
// upgrade-specific variables set.
func buildChildEnv(listenerFD, readyFD int) []string {
	env := os.Environ()
	// Remove any existing upgrade vars to avoid stacking.
	filtered := env[:0]
	for _, e := range env {
		if len(e) > len(EnvUpgradeFD) && e[:len(EnvUpgradeFD)+1] == EnvUpgradeFD+"=" {
			continue
		}
		if len(e) > len(EnvUpgradeReady) && e[:len(EnvUpgradeReady)+1] == EnvUpgradeReady+"=" {
			continue
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered,
		fmt.Sprintf("%s=%d", EnvUpgradeFD, listenerFD),
		fmt.Sprintf("%s=%d", EnvUpgradeReady, readyFD),
	)
	return filtered
}

// listenerFile extracts an *os.File from a net.Listener. The listener must
// have an underlying type that implements the File() method (e.g.
// *net.TCPListener or *net.UnixListener).
func listenerFile(ln net.Listener) (*os.File, error) {
	type filer interface {
		File() (*os.File, error)
	}
	f, ok := ln.(filer)
	if !ok {
		return nil, errors.New("listener does not support File()")
	}
	return f.File()
}
