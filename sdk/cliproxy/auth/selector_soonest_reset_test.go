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
