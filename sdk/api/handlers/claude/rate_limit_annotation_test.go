package claude

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAnnotateClaudeRateLimit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	header := http.Header{}
	header.Set("Retry-After", "3723")
	resp := claudeErrorResponse{Type: "error", Error: claudeErrorDetail{Type: "rate_limit_error", Message: "cooling down"}}

	annotateClaudeRateLimit(header, &resp, now)

	if !strings.HasSuffix(resp.Error.Message, "(reset after 1h2m3s)") {
		t.Fatalf("message = %q", resp.Error.Message)
	}
	if got := header.Get("Anthropic-Ratelimit-Unified-Status"); got != "rejected" {
		t.Fatalf("status header = %q", got)
	}
	if got := header.Get("Anthropic-Ratelimit-Unified-Reset"); got != "1003723" {
		t.Fatalf("reset header = %q", got)
	}
}

func TestAnnotateClaudeRateLimitWithoutRetryAfter(t *testing.T) {
	header := http.Header{}
	resp := claudeErrorResponse{Error: claudeErrorDetail{Message: "limited"}}
	annotateClaudeRateLimit(header, &resp, time.Now())
	if resp.Error.Message != "limited" || len(header) != 0 {
		t.Fatalf("unexpected annotation: %q %v", resp.Error.Message, header)
	}
}
