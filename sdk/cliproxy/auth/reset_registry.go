package auth

import (
	"sync"
	"time"
)

// QuotaWindowWeekly and QuotaWindowFiveHour name the reset windows tracked per credential.
const (
	QuotaWindowWeekly   = "weekly"
	QuotaWindowFiveHour = "5h"
)

var quotaResets = struct {
	mu   sync.RWMutex
	data map[string]map[string]time.Time
}{data: make(map[string]map[string]time.Time)}

// SetQuotaResets records the known quota reset instants for an auth, replacing earlier data.
func SetQuotaResets(authID string, resets map[string]time.Time) {
	if authID == "" {
		return
	}
	quotaResets.mu.Lock()
	defer quotaResets.mu.Unlock()
	if len(resets) == 0 {
		delete(quotaResets.data, authID)
		return
	}
	copied := make(map[string]time.Time, len(resets))
	for window, at := range resets {
		copied[window] = at
	}
	quotaResets.data[authID] = copied
}

// NextQuotaReset returns the soonest upcoming reset for an auth. The weekly window wins
// over the 5-hour window because it is the one whose unused quota is actually lost.
func NextQuotaReset(authID string, now time.Time) (time.Time, bool) {
	quotaResets.mu.RLock()
	resets := quotaResets.data[authID]
	quotaResets.mu.RUnlock()
	for _, window := range []string{QuotaWindowWeekly, QuotaWindowFiveHour} {
		if at, ok := resets[window]; ok && at.After(now) {
			return at, true
		}
	}
	return time.Time{}, false
}
