package helps

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOfficialGeminiCLI(t *testing.T) {
	verify := NewOfficialClientVerifier(nil)
	for _, tc := range []struct {
		ua   string
		want bool
	}{
		{"GeminiCLI/0.34.0/gemini-pro (linux; x64; terminal)", true},
		{"GeminiCLI-a2a-server/0.34.0/gemini-pro (linux; x64; vscode)", true},
		{"GeminiCLI-fake", false},
		{"other GeminiCLI/0.34.0", false},
		{"", false},
	} {
		t.Run(tc.ua, func(t *testing.T) {
			got := verify(context.Background(), "gemini-cli", cliproxyexecutor.Options{Headers: http.Header{"User-Agent": []string{tc.ua}}})
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
