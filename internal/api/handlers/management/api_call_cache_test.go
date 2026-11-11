package management

import (
	"testing"
	"time"
)

func TestAPICallCacheTTLAndPurge(t *testing.T) {
	now := time.Unix(1000, 0)
	c := &apiCallCache{entries: map[string]map[string]apiCallCacheEntry{}, now: func() time.Time { return now }}
	key := apiCallCacheKey("GET", "https://x/usage", "", "", map[string]string{"B": "2", "A": "1"})
	if key != apiCallCacheKey("GET", "https://x/usage", "", "", map[string]string{"A": "1", "B": "2"}) {
		t.Fatal("key must not depend on header order")
	}
	c.set("1", key, apiCallResponse{StatusCode: 200, Body: "ok"})
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
	c.set("1", key, apiCallResponse{StatusCode: 200})
	c.purge("1")
	if _, ok := c.get("1", key); ok {
		t.Fatal("expected purge")
	}
}
