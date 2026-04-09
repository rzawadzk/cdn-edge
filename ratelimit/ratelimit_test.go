package ratelimit

import (
	"testing"
)

func TestAllowWithinLimit(t *testing.T) {
	l := New(10, 10)
	for i := 0; i < 10; i++ {
		if !l.Allow("1.2.3.4") {
			t.Errorf("request %d should be allowed", i)
		}
	}
}

func TestDenyOverLimit(t *testing.T) {
	l := New(10, 5) // 5 burst, 10 rps
	// Exhaust burst.
	for i := 0; i < 5; i++ {
		l.Allow("1.2.3.4")
	}
	// Next should be denied (no time has passed for refill).
	if l.Allow("1.2.3.4") {
		t.Error("expected request to be denied after burst exhaustion")
	}
}

func TestDifferentIPsIndependent(t *testing.T) {
	l := New(10, 2)
	l.Allow("1.1.1.1")
	l.Allow("1.1.1.1")
	// 1.1.1.1 is out of burst, but 2.2.2.2 should be fine.
	if !l.Allow("2.2.2.2") {
		t.Error("different IP should have its own bucket")
	}
}
