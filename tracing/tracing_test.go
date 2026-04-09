package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// setupTestTracer installs an in-memory span exporter and returns it along
// with a cleanup function that restores the previous provider.
func setupTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func TestMiddleware_CreatesSpanWithAttributes(t *testing.T) {
	exp := setupTestTracer(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	handler := Middleware(inner)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/static/logo.png?v=2", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span, got none")
	}

	span := spans[0]
	if span.Name != "GET /static/logo.png" {
		t.Errorf("unexpected span name: %s", span.Name)
	}

	attrs := make(map[string]interface{})
	for _, a := range span.Attributes {
		attrs[string(a.Key)] = a.Value.AsInterface()
	}

	assertAttr := func(key string, want interface{}) {
		t.Helper()
		got, ok := attrs[key]
		if !ok {
			t.Errorf("missing attribute %q", key)
			return
		}
		if got != want {
			t.Errorf("attribute %q = %v, want %v", key, got, want)
		}
	}

	assertAttr("http.method", "GET")
	assertAttr("http.status_code", int64(200))
	assertAttr("cache.status", "HIT")
	assertAttr("http.host", "example.com")
}

func TestMiddleware_Records5xxAsError(t *testing.T) {
	exp := setupTestTracer(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/fail", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}

	span := spans[0]

	// Check status code attribute.
	found := false
	for _, a := range span.Attributes {
		if string(a.Key) == "http.status_code" && a.Value.AsInt64() == 502 {
			found = true
		}
	}
	if !found {
		t.Error("expected http.status_code=502 attribute")
	}

	// The span status should indicate an error.
	if span.Status.Code != codes.Error {
		t.Errorf("expected error status code, got %v", span.Status.Code)
	}
}

func TestMiddleware_NoCacheHeader(t *testing.T) {
	exp := setupTestTracer(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/no-cache", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}

	// cache.status should NOT be set when X-Cache header is absent.
	for _, a := range spans[0].Attributes {
		if string(a.Key) == "cache.status" {
			t.Error("cache.status should not be set when X-Cache header is absent")
		}
	}
}

func TestDisabledConfig_ReturnsNoop(t *testing.T) {
	shutdown, err := Init("test-service", Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init with disabled config should not error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("noop shutdown should not error: %v", err)
	}

	// The provider should be a noop — spans from it should be non-recording.
	tracer := otel.Tracer("test")
	_, span := tracer.Start(context.Background(), "test-span")
	if span.IsRecording() {
		t.Error("expected noop span to be non-recording")
	}
	span.End()
}

func TestSpanFromContext_ReturnsValidSpan(t *testing.T) {
	setupTestTracer(t)

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "parent")
	defer span.End()

	got := SpanFromContext(ctx)
	if got.SpanContext().TraceID() != span.SpanContext().TraceID() {
		t.Error("SpanFromContext should return the span from the context")
	}
	if !got.SpanContext().IsValid() {
		t.Error("SpanFromContext should return a valid span")
	}
}

func TestSpanFromContext_NoSpan(t *testing.T) {
	// Should not panic when no span is in context.
	span := SpanFromContext(context.Background())
	if span == nil {
		t.Error("SpanFromContext should never return nil")
	}
}

func TestAddCacheEvent(t *testing.T) {
	exp := setupTestTracer(t)

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "cache-op")

	AddCacheEvent(ctx, "MISS")
	span.End()

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}

	events := spans[0].Events
	if len(events) == 0 {
		t.Fatal("expected at least one event")
	}

	evt := events[0]
	if evt.Name != "cache" {
		t.Errorf("event name = %q, want %q", evt.Name, "cache")
	}

	found := false
	for _, a := range evt.Attributes {
		if string(a.Key) == "cache.status" && a.Value.AsString() == "MISS" {
			found = true
		}
	}
	if !found {
		t.Error("expected cache.status=MISS attribute on event")
	}
}

func TestResponseWriter_CapturesStatusCode(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	rw.WriteHeader(http.StatusNotFound)
	if rw.statusCode != http.StatusNotFound {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusNotFound)
	}

	// Second call should not overwrite.
	rw.WriteHeader(http.StatusInternalServerError)
	if rw.statusCode != http.StatusNotFound {
		t.Errorf("statusCode = %d after second WriteHeader, want %d", rw.statusCode, http.StatusNotFound)
	}
}

func TestResponseWriter_DefaultStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}

	// Writing without explicit WriteHeader should keep default 200.
	rw.Write([]byte("hello"))
	if rw.statusCode != http.StatusOK {
		t.Errorf("statusCode = %d, want %d", rw.statusCode, http.StatusOK)
	}
}

func TestMiddleware_PropagatesContext(t *testing.T) {
	setupTestTracer(t)

	var innerSpan trace.Span
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerSpan = SpanFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if innerSpan == nil {
		t.Fatal("inner handler should have access to span via context")
	}
	if !innerSpan.SpanContext().IsValid() {
		t.Error("inner span should have a valid span context")
	}
}
