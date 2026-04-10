package cache

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// ErrStreamAborted is returned to readers when the streaming download fails.
var ErrStreamAborted = errors.New("streaming entry: download aborted")

// StreamingEntry represents a cache entry that is still being written.
// Concurrent readers can consume bytes as they arrive from the origin.
type StreamingEntry struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	done     chan struct{} // closed when fully downloaded
	err      error        // set if download failed
	header   http.Header  // available immediately
	status   int          // HTTP status code
	complete bool
}

// NewStreamingEntry creates a new StreamingEntry with response headers
// available immediately. The caller should then Write() body chunks and
// call Close() when the download is finished.
func NewStreamingEntry(header http.Header, status int) *StreamingEntry {
	se := &StreamingEntry{
		header: header,
		status: status,
		done:   make(chan struct{}),
	}
	se.cond = sync.NewCond(&se.mu)
	return se
}

// Header returns the HTTP response headers.
func (se *StreamingEntry) Header() http.Header {
	return se.header
}

// StatusCode returns the HTTP status code.
func (se *StreamingEntry) StatusCode() int {
	return se.status
}

// Write appends bytes to the streaming buffer and wakes up waiting readers.
func (se *StreamingEntry) Write(p []byte) (int, error) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.complete {
		return 0, errors.New("streaming entry: write after close")
	}
	se.buf = append(se.buf, p...)
	se.cond.Broadcast()
	return len(p), nil
}

// Close marks the entry as complete. If err is non-nil, readers will
// receive that error instead of EOF once they exhaust buffered data.
func (se *StreamingEntry) Close(err error) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.complete {
		return
	}
	se.complete = true
	se.err = err
	se.cond.Broadcast()
	close(se.done)
}

// IsComplete returns true if the download has finished (successfully or not).
func (se *StreamingEntry) IsComplete() bool {
	se.mu.Lock()
	defer se.mu.Unlock()
	return se.complete
}

// Err returns the error set on Close, if any.
func (se *StreamingEntry) Err() error {
	se.mu.Lock()
	defer se.mu.Unlock()
	return se.err
}

// Len returns the current buffer length.
func (se *StreamingEntry) Len() int {
	se.mu.Lock()
	defer se.mu.Unlock()
	return len(se.buf)
}

// ToEntry converts the completed StreamingEntry to a regular *Entry for
// permanent cache storage. Panics if called before Close().
func (se *StreamingEntry) ToEntry(ttl, swr time.Duration, etag, lastMod, varyKey string) *Entry {
	se.mu.Lock()
	defer se.mu.Unlock()
	if !se.complete {
		panic("StreamingEntry.ToEntry called before Close")
	}
	if se.err != nil {
		return nil
	}
	body := make([]byte, len(se.buf))
	copy(body, se.buf)
	return &Entry{
		Body:                 body,
		Header:               se.header.Clone(),
		StatusCode:           se.status,
		StoredAt:             time.Now(),
		TTL:                  ttl,
		ETag:                 etag,
		LastMod:              lastMod,
		VaryKey:              varyKey,
		StaleWhileRevalidate: swr,
	}
}

// NewReader returns a StreamingReader that reads from the given byte offset.
// The reader will block when it catches up to the writer, and return io.EOF
// (or the close error) once the entry is complete and all bytes are consumed.
func (se *StreamingEntry) NewReader(offset int) *StreamingReader {
	return &StreamingReader{
		se:     se,
		offset: offset,
	}
}

// StreamingReader reads from a StreamingEntry, blocking when it catches up
// to the write position and resuming when more data arrives.
type StreamingReader struct {
	se     *StreamingEntry
	offset int
}

// Read implements io.Reader. It blocks if no new data is available yet,
// and returns io.EOF when the entry is complete and all bytes are consumed.
func (sr *StreamingReader) Read(p []byte) (int, error) {
	sr.se.mu.Lock()
	defer sr.se.mu.Unlock()

	for {
		// Data available to read?
		if sr.offset < len(sr.se.buf) {
			n := copy(p, sr.se.buf[sr.offset:])
			sr.offset += n
			return n, nil
		}

		// No more data and entry is complete.
		if sr.se.complete {
			if sr.se.err != nil {
				return 0, sr.se.err
			}
			return 0, io.EOF
		}

		// Wait for more data or completion.
		sr.se.cond.Wait()
	}
}
