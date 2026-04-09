package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type contextKey int

const requestIDKey contextKey = 0

// Log levels.
const (
	LevelDebug int32 = iota
	LevelInfo
	LevelError
)

// Logger writes structured JSON log lines.
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level atomic.Int32
}

// New creates a Logger that writes to stdout at info level.
func New() *Logger {
	l := &Logger{out: os.Stdout}
	l.level.Store(LevelInfo)
	return l
}

// SetLevel changes the log level at runtime.
func (l *Logger) SetLevel(level string) {
	switch level {
	case "debug":
		l.level.Store(LevelDebug)
	case "info":
		l.level.Store(LevelInfo)
	case "error":
		l.level.Store(LevelError)
	}
}

// Level returns the current log level as a string.
func (l *Logger) Level() string {
	switch l.level.Load() {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelError:
		return "error"
	}
	return "info"
}

// AccessEntry is one structured access log line.
type AccessEntry struct {
	Timestamp   string `json:"ts"`
	Level       string `json:"level"`
	RequestID   string `json:"request_id"`
	Method      string `json:"method"`
	Host        string `json:"host"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	BytesSent   int    `json:"bytes_sent"`
	DurationMs  float64 `json:"duration_ms"`
	CacheStatus string `json:"cache_status"`
	ClientIP    string `json:"client_ip"`
	UserAgent   string `json:"user_agent"`
}

// LogEntry is a general structured log line.
type LogEntry struct {
	Timestamp string `json:"ts"`
	Level     string `json:"level"`
	Msg       string `json:"msg"`
	RequestID string `json:"request_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (l *Logger) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	data = append(data, '\n')
	l.mu.Lock()
	l.out.Write(data)
	l.mu.Unlock()
}

// Access logs a completed HTTP request (always logged at info level or above).
func (l *Logger) Access(entry AccessEntry) {
	if l.level.Load() > LevelInfo {
		return
	}
	entry.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	entry.Level = "info"
	l.write(entry)
}

// Debug logs a debug message (only if level is debug).
func (l *Logger) Debug(msg string, requestID ...string) {
	if l.level.Load() > LevelDebug {
		return
	}
	e := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     "debug",
		Msg:       msg,
	}
	if len(requestID) > 0 {
		e.RequestID = requestID[0]
	}
	l.write(e)
}

// Info logs an informational message.
func (l *Logger) Info(msg string, requestID ...string) {
	if l.level.Load() > LevelInfo {
		return
	}
	e := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     "info",
		Msg:       msg,
	}
	if len(requestID) > 0 {
		e.RequestID = requestID[0]
	}
	l.write(e)
}

// Error logs an error message (always logged).
func (l *Logger) Error(msg string, err error, requestID ...string) {
	e := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     "error",
		Msg:       msg,
	}
	if err != nil {
		e.Error = err.Error()
	}
	if len(requestID) > 0 {
		e.RequestID = requestID[0]
	}
	l.write(e)
}

// GenerateRequestID creates a random 8-byte hex request ID.
func GenerateRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// WithRequestID returns a new context with the given request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// GetRequestID extracts the request ID from context.
func GetRequestID(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// responseWriter wraps http.ResponseWriter to capture status and bytes written.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytesWritten int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Middleware returns an HTTP middleware that adds request IDs and logs access.
func (l *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = GenerateRequestID()
		}

		ctx := WithRequestID(r.Context(), reqID)
		r = r.WithContext(ctx)

		rw := &responseWriter{ResponseWriter: w, status: 200}
		rw.Header().Set("X-Request-ID", reqID)

		start := time.Now()
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		l.Access(AccessEntry{
			RequestID:   reqID,
			Method:      r.Method,
			Host:        r.Host,
			Path:        r.URL.Path,
			Status:      rw.status,
			BytesSent:   rw.bytesWritten,
			DurationMs:  float64(duration.Microseconds()) / 1000.0,
			CacheStatus: rw.Header().Get("X-Cache"),
			ClientIP:    clientIP(r),
			UserAgent:   r.UserAgent(),
		})
	})
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	return r.RemoteAddr
}
