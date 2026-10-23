package cache

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

type entry struct {
	key     string
	body    []byte
	status  int
	headers http.Header
	expires time.Time
}

// MemoryCache is a bounded LRU + TTL cache.
//   - Get promotes the entry to most-recently-used (true LRU, not random).
//   - Set evicts expired entries first, then least-recently-used when full.
type MemoryCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     *list.List // front = most recent; each Value is *entry
	maxKeys int
	// maxBytes bounds the SUM of cached body sizes.
	//
	// Eviction used to be by key count only. Each cached completion body may be
	// up to 1 MiB (proxy cache cap), so 512 entries was a ~512 MiB steady-state
	// ceiling on a 1 GiB target container — an OOM with no single request to
	// blame, reached simply by raising CACHE_TTL_SECONDS.
	maxBytes int64
	bytes    int64
}

// DefaultMaxCacheBytes bounds total cached response bytes.
const DefaultMaxCacheBytes int64 = 64 << 20 // 64 MiB

func NewMemoryCache(maxKeys int) *MemoryCache {
	if maxKeys <= 0 {
		maxKeys = 512
	}
	return &MemoryCache{
		entries:  make(map[string]*list.Element, maxKeys),
		lru:      list.New(),
		maxKeys:  maxKeys,
		maxBytes: DefaultMaxCacheBytes,
	}
}

// SetMaxBytes overrides the byte budget (operator knob).
func (m *MemoryCache) SetMaxBytes(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.maxBytes = n
	m.evictToBudgetLocked()
	m.mu.Unlock()
}

// Bytes reports the current cached body bytes (gauge/tests).
func (m *MemoryCache) Bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes
}

func (m *MemoryCache) Get(key string) ([]byte, int, http.Header, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[key]
	if !ok {
		return nil, 0, nil, false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expires) {
		m.removeLocked(el)
		return nil, 0, nil, false
	}
	m.lru.MoveToFront(el)
	return e.body, e.status, e.headers, true
}

func (m *MemoryCache) Set(key string, body []byte, status int, headers http.Header, ttlSeconds int) {
	if ttlSeconds <= 0 {
		ttlSeconds = 30
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	expires := time.Now().Add(time.Duration(ttlSeconds) * time.Second)
	if el, ok := m.entries[key]; ok {
		e := el.Value.(*entry)
		m.bytes -= int64(len(e.body))
		e.body = append([]byte(nil), body...)
		m.bytes += int64(len(e.body))
		e.status = status
		e.headers = headers.Clone()
		e.expires = expires
		m.lru.MoveToFront(el)
		m.evictToBudgetLocked()
		return
	}

	// Room-making: evict expired entries first (cheap sweep), then LRU tail.
	for len(m.entries) >= m.maxKeys {
		if !m.evictExpiredLocked() {
			break
		}
	}
	for len(m.entries) >= m.maxKeys {
		tail := m.lru.Back()
		if tail == nil {
			break
		}
		m.removeLocked(tail)
	}

	b := append([]byte(nil), body...)
	el := m.lru.PushFront(&entry{key: key, body: b, status: status, headers: headers.Clone(), expires: expires})
	m.entries[key] = el
	m.bytes += int64(len(b))
	m.evictToBudgetLocked()
}

// evictToBudgetLocked drops LRU-tail entries until the byte budget is met. A
// single entry larger than the whole budget is not cached at all (it would
// evict everything and still overshoot).
func (m *MemoryCache) evictToBudgetLocked() {
	for m.bytes > m.maxBytes {
		tail := m.lru.Back()
		if tail == nil {
			return
		}
		m.removeLocked(tail)
	}
}

// evictExpiredLocked removes up to one entry that has already expired (w.r.t.
// the current time) and reports whether any was removed. Sweeping amortizes
// over Sets without an O(n) pass on every insert.
//
// The previous signature compared each entry against the NEW entry's expiry:
// with equal TTLs every existing entry expired "before" the incoming one, so
// the sweep evicted a random map-ordered entry on every insert and the LRU
// tail eviction below never ran.
func (m *MemoryCache) evictExpiredLocked() bool {
	now := time.Now()
	for k, el := range m.entries {
		if now.After(el.Value.(*entry).expires) {
			m.removeLocked(el)
			_ = k
			return true
		}
	}
	return false
}

func (m *MemoryCache) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	m.lru.Remove(el)
	delete(m.entries, e.key)
	m.bytes -= int64(len(e.body))
	if m.bytes < 0 {
		m.bytes = 0
	}
}

func (m *MemoryCache) Invalidate(pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pattern == "*" {
		m.entries = make(map[string]*list.Element, m.maxKeys)
		m.lru.Init()
		m.bytes = 0
		return
	}
	var doomed []*list.Element
	for k, el := range m.entries {
		if len(k) >= len(pattern) && k[:len(pattern)] == pattern {
			doomed = append(doomed, el)
		}
	}
	for _, el := range doomed {
		m.removeLocked(el)
	}
}

var _ Cache = (*MemoryCache)(nil)
