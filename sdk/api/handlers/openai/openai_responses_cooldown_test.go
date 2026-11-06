package openai

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestCodexCooldownResponse(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	msg := &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("gpt-5", "codex", 90*time.Second),
	}

	got := codexCooldownResponse(msg, now)
	if got == nil || !got.DirectResponse {
		t.Fatalf("expected direct response, got %+v", got)
	}
	if v := gjson.GetBytes(got.Body, "error.type").String(); v != "usage_limit_reached" {
		t.Fatalf("type = %q", v)
	}
	if v := gjson.GetBytes(got.Body, "error.resets_at").Int(); v != 2_000_090 {
		t.Fatalf("resets_at = %d", v)
	}
	if v := got.Headers.Get("X-Codex-Primary-Reset-At"); v != "2000090" {
		t.Fatalf("reset header = %q", v)
	}
}

func TestCodexCooldownResponseIgnoresOtherErrors(t *testing.T) {
	other := &interfaces.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: http.ErrAbortHandler}
	if codexCooldownResponse(other, time.Now()) != nil {
		t.Fatal("non-cooldown error must pass through")
	}
}
