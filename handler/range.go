package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rzawadzk/cdn-edge/cache"
)

// parseRange parses a Range header value like "bytes=0-499" and returns start, end
// (inclusive) indices. Returns ok=false for multi-range or unparseable ranges.
// The end value is clamped to contentLength-1.
func parseRange(rangeHeader string, contentLength int) (start, end int, ok bool) {
	if contentLength <= 0 {
		return 0, 0, false
	}

	// Must start with "bytes=".
	const prefix = "bytes="
	if !strings.HasPrefix(rangeHeader, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(rangeHeader, prefix)

	// Reject multi-range (contains comma).
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}

	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, false
	}

	dashIdx := strings.Index(spec, "-")
	if dashIdx < 0 {
		return 0, 0, false
	}

	startStr := strings.TrimSpace(spec[:dashIdx])
	endStr := strings.TrimSpace(spec[dashIdx+1:])

	// Suffix range: bytes=-500 means last 500 bytes.
	if startStr == "" {
		suffix, err := strconv.Atoi(endStr)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		start = contentLength - suffix
		if start < 0 {
			start = 0
		}
		return start, contentLength - 1, true
	}

	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 {
		return 0, 0, false
	}

	if start >= contentLength {
		return 0, 0, false
	}

	// Open-ended range: bytes=500-
	if endStr == "" {
		return start, contentLength - 1, true
	}

	end, err = strconv.Atoi(endStr)
	if err != nil || end < start {
		return 0, 0, false
	}

	// Clamp end to content length.
	if end >= contentLength {
		end = contentLength - 1
	}

	return start, end, true
}

// serveRange writes a 206 Partial Content response from a cached entry.
func serveRange(w http.ResponseWriter, entry *cache.Entry, start, end int) {
	total := len(entry.Body)

	// Copy relevant headers from the cached entry. GetHeader() decodes the
	// compressed form on demand when header compression is enabled.
	for k, vals := range entry.GetHeader() {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.Header().Set("X-Cache", "HIT")

	// Remove Content-Length from origin if it was set (we override above).
	// Already handled by Set above.

	w.WriteHeader(http.StatusPartialContent)
	w.Write(entry.Body[start : end+1])
}
