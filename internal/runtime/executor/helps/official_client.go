package helps

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	gjson "github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	codexoptimize "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/optimize-multi-agent-v2"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

// officialClaudeEntrypoints are the Claude Code entrypoints accepted when a provider is
// restricted to its official client: the native CLI and VS Code extension plus the
// Claude Agent SDK (sdk-ts, sdk-py), which embeds Claude Code (used by apps such as T3 Code).
// This is wider than nativeClaudeEntrypoints on purpose and only affects the restriction.
var officialClaudeEntrypoints = map[string]bool{
	"cli":           true,
	"sdk-cli":       true,
	"claude-vscode": true,
	"sdk-ts":        true,
	"sdk-py":        true,
	// T3 Code launches Claude Code with CLAUDE_CODE_ENTRYPOINT=t3-code.
	"t3-code": true,
}

// officialDesktopEntrypoints are the Claude desktop app entrypoints. The desktop app
// (including its gateway credential probe) does not always send X-App, the Claude Code
// beta header, or metadata.user_id, so a matching native User-Agent is sufficient.
var officialDesktopEntrypoints = map[string]bool{
	"claude-desktop":    true,
	"claude-desktop-3p": true,
	"local-agent":       true,
	"local_agent":       true,
}

var geminiCLIUserAgent = regexp.MustCompile(`^GeminiCLI(?:-[a-zA-Z0-9-]+)?/[0-9]+\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9.-]+)?(?:/|\s|$)`)

// NewOfficialClientVerifier returns the verifier that enforces the per-credential
// official_client_only restriction. It inspects client-supplied headers and body,
// so it is a policy gate against accidental or casual use of a subscription
// credential through the proxy, not a defense against a client that deliberately
// forges those headers.
func NewOfficialClientVerifier(currentConfig func() *config.Config) cliproxyauth.OfficialClientVerifier {
	return func(ctx context.Context, provider string, opts cliproxyexecutor.Options) bool {
		headers := officialClientRequestHeaders(ctx, opts.Headers)
		accepted := verifyOfficialClient(currentConfig, provider, headers, opts)
		logOfficialClientDecision(provider, accepted, headers, opts)
		return accepted
	}
}

// logOfficialClientDecision records every official-client verdict with the
// client-identifying headers so unblocking another client later needs no guessing.
// Header values for credentials are never logged; other headers are listed by name only.
func logOfficialClientDecision(provider string, accepted bool, headers http.Header, opts cliproxyexecutor.Options) {
	path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	userAgent := strings.TrimSpace(headerValue(headers, "User-Agent"))
	entrypoint, _ := parseClaudeCodeUserAgentDetails(userAgent)
	hasUserID := gjson.GetBytes(opts.OriginalRequest, "metadata.user_id").Type == gjson.String
	log.WithFields(log.Fields{
		"provider":       provider,
		"accepted":       accepted,
		"path":           path,
		"user_agent":     userAgent,
		"entrypoint":     entrypoint,
		"x_app":          headerValue(headers, "X-App"),
		"anthropic_beta": headerValue(headers, "Anthropic-Beta"),
		"originator":     headerValue(headers, "Originator"),
		"session_id":     headerValue(headers, "X-Claude-Code-Session-Id") != "",
		"has_user_id":    hasUserID,
		"header_names":   strings.Join(names, ","),
	}).Info("official client check")
}

func verifyOfficialClient(currentConfig func() *config.Config, provider string, headers http.Header, opts cliproxyexecutor.Options) bool {
	switch provider {
	case "claude":
		var cfg *config.Config
		if currentConfig != nil {
			cfg = currentConfig()
		}
		path, _ := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string)
		countTokens := strings.HasSuffix(strings.TrimSpace(path), "/count_tokens")
		return IsOfficialClaudeCodeRequest(headers, opts.OriginalRequest, countTokens, cfg)
	case "codex":
		userAgent := headerValue(headers, "User-Agent")
		// T3 Code runs the Codex app-server with originator "T3 Code".
		return codexoptimize.IsCodexClientUserAgent(userAgent) ||
			strings.HasPrefix(strings.TrimSpace(userAgent), "T3 Code/")
	case "gemini-cli":
		return geminiCLIUserAgent.MatchString(strings.TrimSpace(headerValue(headers, "User-Agent")))
	default:
		return false
	}
}

// IsOfficialClaudeCodeRequest reports whether a request carries the signals of a
// first-party Claude Code CLI/VS Code session. Unlike DetectClaudeCodeRequest's
// Confirmed flag it does not pin the User-Agent version to the configured
// baseline, so a Claude Code auto-update never locks genuine users out.
func IsOfficialClaudeCodeRequest(headers http.Header, payload []byte, countTokens bool, cfg *config.Config) bool {
	if DetectClaudeCodeRequest(headers, payload, countTokens, cfg).Confirmed {
		return true
	}
	userAgent := strings.TrimSpace(headerValue(headers, "User-Agent"))
	parsed, validUserAgent := parseClaudeCodeUserAgent(userAgent)
	if !validUserAgent {
		return false
	}
	entrypoint := parsed.entrypoint
	if officialDesktopEntrypoints[entrypoint] {
		return true
	}
	if !officialClaudeEntrypoints[entrypoint] {
		return false
	}
	if headerValue(headers, "X-App") != "cli" || !headerContainsClaudeCodeBeta(headers) {
		return false
	}
	if countTokens {
		return true
	}
	userID := gjson.GetBytes(payload, "metadata.user_id")
	return userID.Type == gjson.String && isValidUserID(userID.String())
}

// officialClientRequestHeaders merges the inbound gin request headers with the
// headers carried on the execution options, the latter taking precedence.
func officialClientRequestHeaders(ctx context.Context, incoming http.Header) http.Header {
	resolved := make(http.Header, len(incoming))
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			resolved = ginCtx.Request.Header.Clone()
		}
	}
	for key, values := range incoming {
		resolved[key] = append([]string(nil), values...)
	}
	return resolved
}
