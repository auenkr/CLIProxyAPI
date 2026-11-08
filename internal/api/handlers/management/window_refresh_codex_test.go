package management

import "testing"

func TestCodexFiveHourUnused(t *testing.T) {
	idle := map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{
		"limit_window_seconds": 18000.0, "used_percent": 0.0, "reset_at": 1794150023.0}}}
	if !codexFiveHourUnused(idle) {
		t.Fatal("0% used must be idle")
	}
	running := map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{
		"limit_window_seconds": 18000.0, "used_percent": 3.0, "reset_at": 1794150023.0}}}
	if codexFiveHourUnused(running) {
		t.Fatal("3% used must be running")
	}
	if codexFiveHourUnused(map[string]any{}) {
		t.Fatal("missing data must not be treated as idle")
	}
}
