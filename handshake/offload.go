// Package handshake provides a net.Listener wrapper that offloads TCP accept
// and TLS handshake work onto a bounded worker pool, keeping the request-serving
// goroutines free from handshake CPU contention.
package handshake

import (
	"crypto/tls"
	"errors"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls the handshake pool.
type Config struct {
	// Workers is the number of handshake workers. Defaults to NumCPU * 2.
	Workers int
	// QueueSize is the max pending handshakes buffered. Defaults to 1024.
	QueueSize int
	// HandshakeTimeout bounds each TLS handshake. Defaults to 10s.
	HandshakeTimeout time.Duration
}

// Logger is the minimal logging interface the listener uses. It matches
// *logging.Logger's Error/Info methods but avoids an import cycle.
type Logger interface {
	Info(msg string, reqID ...string)
	Error(msg string, err error, reqID ...string)
}

// nopLogger is used when no logger is supplied.
type nopLogger struct{}

func (nopLogger) Info(string, ...string)         {}
func (nopLogger) Error(string, error, ...string) {}

// acceptResult is what Accept() returns to callers. Either conn is set or err is.
type acceptResult struct {
	conn net.Conn
	err  error
}

// OffloadListener wraps a net.Listener and performs TLS handshakes on a
// dedicated worker pool, so the accept loop returns immediately and request
// serving is not starved by handshake CPU.
type OffloadListener struct {
	inner     net.Listener
	tlsConfig *tls.Config
	cfg       Config
	log       Logger

	inbox  chan net.Conn     // raw conns awaiting handshake
	outbox chan acceptResult // handshaken conns ready for Accept()

	wg sync.WaitGroup

	closeOnce sync.Once
	closed    chan struct{}

	// Stats (atomic).
	queued    atomic.Int64 // current depth of inbox
	completed atomic.Int64 // total successful handshakes
	failed    atomic.Int64 // total failed / dropped handshakes
}

// NewOffloadListener creates a listener that offloads accept and TLS
// handshakes onto a worker pool. If tlsConfig is nil, raw conns are passed
// through unmodified (the accept work is still offloaded).
func NewOffloadListener(inner net.Listener, tlsConfig *tls.Config, cfg Config) *OffloadListener {
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.NumCPU() * 2
		if cfg.Workers < 2 {
			cfg.Workers = 2
		}
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}

	l := &OffloadListener{
		inner:     inner,
		tlsConfig: tlsConfig,
		cfg:       cfg,
		log:       nopLogger{},
		inbox:     make(chan net.Conn, cfg.QueueSize),
		outbox:    make(chan acceptResult, cfg.QueueSize),
		closed:    make(chan struct{}),
	}

	// Start workers.
	for i := 0; i < cfg.Workers; i++ {
		l.wg.Add(1)
		go l.worker()
	}

	// Start accept loop.
	l.wg.Add(1)
	go l.acceptLoop()

	return l
}

// SetLogger attaches a logger for drop / error events.
func (l *OffloadListener) SetLogger(log Logger) {
	if log == nil {
		l.log = nopLogger{}
		return
	}
	l.log = log
}

// acceptLoop runs inner.Accept() on a dedicated goroutine and pushes raw
// conns into the inbox channel. If the inbox is full, the conn is closed and
// counted as failed to apply backpressure.
func (l *OffloadListener) acceptLoop() {
	defer l.wg.Done()
	defer close(l.inbox)

	for {
		// Fast exit if closed.
		select {
		case <-l.closed:
			return
		default:
		}

		raw, err := l.inner.Accept()
		if err != nil {
			select {
			case <-l.closed:
				return
			default:
			}
			// Propagate a terminal accept error to any Accept() caller.
			// Only do so if we are not closing.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				// transient, retry
				continue
			}
			// Non-temporary error; forward and bail.
			select {
			case l.outbox <- acceptResult{err: err}:
			case <-l.closed:
			}
			return
		}

		// Try to enqueue for handshake. If full, drop (non-blocking).
		select {
		case l.inbox <- raw:
			l.queued.Add(1)
		case <-l.closed:
			_ = raw.Close()
			return
		default:
			l.failed.Add(1)
			l.log.Error("handshake queue full, dropping conn", nil)
			_ = raw.Close()
		}
	}
}

// worker pulls raw conns off the inbox, performs the TLS handshake with a
// timeout, and forwards the completed conn (or error) to the outbox.
func (l *OffloadListener) worker() {
	defer l.wg.Done()

	for raw := range l.inbox {
		l.queued.Add(-1)

		conn, err := l.handshake(raw)
		if err != nil {
			l.failed.Add(1)
			_ = raw.Close()
			// Handshake errors are per-client and should not be surfaced to
			// the accept loop caller — the server keeps accepting.
			l.log.Error("tls handshake failed", err)
			continue
		}

		l.completed.Add(1)
		select {
		case l.outbox <- acceptResult{conn: conn}:
		case <-l.closed:
			_ = conn.Close()
			return
		}
	}
}

// handshake performs a bounded TLS handshake on raw. If tlsConfig is nil,
// raw is returned as-is (pass-through).
func (l *OffloadListener) handshake(raw net.Conn) (net.Conn, error) {
	if l.tlsConfig == nil {
		return raw, nil
	}

	// Apply a deadline to bound slow / malicious clients.
	deadline := time.Now().Add(l.cfg.HandshakeTimeout)
	if err := raw.SetDeadline(deadline); err != nil {
		return nil, err
	}

	tlsConn := tls.Server(raw, l.tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}

	// Clear the deadline so regular read/write timeouts from http.Server apply.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}

	return tlsConn, nil
}

// Accept returns the next fully-handshaken connection. It blocks until a
// completed conn is available or the listener is closed.
func (l *OffloadListener) Accept() (net.Conn, error) {
	select {
	case res, ok := <-l.outbox:
		if !ok {
			return nil, net.ErrClosed
		}
		if res.err != nil {
			return nil, res.err
		}
		return res.conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close stops the accept loop and all workers, then closes the underlying
// listener. Safe to call multiple times.
func (l *OffloadListener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		err = l.inner.Close()

		// Drain any in-flight raw conns after workers exit.
		go func() {
			l.wg.Wait()
			// Close any still-buffered outbox conns so callers don't leak.
			close(l.outbox)
			for res := range l.outbox {
				if res.conn != nil {
					_ = res.conn.Close()
				}
			}
		}()
	})
	return err
}

// Addr returns the underlying listener's address.
func (l *OffloadListener) Addr() net.Addr {
	return l.inner.Addr()
}

// Stats returns (queued, completed, failed).
//   queued   — current inbox depth (pending handshakes)
//   completed — lifetime successful handshakes
//   failed   — lifetime failed or dropped handshakes
func (l *OffloadListener) Stats() (queued, completed, failed int64) {
	return l.queued.Load(), l.completed.Load(), l.failed.Load()
}
