package management

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// apiCallCacheTTL bounds how long a successful GET api-call response is reused.
// Upstream usage endpoints rate-limit aggressively, so repeated panel loads
// within this window are served from memory.
const apiCallCacheTTL = 60 * time.Second

type apiCallCacheEntry struct {
	at   time.Time
	resp apiCallResponse
	// lastGood keeps the most recent 2xx response; it outlives TTL and purges so
	// a rate-limited upstream can still be answered with the last known data.
	lastGood *apiCallResponse
}

type apiCallCache struct {
	mu      sync.Mutex
	entries map[string]map[string]apiCallCacheEntry // authIndex -> request key -> entry
	now     func() time.Time
}

var sharedAPICallCache = &apiCallCache{entries: map[string]map[string]apiCallCacheEntry{}, now: time.Now}

func apiCallCacheKey(method, urlStr, proxyURL, data string, headers map[string]string) string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(method + "\x00" + urlStr + "\x00" + proxyURL + "\x00" + data)
	for _, k := range keys {
		b.WriteString("\x00" + strings.ToLower(k) + "=" + headers[k])
	}
	return b.String()
}

func (c *apiCallCache) get(authIndex, key string) (apiCallResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[authIndex][key]
	if !ok || entry.at.IsZero() || c.now().Sub(entry.at) >= apiCallCacheTTL {
		return apiCallResponse{}, false
	}
	return entry.resp, true
}

// store records an upstream result and returns what the client should see:
// 2xx is cached as-is; 429/5xx falls back to the last good response when one
// exists, otherwise the error itself is cached so retries stop hammering upstream.
func (c *apiCallCache) store(authIndex, key string, resp apiCallResponse) (apiCallResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.entries[authIndex]
	if bucket == nil {
		bucket = map[string]apiCallCacheEntry{}
		c.entries[authIndex] = bucket
	}
	entry := bucket[key]
	now := c.now()
	stale := false
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		good := resp
		entry.lastGood = &good
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		if entry.lastGood != nil {
			resp, stale = *entry.lastGood, true
		}
	default:
		return resp, false
	}
	entry.at, entry.resp = now, resp
	bucket[key] = entry
	return resp, stale
}

// purge marks cached responses stale for one auth index (or all when empty);
// last good responses are kept as a rate-limit fallback.
func (c *apiCallCache) purge(authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for idx, bucket := range c.entries {
		if authIndex != "" && idx != authIndex {
			continue
		}
		for k, e := range bucket {
			e.at = time.Time{}
			bucket[k] = e
		}
	}
}

// PurgeAPICallCache clears cached api-call responses so the next request hits upstream.
//
//	DELETE /v8/requests/api-call/cache?auth_index=<index>
func (h *Handler) PurgeAPICallCache(c *gin.Context) {
	sharedAPICallCache.purge(strings.TrimSpace(c.Query("auth_index")))
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
