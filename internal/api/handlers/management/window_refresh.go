package management

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/windowrefresh"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// StartWindowRefresh runs the 5-hour window refresh scheduler until ctx is cancelled.
func (h *Handler) StartWindowRefresh(ctx context.Context) {
	scheduler := &windowrefresh.Scheduler{
		List: func() []*coreauth.Auth {
			h.mu.Lock()
			manager := h.authManager
			h.mu.Unlock()
			if manager == nil {
				return nil
			}
			return manager.List()
		},
		Config: func() config.WindowRefreshConfig {
			if cfg := h.configSnapshot(); cfg != nil {
				return cfg.WindowRefresh
			}
			return config.WindowRefreshConfig{}
		},
		Send: windowrefresh.DefaultSender,
	}
	go scheduler.Run(ctx)
}

// GetWindowRefreshV8 returns the global settings and every account's saved settings.
func (h *Handler) GetWindowRefreshV8(c *gin.Context) {
	out := gin.H{"config": config.WindowRefreshConfig{}, "accounts": map[string]config.WindowRefreshAccount{}}
	if cfg := h.configSnapshot(); cfg != nil {
		out["config"] = cfg.WindowRefresh
	}
	accounts := map[string]config.WindowRefreshAccount{}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager != nil {
		for _, auth := range manager.List() {
			account, _ := windowrefresh.AccountSettings(auth)
			accounts[windowrefresh.AccountKey(auth)] = account
		}
	}
	out["accounts"] = accounts
	c.JSON(http.StatusOK, out)
}

// PutWindowRefreshV8 saves one account's window refresh settings on its auth record, which the
// auth store (file or Postgres) persists with the credential.
func (h *Handler) PutWindowRefreshV8(c *gin.Context) {
	var body struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Anchor  string `json:"anchor"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil || strings.TrimSpace(body.Name) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	anchor := strings.TrimSpace(body.Anchor)
	if anchor != "" {
		if _, errParse := time.Parse(time.RFC3339, anchor); errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "anchor must be an RFC3339 timestamp"})
			return
		}
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	name := strings.TrimSpace(body.Name)
	var target *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		target = auth
	} else {
		for _, auth := range h.authManager.List() {
			if auth.FileName == name {
				target = auth
				break
			}
		}
	}
	if target == nil || target.AuthSourceKind() == coreauth.AuthSourceConfig || coreauth.IsPluginVirtualAuth(target) {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	_, committed, errModify := h.authManager.Modify(c.Request.Context(), target.ID, target.RegistrationEpoch, func(current *coreauth.Auth) error {
		if current.Metadata == nil {
			current.Metadata = map[string]any{}
		}
		current.Metadata[windowrefresh.MetadataKey] = map[string]any{"enabled": body.Enabled, "anchor": anchor}
		return nil
	})
	if errModify != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errModify.Error()})
		return
	}
	if !committed {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
