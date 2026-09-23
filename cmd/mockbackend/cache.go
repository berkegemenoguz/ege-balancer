package main

import (
	"container/list"
	"sync"
)

// cache is what a backend remembers about its clients: a bounded set of
// session keys, forgetting the one seen longest ago once it is full. It stands
// for whatever a real service keeps per client — a session, a warmed row, a
// rendered page — so that a request whose session is remembered costs less
// than one whose session is not.
type cache struct {
	capacity int

	mu      sync.Mutex
	order   *list.List // the most recently seen session at the front
	entries map[string]*list.Element
}

// newCache returns a cache that remembers up to capacity sessions.
func newCache(capacity int) *cache {
	return &cache{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

// seen reports whether key was remembered, and remembers it from now on
// either way.
func (c *cache) seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, remembered := c.entries[key]; remembered {
		c.order.MoveToFront(entry)
		return true
	}

	c.entries[key] = c.order.PushFront(key)
	if c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		forgotten, _ := oldest.Value.(string)
		delete(c.entries, forgotten)
	}
	return false
}

// size is how many sessions are remembered.
func (c *cache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// clear forgets every session, as a restart would.
func (c *cache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.entries)
}
