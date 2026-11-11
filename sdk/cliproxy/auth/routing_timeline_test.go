package auth

import (
	"net/http"
	"testing"
	"time"
)

func resetTimelineState(t *testing.T) {
	t.Helper()
	SetRoutingTimeline(nil)
	quotaUsage.mu.Lock()
	quotaUsage.data = make(map[string]quotaUsageEntry)
	quotaUsage.mu.Unlock()
	t.Cleanup(func() { SetRoutingTimeline(nil) })
}

func TestTimelineDecision(t *testing.T) {
	resetTimelineState(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	a := &Auth{ID: "a", Provider: "claude"}
	b := &Auth{ID: "b", Provider: "claude"}
	c := &Auth{ID: "c", Provider: "claude"}
	g := &Auth{ID: "g", Provider: "gemini"}

	if currentTimelineDecision(now) != nil {
		t.Fatal("empty timeline must not restrict routing")
	}

	SetRoutingTimeline([]TimelineBlock{
		{Provider: "Claude", Accounts: []string{"a"}, Until: now.Add(time.Hour), MaxUsage: map[string]float64{QuotaWindowFiveHour: 85}},
		{Provider: "claude", Accounts: []string{"b"}, Until: now.Add(2 * time.Hour)},
	})

	d := currentTimelineDecision(now)
	if !d.allows(a) || d.allows(b) || d.allows(c) || !d.allows(g) {
		t.Fatal("first block should own claude traffic and leave other providers alone")
	}

	// Usage limit ends the first block early and protects its account.
	SetQuotaUsage("a", map[string]float64{QuotaWindowFiveHour: 90}, now)
	d = currentTimelineDecision(now)
	if d.allows(a) || !d.allows(b) || d.allows(c) {
		t.Fatal("exhausted block should hand over to the next block")
	}

	// After the first deadline, the protection lifts with the block.
	d = currentTimelineDecision(now.Add(90 * time.Minute))
	if d.allows(a) || !d.allows(b) {
		t.Fatal("second block should be active")
	}

	// After the last block, routing is normal again.
	if currentTimelineDecision(now.Add(3*time.Hour)) != nil {
		t.Fatal("expired timeline must not restrict routing")
	}
}

func TestObserveQuotaUsageHeaders(t *testing.T) {
	resetTimelineState(t)
	now := time.Now()
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.42")
	observeQuotaUsageHeaders("a", "claude", h, now)
	if got, _ := QuotaUsage("a", QuotaWindowFiveHour); got < 41.9 || got > 42.1 {
		t.Fatalf("claude 5h usage = %v", got)
	}

	h = http.Header{}
	h.Set("X-Codex-Secondary-Used-Percent", "77")
	h.Set("X-Codex-Secondary-Window-Minutes", "10080")
	observeQuotaUsageHeaders("x", "codex", h, now)
	if got, _ := QuotaUsage("x", QuotaWindowWeekly); got != 77 {
		t.Fatalf("codex weekly usage = %v", got)
	}

	// Older observations never overwrite newer ones.
	SetQuotaUsage("x", map[string]float64{QuotaWindowWeekly: 10}, now.Add(-time.Minute))
	if got, _ := QuotaUsage("x", QuotaWindowWeekly); got != 77 {
		t.Fatalf("stale sample overwrote usage: %v", got)
	}
}
