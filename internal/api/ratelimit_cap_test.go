package api

import (
	"container/list"
	"testing"
)

func TestRateLimiter_EvictsLeastRecentlyUsedWhenFull(t *testing.T) {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		lru:         list.New(),
		rate:        100,
		burst:       100,
		maxVisitors: 2,
	}

	rl.allow("1.1.1.1") // LRU order (back -> front): 1.1.1.1
	rl.allow("2.2.2.2") // 1.1.1.1, 2.2.2.2  (1.1.1.1 now least-recently-used)
	rl.allow("3.3.3.3") // at capacity -> evict LRU (1.1.1.1), admit 3.3.3.3

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.visitors) != 2 {
		t.Fatalf("expected visitor map capped at 2, got %d", len(rl.visitors))
	}
	if rl.lru.Len() != 2 {
		t.Fatalf("expected LRU list length 2, got %d", rl.lru.Len())
	}
	if _, ok := rl.visitors["1.1.1.1"]; ok {
		t.Error("expected least-recently-used 1.1.1.1 to be evicted")
	}
	if _, ok := rl.visitors["3.3.3.3"]; !ok {
		t.Error("expected new visitor 3.3.3.3 to be admitted")
	}
}

func TestRateLimiter_RecentlyUsedSurvivesEviction(t *testing.T) {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		lru:         list.New(),
		rate:        100,
		burst:       100,
		maxVisitors: 2,
	}

	rl.allow("1.1.1.1")
	rl.allow("2.2.2.2")
	rl.allow("1.1.1.1") // touch 1.1.1.1 -> now most-recently-used; 2.2.2.2 is LRU
	rl.allow("3.3.3.3") // evicts LRU (2.2.2.2), not the recently-used 1.1.1.1

	rl.mu.Lock()
	defer rl.mu.Unlock()
	if _, ok := rl.visitors["1.1.1.1"]; !ok {
		t.Error("expected recently-used 1.1.1.1 to survive eviction")
	}
	if _, ok := rl.visitors["2.2.2.2"]; ok {
		t.Error("expected least-recently-used 2.2.2.2 to be evicted")
	}
}

func TestRateLimiter_UnlimitedVisitorsWhenCapNonPositive(t *testing.T) {
	rl := &RateLimiter{
		visitors:    make(map[string]*visitor),
		lru:         list.New(),
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
