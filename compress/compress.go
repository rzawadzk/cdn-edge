package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

// Middleware transparently compresses responses with gzip when the client supports it.
// Brotli would require a third-party library; gzip is stdlib-only.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip if client doesn't accept gzip.
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)

		grw := &gzipResponseWriter{
			ResponseWriter: w,
			gz:             gz,
		}
		defer func() {
			grw.Close()
			gzipWriterPool.Put(gz)
		}()

		next.ServeHTTP(grw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz            *gzip.Writer
	headerWritten bool
	shouldCompress bool
	checked       bool
}

func (grw *gzipResponseWriter) check() {
	if grw.checked {
		return
	}
	grw.checked = true

	ct := grw.Header().Get("Content-Type")
	// Only compress text-like content types.
	grw.shouldCompress = isCompressible(ct)

	if grw.shouldCompress {
		grw.Header().Del("Content-Length") // will change after compression
		grw.Header().Set("Content-Encoding", "gzip")
		grw.Header().Add("Vary", "Accept-Encoding")
	}
}

func (grw *gzipResponseWriter) WriteHeader(code int) {
	grw.check()
	grw.headerWritten = true
	grw.ResponseWriter.WriteHeader(code)
}

func (grw *gzipResponseWriter) Write(b []byte) (int, error) {
	if !grw.headerWritten {
		// Sniff content type if not set.
		if grw.Header().Get("Content-Type") == "" {
			grw.Header().Set("Content-Type", http.DetectContentType(b))
		}
		grw.check()
		grw.headerWritten = true
	}
	if grw.shouldCompress {
		return grw.gz.Write(b)
	}
	return grw.ResponseWriter.Write(b)
}

func (grw *gzipResponseWriter) Close() {
	if grw.shouldCompress {
		grw.gz.Close()
	}
}

func (grw *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return grw.ResponseWriter
}

func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(enc) == "gzip" {
			return true
		}
	}
	return false
}

func isCompressible(ct string) bool {
	compressible := []string{
		"text/", "application/json", "application/javascript",
		"application/xml", "application/xhtml+xml", "image/svg+xml",
		"application/wasm",
	}
	ct = strings.ToLower(ct)
	for _, prefix := range compressible {
		if strings.Contains(ct, prefix) {
			return true
		}
	}
	return false
}
