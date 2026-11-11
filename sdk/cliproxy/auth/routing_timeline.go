package auth

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// TimelineBlock pins a provider's routing to a set of credentials until a deadline
// or until any of the usage limits is reached, whichever comes first.
type TimelineBlock struct {
	Provider string
	Accounts []string
	Until    time.Time
	// MaxUsage maps a quota window (QuotaWindowFiveHour, QuotaWindowWeekly) to the
	// used percentage at which a credential leaves the block. Missing or zero means no limit.
	MaxUsage map[string]float64
}

var routingTimeline atomic.Pointer[[]TimelineBlock]

// SetRoutingTimeline replaces the active routing timeline. Blocks keep their order.
func SetRoutingTimeline(blocks []TimelineBlock) {
	if len(blocks) == 0 {
		routingTimeline.Store(nil)
		return
	}
	copied := make([]TimelineBlock, len(blocks))
	for i, block := range blocks {
		block.Provider = strings.ToLower(strings.TrimSpace(block.Provider))
		block.Accounts = append([]string(nil), block.Accounts...)
		limits := make(map[string]float64, len(block.MaxUsage))
		for window, limit := range block.MaxUsage {
			if limit > 0 {
				limits[window] = limit
			}
		}
		block.MaxUsage = limits
		copied[i] = block
	}
	routingTimeline.Store(&copied)
}

type quotaUsageEntry struct {
	used map[string]float64
	at   map[string]time.Time
}

var quotaUsage = struct {
	mu   sync.RWMutex
	data map[string]quotaUsageEntry
}{data: make(map[string]quotaUsageEntry)}

// SetQuotaUsage records used percentages (0-100) per quota window for an auth.
// Each window keeps whichever observation is newest.
func SetQuotaUsage(authID string, used map[string]float64, at time.Time) {
	if authID == "" || len(used) == 0 {
		return
	}
	quotaUsage.mu.Lock()
	defer quotaUsage.mu.Unlock()
	entry, ok := quotaUsage.data[authID]
	if !ok {
		entry = quotaUsageEntry{used: make(map[string]float64), at: make(map[string]time.Time)}
	}
	for window, value := range used {
		if prev, seen := entry.at[window]; seen && prev.After(at) {
			continue
		}
		entry.used[window] = value
		entry.at[window] = at
	}
	quotaUsage.data[authID] = entry
}

// QuotaUsage returns the latest known used percentage for an auth's quota window.
func QuotaUsage(authID, window string) (float64, bool) {
	quotaUsage.mu.RLock()
	defer quotaUsage.mu.RUnlock()
	value, ok := quotaUsage.data[authID].used[window]
	return value, ok
}

// QuotaResets returns a copy of the known reset instants per quota window for an auth.
func QuotaResets(authID string) map[string]time.Time {
	quotaResets.mu.RLock()
	defer quotaResets.mu.RUnlock()
	copied := make(map[string]time.Time, len(quotaResets.data[authID]))
	for window, at := range quotaResets.data[authID] {
		copied[window] = at
	}
	return copied
}

// ActiveTimelineAccounts reports, per provider, the credential IDs the timeline currently
// routes to, and the credentials held back because they reached their usage limit.
func ActiveTimelineAccounts(now time.Time) (active map[string][]string, protected []string) {
	active = make(map[string][]string)
	decision := currentTimelineDecision(now)
	if decision == nil {
		return active, nil
	}
	for provider, ids := range decision.active {
		for id := range ids {
			active[provider] = append(active[provider], id)
		}
	}
	for id := range decision.protected {
		protected = append(protected, id)
	}
	return active, protected
}

// observeQuotaUsageHeaders feeds live utilization from upstream response headers.
func observeQuotaUsageHeaders(authID, provider string, headers http.Header, at time.Time) {
	if len(headers) == 0 {
		return
	}
	used := make(map[string]float64)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		// Anthropic reports utilization as a 0-1 fraction.
		for header, window := range map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": QuotaWindowFiveHour,
			"Anthropic-Ratelimit-Unified-7d-Utilization": QuotaWindowWeekly,
		} {
			if value, ok := parseUsageNumber(headers.Get(header)); ok {
				used[window] = value * 100
			}
		}
	case "codex":
		for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
			value, okValue := parseUsageNumber(headers.Get(prefix + "Used-Percent"))
			minutes, okMinutes := parseUsageNumber(headers.Get(prefix + "Window-Minutes"))
			if !okValue || !okMinutes {
				continue
			}
			switch int(minutes) {
			case 300:
				used[QuotaWindowFiveHour] = value
			case 10080:
				used[QuotaWindowWeekly] = value
			}
		}
	}
	SetQuotaUsage(authID, used, at)
}

func parseUsageNumber(raw string) (float64, bool) {
	value, errParse := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, errParse == nil && value >= 0
}

// timelineDecision is the timeline evaluated once per request.
type timelineDecision struct {
	// active maps a provider to the credential IDs its current block allows.
	active map[string]map[string]struct{}
	// protected holds credentials that reached their block's usage limit and must stay idle
	// until that block ends, so normal routing cannot drain the reserved balance.
	protected map[string]struct{}
}

func (b TimelineBlock) overLimit(authID string) bool {
	for window, limit := range b.MaxUsage {
		if used, ok := QuotaUsage(authID, window); ok && used >= limit {
			return true
		}
	}
	return false
}

func currentTimelineDecision(now time.Time) *timelineDecision {
	blocks := routingTimeline.Load()
	if blocks == nil {
		return nil
	}
	decision := &timelineDecision{
		active:    make(map[string]map[string]struct{}),
		protected: make(map[string]struct{}),
	}
	for _, block := range *blocks {
		if block.Provider == "" || !block.Until.After(now) {
			continue
		}
		usable := make(map[string]struct{}, len(block.Accounts))
		for _, id := range block.Accounts {
			if block.overLimit(id) {
				decision.protected[id] = struct{}{}
			} else {
				usable[id] = struct{}{}
			}
		}
		if _, claimed := decision.active[block.Provider]; claimed || len(usable) == 0 {
			continue
		}
		decision.active[block.Provider] = usable
	}
	if len(decision.active) == 0 && len(decision.protected) == 0 {
		return nil
	}
	return decision
}

func (d *timelineDecision) allows(auth *Auth) bool {
	if d == nil || auth == nil {
		return true
	}
	if _, protected := d.protected[auth.ID]; protected {
		return false
	}
	allowed, ok := d.active[strings.ToLower(strings.TrimSpace(auth.Provider))]
	if !ok {
		return true
	}
	_, member := allowed[auth.ID]
	return member
}
