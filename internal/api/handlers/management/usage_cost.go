package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagecost"
)

// GetUsageCost returns the API-equivalent cost report for ?days=N (default 7).
func (h *Handler) GetUsageCost(c *gin.Context) {
	if h == nil || h.usageCost == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage cost unavailable"})
		return
	}
	days, _ := strconv.Atoi(c.Query("days"))
	maxDays := int(usagecost.Retention / (24 * time.Hour))
	if days <= 0 {
		days = 7
	}
	days = min(days, maxDays)
	c.JSON(http.StatusOK, h.usageCost.Query(time.Now().Add(-time.Duration(days)*24*time.Hour)))
}
