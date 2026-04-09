package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
)

var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return w
	},
}

var brotliWriterPool = sync.Pool{
	New: func() any {
		return brotli.NewWriterOptions(io.Discard, brotli.WriterOptions{Quality: 4})
	},
}

type encoding int

const (
	encodingNone encoding = iota
	encodingBrotli
	encodingGzip
)

// Middleware transparently compresses responses with brotli or gzip when the
// client supports it. Brotli is preferred over gzip when both are accepted.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := selectEncoding(r)
		if enc == encodingNone {
			next.ServeHTTP(w, r)
			return
		}

		switch enc {
		case encodingBrotli:
			bw := brotliWriterPool.Get().(*brotli.Writer)
			bw.Reset(w)

			crw := &compressResponseWriter{
				ResponseWriter: w,
				writer:         bw,
				encoding:       "br",
			}
			defer func() {
				crw.Close()
				brotliWriterPool.Put(bw)
			}()

			next.ServeHTTP(crw, r)

		case encodingGzip:
			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(w)

			crw := &compressResponseWriter{
				ResponseWriter: w,
				writer:         gz,
				encoding:       "gzip",
			}
			defer func() {
				crw.Close()
				gzipWriterPool.Put(gz)
			}()

			next.ServeHTTP(crw, r)
		}
	})
}

// compressWriter is the interface satisfied by both gzip.Writer and brotli.Writer.
type compressWriter interface {
	io.Writer
	Close() error
	Flush() error
}

type compressResponseWriter struct {
	http.ResponseWriter
	writer         compressWriter
	encoding       string
	headerWritten  bool
	shouldCompress bool
	checked        bool
}

func (crw *compressResponseWriter) check() {
	if crw.checked {
		return
	}
	crw.checked = true

	ct := crw.Header().Get("Content-Type")
	// Only compress text-like content types.
	crw.shouldCompress = isCompressible(ct)

	if crw.shouldCompress {
		crw.Header().Del("Content-Length") // will change after compression
		crw.Header().Set("Content-Encoding", crw.encoding)
		crw.Header().Add("Vary", "Accept-Encoding")
	}
}

func (crw *compressResponseWriter) WriteHeader(code int) {
	crw.check()
	crw.headerWritten = true
	crw.ResponseWriter.WriteHeader(code)
}

func (crw *compressResponseWriter) Write(b []byte) (int, error) {
	if !crw.headerWritten {
		// Sniff content type if not set.
		if crw.Header().Get("Content-Type") == "" {
			crw.Header().Set("Content-Type", http.DetectContentType(b))
		}
		crw.check()
		crw.headerWritten = true
	}
	if crw.shouldCompress {
		return crw.writer.Write(b)
	}
	return crw.ResponseWriter.Write(b)
}

func (crw *compressResponseWriter) Close() {
	if crw.shouldCompress {
		crw.writer.Close()
	}
}

func (crw *compressResponseWriter) Unwrap() http.ResponseWriter {
	return crw.ResponseWriter
}

// selectEncoding picks the best encoding the client supports.
// Brotli is preferred over gzip when both are accepted.
func selectEncoding(r *http.Request) encoding {
	ae := r.Header.Get("Accept-Encoding")
	hasBrotli := false
	hasGzip := false
	for _, enc := range strings.Split(ae, ",") {
		// Strip quality values (e.g. "gzip;q=0.8") for simple matching.
		enc = strings.TrimSpace(enc)
		enc, _, _ = strings.Cut(enc, ";")
		enc = strings.TrimSpace(enc)
		switch enc {
		case "br":
			hasBrotli = true
		case "gzip":
			hasGzip = true
		}
	}
	if hasBrotli {
		return encodingBrotli
	}
	if hasGzip {
		return encodingGzip
	}
	return encodingNone
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
