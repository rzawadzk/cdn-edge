package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cespare/xxhash/v2"
)

// ---------- Entry (unchanged) ----------

// Entry represents a cached HTTP response.
//
// Headers may be stored either in Header (uncompressed, fastest access) or
// in CompressedHeader (zstd-compressed, smaller memory footprint). Callers
// should use GetHeader() which returns whichever is populated. When the
// cache is configured with Options.CompressHeaders, Put() will compress
// Header and clear the raw field to save RAM.
type Entry struct {
	Body             []byte
	Header           http.Header
	CompressedHeader *CompressedHeader
	StatusCode       int
	StoredAt         time.Time
	TTL              time.Duration
	ETag             string
	LastMod          string
	VaryKey          string // secondary key derived from Vary headers
	StaleWhileRevalidate time.Duration
}

// GetHeader returns the entry's headers, decoding the compressed form on
// demand if Header is nil. The returned map is safe to read; callers must
// clone it before mutating (Decode() already returns a fresh map, but Header
// may alias the original response for uncompressed entries).
func (e *Entry) GetHeader() http.Header {
	if e == nil {
		return nil
	}
	if e.Header != nil {
		return e.Header
	}
	if e.CompressedHeader != nil {
		return e.CompressedHeader.Decode()
	}
	return http.Header{}
}

// IsExpired returns true if this entry has passed its TTL.
func (e *Entry) IsExpired() bool {
	if e.TTL <= 0 {
		return false
	}
	return time.Since(e.StoredAt) > e.TTL
}

// IsStaleServable returns true if the entry is expired but within stale-while-revalidate window.
func (e *Entry) IsStaleServable() bool {
	if e.StaleWhileRevalidate <= 0 || e.TTL <= 0 {
		return false
	}
	age := time.Since(e.StoredAt)
	return age > e.TTL && age <= e.TTL+e.StaleWhileRevalidate
}

// ---------- Stats ----------

// Stats holds cache hit/miss counters.
type Stats struct {
	Hits       atomic.Int64
	Misses     atomic.Int64
	Evicts     atomic.Int64
	StaleHits  atomic.Int64
}

// ---------- Options ----------

// Options configures the cache.
type Options struct {
	MaxItems        int
	MaxEntryBytes   int64
	MaxKeyLen       int
	DiskDir         string
	DiskMaxBytes    int64
	NumShards       int  // number of shards (default 256, must be power of 2)
	CompressHeaders bool // zstd-compress cached HTTP headers to reduce RAM use
}

// ---------- Shard ----------

// cacheShard is a single LRU partition with its own lock.
type cacheShard struct {
	mu        sync.RWMutex
	items     map[string]*list.Element
	eviction  *list.List
	maxItems  int
}

type lruItem struct {
	key   string
	entry *Entry
}

func newShard(maxItems int) *cacheShard {
	return &cacheShard{
		items:    make(map[string]*list.Element),
		eviction: list.New(),
		maxItems: maxItems,
	}
}

// ---------- Cache (sharded + TinyLFU) ----------

const defaultNumShards = 256

// Cache is a sharded two-tier cache: in-memory LRU shards with TinyLFU
// admission backed by optional disk storage. The disk tier is shared.
type Cache struct {
	shards          []*cacheShard
	numShards       uint64
	shardMask       uint64 // numShards - 1
	maxEntryBytes   int64
	maxKeyLen       int
	diskDir         string
	diskMaxBytes    int64
	diskMu          sync.Mutex // protects disk operations
	diskUsedBytes   int64
	stats           Stats
	lfu             *TinyLFU
	compressHeaders bool
}

// New creates a cache with the given options.
func New(maxItems int, maxEntryBytes int64, diskDir string, diskMaxBytes int64) (*Cache, error) {
	return NewWithOptions(Options{
		MaxItems:      maxItems,
		MaxEntryBytes: maxEntryBytes,
		DiskDir:       diskDir,
		DiskMaxBytes:  diskMaxBytes,
	})
}

// NewWithOptions creates a cache from Options.
func NewWithOptions(opts Options) (*Cache, error) {
	numShards := opts.NumShards
	if numShards <= 0 {
		numShards = defaultNumShards
	}
	// Round up to power of two.
	numShards = int(nextPow2(uint64(numShards)))

	maxItems := opts.MaxItems
	if maxItems < 1 {
		maxItems = 1
	}
	// For small caches, collapse to a single shard so the aggregate capacity
	// equals MaxItems exactly. Sharding only pays off above ~64 items.
	if maxItems < 64 {
		numShards = 1
	}
	// Never have more shards than items.
	for numShards > maxItems {
		numShards /= 2
	}
	if numShards < 1 {
		numShards = 1
	}

	if opts.DiskDir != "" {
		if err := os.MkdirAll(opts.DiskDir, 0o755); err != nil {
			return nil, fmt.Errorf("cache: create disk dir: %w", err)
		}
	}

	// Distribute capacity across shards, spreading the remainder across the
	// first N shards so the total matches maxItems exactly.
	perShard := maxItems / numShards
	remainder := maxItems % numShards

	c := &Cache{
		shards:          make([]*cacheShard, numShards),
		numShards:       uint64(numShards),
		shardMask:       uint64(numShards) - 1,
		maxEntryBytes:   opts.MaxEntryBytes,
		maxKeyLen:       opts.MaxKeyLen,
		diskDir:         opts.DiskDir,
		diskMaxBytes:    opts.DiskMaxBytes,
		lfu:             newTinyLFU(maxItems),
		compressHeaders: opts.CompressHeaders,
	}
	for i := range c.shards {
		cap := perShard
		if i < remainder {
			cap++
		}
		c.shards[i] = newShard(cap)
	}

	if opts.DiskDir != "" {
		c.diskUsedBytes = c.calculateDiskUsage()
	}

	return c, nil
}

func (c *Cache) getShard(key string) *cacheShard {
	h := xxhash.Sum64String(key)
	return c.shards[h&c.shardMask]
}

// ---------- NormalizeKey (unchanged) ----------

var builderPool = sync.Pool{
	New: func() any {
		b := &strings.Builder{}
		b.Grow(256)
		return b
	},
}

// NormalizeKey builds a Vary-aware cache key from the primary key and request headers.
func NormalizeKey(host, path string, query string, varyHeader string, reqHeaders http.Header) string {
	// Fast path: no query, no vary, host already lowercase — just concatenate.
	if query == "" && varyHeader == "" {
		if isLowerASCII(host) {
			return host + path
		}
		return strings.ToLower(host) + path
	}

	sb := builderPool.Get().(*strings.Builder)
	sb.Reset()

	// Normalize: lowercase host + path.
	if isLowerASCII(host) {
		sb.WriteString(host)
	} else {
		for i := 0; i < len(host); i++ {
			c := host[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			sb.WriteByte(c)
		}
	}
	sb.WriteString(path)

	if query != "" {
		sb.WriteByte('?')
		writeSortedQuery(sb, query)
	}

	if varyHeader == "" {
		result := sb.String()
		builderPool.Put(sb)
		return result
	}

	// Build secondary key from Vary header values.
	vary := parseVary(varyHeader)
	if len(vary) > 0 {
		sb.WriteString("|vary:")
		for i, h := range vary {
			if i > 0 {
				sb.WriteByte('&')
			}
			sb.WriteString(h)
			sb.WriteByte('=')
			sb.WriteString(reqHeaders.Get(h))
		}
	}
	result := sb.String()
	builderPool.Put(sb)
	return result
}

func isLowerASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return false
		}
	}
	return true
}

func sortQuery(q string) string {
	parts := strings.Split(q, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func writeSortedQuery(sb *strings.Builder, q string) {
	if !strings.Contains(q, "&") {
		sb.WriteString(q)
		return
	}
	parts := strings.Split(q, "&")
	sort.Strings(parts)
	for i, p := range parts {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(p)
	}
}

func parseVary(vary string) []string {
	if vary == "*" {
		return nil
	}
	var headers []string
	for _, h := range strings.Split(vary, ",") {
		h = strings.TrimSpace(strings.ToLower(h))
		if h != "" {
			headers = append(headers, http.CanonicalHeaderKey(h))
		}
	}
	sort.Strings(headers)
	return headers
}

// ---------- Get ----------

// Get retrieves an entry by key. Returns nil on miss.
// If the entry is stale but within stale-while-revalidate, it is still returned
// (caller should check IsStaleServable and trigger async revalidation).
func (c *Cache) Get(key string) *Entry {
	// Always record frequency for TinyLFU admission decisions.
	c.lfu.Increment(key)

	shard := c.getShard(key)

	// Take the write lock directly: we need it to move the element to the
	// front on a hit, and dereferencing elem.Value lock-free races with a
	// concurrent Put() that rewrites the lruItem's entry field in place.
	shard.mu.Lock()
	elem, ok := shard.items[key]
	if ok {
		entry := elem.Value.(*lruItem).entry
		if entry.IsExpired() {
			if entry.IsStaleServable() {
				shard.eviction.MoveToFront(elem)
				shard.mu.Unlock()
				c.stats.StaleHits.Add(1)
				return entry
			}
			// Remove expired entry while we still hold the lock.
			shard.eviction.Remove(elem)
			delete(shard.items, key)
			shard.mu.Unlock()
			if c.diskDir != "" {
				c.removeFromDisk(key)
			}
			c.stats.Misses.Add(1)
			return nil
		}
		shard.eviction.MoveToFront(elem)
		shard.mu.Unlock()
		c.stats.Hits.Add(1)
		return entry
	}
	shard.mu.Unlock()

	// Try disk.
	if c.diskDir != "" {
		entry, err := c.loadFromDisk(key)
		if err == nil && entry != nil {
			if entry.IsExpired() && !entry.IsStaleServable() {
				_ = c.removeFromDisk(key)
				c.stats.Misses.Add(1)
				return nil
			}
			c.putInShard(shard, key, entry)
			if entry.IsExpired() {
				c.stats.StaleHits.Add(1)
			} else {
				c.stats.Hits.Add(1)
			}
			return entry
		}
	}

	c.stats.Misses.Add(1)
	return nil
}

// ---------- Put ----------

// Put stores an entry in the cache. Respects maxEntryBytes and maxKeyLen.
// When CompressHeaders is enabled, the entry's Header is compressed into
// CompressedHeader and the raw map is cleared to save memory.
func (c *Cache) Put(key string, entry *Entry) {
	if c.maxEntryBytes > 0 && int64(len(entry.Body)) > c.maxEntryBytes {
		return
	}
	if c.maxKeyLen > 0 && len(key) > c.maxKeyLen {
		return
	}
	if c.compressHeaders && entry.Header != nil && entry.CompressedHeader == nil {
		entry.CompressedHeader = NewCompressedHeader(entry.Header)
		entry.Header = nil
	}
	shard := c.getShard(key)
	c.putInShard(shard, key, entry)
}

func (c *Cache) putInShard(shard *cacheShard, key string, entry *Entry) {
	shard.mu.Lock()
	defer shard.mu.Unlock()

	// Update existing entry.
	if elem, ok := shard.items[key]; ok {
		shard.eviction.MoveToFront(elem)
		elem.Value.(*lruItem).entry = entry
		return
	}

	// Need to evict? Check TinyLFU admission.
	for shard.eviction.Len() >= shard.maxItems {
		victim := shard.eviction.Back()
		if victim == nil {
			break
		}
		victimItem := victim.Value.(*lruItem)
		if !c.lfu.Admit(key, victimItem.key) {
			// Victim is more popular — reject the newcomer.
			return
		}
		c.evictItem(shard, victim)
	}

	elem := shard.eviction.PushFront(&lruItem{key: key, entry: entry})
	shard.items[key] = elem
}

// evictItem removes the LRU victim from the shard (caller holds shard.mu.Lock).
func (c *Cache) evictItem(shard *cacheShard, elem *list.Element) {
	item := elem.Value.(*lruItem)
	shard.eviction.Remove(elem)
	delete(shard.items, item.key)
	c.stats.Evicts.Add(1)

	if c.diskDir != "" {
		c.spillToDisk(item.key, item.entry)
	}
}

// ---------- Delete ----------

// Delete removes an entry from both memory and disk.
func (c *Cache) Delete(key string) {
	shard := c.getShard(key)
	c.deleteShard(shard, key)
}

func (c *Cache) deleteShard(shard *cacheShard, key string) {
	shard.mu.Lock()
	if elem, ok := shard.items[key]; ok {
		shard.eviction.Remove(elem)
		delete(shard.items, key)
	}
	shard.mu.Unlock()

	if c.diskDir != "" {
		c.removeFromDisk(key)
	}
}

// ---------- Purge ----------

// Purge clears all cached entries from memory.
func (c *Cache) Purge() {
	for _, shard := range c.shards {
		shard.mu.Lock()
		shard.items = make(map[string]*list.Element)
		shard.eviction.Init()
		shard.mu.Unlock()
	}
}

// ---------- GetStats ----------

// GetStats returns current hit/miss/eviction/stale counters.
func (c *Cache) GetStats() (hits, misses, evicts, staleHits int64) {
	return c.stats.Hits.Load(), c.stats.Misses.Load(), c.stats.Evicts.Load(), c.stats.StaleHits.Load()
}

// ---------- Len ----------

// Len returns the number of items currently in memory.
func (c *Cache) Len() int {
	total := 0
	for _, shard := range c.shards {
		shard.mu.RLock()
		total += len(shard.items)
		shard.mu.RUnlock()
	}
	return total
}

// ---------- Disk tier (shared, protected by diskMu) ----------

func (c *Cache) spillToDisk(key string, entry *Entry) {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()

	if c.diskMaxBytes > 0 {
		entrySize := int64(len(entry.Body) + 512)
		for c.diskUsedBytes+entrySize > c.diskMaxBytes {
			if !c.evictOldestDiskEntry() {
				return
			}
		}
	}
	if err := c.saveToDisk(key, entry); err == nil {
		c.diskUsedBytes += int64(len(entry.Body) + 512)
	}
}

func (c *Cache) evictOldestDiskEntry() bool {
	entries, err := os.ReadDir(c.diskDir)
	if err != nil || len(entries) == 0 {
		return false
	}

	var oldest os.DirEntry
	var oldestTime time.Time
	first := true
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if first || info.ModTime().Before(oldestTime) {
			oldest = e
			oldestTime = info.ModTime()
			first = false
		}
	}
	if oldest == nil {
		return false
	}

	path := filepath.Join(c.diskDir, oldest.Name())
	info, _ := oldest.Info()
	if info != nil {
		c.diskUsedBytes -= info.Size()
		if c.diskUsedBytes < 0 {
			c.diskUsedBytes = 0
		}
	}
	os.Remove(path)
	return true
}

func (c *Cache) calculateDiskUsage() int64 {
	entries, err := os.ReadDir(c.diskDir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total
}

// diskEntry is the JSON-serializable form of Entry for disk storage.
type diskEntry struct {
	Body                 []byte      `json:"body"`
	Header               http.Header `json:"header"`
	StatusCode           int         `json:"status_code"`
	StoredAt             time.Time   `json:"stored_at"`
	TTLNs                int64       `json:"ttl_ns"`
	ETag                 string      `json:"etag"`
	LastMod              string      `json:"last_mod"`
	VaryKey              string      `json:"vary_key"`
	StaleWhileRevalidate int64       `json:"swr_ns"`
}

func (c *Cache) diskPath(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(c.diskDir, hex.EncodeToString(h[:])+".json")
}

func (c *Cache) saveToDisk(key string, entry *Entry) error {
	// Decode compressed headers for disk persistence (JSON disk format uses
	// the plain http.Header representation).
	hdr := entry.Header
	if hdr == nil && entry.CompressedHeader != nil {
		hdr = entry.CompressedHeader.Decode()
	}
	de := diskEntry{
		Body:                 entry.Body,
		Header:               hdr,
		StatusCode:           entry.StatusCode,
		StoredAt:             entry.StoredAt,
		TTLNs:                int64(entry.TTL),
		ETag:                 entry.ETag,
		LastMod:              entry.LastMod,
		VaryKey:              entry.VaryKey,
		StaleWhileRevalidate: int64(entry.StaleWhileRevalidate),
	}
	data, err := json.Marshal(de)
	if err != nil {
		return err
	}
	return os.WriteFile(c.diskPath(key), data, 0o644)
}

func (c *Cache) loadFromDisk(key string) (*Entry, error) {
	data, err := os.ReadFile(c.diskPath(key))
	if err != nil {
		return nil, err
	}
	var de diskEntry
	if err := json.Unmarshal(data, &de); err != nil {
		return nil, err
	}
	e := &Entry{
		Body:                 de.Body,
		Header:               de.Header,
		StatusCode:           de.StatusCode,
		StoredAt:             de.StoredAt,
		TTL:                  time.Duration(de.TTLNs),
		ETag:                 de.ETag,
		LastMod:              de.LastMod,
		VaryKey:              de.VaryKey,
		StaleWhileRevalidate: time.Duration(de.StaleWhileRevalidate),
	}
	// When header compression is enabled, re-compress on load so the
	// promoted in-memory entry also saves RAM.
	if c.compressHeaders && e.Header != nil {
		e.CompressedHeader = NewCompressedHeader(e.Header)
		e.Header = nil
	}
	return e, nil
}

func (c *Cache) removeFromDisk(key string) error {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()

	path := c.diskPath(key)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	c.diskUsedBytes -= info.Size()
	if c.diskUsedBytes < 0 {
		c.diskUsedBytes = 0
	}
	return os.Remove(path)
}
