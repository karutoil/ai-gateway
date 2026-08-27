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
}

func NewMemoryCache(maxKeys int) *MemoryCache {
	if maxKeys <= 0 {
		maxKeys = 512
	}
	return &MemoryCache{
		entries: make(map[string]*list.Element, maxKeys),
		lru:     list.New(),
		maxKeys: maxKeys,
	}
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
		e.body = append([]byte(nil), body...)
		e.status = status
		e.headers = headers.Clone()
		e.expires = expires
		m.lru.MoveToFront(el)
		return
	}

	// Room-making: evict expired entries first (cheap sweep), then LRU tail.
	for len(m.entries) >= m.maxKeys {
		if !m.evictExpiredLocked(expires) {
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
}

// evictExpiredLocked removes up to one expired entry per call and reports
// whether any was removed. Sweeping amortizes over Sets without an O(n) pass
// on every insert.
func (m *MemoryCache) evictExpiredLocked(before time.Time) bool {
	now := time.Now()
	for k, el := range m.entries {
		if before.After(el.Value.(*entry).expires) || now.After(el.Value.(*entry).expires) {
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
}

func (m *MemoryCache) Invalidate(pattern string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pattern == "*" {
		m.entries = make(map[string]*list.Element, m.maxKeys)
		m.lru.Init()
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
