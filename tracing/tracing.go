// Package tracing provides OpenTelemetry distributed tracing for the CDN edge server.
package tracing

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const tracerName = "github.com/rzawadzk/cdn-edge/tracing"

// Config holds configuration for the tracing subsystem.
type Config struct {
	// Enabled controls whether tracing is active. When false, Init returns a
	// noop shutdown and Middleware passes requests through unchanged.
	Enabled bool

	// Endpoint is the OTLP collector gRPC address (default "localhost:4317").
	Endpoint string

	// SampleRate is a value between 0.0 and 1.0 controlling the fraction of
	// traces that are sampled (default 1.0 = sample everything).
	SampleRate float64

	// Insecure disables TLS for the gRPC connection to the collector.
	Insecure bool
}

func (c *Config) defaults() {
	if c.Endpoint == "" {
		c.Endpoint = "localhost:4317"
	}
	if c.SampleRate <= 0 {
		c.SampleRate = 1.0
	}
	if c.SampleRate > 1.0 {
		c.SampleRate = 1.0
	}
}

// Init sets up the global OpenTelemetry tracer provider with an OTLP gRPC
// exporter. It returns a shutdown function that flushes pending spans.
// When cfg.Enabled is false it returns a noop shutdown and nil error.
func Init(serviceName string, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		// Install a noop provider so SpanFromContext still returns a valid (noop) span.
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	cfg.defaults()

	ctx := context.Background()

	// Build gRPC dial options.
	dialOpts := []grpc.DialOption{}
	if cfg.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithDialOption(dialOpts...),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create resource: %w", err)
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

// Middleware returns HTTP middleware that creates a span for each request and
// records standard HTTP and cache attributes once the handler completes.
func Middleware(next http.Handler) http.Handler {
	tracer := otel.Tracer(tracerName)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract any incoming trace context from request headers.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		spanName := r.Method + " " + r.URL.Path
		ctx, span := tracer.Start(ctx, spanName,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				semconv.HTTPMethodKey.String(r.Method),
				semconv.HTTPTargetKey.String(r.URL.RequestURI()),
				attribute.String("http.host", r.Host),
				attribute.String("http.url", r.URL.String()),
			),
		)
		defer span.End()

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r.WithContext(ctx))

		span.SetAttributes(
			semconv.HTTPStatusCodeKey.Int(rw.statusCode),
		)

		if cacheStatus := rw.Header().Get("X-Cache"); cacheStatus != "" {
			span.SetAttributes(attribute.String("cache.status", cacheStatus))
		}

		if rw.statusCode >= 500 {
			span.SetStatus(codes.Error, "HTTP "+strconv.Itoa(rw.statusCode))
		}
	})
}

// SpanFromContext returns the current span from the context. This is a
// convenience wrapper around trace.SpanFromContext.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// AddCacheEvent adds a cache hit/miss event to the current span.
func AddCacheEvent(ctx context.Context, cacheStatus string) {
	span := trace.SpanFromContext(ctx)
	span.AddEvent("cache", trace.WithAttributes(
		attribute.String("cache.status", cacheStatus),
	))
}

// responseWriter wraps http.ResponseWriter to capture the status code and
// expose the written headers after the handler completes.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	return rw.ResponseWriter.Write(b)
}
