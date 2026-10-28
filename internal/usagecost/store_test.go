package usagecost

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func record(provider, model string, at time.Time) coreusage.Record {
	return coreusage.Record{
		Provider: provider, Model: model, RequestedAt: at,
		Detail: coreusage.Detail{TokenBreakdown: coreusage.TokenBreakdown{
			SchemaVersion: coreusage.TokenAccountingSchemaVersion,
			TotalTokens:   4_000_000,
			Input:         coreusage.TokenInputBreakdown{TotalTokens: 3_000_000, UncachedTokens: 1_000_000, CacheReadTokens: 2_000_000},
			Output:        coreusage.TokenOutputBreakdown{TotalTokens: 1_000_000, NonReasoningTokens: 600_000, ReasoningTokens: 400_000},
		}},
	}
}

func TestQueryPricesByModel(t *testing.T) {
	s := New("", NewPricer(map[string]Price{"custom-x": {Input: 2, Output: 4}}))
	now := time.Now()
	s.HandleUsage(context.Background(), record("claude", "claude-sonnet-5-5", now))
	s.HandleUsage(context.Background(), record("codex", "custom-x-1", now))
	s.HandleUsage(context.Background(), record("other", "mystery", now))

	r := s.Query(now.Add(-time.Hour))
	if len(r.Models) != 3 || len(r.Hourly) != 1 {
		t.Fatalf("models=%d hourly=%d", len(r.Models), len(r.Hourly))
	}
	// sonnet: 1*3 + 2*0.3 + 1*15 = 18.6
	if got := r.Models[0].Costs.Total; math.Abs(got-18.6) > 1e-9 || r.Models[0].Model != "claude-sonnet-5-5" {
		t.Fatalf("sonnet cost = %v (%s)", got, r.Models[0].Model)
	}
	// custom: 1*2 + 1*4 = 6
	if got := r.Models[1].Costs.Total; math.Abs(got-6) > 1e-9 {
		t.Fatalf("custom cost = %v", got)
	}
	if r.Unpriced != 4_000_000 || r.Models[2].Priced {
		t.Fatalf("unpriced=%d priced=%v", r.Unpriced, r.Models[2].Priced)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-cost.state")
	s := New(path, NewPricer(nil))
	now := time.Now()
	s.HandleUsage(context.Background(), record("claude", "claude-haiku-5", now))
	s.flush()
	r := New(path, NewPricer(nil)).Query(now.Add(-time.Hour))
	if r.Requests != 1 || r.Tokens.Total() != 4_000_000 {
		t.Fatalf("restored %+v", r.Tokens)
	}
}
