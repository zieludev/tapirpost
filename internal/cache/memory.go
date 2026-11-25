// Package cache holds the places the delivery API can keep its responses:
// the memory of the process, or a server that speaks the Redis protocol.
package cache

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// errTTL is returned for a time to live that would keep a value forever.
// Values are stored under keys that are never asked for again once the
// content changes, so every one of them has to expire.
var errTTL = errors.New("cache: the time to live must be positive")

// itemOverhead approximates what an item costs besides its key and value.
const itemOverhead = 128

// Memory is a cache in the memory of the process. It holds at most a fixed
// number of bytes and drops the least recently used values to stay there.
type Memory struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	order    *list.List // most recently used first
	items    map[string]*list.Element
	now      func() time.Time
}

type memoryItem struct {
	key     string
	value   []byte
	expires time.Time
}

func (i *memoryItem) size() int64 {
	return int64(len(i.key)+len(i.value)) + itemOverhead
}

// NewMemory returns a cache that holds up to maxBytes.
func NewMemory(maxBytes int64) *Memory {
	return &Memory{
		maxBytes: maxBytes,
		order:    list.New(),
		items:    make(map[string]*list.Element),
		now:      time.Now,
	}
}

// Get returns the value stored under key. The caller must not modify it.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	item := el.Value.(*memoryItem)
	if !m.now().Before(item.expires) {
		m.remove(el)
		return nil, false, nil
	}
	m.order.MoveToFront(el)
	return item.value, true, nil
}

// Set stores a copy of value under key for ttl. A value that is larger than
// the whole cache is not stored.
func (m *Memory) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return errTTL
	}
	item := &memoryItem{key: key, value: append([]byte(nil), value...)}

	m.mu.Lock()
	defer m.mu.Unlock()
	item.expires = m.now().Add(ttl)
	if el, ok := m.items[key]; ok {
		m.remove(el)
	}
	if item.size() > m.maxBytes {
		return nil
	}
	m.items[key] = m.order.PushFront(item)
	m.used += item.size()
	for m.used > m.maxBytes {
		m.remove(m.order.Back())
	}
	return nil
}

// Len returns the number of stored values, expired ones included.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// Bytes returns the size the stored values are accounted for.
func (m *Memory) Bytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

func (m *Memory) remove(el *list.Element) {
	item := m.order.Remove(el).(*memoryItem)
	delete(m.items, item.key)
	m.used -= item.size()
}
