package proxy

import (
	"errors"
	"sync"
	"time"
)

// Circuit states.
const (
	StateClosed   = iota // normal operation, requests pass through
	StateOpen            // origin is down, fail fast
	StateHalfOpen        // testing if origin recovered
)

// ErrCircuitOpen is returned when the circuit breaker is open.
var ErrCircuitOpen = errors.New("circuit breaker is open: origin unavailable")

// CircuitBreaker protects the origin from being overwhelmed when it's unhealthy.
type CircuitBreaker struct {
	mu               sync.Mutex
	state            int
	failures         int
	successes        int
	failureThreshold int
	successThreshold int
	openTimeout      time.Duration
	lastFailure      time.Time
}

// NewCircuitBreaker creates a circuit breaker.
// It opens after failureThreshold consecutive failures and closes again
// after successThreshold consecutive successes in half-open state.
func NewCircuitBreaker(failureThreshold, successThreshold int, openTimeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		openTimeout:      openTimeout,
	}
}

// Allow checks if a request is allowed through.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(cb.lastFailure) > cb.openTimeout {
			cb.state = StateHalfOpen
			cb.successes = 0
			return true
		}
		return false
	case StateHalfOpen:
		return true
	}
	return true
}

// RecordSuccess records a successful origin request.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures = 0
	if cb.state == StateHalfOpen {
		cb.successes++
		if cb.successes >= cb.successThreshold {
			cb.state = StateClosed
		}
	}
}

// RecordFailure records a failed origin request.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.lastFailure = time.Now()
	cb.failures++
	if cb.state == StateHalfOpen || cb.failures >= cb.failureThreshold {
		cb.state = StateOpen
		cb.successes = 0
	}
}

// State returns the current state name.
func (cb *CircuitBreaker) State() string {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	switch cb.state {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	}
	return "unknown"
}
