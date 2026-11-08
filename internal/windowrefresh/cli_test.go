package windowrefresh

import (
	"slices"
	"testing"
)

func TestPingArgsUseSmallModelsAndLowEffort(t *testing.T) {
	claude := claudePingArgs()
	if !slices.Contains(claude, "claude-haiku-5-5") || !slices.Contains(claude, "low") {
		t.Fatalf("claude args: %v", claude)
	}
	codex := codexPingArgs()
	if !slices.Contains(codex, "gpt-6-luna") || !slices.Contains(codex, `model_reasoning_effort="low"`) {
		t.Fatalf("codex args: %v", codex)
	}
}
