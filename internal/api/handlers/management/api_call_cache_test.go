package management

import (
	"testing"
	"time"
)

func TestAPICallCacheTTLPurgeAndRateLimitFallback(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &apiCallCache{entries: map[string]map[string]apiCallCacheEntry{}, now: func() time.Time { return now }}
	key := apiCallCacheKey("GET", "https://x/usage", "", "", map[string]string{"B": "2", "A": "1"})
	if key != apiCallCacheKey("GET", "https://x/usage", "", "", map[string]string{"A": "1", "B": "2"}) {
		t.Fatal("key must not depend on header order")
	}
	c.store("1", key, apiCallResponse{StatusCode: 200, Body: "ok"})
	if r, ok := c.get("1", key); !ok || r.Body != "ok" {
		t.Fatal("expected hit")
	}
	if _, ok := c.get("2", key); ok {
		t.Fatal("cache must be per auth index")
	}
	now = now.Add(apiCallCacheTTL)
	if _, ok := c.get("1", key); ok {
		t.Fatal("expected expiry at TTL")
	}

	// Rate limit after purge serves the last good body and caches it.
	c.purge("1")
	if _, ok := c.get("1", key); ok {
		t.Fatal("expected purge")
	}
	if r, stale := c.store("1", key, apiCallResponse{StatusCode: 429}); !stale || r.Body != "ok" {
		t.Fatalf("expected stale fallback, got %+v", r)
	}
	if r, ok := c.get("1", key); !ok || r.Body != "ok" {
		t.Fatal("fallback should be cached")
	}

	// Without a good response the 429 itself is cached for the TTL.
	other := apiCallCacheKey("GET", "https://x/grants", "", "", nil)
	if r, stale := c.store("1", other, apiCallResponse{StatusCode: 429}); stale || r.StatusCode != 429 {
		t.Fatal("expected raw 429")
	}
	if r, ok := c.get("1", other); !ok || r.StatusCode != 429 {
		t.Fatal("429 should be cached")
	}
	// 4xx other than 429 is never cached.
	bad := apiCallCacheKey("GET", "https://x/bad", "", "", nil)
	c.store("1", bad, apiCallResponse{StatusCode: 401})
	if _, ok := c.get("1", bad); ok {
		t.Fatal("401 must not be cached")
	}
}

func TestAPICallCacheHonorsRetryAfter(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &apiCallCache{entries: map[string]map[string]apiCallCacheEntry{}, now: func() time.Time { return now }}
	key := apiCallCacheKey("GET", "https://x/grants", "", "", nil)
	c.store("1", key, apiCallResponse{StatusCode: 200, Body: "ok"})
	limited := apiCallResponse{StatusCode: 429, Header: map[string][]string{"Retry-After": {"300"}}}
	if r, stale := c.store("1", key, limited); !stale || r.Body != "ok" {
		t.Fatal("expected last good body")
	}
	c.purge("1")
	now = now.Add(200 * time.Second)
	if r, ok := c.get("1", key); !ok || r.Body != "ok" {
		t.Fatal("purge and TTL must not lift a Retry-After block")
	}
	if r, ok := c.blocked("1", key); !ok || r.StatusCode != 429 {
		t.Fatal("fresh reads must see the 429 while blocked")
	}
	now = now.Add(100 * time.Second)
	if _, ok := c.blocked("1", key); ok {
		t.Fatal("block must end at Retry-After")
	}
	if _, ok := c.get("1", key); ok {
		t.Fatal("purged entry should miss once the block ends")
	}
	if at, ok := retryAfter(map[string][]string{"retry-after": {"Wed, 21 Oct 2015 07:28:00 GMT"}}, now); !ok || at.Year() != 2015 {
		t.Fatal("expected HTTP-date Retry-After")
	}
}
