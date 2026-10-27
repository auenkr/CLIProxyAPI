package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const quotaCollectionInterval = 5 * time.Minute

// StartQuotaCollector records history independently of management UI requests.
func (h *Handler) StartQuotaCollector(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(quotaCollectionInterval)
		defer ticker.Stop()
		for {
			h.collectQuota(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (h *Handler) collectQuota(ctx context.Context) {
	store := h.quotaHistory()
	store.Since(0) // Prune even when no credentials can be polled.
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return
	}
	for _, auth := range manager.List() {
		if ctx.Err() != nil {
			return
		}
		if auth == nil || auth.Disabled {
			continue
		}
		samples, errCollect := h.collectCredentialQuota(ctx, auth)
		if errCollect != nil {
			log.WithField("provider", auth.Provider).Debug("quota history: collection failed")
			continue
		}
		store.Add(samples)
	}
}

func (h *Handler) collectCredentialQuota(ctx context.Context, auth *coreauth.Auth) ([]quotahistory.Sample, error) {
	name := auth.FileName
	if name == "" {
		name = auth.ID
	}
	at := time.Now().UnixMilli()
	makeSample := func(window string, remaining float64) quotahistory.Sample {
		return quotahistory.Sample{Provider: auth.Provider, Name: name, At: at, Window: window, Remaining: remaining}
	}
	pluginSamples := func(quota pluginapi.QuotaFetchResponse) []quotahistory.Sample {
		// Match frontend plugin cache identities, including virtual credentials.
		var samples []quotahistory.Sample
		for _, group := range quota.Groups {
			for _, bucket := range group.Buckets {
				if window := quotaWindow(bucket.Window); window != "" {
					sample := makeSample(window, bucket.RemainingFraction*100)
					switch auth.Provider {
					case "antigravity", "claude", "codex", "kimi", "kimi-ai", "meta", "xai":
						// Built-in quota cards use the physical filename.
					case "devin":
						sample.Name += "\x00" + auth.Index
					default:
						sample.Provider = "plugin"
						sample.Name += "\x00" + auth.Index
					}
					samples = append(samples, sample)
				}
			}
		}
		return samples
	}
	h.mu.Lock()
	host := h.pluginHost
	h.mu.Unlock()
	if host != nil {
		quota, handled, errFetch := host.FetchQuota(ctx, pluginapi.QuotaFetchRequest{
			AuthIndex: auth.Index, AuthID: auth.ID, Provider: auth.Provider, Metadata: auth.Metadata, Attributes: auth.Attributes,
		})
		if handled {
			if errFetch != nil {
				return nil, errFetch
			}
			return pluginSamples(quota), nil
		}
	}
	if probe, ok := auth.Metadata["quota_probe"].(map[string]any); ok {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = (&http.Request{}).WithContext(ctx)
		quota, handled, errProbe := h.executeQuotaProbe(c, auth, probe)
		if handled {
			return pluginSamples(quota), errProbe
		}
	}
	url := ""
	method := http.MethodGet
	var body io.Reader
	switch auth.Provider {
	case "claude":
		url = "https://api.anthropic.com/api/oauth/usage"
	case "codex":
		url = "https://chatgpt.com/backend-api/wham/usage"
	case "antigravity":
		project, _ := auth.Metadata["project_id"].(string)
		if project == "" {
			project, _ = auth.Metadata["projectId"].(string)
		}
		if project == "" {
			for _, key := range []string{"project_id", "projectId", "gemini_virtual_project"} {
				if project = strings.TrimSpace(auth.Attributes[key]); project != "" {
					break
				}
			}
		}
		if project == "" {
			return nil, fmt.Errorf("quota history: missing project")
		}
		data, _ := json.Marshal(map[string]string{"project": project})
		body = bytes.NewReader(data)
		method = http.MethodPost
		url = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	default:
		return nil, nil
	}
	token, errToken := h.resolveTokenForAuth(ctx, auth, "")
	if errToken != nil || token == "" {
		return nil, fmt.Errorf("quota history: credential token unavailable")
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, url, body)
	if errRequest != nil {
		return nil, errRequest
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if auth.Provider == "claude" {
		req.Header.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	} else if auth.Provider == "antigravity" {
		req.Header.Set("User-Agent", "antigravity/hub/2.23.0 darwin/arm64")
	} else {
		req.Header.Set("User-Agent", "codex_cli_rs/0.134.0")
		for _, key := range []string{"account_id", "chatgpt_account_id"} {
			if value, ok := auth.Metadata[key].(string); ok && value != "" {
				req.Header.Set("Chatgpt-Account-Id", value)
				break
			}
		}
	}
	client := &http.Client{Transport: h.apiCallTransport(auth, "")}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return nil, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debug("quota history: response close failed")
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota history: upstream status %d", resp.StatusCode)
	}
	var payload map[string]any
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); errDecode != nil {
		return nil, errDecode
	}
	return quotaPayloadSamples(auth.Provider, name, at, payload), nil
}

func quotaWindow(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "5h", "five_hour", "five-hour", "18000", "18000s":
		return "5h"
	case "weekly", "week", "7d", "seven_day", "seven-day", "604800", "604800s":
		return "weekly"
	}
	return ""
}

func quotaPayloadSamples(provider, name string, at int64, payload map[string]any) []quotahistory.Sample {
	var samples []quotahistory.Sample
	appendWindow := func(window string, raw any, percentKey, alternate string) {
		data, ok := raw.(map[string]any)
		if !ok {
			return
		}
		value, exists := data[percentKey]
		if !exists {
			value = data[alternate]
		}
		used, ok := quotaNumber(value)
		if !ok {
			return
		}
		samples = append(samples, quotahistory.Sample{Provider: provider, Name: name, At: at, Window: window, Remaining: 100 - used})
	}
	if provider == "claude" {
		appendWindow("5h", payload["five_hour"], "utilization", "utilization")
		appendWindow("weekly", payload["seven_day"], "utilization", "utilization")
	} else if provider == "codex" {
		raw := payload["rate_limit"]
		if raw == nil {
			raw = payload["rateLimit"]
		}
		limits, _ := raw.(map[string]any)
		for _, key := range []string{"primary_window", "secondary_window", "primaryWindow", "secondaryWindow"} {
			data, _ := limits[key].(map[string]any)
			seconds, ok := quotaNumber(data["limit_window_seconds"])
			if !ok {
				seconds, _ = quotaNumber(data["limitWindowSeconds"])
			}
			window := quotaWindow(fmt.Sprintf("%.0f", seconds))
			if window != "" {
				appendWindow(window, data, "used_percent", "usedPercent")
			}
		}
	} else if provider == "antigravity" {
		groups, _ := payload["groups"].([]any)
		for _, rawGroup := range groups {
			group, _ := rawGroup.(map[string]any)
			buckets, _ := group["buckets"].([]any)
			for _, rawBucket := range buckets {
				bucket, _ := rawBucket.(map[string]any)
				value, ok := quotaNumber(bucket["remainingFraction"])
				if !ok {
					value, ok = quotaNumber(bucket["remaining_fraction"])
				}
				windowValue, _ := bucket["window"].(string)
				if window := quotaWindow(windowValue); ok && window != "" {
					samples = append(samples, quotahistory.Sample{Provider: provider, Name: name, At: at, Window: window, Remaining: value * 100})
				}
			}
		}
	}
	return samples
}

func quotaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		number, errParse := strconv.ParseFloat(typed, 64)
		return number, errParse == nil
	default:
		return 0, false
	}
}
