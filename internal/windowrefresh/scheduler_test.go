package windowrefresh

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

type harness struct {
	s    *Scheduler
	now  time.Time
	end  time.Time // window end reported by the fake provider; zero = no window
	sent atomic.Int32
}

func newHarness(anchor string) *harness {
	h := &harness{now: t0}
	auth := &coreauth.Auth{ID: "id1", FileName: "a.json", Provider: "claude", Metadata: map[string]any{
		MetadataKey: map[string]any{"enabled": true, "anchor": anchor},
	}}
	h.s = &Scheduler{
		List:   func() []*coreauth.Auth { return []*coreauth.Auth{auth} },
		Config: func() config.WindowRefreshConfig { return config.WindowRefreshConfig{} },
		Now:    func() time.Time { return h.now },
		Usage:  func(context.Context, *coreauth.Auth) (time.Time, error) { return h.end, nil },
		Send: func(context.Context, *coreauth.Auth, config.WindowRefreshConfig) error {
			h.sent.Add(1)
			h.end = h.now.Add(Window) // a successful ping starts a window
			return nil
		},
	}
	return h
}

// tick runs one scheduler pass and waits for the background check to finish.
func (h *harness) tick() {
	h.s.Tick(context.Background())
	for i := 0; i < 2000; i++ {
		h.s.mu.Lock()
		busy := h.s.inflight["a.json"]
		h.s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAllowed(t *testing.T) {
	x := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	if !Allowed(x, x.Add(-6*time.Hour)) { // schedule 04:00, X-5h = 05:00
		t.Fatal("04:00 must run")
	}
	if Allowed(x, x.Add(-4*time.Hour)) { // schedule 06:00
		t.Fatal("06:00 must not run")
	}
	if !Allowed(time.Time{}, x) {
		t.Fatal("no anchor must run")
	}
}

func TestNoWindowPingsImmediatelyThenWaitsForReset(t *testing.T) {
	h := newHarness("")
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("expected immediate ping, got %d", h.sent.Load())
	}
	h.now = h.now.Add(time.Hour)
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("pinged while the window was running: %d", h.sent.Load())
	}
	h.now = t0.Add(Window) // window ended, provider reports none
	h.end = time.Time{}
	h.tick()
	if h.sent.Load() != 2 {
		t.Fatalf("expected ping at reset, got %d", h.sent.Load())
	}
}

func TestDisabledAuthStillGetsWindowRefresh(t *testing.T) {
	h := newHarness("")
	auth := &coreauth.Auth{ID: "disabled", FileName: "disabled.json", Provider: "claude", Disabled: true, Metadata: map[string]any{
		MetadataKey: map[string]any{"enabled": true},
	}}
	h.s.List = func() []*coreauth.Auth { return []*coreauth.Auth{auth} }
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("expected disabled auth to be refreshed, got %d pings", h.sent.Load())
	}
}

func TestRunningWindowIsNotPinged(t *testing.T) {
	h := newHarness("")
	h.end = t0.Add(2 * time.Hour)
	h.tick()
	h.now = t0.Add(2*time.Hour - time.Second)
	h.tick()
	if h.sent.Load() != 0 {
		t.Fatalf("pinged before the window ended: %d", h.sent.Load())
	}
	h.now = t0.Add(2 * time.Hour)
	h.end = time.Time{}
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("expected ping when the window ended, got %d", h.sent.Load())
	}
}

func TestAnchorBlocksPingThatWouldEndAfterIt(t *testing.T) {
	// Anchor 12:00: a ping now (10:00) ends 15:00 > anchor, so it is skipped until the anchor passes.
	h := newHarness(t0.Add(2 * time.Hour).Format(time.RFC3339))
	h.tick()
	if h.sent.Load() != 0 {
		t.Fatalf("pinged although it would end after the anchor")
	}
	h.now = t0.Add(2*time.Hour + time.Minute)
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("expected ping once the anchor passed, got %d", h.sent.Load())
	}
}

func TestAnchorAllowsEarlyPing(t *testing.T) {
	h := newHarness(t0.Add(6 * time.Hour).Format(time.RFC3339)) // X-5h = 11:00 > 10:00
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("expected ping before X-5h, got %d", h.sent.Load())
	}
}

func TestUnconfirmedWindowBacksOffAfterPing(t *testing.T) {
	h := newHarness("")
	h.s.Send = func(context.Context, *coreauth.Auth, config.WindowRefreshConfig) error {
		h.sent.Add(1) // provider keeps reporting no window (e.g. Codex still at 0%)
		return nil
	}
	h.tick()
	h.now = h.now.Add(retryAfter + time.Minute)
	h.tick()
	if h.sent.Load() != 1 {
		t.Fatalf("pinged again inside the back-off: %d", h.sent.Load())
	}
	h.now = h.now.Add(unconfirmedBackoff)
	h.tick()
	if h.sent.Load() != 2 {
		t.Fatalf("expected retry after the back-off, got %d", h.sent.Load())
	}
}
