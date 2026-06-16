package api

import (
	"testing"
	"time"
)

func TestRateLimiter_EvictsLeastRecentlySeenWhenFull(t *testing.T) {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		rate:        100,
		burst:       100,
		maxVisitors: 2,
	}

	rl.allow("1.1.1.1")
	rl.allow("2.2.2.2")

	// Make 1.1.1.1 the least-recently-seen so the cap evicts it deterministically.
	rl.mu.Lock()
	rl.visitors["1.1.1.1"].lastSeen = time.Now().Add(-time.Hour)
	rl.mu.Unlock()

	rl.allow("3.3.3.3") // map is at capacity (2) -> evict LRU, then admit

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.visitors) != 2 {
		t.Fatalf("expected visitor map capped at 2, got %d", len(rl.visitors))
	}
	if _, ok := rl.visitors["1.1.1.1"]; ok {
		t.Error("expected least-recently-seen 1.1.1.1 to be evicted")
	}
	if _, ok := rl.visitors["3.3.3.3"]; !ok {
		t.Error("expected new visitor 3.3.3.3 to be admitted")
	}
}

func TestRateLimiter_UnlimitedVisitorsWhenCapNonPositive(t *testing.T) {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		rate:        100,
		burst:       100,
		maxVisitors: 0, // unlimited
	}
	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		rl.allow(ip)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.visitors) != 4 {
		t.Fatalf("expected no eviction with non-positive cap, got %d visitors", len(rl.visitors))
	}
}
