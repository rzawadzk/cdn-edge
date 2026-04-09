package proxy

import (
	"testing"
	"time"
)

func TestCircuitBreakerClosedByDefault(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 5*time.Second)
	if !cb.Allow() {
		t.Error("expected closed circuit to allow requests")
	}
	if cb.State() != "closed" {
		t.Errorf("state = %q, want closed", cb.State())
	}
}

func TestCircuitBreakerOpensAfterFailures(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 5*time.Second)

	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}

	if cb.Allow() {
		t.Error("expected open circuit to reject requests")
	}
	if cb.State() != "open" {
		t.Errorf("state = %q, want open", cb.State())
	}
}

func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	cb := NewCircuitBreaker(3, 2, 5*time.Second)

	cb.RecordFailure()
	cb.RecordFailure()
	cb.RecordSuccess() // reset failure count

	if !cb.Allow() {
		t.Error("expected circuit to allow after success reset")
	}
}

func TestCircuitBreakerHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 10*time.Millisecond)

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != "open" {
		t.Fatalf("state = %q, want open", cb.State())
	}

	// Wait for open timeout.
	time.Sleep(15 * time.Millisecond)

	if !cb.Allow() {
		t.Error("expected half-open circuit to allow a probe request")
	}
	if cb.State() != "half-open" {
		t.Errorf("state = %q, want half-open", cb.State())
	}

	// Two successes should close the circuit.
	cb.RecordSuccess()
	cb.RecordSuccess()
	if cb.State() != "closed" {
		t.Errorf("state = %q, want closed after recovery", cb.State())
	}
}

func TestCircuitBreakerHalfOpenFailure(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 10*time.Millisecond)

	cb.RecordFailure()
	cb.RecordFailure()
	time.Sleep(15 * time.Millisecond)

	cb.Allow() // transition to half-open
	cb.RecordFailure() // should re-open

	if cb.State() != "open" {
		t.Errorf("state = %q, want open after half-open failure", cb.State())
	}
}
