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
	mu     sync.RWMutex
	data   map[string]map[string]time.Time
	polled map[string]time.Time
}{data: make(map[string]map[string]time.Time), polled: make(map[string]time.Time)}

// SetQuotaResets records the known quota reset instants for an auth, replacing earlier data.
func SetQuotaResets(authID string, resets map[string]time.Time) {
	SetQuotaResetsAt(authID, resets, time.Now())
}

// SetQuotaResetsAt is SetQuotaResets with an explicit poll time. An empty resets map with a
// poll time means the provider was polled and reported no active window.
func SetQuotaResetsAt(authID string, resets map[string]time.Time, polledAt time.Time) {
	if authID == "" {
		return
	}
	quotaResets.mu.Lock()
	defer quotaResets.mu.Unlock()
	quotaResets.polled[authID] = polledAt
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

// QuotaPolledAt returns when the provider's quota was last polled successfully for an auth.
func QuotaPolledAt(authID string) (time.Time, bool) {
	quotaResets.mu.RLock()
	defer quotaResets.mu.RUnlock()
	at, ok := quotaResets.polled[authID]
	return at, ok
}

// QuotaResetAt returns the recorded reset instant of one window for an auth.
func QuotaResetAt(authID, window string) (time.Time, bool) {
	quotaResets.mu.RLock()
	defer quotaResets.mu.RUnlock()
	at, ok := quotaResets.data[authID][window]
	return at, ok
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
