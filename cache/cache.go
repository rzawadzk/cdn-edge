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
)

// Entry represents a cached HTTP response.
type Entry struct {
	Body       []byte
	Header     http.Header
	StatusCode int
	StoredAt   time.Time
	TTL        time.Duration
	ETag       string
	LastMod    string
	VaryKey    string // secondary key derived from Vary headers
	StaleWhileRevalidate time.Duration
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

// Stats holds cache hit/miss counters.
type Stats struct {
	Hits       atomic.Int64
	Misses     atomic.Int64
	Evicts     atomic.Int64
	StaleHits  atomic.Int64
}

// Cache is a two-tier cache: in-memory LRU backed by optional disk storage.
type Cache struct {
	mu             sync.RWMutex
	items          map[string]*list.Element
	eviction       *list.List
	maxItems       int
	maxEntryBytes  int64
	maxKeyLen      int
	diskDir        string
	diskMaxBytes   int64
	diskUsedBytes  int64
	stats          Stats
}

type lruItem struct {
	key   string
	entry *Entry
}

// New creates a cache with the given maximum in-memory item count.
// If diskDir is non-empty, evicted items are persisted to disk.
// Options configures the cache.
type Options struct {
	MaxItems      int
	MaxEntryBytes int64
	MaxKeyLen     int
	DiskDir       string
	DiskMaxBytes  int64
}

// New creates a cache with the given options.
// For backwards compatibility, also accepts positional args via NewLegacy.
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
	return newCache(opts.MaxItems, opts.MaxEntryBytes, opts.MaxKeyLen, opts.DiskDir, opts.DiskMaxBytes)
}

func newCache(maxItems int, maxEntryBytes int64, maxKeyLen int, diskDir string, diskMaxBytes int64) (*Cache, error) {
	if diskDir != "" {
		if err := os.MkdirAll(diskDir, 0o755); err != nil {
			return nil, fmt.Errorf("cache: create disk dir: %w", err)
		}
	}
	c := &Cache{
		items:         make(map[string]*list.Element),
		eviction:      list.New(),
		maxItems:      maxItems,
		maxEntryBytes: maxEntryBytes,
		maxKeyLen:     maxKeyLen,
		diskDir:       diskDir,
		diskMaxBytes:  diskMaxBytes,
	}
	if diskDir != "" {
		c.diskUsedBytes = c.calculateDiskUsage()
	}
	return c, nil
}

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

// isLowerASCII returns true if s contains no uppercase ASCII letters.
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

// writeSortedQuery writes sorted query params directly to the builder.
func writeSortedQuery(sb *strings.Builder, q string) {
	// If no '&', the query has a single param — no sorting needed.
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
		return nil // Vary: * means never cache (handled by caller)
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

// Get retrieves an entry by key. Returns nil on miss.
// If the entry is stale but within stale-while-revalidate, it is still returned
// (caller should check IsStaleServable and trigger async revalidation).
func (c *Cache) Get(key string) *Entry {
	c.mu.RLock()
	elem, ok := c.items[key]
	c.mu.RUnlock()

	if ok {
		entry := elem.Value.(*lruItem).entry
		if entry.IsExpired() {
			if entry.IsStaleServable() {
				c.mu.Lock()
				c.eviction.MoveToFront(elem)
				c.mu.Unlock()
				c.stats.StaleHits.Add(1)
				return entry
			}
			c.Delete(key)
			c.stats.Misses.Add(1)
			return nil
		}
		c.mu.Lock()
		c.eviction.MoveToFront(elem)
		c.mu.Unlock()
		c.stats.Hits.Add(1)
		return entry
	}

	// Try disk.
	if c.diskDir != "" {
		entry, err := c.loadFromDisk(key)
		if err == nil && entry != nil {
			if entry.IsExpired() && !entry.IsStaleServable() {
				_ = c.removeFromDisk(key)
				c.stats.Misses.Add(1)
				return nil
			}
			c.put(key, entry)
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

// Put stores an entry in the cache. Respects maxEntryBytes and maxKeyLen.
func (c *Cache) Put(key string, entry *Entry) {
	if c.maxEntryBytes > 0 && int64(len(entry.Body)) > c.maxEntryBytes {
		return // too large to cache
	}
	if c.maxKeyLen > 0 && len(key) > c.maxKeyLen {
		return // key too long — pathological URL
	}
	c.put(key, entry)
}

func (c *Cache) put(key string, entry *Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.eviction.MoveToFront(elem)
		elem.Value.(*lruItem).entry = entry
		return
	}

	for c.eviction.Len() >= c.maxItems {
		c.evictOldest()
	}

	elem := c.eviction.PushFront(&lruItem{key: key, entry: entry})
	c.items[key] = elem
}

// Delete removes an entry from both memory and disk.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.eviction.Remove(elem)
		delete(c.items, key)
	}
	if c.diskDir != "" {
		c.removeFromDisk(key)
	}
}

// Purge clears all cached entries.
func (c *Cache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.eviction.Init()
}

// GetStats returns current hit/miss/eviction/stale counters.
func (c *Cache) GetStats() (hits, misses, evicts, staleHits int64) {
	return c.stats.Hits.Load(), c.stats.Misses.Load(), c.stats.Evicts.Load(), c.stats.StaleHits.Load()
}

// Len returns the number of items currently in memory.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

func (c *Cache) evictOldest() {
	elem := c.eviction.Back()
	if elem == nil {
		return
	}
	item := elem.Value.(*lruItem)
	c.eviction.Remove(elem)
	delete(c.items, item.key)
	c.stats.Evicts.Add(1)

	// Spill to disk if space allows.
	if c.diskDir != "" {
		c.spillToDisk(item.key, item.entry)
	}
}

func (c *Cache) spillToDisk(key string, entry *Entry) {
	if c.diskMaxBytes > 0 {
		entrySize := int64(len(entry.Body) + 512) // rough estimate including headers
		// Evict oldest disk entries if over budget.
		for c.diskUsedBytes+entrySize > c.diskMaxBytes {
			if !c.evictOldestDiskEntry() {
				return // can't free space
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
	de := diskEntry{
		Body:                 entry.Body,
		Header:               entry.Header,
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
	return &Entry{
		Body:                 de.Body,
		Header:               de.Header,
		StatusCode:           de.StatusCode,
		StoredAt:             de.StoredAt,
		TTL:                  time.Duration(de.TTLNs),
		ETag:                 de.ETag,
		LastMod:              de.LastMod,
		VaryKey:              de.VaryKey,
		StaleWhileRevalidate: time.Duration(de.StaleWhileRevalidate),
	}, nil
}

func (c *Cache) removeFromDisk(key string) error {
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
