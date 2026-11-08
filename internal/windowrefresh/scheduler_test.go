package windowrefresh

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func newTestScheduler(now *time.Time, account config.WindowRefreshAccount, sent *atomic.Int32) *Scheduler {
	auth := &coreauth.Auth{ID: "id1", FileName: "a.json", Provider: "claude", Metadata: map[string]any{
		MetadataKey: map[string]any{"enabled": account.Enabled, "anchor": account.Anchor},
	}}
	done := make(chan struct{}, 8)
	s := &Scheduler{
		List: func() []*coreauth.Auth { return []*coreauth.Auth{auth} },
		Config: func() config.WindowRefreshConfig {
			return config.WindowRefreshConfig{}
		},
		Now: func() time.Time { return *now },
	}
	s.Send = func(context.Context, *coreauth.Auth, config.WindowRefreshConfig) error {
		sent.Add(1)
		done <- struct{}{}
		return nil
	}
	return s
}

func tickAndWait(s *Scheduler, sent *atomic.Int32, want int32) {
	s.Tick(context.Background())
	for i := 0; i < 1000 && sent.Load() < want; i++ {
		time.Sleep(time.Millisecond)
	}
	// Wait for the background goroutine to release its in-flight slot.
	for i := 0; i < 1000; i++ {
		s.mu.Lock()
		busy := s.inflight["a.json"]
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func TestProviderResetWinsOverAnchor(t *testing.T) {
	now := t0
	var sent atomic.Int32
	// Anchor boundary (09:00) is in the past, but the provider says the window resets at 11:00.
	s := newTestScheduler(&now, config.WindowRefreshAccount{Enabled: true, Anchor: t0.Add(-time.Hour).Format(time.RFC3339)}, &sent)
	defer coreauth.SetQuotaResets("id1", nil)
	coreauth.SetQuotaResetsAt("id1", map[string]time.Time{coreauth.QuotaWindowFiveHour: t0.Add(time.Hour)}, now)

	now = t0.Add(StartupGrace + time.Minute)
	tickAndWait(s, &sent, 0)
	if sent.Load() != 0 {
		t.Fatalf("pinged while a window was running")
	}
	coreauth.SetQuotaResetsAt("id1", nil, now) // provider reports no window after the reset
	now = t0.Add(time.Hour + time.Second)
	tickAndWait(s, &sent, 1)
	if sent.Load() != 1 {
		t.Fatalf("expected ping at provider reset, got %d", sent.Load())
	}
	// Stale poll from before the ping must not trigger a second ping.
	now = now.Add(retryAfter + time.Minute)
	tickAndWait(s, &sent, 1)
	if sent.Load() != 1 {
		t.Fatalf("pinged again on stale data: %d", sent.Load())
	}
}

func TestIdleAccountStartsWindowAfterGrace(t *testing.T) {
	now := t0
	var sent atomic.Int32
	s := newTestScheduler(&now, config.WindowRefreshAccount{Enabled: true}, &sent)
	defer coreauth.SetQuotaResets("id1", nil)
	coreauth.SetQuotaResetsAt("id1", nil, now)

	tickAndWait(s, &sent, 0) // inside the startup grace period
	if sent.Load() != 0 {
		t.Fatalf("pinged during startup grace")
	}
	now = t0.Add(StartupGrace + time.Minute)
	tickAndWait(s, &sent, 1)
	if sent.Load() != 1 {
		t.Fatalf("idle account was not started, got %d", sent.Load())
	}
	now = now.Add(retryAfter + time.Minute)
	tickAndWait(s, &sent, 1)
	if sent.Load() != 1 {
		t.Fatalf("pinged again before a fresh poll: %d", sent.Load())
	}
}

func TestIdleAccountWaitsForAnchor(t *testing.T) {
	now := t0
	var sent atomic.Int32
	s := newTestScheduler(&now, config.WindowRefreshAccount{Enabled: true, Anchor: t0.Add(time.Hour).Format(time.RFC3339)}, &sent)
	defer coreauth.SetQuotaResets("id1", nil)
	coreauth.SetQuotaResetsAt("id1", nil, now)

	now = t0.Add(StartupGrace + time.Minute)
	tickAndWait(s, &sent, 0)
	if sent.Load() != 0 {
		t.Fatalf("pinged before anchor")
	}
	now = t0.Add(time.Hour + time.Minute)
	tickAndWait(s, &sent, 1)
	if sent.Load() != 1 {
		t.Fatalf("expected ping after anchor, got %d", sent.Load())
	}
}
