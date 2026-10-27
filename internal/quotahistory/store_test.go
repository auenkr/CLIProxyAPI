package quotahistory

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestAddDedupesPrunesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.json")
	base := time.Unix(1_800_000_000, 0)
	s := New(path)
	s.now = func() time.Time { return base }
	at := base.UnixMilli()

	n := s.Add([]Sample{
		{Provider: "claude", Name: "a", At: at, Remaining: 50},
		{Provider: "claude", Name: "a", At: at + 1000, Remaining: 49}, // too close
		{Provider: "claude", Name: "b", At: at, Remaining: 120},       // invalid
		{Provider: "claude", Name: "a", At: at - int64(Retention/time.Millisecond) - 1000, Remaining: 10},
	})
	if n != 1 {
		t.Fatalf("kept = %d, want 1", n)
	}
	reloaded := New(path)
	if got := reloaded.Since(0); len(got) != 1 || got[0].Remaining != 50 {
		t.Fatalf("reloaded = %+v", got)
	}
}

func TestIndependentWindowsAndReadPruning(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	s := New(filepath.Join(t.TempDir(), "history.json"))
	s.now = func() time.Time { return base }
	at := base.UnixMilli()
	if got := s.Add([]Sample{
		{Provider: "claude", Name: "a", Window: "5h", At: at, Remaining: 40},
		{Provider: "claude", Name: "a", Window: "weekly", At: at, Remaining: 80},
		{Provider: "claude", Name: "a", Window: "5h", At: at - 1000, Remaining: 45},
		{Provider: "claude", Name: "b", At: at, Remaining: math.NaN()},
	}); got != 2 {
		t.Fatalf("accepted %d samples, want 2", got)
	}
	s.SetRetention(24 * time.Hour)
	base = base.Add(25 * time.Hour)
	if got := s.Since(0); len(got) != 0 {
		t.Fatalf("expired samples returned: %+v", got)
	}
	if got := New(s.path).Since(0); len(got) != 0 {
		t.Fatalf("expired samples persisted: %+v", got)
	}
}

func TestRetentionChangePrunesImmediately(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	s := New("")
	s.now = func() time.Time { return base }
	s.Add([]Sample{{Name: "a", At: base.Add(-48 * time.Hour).UnixMilli(), Remaining: 50}})
	s.SetRetention(24 * time.Hour)
	if got := s.Since(0); len(got) != 0 {
		t.Fatalf("retention change retained expired samples: %+v", got)
	}
}
