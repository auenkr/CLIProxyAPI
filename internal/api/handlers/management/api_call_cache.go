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
	if !ok || c.now().Sub(entry.at) >= apiCallCacheTTL {
		return apiCallResponse{}, false
	}
	return entry.resp, true
}

func (c *apiCallCache) set(authIndex, key string, resp apiCallResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket := c.entries[authIndex]
	if bucket == nil {
		bucket = map[string]apiCallCacheEntry{}
		c.entries[authIndex] = bucket
	}
	now := c.now()
	for k, e := range bucket {
		if now.Sub(e.at) >= apiCallCacheTTL {
			delete(bucket, k)
		}
	}
	bucket[key] = apiCallCacheEntry{at: now, resp: resp}
}

// purge drops cached responses for one auth index, or everything when empty.
func (c *apiCallCache) purge(authIndex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if authIndex == "" {
		c.entries = map[string]map[string]apiCallCacheEntry{}
		return
	}
	delete(c.entries, authIndex)
}

// PurgeAPICallCache clears cached api-call responses so the next request hits upstream.
//
//	DELETE /v8/requests/api-call/cache?auth_index=<index>
func (h *Handler) PurgeAPICallCache(c *gin.Context) {
	sharedAPICallCache.purge(strings.TrimSpace(c.Query("auth_index")))
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
