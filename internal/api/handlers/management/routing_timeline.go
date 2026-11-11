package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetRoutingTimelineStatus reports the per-credential usage and reset data the routing
// timeline acts on, plus which credentials the timeline is routing to right now.
func (h *Handler) GetRoutingTimelineStatus(c *gin.Context) {
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	now := time.Now()
	active, protected := coreauth.ActiveTimelineAccounts(now)
	accounts := make([]gin.H, 0)
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		usage := gin.H{}
		for _, window := range []string{coreauth.QuotaWindowFiveHour, coreauth.QuotaWindowWeekly} {
			if used, ok := coreauth.QuotaUsage(auth.ID, window); ok {
				usage[window] = used
			}
		}
		resets := gin.H{}
		for window, at := range coreauth.QuotaResets(auth.ID) {
			if at.After(now) {
				resets[window] = at.UTC().Format(time.RFC3339)
			}
		}
		name := authEmail(auth)
		if name == "" {
			name = strings.TrimSpace(auth.Label)
		}
		if name == "" {
			name = auth.ID
		}
		accounts = append(accounts, gin.H{
			"id":       auth.ID,
			"provider": strings.ToLower(strings.TrimSpace(auth.Provider)),
			"name":     name,
			"disabled": auth.Disabled,
			"usage":    usage,
			"resets":   resets,
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		pi, pj := accounts[i]["provider"].(string), accounts[j]["provider"].(string)
		if pi != pj {
			return pi < pj
		}
		return accounts[i]["name"].(string) < accounts[j]["name"].(string)
	})
	c.JSON(http.StatusOK, gin.H{
		"now":       now.UTC().Format(time.RFC3339),
		"accounts":  accounts,
		"active":    active,
		"protected": protected,
	})
}
