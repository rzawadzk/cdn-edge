package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Response holds the raw data fetched from an origin server.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// Origin fetches content from upstream origin servers.
// It implements single-flight request coalescing so that concurrent requests
// for the same resource only trigger one origin fetch.
type Origin struct {
	client          *http.Client
	cb              *CircuitBreaker
	coalesceTimeout time.Duration
	maxBodyBytes    int64
	hostOverride    string

	mu      sync.Mutex
	flights map[string]*flight
}

type flight struct {
	done chan struct{}
	res  *Response
	err  error
}

// Options configures a new Origin.
type Options struct {
	Timeout             time.Duration
	CoalesceTimeout     time.Duration
	MaxBodyBytes        int64
	HostOverride        string
	MaxIdleConnsPerHost int
}

// New creates an Origin proxy.
func New(opts Options) *Origin {
	maxIdle := opts.MaxIdleConnsPerHost
	if maxIdle <= 0 {
		maxIdle = 100
	}
	transport := &http.Transport{
		MaxIdleConns:        maxIdle * 2,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
	}
	return &Origin{
		client: &http.Client{
			Timeout:   opts.Timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		cb:              NewCircuitBreaker(5, 3, 30*time.Second),
		coalesceTimeout: opts.CoalesceTimeout,
		maxBodyBytes:    opts.MaxBodyBytes,
		hostOverride:    opts.HostOverride,
		flights:         make(map[string]*flight),
	}
}

// CircuitState returns the current circuit breaker state.
func (o *Origin) CircuitState() string {
	return o.cb.State()
}

// Fetch retrieves a resource from the origin. Concurrent calls with the same
// cacheKey will coalesce into a single origin request (single-flight pattern).
// The provided context is honored — if it is canceled while waiting for a
// coalesced request, Fetch returns ctx.Err().
func (o *Origin) Fetch(ctx context.Context, cacheKey, originURL string, reqHeader http.Header) (*Response, error) {
	if !o.cb.Allow() {
		return nil, ErrCircuitOpen
	}

	o.mu.Lock()
	if f, ok := o.flights[cacheKey]; ok {
		o.mu.Unlock()

		// Wait for the in-flight request, respecting context and coalesce timeout.
		timer := time.NewTimer(o.coalesceTimeout)
		defer timer.Stop()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("origin: coalesced request timed out after %v", o.coalesceTimeout)
		}
	}

	f := &flight{done: make(chan struct{})}
	o.flights[cacheKey] = f
	o.mu.Unlock()

	f.res, f.err = o.doFetch(ctx, originURL, reqHeader)

	// Record result in circuit breaker.
	if f.err != nil {
		o.cb.RecordFailure()
	} else if f.res.StatusCode >= 500 {
		o.cb.RecordFailure()
	} else {
		o.cb.RecordSuccess()
	}

	close(f.done)

	o.mu.Lock()
	delete(o.flights, cacheKey)
	o.mu.Unlock()

	return f.res, f.err
}

func (o *Origin) doFetch(ctx context.Context, originURL string, reqHeader http.Header) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL, nil)
	if err != nil {
		return nil, fmt.Errorf("origin: build request: %w", err)
	}

	// Forward select headers from the client.
	forwardHeaders := []string{"Accept", "Accept-Encoding", "Accept-Language", "Range"}
	for _, h := range forwardHeaders {
		if v := reqHeader.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if xff := reqHeader.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	req.Header.Set("User-Agent", "CDN-Edge/1.0")

	// Override Host header if configured (single-tenant mode).
	if o.hostOverride != "" {
		req.Host = o.hostOverride
	}
	// Allow per-request Host override (multi-tenant mode sets Host in reqHeader).
	if h := reqHeader.Get("Host"); h != "" && o.hostOverride == "" {
		req.Host = h
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("origin: fetch: %w", err)
	}
	defer resp.Body.Close()

	maxBody := o.maxBodyBytes
	if maxBody <= 0 {
		maxBody = 100 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("origin: read body: %w", err)
	}

	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       body,
	}, nil
}
