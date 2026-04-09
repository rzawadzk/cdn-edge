package proxy

import (
	"testing"
)

func TestRoundRobinDistribution(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	b3 := &Backend{URL: "http://c"}
	bal := NewBalancer([]*Backend{b1, b2, b3}, RoundRobin)

	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		be, err := bal.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[be.URL]++
	}

	for _, url := range []string{"http://a", "http://b", "http://c"} {
		if counts[url] != 10 {
			t.Errorf("backend %s got %d requests, want 10", url, counts[url])
		}
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	b1 := &Backend{URL: "http://a", Weight: 2}
	b2 := &Backend{URL: "http://b", Weight: 1}
	bal := NewBalancer([]*Backend{b1, b2}, RoundRobin)

	// Weighted list should be [a, a, b], so in 30 requests: a=20, b=10
	counts := map[string]int{}
	for i := 0; i < 30; i++ {
		be, err := bal.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[be.URL]++
	}

	if counts["http://a"] != 20 {
		t.Errorf("backend a got %d, want 20", counts["http://a"])
	}
	if counts["http://b"] != 10 {
		t.Errorf("backend b got %d, want 10", counts["http://b"])
	}
}

func TestFailoverWhenBackendDown(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	bal := NewBalancer([]*Backend{b1, b2}, RoundRobin)

	bal.MarkDown(b1)

	// All requests should go to b2.
	for i := 0; i < 10; i++ {
		be, err := bal.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if be.URL != "http://b" {
			t.Errorf("got %s, want http://b", be.URL)
		}
	}
}

func TestAllBackendsDownReturnsError(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	bal := NewBalancer([]*Backend{b1, b2}, RoundRobin)

	bal.MarkDown(b1)
	bal.MarkDown(b2)

	_, err := bal.Next()
	if err != ErrNoHealthyBackends {
		t.Errorf("got err=%v, want ErrNoHealthyBackends", err)
	}
}

func TestMarkUpRestoresBackend(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	bal := NewBalancer([]*Backend{b1}, RoundRobin)

	bal.MarkDown(b1)
	_, err := bal.Next()
	if err != ErrNoHealthyBackends {
		t.Fatalf("expected error when all down")
	}

	bal.MarkUp(b1)
	be, err := bal.Next()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if be.URL != "http://a" {
		t.Errorf("got %s, want http://a", be.URL)
	}
}

func TestHealthyReturnsOnlyHealthy(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	b3 := &Backend{URL: "http://c"}
	bal := NewBalancer([]*Backend{b1, b2, b3}, RoundRobin)

	bal.MarkDown(b2)

	healthy := bal.Healthy()
	if len(healthy) != 2 {
		t.Fatalf("got %d healthy, want 2", len(healthy))
	}
	for _, h := range healthy {
		if h.URL == "http://b" {
			t.Error("b should not be in healthy list")
		}
	}
}

func TestRandomStrategy(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	bal := NewBalancer([]*Backend{b1, b2}, Random)

	counts := map[string]int{}
	for i := 0; i < 100; i++ {
		be, err := bal.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[be.URL]++
	}

	// Both should get at least some requests.
	if counts["http://a"] == 0 || counts["http://b"] == 0 {
		t.Errorf("random distribution too skewed: a=%d b=%d", counts["http://a"], counts["http://b"])
	}
}

func TestRandomStrategyFailover(t *testing.T) {
	b1 := &Backend{URL: "http://a"}
	b2 := &Backend{URL: "http://b"}
	bal := NewBalancer([]*Backend{b1, b2}, Random)

	bal.MarkDown(b1)

	for i := 0; i < 10; i++ {
		be, err := bal.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if be.URL != "http://b" {
			t.Errorf("got %s, want http://b", be.URL)
		}
	}
}

func TestDefaultWeightIsOne(t *testing.T) {
	b := &Backend{URL: "http://a"}
	_ = NewBalancer([]*Backend{b}, RoundRobin)
	if b.Weight != 1 {
		t.Errorf("default weight = %d, want 1", b.Weight)
	}
}

func TestEmptyBackendsReturnsError(t *testing.T) {
	bal := NewBalancer([]*Backend{}, RoundRobin)
	_, err := bal.Next()
	if err != ErrNoHealthyBackends {
		t.Errorf("got err=%v, want ErrNoHealthyBackends", err)
	}
}
