package openai

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// WriteErrorResponse reports proxy-side model cooldowns in the usage_limit_reached
// form Codex clients parse, so they learn when the limit resets and can retry.
func (h *OpenAIResponsesAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage) {
	if direct := codexCooldownResponse(msg, time.Now()); direct != nil {
		h.BaseAPIHandler.WriteErrorResponse(c, direct)
		return
	}
	h.BaseAPIHandler.WriteErrorResponse(c, msg)
}

func codexCooldownResponse(msg *interfaces.ErrorMessage, now time.Time) *interfaces.ErrorMessage {
	if msg == nil || msg.DirectResponse || msg.Error == nil || msg.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	var marker interface{ IsModelCooldown() bool }
	if !errors.As(msg.Error, &marker) || marker == nil || !marker.IsModelCooldown() {
		return nil
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(coreauth.SafeResponseHeaders(msg.Error).Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return nil
	}
	resetsAt := now.Add(time.Duration(seconds) * time.Second).Unix()
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type":              "usage_limit_reached",
		"message":           "The usage limit has been reached",
		"resets_at":         resetsAt,
		"resets_in_seconds": seconds,
	}})
	if err != nil {
		return nil
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Retry-After", strconv.Itoa(seconds))
	headers.Set("X-Codex-Primary-Used-Percent", "100")
	headers.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(resetsAt, 10))
	return &interfaces.ErrorMessage{
		StatusCode:     http.StatusTooManyRequests,
		Error:          msg.Error,
		DirectResponse: true,
		Body:           body,
		Headers:        headers,
	}
}
