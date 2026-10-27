package management

import (
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

func (h *Handler) quotaHistory() *quotahistory.Store {
	h.quotaHistoryOnce.Do(func() {
		path := ""
		if h.configFilePath != "" {
			path = filepath.Join(filepath.Dir(h.configFilePath), "quota-history.json")
		}
		h.quotaHistoryStore = quotahistory.New(path)
	})
	retention := quotahistory.Retention
	if cfg := h.configSnapshot(); cfg != nil && cfg.QuotaHistoryRetentionDays > 0 {
		// Bound the duration before multiplication to avoid integer overflow.
		days := min(cfg.QuotaHistoryRetentionDays, 36500)
		retention = time.Duration(days) * 24 * time.Hour
	}
	h.quotaHistoryStore.SetRetention(retention)
	return h.quotaHistoryStore
}

// GetQuotaHistory returns recorded quota samples, optionally limited by ?since=<unix ms>.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	since, _ := strconv.ParseInt(c.Query("since"), 10, 64)
	c.JSON(http.StatusOK, gin.H{"samples": h.quotaHistory().Since(since)})
}

// PostQuotaHistory records quota samples observed by the management UI.
func (h *Handler) PostQuotaHistory(c *gin.Context) {
	var req struct {
		Samples []quotahistory.Sample `json:"samples"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"recorded": h.quotaHistory().Add(req.Samples)})
}
