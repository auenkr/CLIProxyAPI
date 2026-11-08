// Package windowrefresh sends a tiny "Hi" message through each provider's official CLI so an
// account's 5-hour quota window starts as soon as the previous one has ended.
package windowrefresh

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// Window is the length of the quota window being refreshed.
	Window       = 5 * time.Hour
	tickInterval = 30 * time.Second
	// retryAfter is the wait after a failed usage fetch, ping, or a ping that did not start a window.
	retryAfter = 5 * time.Minute
	// unconfirmedBackoff is the wait after a ping whose window the provider does not report yet
	// (usage can still read 0%), so the account is not pinged repeatedly.
	unconfirmedBackoff = 30 * time.Minute
)

// MetadataKey is the auth metadata key holding an account's settings, so they are stored with
// the credential (including in Postgres) rather than in config.yaml.
const MetadataKey = "window_refresh"

// ErrUnsupported is returned by a Sender for providers without an official CLI integration.
var ErrUnsupported = errors.New("window refresh: provider not supported")

// Sender delivers the ping for one account through the provider's official CLI.
type Sender func(ctx context.Context, auth *coreauth.Auth, cfg config.WindowRefreshConfig) error

// UsageFunc polls the provider and returns the end of the account's running 5-hour window, or
// the zero time when no window is running.
type UsageFunc func(ctx context.Context, auth *coreauth.Auth) (time.Time, error)

// Scheduler keeps each enabled account's 5-hour window running. Per account it asks the provider
// for the window end: with no window running it pings now, otherwise it checks again when the
// window ends. The user's reset time (anchor) can veto a ping, see Allowed.
type Scheduler struct {
	List   func() []*coreauth.Auth
	Config func() config.WindowRefreshConfig
	Usage  UsageFunc
	Send   Sender
	Now    func() time.Time

	mu       sync.Mutex
	next     map[string]time.Time // earliest time an account is looked at again
	settings map[string]string    // settings seen at the last look, to react to edits
	inflight map[string]bool
}

// Run ticks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick looks at every enabled account whose next check is due and handles it in the background.
func (s *Scheduler) Tick(ctx context.Context) {
	if s.List == nil || s.Config == nil || s.Usage == nil || s.Send == nil {
		return
	}
	cfg := s.Config()
	now := s.now()
	for _, auth := range s.List() {
		if auth == nil {
			continue
		}
		account, _ := AccountSettings(auth)
		if !account.Enabled {
			continue
		}
		key := AccountKey(auth)
		if !s.claim(key, account, now) {
			continue
		}
		go s.handle(ctx, auth, key, account, cfg)
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// claim reserves the account for a check when its next-check time has come (or its settings
// changed since the last look).
func (s *Scheduler) claim(key string, account config.WindowRefreshAccount, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next == nil {
		s.next = map[string]time.Time{}
		s.settings = map[string]string{}
		s.inflight = map[string]bool{}
	}
	if s.inflight[key] {
		return false
	}
	signature := account.Anchor
	if s.settings[key] != signature {
		s.settings[key] = signature
		delete(s.next, key)
	}
	if now.Before(s.next[key]) {
		return false
	}
	s.inflight[key] = true
	return true
}

func (s *Scheduler) release(key string, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key)
	s.next[key] = next
}

// handle does one check: read the window end from the provider, ping if no window is running
// and the anchor allows it, then schedule the next check.
func (s *Scheduler) handle(ctx context.Context, auth *coreauth.Auth, key string, account config.WindowRefreshAccount, cfg config.WindowRefreshConfig) {
	entry := log.WithField("provider", auth.Provider).WithField("account", key)
	next := s.now().Add(retryAfter)
	defer func() { s.release(key, next) }()

	end, errUsage := s.Usage(ctx, auth)
	if errUsage != nil {
		entry.WithError(errUsage).Warn("window refresh: usage fetch failed")
		return
	}
	now := s.now()
	anchor := parseAnchor(account.Anchor, now)
	if end.After(now) {
		// A window is running: look again when it ends, unless the anchor forbids a ping then.
		next = s.planAt(entry, end, anchor)
		entry.WithField("window_ends", end.Format(time.RFC3339)).Info("window refresh: window running, next check at its end")
		return
	}
	if !Allowed(anchor, now) {
		next = anchor
		entry.WithField("anchor", anchor.Format(time.RFC3339)).Info("window refresh: ping skipped, it would end the window after the anchor")
		return
	}
	switch errSend := s.Send(ctx, auth, cfg); {
	case errors.Is(errSend, ErrUnsupported):
		entry.Warn("window refresh: provider has no official CLI integration yet")
		next = now.Add(24 * time.Hour)
		return
	case errSend != nil:
		entry.WithError(errSend).Warn("window refresh: ping failed")
		return
	}
	entry.Info("window refresh: ping sent")
	// Confirm the window started so the next check does not ping again on stale data.
	end, errUsage = s.Usage(ctx, auth)
	now = s.now()
	if errUsage != nil || !end.After(now) {
		entry.Warn("window refresh: ping sent but no 5-hour window is reported yet")
		next = now.Add(unconfirmedBackoff)
		return
	}
	next = s.planAt(entry, end, anchor)
	entry.WithField("window_ends", end.Format(time.RFC3339)).Info("window refresh: next check scheduled")
}

// planAt returns when to look at the account again given a window ending at end. A ping at
// that time must be allowed by the anchor; otherwise the next look is at the anchor itself.
func (s *Scheduler) planAt(entry *log.Entry, end, anchor time.Time) time.Time {
	if Allowed(anchor, end) {
		return end
	}
	entry.WithField("anchor", anchor.Format(time.RFC3339)).Info("window refresh: scheduled ping skipped, it would end the window after the anchor")
	return anchor
}

// Allowed reports whether a ping at time t may run given the user's reset time (anchor). A ping
// at t starts a window that ends at t+Window; it is allowed only when the anchor minus the
// window length is later than t. A zero anchor means no restriction.
func Allowed(anchor, t time.Time) bool {
	if anchor.IsZero() {
		return true
	}
	return anchor.Add(-Window).After(t)
}

// parseAnchor returns the user's reset time, or the zero time when unset, invalid or already
// in the past (a passed reset time no longer restricts anything).
func parseAnchor(value string, now time.Time) time.Time {
	anchor, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil || !anchor.After(now) {
		return time.Time{}
	}
	return anchor
}

// AccountSettings reads the per-account settings saved on the auth record. Accounts with no saved
// setting are enabled by default; only an explicit enabled=false turns the refresh off.
func AccountSettings(auth *coreauth.Auth) (config.WindowRefreshAccount, bool) {
	out := config.WindowRefreshAccount{Enabled: true}
	raw, ok := auth.Metadata[MetadataKey].(map[string]any)
	if !ok {
		return out, false
	}
	if enabled, isBool := raw["enabled"].(bool); isBool {
		out.Enabled = enabled
	}
	out.Anchor, _ = raw["anchor"].(string)
	return out, true
}

// AccountKey is the config key of an account: its auth file name, falling back to the ID.
func AccountKey(auth *coreauth.Auth) string {
	if name := strings.TrimSpace(auth.FileName); name != "" {
		return name
	}
	return auth.ID
}
