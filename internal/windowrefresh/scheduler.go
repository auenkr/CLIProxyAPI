// Package windowrefresh sends a tiny "Hi" message through each provider's official SDK when an
// account's 5-hour quota window resets, so the next window starts counting immediately.
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
	retryAfter   = 5 * time.Minute
	// StartupGrace delays starting windows for idle accounts after the service starts.
	StartupGrace = 10 * time.Minute
)

// ErrUnsupported is returned by a Sender for providers without an official SDK integration.
var ErrUnsupported = errors.New("window refresh: provider not supported")

// Sender delivers the ping for one account through the provider's official SDK.
type Sender func(ctx context.Context, auth *coreauth.Auth, cfg config.WindowRefreshConfig) error

// Scheduler decides when each enabled account is due a ping and runs it.
type Scheduler struct {
	List   func() []*coreauth.Auth
	Config func() config.WindowRefreshConfig
	Send   Sender
	Now    func() time.Time

	mu        sync.Mutex
	seeded    map[string]bool
	started   time.Time
	lastFired map[string]time.Time
	lastTry   map[string]time.Time
	pending   map[string]time.Time
	inflight  map[string]bool
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

// Tick evaluates every enabled account once and starts due pings in the background.
func (s *Scheduler) Tick(ctx context.Context) {
	if s.List == nil || s.Config == nil || s.Send == nil {
		return
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	cfg := s.Config()
	for _, auth := range s.List() {
		if auth == nil || auth.Disabled {
			continue
		}
		key := AccountKey(auth)
		account, ok := AccountSettings(auth)
		if !ok || !account.Enabled {
			continue
		}
		if !s.due(auth.ID, key, account, now) {
			continue
		}
		s.fire(ctx, auth, key, cfg, now)
	}
}

// MetadataKey is the auth metadata key holding an account's settings, so they are stored with
// the credential (including in Postgres) rather than in config.yaml.
const MetadataKey = "window_refresh"

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

// due applies the priority: the provider's reported 5h reset wins; with no active window the
// ping starts one (not before the anchor, and not within the startup grace period).
func (s *Scheduler) due(authID, key string, account config.WindowRefreshAccount, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seeded == nil {
		s.seeded = map[string]bool{}
		s.lastFired = map[string]time.Time{}
		s.lastTry = map[string]time.Time{}
		s.pending = map[string]time.Time{}
		s.inflight = map[string]bool{}
		s.started = now
	}
	if s.inflight[key] || now.Sub(s.lastTry[key]) < retryAfter {
		return false
	}
	if reset, ok := coreauth.QuotaResetAt(authID, coreauth.QuotaWindowFiveHour); ok && reset.After(now) {
		s.pending[key] = reset
		return false
	}
	if reset, ok := s.pending[key]; ok {
		return !now.Before(reset)
	}
	// No window is known to be running. Only trust a poll taken after our last ping, otherwise
	// the old window's data would trigger a second ping before the new window shows up.
	polledAt, polled := coreauth.QuotaPolledAt(authID)
	if !polled || !polledAt.After(s.lastFired[key]) || now.Sub(s.started) < StartupGrace {
		return false
	}
	if anchor, errParse := time.Parse(time.RFC3339, strings.TrimSpace(account.Anchor)); errParse == nil && now.Before(anchor) {
		return false
	}
	return true
}

func (s *Scheduler) fire(ctx context.Context, auth *coreauth.Auth, key string, cfg config.WindowRefreshConfig, now time.Time) {
	s.mu.Lock()
	s.inflight[key] = true
	s.lastTry[key] = now
	s.mu.Unlock()
	go func() {
		errSend := s.Send(ctx, auth, cfg)
		s.mu.Lock()
		delete(s.inflight, key)
		if errSend == nil {
			s.lastFired[key] = now
			delete(s.pending, key)
		}
		s.mu.Unlock()
		entry := log.WithField("provider", auth.Provider).WithField("account", key)
		switch {
		case errSend == nil:
			entry.Info("window refresh: ping sent")
		case errors.Is(errSend, ErrUnsupported):
			entry.Warn("window refresh: provider has no official SDK integration yet")
		default:
			entry.WithError(errSend).Warn("window refresh: ping failed")
		}
	}()
}
