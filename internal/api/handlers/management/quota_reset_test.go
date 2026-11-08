package management

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"testing"
	"time"
)

func TestQuotaResetExtraction(t *testing.T) {
	at := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name, provider string
		payload        map[string]any
		window         string
	}{
		{"claude", "claude", map[string]any{"seven_day": map[string]any{"resets_at": at.Format(time.RFC3339)}}, "weekly"},
		{"codex", "codex", map[string]any{"rate_limit": map[string]any{"secondary_window": map[string]any{"limit_window_seconds": 604800.0, "reset_at": float64(at.Unix())}}}, "weekly"},
		{"codex camel", "codex", map[string]any{"rateLimit": map[string]any{"secondaryWindow": map[string]any{"limitWindowSeconds": 604800.0, "resetAt": float64(at.Unix())}}}, "weekly"},
		{"antigravity", "antigravity", map[string]any{"groups": []any{map[string]any{"buckets": []any{map[string]any{"window": "week", "resetTime": at.Format(time.RFC3339)}}}}}, "weekly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaPayloadResets(tc.provider, tc.payload)[tc.window]; !got.Equal(at) {
				t.Fatalf("reset = %v; want %v", got, at)
			}
		})
	}
	payload := map[string]any{"rate_limit": map[string]any{"primary_window": map[string]any{"limit_window_seconds": 18000.0, "used_percent": 0.0, "reset_after_seconds": 3600.0}}}
	if len(quotaPayloadResets("codex", payload)) != 0 {
		t.Fatal("idle 5h window must not rank credentials")
	}
	payload["rate_limit"].(map[string]any)["primary_window"].(map[string]any)["used_percent"] = 10.0
	before := time.Now().Add(time.Hour)
	got := quotaPayloadResets("codex", payload)["5h"]
	if got.Before(before) || got.After(time.Now().Add(time.Hour)) {
		t.Fatalf("relative reset = %v", got)
	}
}

func TestPluginQuotaResetObservation(t *testing.T) {
	const id = "sr-plugin-observation"
	t.Cleanup(func() { coreauth.SetQuotaResets(id, nil) })
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	quota := pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{{Window: "week", ResetTime: at.Format(time.RFC3339)}}}}}
	recordPluginQuotaResets(id, quota)
	recordPluginQuotaResets(id, pluginapi.QuotaFetchResponse{})
	if got, ok := coreauth.NextQuotaReset(id, time.Now()); !ok || !got.Equal(at) {
		t.Fatalf("reset = %v, %v", got, ok)
	}
}

func TestQuotaCollectorPluginResets(t *testing.T) {
	const id = "sr-plugin-collector"
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: id, Provider: "fixture-reset"}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(context.Background(), id)
	host := pluginhost.New()
	host.RegisterPluginForTest("fixture-reset", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: &mockQuotaProvider{identifier: "fixture-reset", fetchFn: func(context.Context, pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
		return pluginapi.QuotaFetchResponse{Groups: []pluginapi.QuotaGroup{{Buckets: []pluginapi.QuotaBucket{{Window: "weekly", ResetTime: at.Format(time.RFC3339)}}}}}, nil
	}}}})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	h.SetPluginHost(host)
	h.collectQuota(context.Background())
	if got, ok := coreauth.NextQuotaReset(id, time.Now()); !ok || !got.Equal(at) {
		t.Fatalf("collector reset = %v, %v", got, ok)
	}
	manager.Remove(context.Background(), id)
	if _, ok := coreauth.NextQuotaReset(id, time.Now()); ok {
		t.Fatal("deleted credential retained reset data")
	}
}
