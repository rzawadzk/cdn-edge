package proxy

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
)

// ErrNoHealthyBackends is returned when all backends are unhealthy.
var ErrNoHealthyBackends = errors.New("balancer: no healthy backends available")

// Strategy defines how the balancer selects the next backend.
type Strategy int

const (
	// RoundRobin cycles through backends sequentially.
	RoundRobin Strategy = iota
	// Random selects a backend at random.
	Random
	// LeastConn is reserved for future implementation; falls back to RoundRobin.
	LeastConn
)

// Backend represents a single origin server.
type Backend struct {
	URL     string // e.g., "https://origin1.example.com"
	Weight  int    // relative weight for weighted round-robin (default 1)
	healthy atomic.Bool

	// activeConns tracks in-flight requests (for future LeastConn).
	activeConns atomic.Int64
}

// IsHealthy reports whether the backend is healthy.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// Balancer distributes requests across multiple origin backends.
type Balancer struct {
	backends []*Backend
	counter  atomic.Uint64
	strategy Strategy

	// expanded holds the weighted list for round-robin selection.
	expanded []*Backend

	mu sync.RWMutex // protects reads during reconfiguration if needed
}

// NewBalancer creates a load balancer with the given backends and strategy.
// All backends start as healthy. Weights default to 1 if not set.
func NewBalancer(backends []*Backend, strategy Strategy) *Balancer {
	for _, b := range backends {
		b.healthy.Store(true)
		if b.Weight <= 0 {
			b.Weight = 1
		}
	}

	expanded := buildWeightedList(backends)

	return &Balancer{
		backends: backends,
		strategy: strategy,
		expanded: expanded,
	}
}

// buildWeightedList creates a flat slice where each backend appears Weight times.
func buildWeightedList(backends []*Backend) []*Backend {
	var list []*Backend
	for _, b := range backends {
		for i := 0; i < b.Weight; i++ {
			list = append(list, b)
		}
	}
	return list
}

// Next returns the next healthy backend based on the configured strategy.
// Returns ErrNoHealthyBackends if no backends are healthy.
func (b *Balancer) Next() (*Backend, error) {
	switch b.strategy {
	case Random:
		return b.nextRandom()
	case LeastConn:
		return b.nextRoundRobin() // fallback
	default:
		return b.nextRoundRobin()
	}
}

func (b *Balancer) nextRoundRobin() (*Backend, error) {
	n := len(b.expanded)
	if n == 0 {
		return nil, ErrNoHealthyBackends
	}

	// Try up to n times to find a healthy backend.
	for i := 0; i < n; i++ {
		idx := b.counter.Add(1) - 1
		backend := b.expanded[idx%uint64(n)]
		if backend.healthy.Load() {
			return backend, nil
		}
	}
	return nil, ErrNoHealthyBackends
}

func (b *Balancer) nextRandom() (*Backend, error) {
	healthy := b.Healthy()
	if len(healthy) == 0 {
		return nil, ErrNoHealthyBackends
	}
	return healthy[rand.Intn(len(healthy))], nil
}

// MarkDown marks a backend as unhealthy.
func (b *Balancer) MarkDown(backend *Backend) {
	backend.healthy.Store(false)
}

// MarkUp marks a backend as healthy.
func (b *Balancer) MarkUp(backend *Backend) {
	backend.healthy.Store(true)
}

// Healthy returns the list of currently healthy backends.
func (b *Balancer) Healthy() []*Backend {
	var result []*Backend
	for _, be := range b.backends {
		if be.healthy.Load() {
			result = append(result, be)
		}
	}
	return result
}

// Backends returns all backends.
func (b *Balancer) Backends() []*Backend {
	return b.backends
}
