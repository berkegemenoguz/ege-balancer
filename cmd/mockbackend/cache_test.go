package main

import (
	"strconv"
	"sync"
	"testing"
)

func TestCacheRemembersASession(t *testing.T) {
	c := newCache(2)

	if c.seen("a") {
		t.Error("a session seen for the first time was reported as remembered")
	}
	if !c.seen("a") {
		t.Error("a session seen before was not remembered")
	}
}

func TestCacheForgetsTheLeastRecentlySeen(t *testing.T) {
	c := newCache(2)
	c.seen("a")
	c.seen("b")
	c.seen("a") // a is now more recent than b
	c.seen("c") // full, so b goes

	if !c.seen("a") {
		t.Error("a was forgotten, want the least recently seen, b, to go")
	}
	if c.seen("b") {
		t.Error("b was still remembered, want it forgotten")
	}
	if got := c.size(); got != 2 {
		t.Errorf("size = %d, want the capacity of 2", got)
	}
}

func TestCacheClearForgetsEverything(t *testing.T) {
	c := newCache(4)
	c.seen("a")
	c.seen("b")
	c.clear()

	if got := c.size(); got != 0 {
		t.Errorf("size = %d after clear, want 0", got)
	}
	if c.seen("a") {
		t.Error("a session was remembered after clear")
	}
}

func TestCacheIsSafeForConcurrentUse(t *testing.T) {
	const capacity = 50
	c := newCache(capacity)

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				c.seen(strconv.Itoa((worker*1000 + i) % 200))
			}
		})
	}
	wg.Wait()

	if got := c.size(); got > capacity {
		t.Errorf("size = %d, want at most the capacity of %d", got, capacity)
	}
}
