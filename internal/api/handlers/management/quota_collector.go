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
		publishQuotaUsage(auth.ID, samples)
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
			recordPluginQuotaResets(auth.ID, quota)
			return pluginSamples(quota), nil
		}
	}
	if probe, ok := auth.Metadata["quota_probe"].(map[string]any); ok {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = (&http.Request{}).WithContext(ctx)
		quota, handled, errProbe := h.executeQuotaProbe(c, auth, probe)
		if handled {
			if errProbe == nil {
				recordPluginQuotaResets(auth.ID, quota)
			}
			return pluginSamples(quota), errProbe
		}
	}
	payload, errPayload := h.fetchQuotaPayload(ctx, auth)
	if errPayload != nil || payload == nil {
		return nil, errPayload
	}
	coreauth.SetQuotaResets(auth.ID, quotaPayloadResets(auth.Provider, payload))
	return quotaPayloadSamples(auth.Provider, name, at, payload), nil
}

// fetchQuotaPayload fetches the provider's usage document for a built-in OAuth credential.
// It returns (nil, nil) for providers without a usage endpoint.
func (h *Handler) fetchQuotaPayload(ctx context.Context, auth *coreauth.Auth) (map[string]any, error) {
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
	return payload, nil
}

// FetchFiveHourReset polls the provider's usage and returns the end of the running 5-hour
// window, or the zero time when no window is running.
func (h *Handler) FetchFiveHourReset(ctx context.Context, auth *coreauth.Auth) (time.Time, error) {
	payload, errPayload := h.fetchQuotaPayload(ctx, auth)
	if errPayload != nil {
		return time.Time{}, errPayload
	}
	if payload == nil {
		return time.Time{}, fmt.Errorf("window refresh: no usage endpoint for provider %s", auth.Provider)
	}
	resets := quotaPayloadResets(auth.Provider, payload)
	coreauth.SetQuotaResets(auth.ID, resets)
	if auth.Provider == "codex" && codexFiveHourUnused(payload) {
		// Codex reports a rolling reset time even before a window starts; 0% used means idle.
		return time.Time{}, nil
	}
	return resets[coreauth.QuotaWindowFiveHour], nil
}

// codexFiveHourUnused reports whether the Codex 5-hour window has no usage yet (not started).
func codexFiveHourUnused(payload map[string]any) bool {
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
		if quotaWindow(fmt.Sprintf("%.0f", seconds)) != coreauth.QuotaWindowFiveHour {
			continue
		}
		used, ok := quotaNumber(data["used_percent"])
		if !ok {
			used, ok = quotaNumber(data["usedPercent"])
		}
		return ok && used <= 0
	}
	return false
}

// quotaPayloadResets extracts upcoming reset instants per window from a usage payload.
func quotaPayloadResets(provider string, payload map[string]any) map[string]time.Time {
	resets := make(map[string]time.Time)
	switch provider {
	case "claude":
		for key, window := range map[string]string{"five_hour": coreauth.QuotaWindowFiveHour, "seven_day": coreauth.QuotaWindowWeekly} {
			data, _ := payload[key].(map[string]any)
			if at, ok := quotaTime(data["resets_at"]); ok {
				resets[window] = at
			}
		}
	case "codex":
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
			if window == "" || (window == coreauth.QuotaWindowFiveHour && codexFiveHourUnused(payload)) {
				continue
			}
			resetAt := data["reset_at"]
			if resetAt == nil {
				resetAt = data["resetAt"]
			}
			resetAfter := data["reset_after_seconds"]
			if resetAfter == nil {
				resetAfter = data["resetAfterSeconds"]
			}
			if at, ok := quotaTime(resetAt); ok {
				resets[window] = at
			} else if after, ok := quotaNumber(resetAfter); ok {
				resets[window] = time.Now().Add(time.Duration(after) * time.Second)
			}
		}
	case "antigravity":
		groups, _ := payload["groups"].([]any)
		for _, rawGroup := range groups {
			group, _ := rawGroup.(map[string]any)
			buckets, _ := group["buckets"].([]any)
			for _, rawBucket := range buckets {
				bucket, _ := rawBucket.(map[string]any)
				windowValue, _ := bucket["window"].(string)
				window := quotaWindow(windowValue)
				rawTime := bucket["resetTime"]
				if rawTime == nil {
					rawTime = bucket["reset_time"]
				}
				if at, ok := quotaTime(rawTime); ok && window != "" {
					if old, exists := resets[window]; !exists || at.Before(old) {
						resets[window] = at
					}
				}
			}
		}
	}
	return resets
}

// quotaTime parses an RFC3339 string or a unix timestamp in seconds or milliseconds.
func quotaTime(value any) (time.Time, bool) {
	if text, ok := value.(string); ok {
		if at, errParse := time.Parse(time.RFC3339, strings.TrimSpace(text)); errParse == nil {
			return at, true
		}
	}
	number, ok := quotaNumber(value)
	if !ok || number <= 0 {
		return time.Time{}, false
	}
	if number > 1e11 {
		return time.UnixMilli(int64(number)), true
	}
	return time.Unix(int64(number), 0), true
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

// publishQuotaUsage shares polled usage with the routing timeline. When a window has
// several buckets (e.g. Antigravity model groups) the most used one wins.
func publishQuotaUsage(authID string, samples []quotahistory.Sample) {
	used := make(map[string]float64)
	var at int64
	for _, sample := range samples {
		value := 100 - sample.Remaining
		if prev, ok := used[sample.Window]; !ok || value > prev {
			used[sample.Window] = value
		}
		if sample.At > at {
			at = sample.At
		}
	}
	coreauth.SetQuotaUsage(authID, used, time.UnixMilli(at))
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

// recordPluginQuotaResets preserves the last usable observation on malformed responses.
func recordPluginQuotaResets(authID string, quota pluginapi.QuotaFetchResponse) {
	resets := make(map[string]time.Time)
	for _, group := range quota.Groups {
		for _, bucket := range group.Buckets {
			window := quotaWindow(bucket.Window)
			if at, ok := quotaTime(bucket.ResetTime); ok && window != "" {
				if old, exists := resets[window]; !exists || at.Before(old) {
					resets[window] = at
				}
			}
		}
	}
	if len(resets) > 0 {
		coreauth.SetQuotaResets(authID, resets)
	}
}
