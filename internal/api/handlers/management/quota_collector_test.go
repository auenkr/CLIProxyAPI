package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestQuotaPayloadSamples(t *testing.T) {
	for _, tc := range []struct {
		provider string
		payload  string
	}{
		{"claude", `{"five_hour":{"utilization":20},"seven_day":{"utilization":40}}`},
		{"codex", `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":20},"secondary_window":{"limit_window_seconds":604800,"used_percent":40}}}`},
		{"codex", `{"rateLimit":{"primaryWindow":{"limitWindowSeconds":"18000","usedPercent":"20"},"secondaryWindow":{"limitWindowSeconds":"604800","usedPercent":"40"}}}`},
		{"antigravity", `{"groups":[{"buckets":[{"window":"5h","remainingFraction":0.8},{"window":"week","remaining_fraction":"0.6"}]}]}`},
	} {
		t.Run(tc.provider+tc.payload[:10], func(t *testing.T) {
			var payload map[string]any
			if errDecode := json.Unmarshal([]byte(tc.payload), &payload); errDecode != nil {
				t.Fatal(errDecode)
			}
			samples := quotaPayloadSamples(tc.provider, "credential", 123, payload)
			if len(samples) != 2 || samples[0].Window != "5h" || samples[0].Remaining != 80 || samples[1].Window != "weekly" || samples[1].Remaining != 60 {
				t.Fatalf("samples = %+v", samples)
			}
		})
	}
}

func TestQuotaCollectorRecordsWithoutUI(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"main","buckets":[{"window":"5h","remainingFraction":0.8}]}]}`))
	}))
	defer upstream.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	var probe map[string]any
	if errDecode := json.NewDecoder(strings.NewReader(`{"url":"` + upstream.URL + `","method":"GET"}`)).Decode(&probe); errDecode != nil {
		t.Fatal(errDecode)
	}
	_, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "a", Index: "a", FileName: "a.json", Provider: "fixture", Metadata: map[string]any{"quota_probe": probe}})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandler(&config.Config{}, "", manager)
	h.collectQuota(context.Background())
	if got := h.quotaHistory().Since(0); len(got) != 1 || got[0].Remaining != 80 || got[0].Provider != "plugin" || !strings.HasPrefix(got[0].Name, "a.json\x00") {
		t.Fatalf("background collection = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.collectQuota(ctx)
	if got := h.quotaHistory().Since(0); len(got) != 1 {
		t.Fatalf("canceled collection changed samples: %+v", got)
	}
}
