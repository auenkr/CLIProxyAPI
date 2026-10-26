package quotahistory

import (
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
