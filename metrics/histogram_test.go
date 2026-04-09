package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHistogramRecord(t *testing.T) {
	rm := NewRequestMetrics()
	rm.Record(5*time.Millisecond, 200)
	rm.Record(50*time.Millisecond, 200)
	rm.Record(500*time.Millisecond, 404)
	rm.Record(2*time.Second, 503)

	w := httptest.NewRecorder()
	rm.WritePrometheus(w)
	body := w.Body.String()

	expected := []string{
		"cdn_request_duration_ms_bucket",
		"cdn_request_duration_ms_sum",
		"cdn_request_duration_ms_count 4",
		`cdn_responses_total{code="2xx"} 2`,
		`cdn_responses_total{code="4xx"} 1`,
		`cdn_responses_total{code="5xx"} 1`,
	}
	for _, s := range expected {
		if !strings.Contains(body, s) {
			t.Errorf("prometheus output missing %q", s)
		}
	}
}

func TestHistogramPercentile(t *testing.T) {
	rm := NewRequestMetrics()
	// 100 requests at 1ms
	for i := 0; i < 90; i++ {
		rm.Record(1*time.Millisecond, 200)
	}
	// 10 requests at 500ms
	for i := 0; i < 10; i++ {
		rm.Record(500*time.Millisecond, 200)
	}

	p50 := rm.Percentile(50)
	p99 := rm.Percentile(99)

	if p50 > 10 {
		t.Errorf("p50 = %f, expected <=10ms", p50)
	}
	if p99 < 100 {
		t.Errorf("p99 = %f, expected >= 100ms", p99)
	}
}

func TestStatusCodeCounters(t *testing.T) {
	rm := NewRequestMetrics()
	rm.Record(1*time.Millisecond, 200)
	rm.Record(1*time.Millisecond, 301)
	rm.Record(1*time.Millisecond, 404)
	rm.Record(1*time.Millisecond, 500)

	if rm.status2xx.Load() != 1 {
		t.Errorf("2xx = %d, want 1", rm.status2xx.Load())
	}
	if rm.status3xx.Load() != 1 {
		t.Errorf("3xx = %d, want 1", rm.status3xx.Load())
	}
	if rm.status4xx.Load() != 1 {
		t.Errorf("4xx = %d, want 1", rm.status4xx.Load())
	}
	if rm.status5xx.Load() != 1 {
		t.Errorf("5xx = %d, want 1", rm.status5xx.Load())
	}
}

func TestRequestMetricsMiddleware(t *testing.T) {
	rm := NewRequestMetrics()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
	h := NewRequestMetricsMiddleware(rm, inner)

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	}

	if rm.totalCount.Load() != 10 {
		t.Errorf("total count = %d, want 10", rm.totalCount.Load())
	}
	if rm.status2xx.Load() != 10 {
		t.Errorf("2xx count = %d, want 10", rm.status2xx.Load())
	}
}
