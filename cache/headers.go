package cache

import (
	"bytes"
	"encoding/gob"
	"net/http"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Byte prefix tags for CompressedHeader storage format.
const (
	hdrFlagRaw  byte = 0x00 // remainder is gob-encoded http.Header
	hdrFlagZstd byte = 0x01 // remainder is zstd(gob-encoded http.Header)
)

// Package-level zstd encoder/decoder. These are safe for concurrent use by
// multiple goroutines (see klauspost/compress/zstd docs), so we keep a single
// instance of each. A sync.Pool wraps the scratch buffers we hand to them.
var (
	hdrEncoder *zstd.Encoder
	hdrDecoder *zstd.Decoder
)

func init() {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		panic("cache: failed to init zstd encoder: " + err.Error())
	}
	hdrEncoder = enc

	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
	)
	if err != nil {
		panic("cache: failed to init zstd decoder: " + err.Error())
	}
	hdrDecoder = dec
}

// bufPool reuses bytes.Buffer instances for gob encoding of http.Header.
var hdrBufPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// CompressedHeader stores an HTTP header in zstd-compressed form to
// reduce cache memory footprint. Decompression happens lazily on access.
//
// The stored byte slice is laid out as:
//
//	[flag byte][payload]
//
// where flag = 0x00 means the payload is a raw gob-encoded http.Header
// (used when compression would not shrink the data) and flag = 0x01 means
// the payload is the zstd-compressed gob-encoded http.Header.
type CompressedHeader struct {
	data []byte
}

// NewCompressedHeader serializes and compresses h. Returns nil if h is empty.
func NewCompressedHeader(h http.Header) *CompressedHeader {
	if len(h) == 0 {
		return nil
	}

	// gob-encode the header into a pooled buffer.
	buf := hdrBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Don't retain unbounded buffers in the pool.
		if buf.Cap() <= 64<<10 {
			hdrBufPool.Put(buf)
		}
	}()

	enc := gob.NewEncoder(buf)
	if err := enc.Encode(map[string][]string(h)); err != nil {
		// Should never happen for http.Header (map[string][]string).
		// Fall back to nil so callers know there's no compressed copy.
		return nil
	}
	raw := buf.Bytes()

	// Compress with zstd. EncodeAll reuses internal buffers on the shared encoder.
	compressed := hdrEncoder.EncodeAll(raw, make([]byte, 0, len(raw)/2+16))

	// If compression did not help (tiny headers, already-high-entropy values),
	// store uncompressed with the raw flag byte. +1 accounts for flag overhead.
	if len(compressed)+1 >= len(raw)+1 {
		out := make([]byte, 1+len(raw))
		out[0] = hdrFlagRaw
		copy(out[1:], raw)
		return &CompressedHeader{data: out}
	}

	out := make([]byte, 1+len(compressed))
	out[0] = hdrFlagZstd
	copy(out[1:], compressed)
	return &CompressedHeader{data: out}
}

// Decode decompresses and deserializes the header. The returned map is a
// fresh copy that callers may mutate freely without affecting the compressed
// storage.
func (ch *CompressedHeader) Decode() http.Header {
	if ch == nil || len(ch.data) == 0 {
		return http.Header{}
	}

	flag := ch.data[0]
	payload := ch.data[1:]

	var raw []byte
	switch flag {
	case hdrFlagRaw:
		raw = payload
	case hdrFlagZstd:
		decoded, err := hdrDecoder.DecodeAll(payload, nil)
		if err != nil {
			return http.Header{}
		}
		raw = decoded
	default:
		return http.Header{}
	}

	var m map[string][]string
	dec := gob.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return http.Header{}
	}

	// Clone the value slices so callers can mutate without affecting any
	// future Decode() result (gob already gave us a fresh map, but be explicit).
	out := make(http.Header, len(m))
	for k, v := range m {
		vv := make([]string, len(v))
		copy(vv, v)
		out[k] = vv
	}
	return out
}

// Size returns the compressed byte size (including the flag byte prefix).
func (ch *CompressedHeader) Size() int {
	if ch == nil {
		return 0
	}
	return len(ch.data)
}

// Get returns a single header value without the caller having to manage the
// returned map. Returns "" if the key is not present.
//
// TODO: this currently performs a full Decode(). A dictionary-based or
// streaming lookup that avoids materializing the whole map is possible and
// would speed up hot-path checks like Cache-Control/ETag. Pingora does this
// via trained zstd dictionaries.
func (ch *CompressedHeader) Get(key string) string {
	if ch == nil {
		return ""
	}
	return ch.Decode().Get(key)
}
