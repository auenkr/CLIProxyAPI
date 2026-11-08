package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestSoonestResetSelectorPicksEarliestReset(t *testing.T) {
	now := time.Now()
	SetQuotaResets("sr-late", map[string]time.Time{QuotaWindowWeekly: now.Add(96 * time.Hour)})
	SetQuotaResets("sr-soon", map[string]time.Time{QuotaWindowWeekly: now.Add(20 * time.Hour)})
	SetQuotaResets("sr-past", map[string]time.Time{QuotaWindowWeekly: now.Add(-time.Hour)})
	t.Cleanup(func() {
		for _, id := range []string{"sr-late", "sr-soon", "sr-past", "sr-unknown"} {
			SetQuotaResets(id, nil)
		}
	})

	selector := &SoonestResetSelector{}
	auths := []*Auth{{ID: "sr-unknown"}, {ID: "sr-late"}, {ID: "sr-soon"}, {ID: "sr-past"}}
	for i := 0; i < 3; i++ {
		got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		if got.ID != "sr-soon" {
			t.Fatalf("Pick() = %q, want sr-soon", got.ID)
		}
	}
}

func TestSoonestResetSelectorFallsBackWithoutResetData(t *testing.T) {
	selector := &SoonestResetSelector{}
	auths := []*Auth{{ID: "sr-x"}, {ID: "sr-y"}}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		seen[got.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("fallback should round-robin, saw %v", seen)
	}
}

func TestSoonestResetEligibilityAndWindows(t *testing.T) {
	now := time.Now()
	a := &Auth{ID: "sr-eligible-a"}
	b := &Auth{ID: "sr-eligible-b"}
	t.Cleanup(func() { SetQuotaResets(a.ID, nil); SetQuotaResets(b.ID, nil) })
	SetQuotaResets(a.ID, map[string]time.Time{QuotaWindowWeekly: now.Add(48 * time.Hour), QuotaWindowFiveHour: now.Add(time.Hour)})
	SetQuotaResets(b.ID, map[string]time.Time{QuotaWindowFiveHour: now.Add(3 * time.Hour)})
	selector := &SoonestResetSelector{}
	pick := func(ctx context.Context, provider, want string) {
		t.Helper()
		got, err := selector.Pick(ctx, provider, "", cliproxyexecutor.Options{}, []*Auth{a, b})
		if err != nil || got.ID != want {
			t.Fatalf("Pick = %v, %v; want %s", got, err, want)
		}
	}
	pick(context.Background(), "claude", b.ID)
	a.Attributes = map[string]string{"priority": "10"}
	pick(context.Background(), "claude", a.ID)
	a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}
	pick(context.Background(), "claude", b.ID)
	a.Quota = QuotaState{}
	a.Attributes = map[string]string{"websockets": "true"}
	pick(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), "codex", a.ID)
}

func TestSoonestResetBuiltInUsesPrevalidatedCandidates(t *testing.T) {
	selector := &SoonestResetSelector{}
	if !isBuiltInSelector(selector) || selectionArgForSelector(selector, "public-alias") != "" {
		t.Fatal("soonest-reset must use built-in model handling")
	}
	if selectorStrategy(selector) != schedulerStrategyCustom {
		t.Fatal("reset selection must bypass scheduler fast path")
	}
	ctx := selectorContextForAvailableAuths(context.Background(), selector, "public-alias")
	if valid, _ := ctx.Value(prevalidatedAuthCandidatesKey{}).(bool); !valid {
		t.Fatal("missing prevalidated candidate context")
	}
}

func TestSoonestResetSessionAffinity(t *testing.T) {
	now := time.Now()
	a, b := &Auth{ID: "sr-affinity-a"}, &Auth{ID: "sr-affinity-b"}
	t.Cleanup(func() { SetQuotaResets(a.ID, nil); SetQuotaResets(b.ID, nil) })
	SetQuotaResets(a.ID, map[string]time.Time{QuotaWindowWeekly: now.Add(time.Hour)})
	SetQuotaResets(b.ID, map[string]time.Time{QuotaWindowWeekly: now.Add(2 * time.Hour)})
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	opts := cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_xxx_account__session_ac980658-63bd-4fb3-97ba-8da64cb1e344"}}`)}
	pick := func(want string) {
		t.Helper()
		got, err := selector.Pick(context.Background(), "claude", "", opts, []*Auth{a, b})
		if err != nil || got.ID != want {
			t.Fatalf("Pick = %v, %v; want %s", got, err, want)
		}
	}
	pick(a.ID)
	SetQuotaResets(b.ID, map[string]time.Time{QuotaWindowWeekly: now.Add(30 * time.Minute)})
	pick(a.ID)
	a.Disabled = true
	pick(b.ID)
}
