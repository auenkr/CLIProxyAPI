package windowrefresh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	pingPrompt = "Hi"
	// The cheapest models and lowest effort, so a ping uses as little quota as possible.
	claudePingModel = "claude-haiku-5-5"
	codexPingModel  = "gpt-6-luna"
	pingEffort      = "low"
)

// DefaultSender routes each provider to its installed official CLI.
func DefaultSender(ctx context.Context, auth *coreauth.Auth, cfg config.WindowRefreshConfig) error {
	switch strings.ToLower(auth.Provider) {
	case "claude":
		return sendClaude(ctx, auth, cfg)
	case "codex":
		return sendCodex(ctx, auth, cfg)
	default:
		return ErrUnsupported
	}
}

func metaString(auth *coreauth.Auth, key string) string {
	v, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(v)
}

// resolveBinary returns the configured path, or the named binary from PATH.
func resolveBinary(configured, name string) (string, error) {
	if path := strings.TrimSpace(configured); path != "" {
		return path, nil
	}
	found, errLook := exec.LookPath(name)
	if errLook != nil {
		return "", fmt.Errorf("%s binary not found; set window-refresh.%s-binary-path", name, name)
	}
	return found, nil
}

// stateDir is where per-account Claude config dirs live so each account keeps one stable
// device identity between pings.
func stateDir(cfg config.WindowRefreshConfig) (string, error) {
	if dir := strings.TrimSpace(cfg.StateDir); dir != "" {
		return dir, nil
	}
	base, errCache := os.UserCacheDir()
	if errCache != nil {
		return "", errCache
	}
	return filepath.Join(base, "cliproxy-window-refresh"), nil
}

// sendClaude runs `claude -p` against a Claude config dir holding this account's
// .credentials.json (the same login method as an interactive `claude` session), deleted after the
// run. The refresh
// token is left empty so the CLI cannot rotate the proxy's stored credential.
func sendClaude(ctx context.Context, auth *coreauth.Auth, cfg config.WindowRefreshConfig) error {
	access := metaString(auth, "access_token")
	if access == "" {
		return fmt.Errorf("claude account has no access token")
	}
	binary, errBinary := resolveBinary(cfg.ClaudeBinaryPath, "claude")
	if errBinary != nil {
		return errBinary
	}
	base, errBase := stateDir(cfg)
	if errBase != nil {
		return errBase
	}
	sum := sha256.Sum256([]byte(AccountKey(auth)))
	dir := filepath.Join(base, "claude", hex.EncodeToString(sum[:6]))
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return errMkdir
	}
	expiresAt := time.Now().Add(time.Hour)
	if parsed, errParse := time.Parse(time.RFC3339, metaString(auth, "expired")); errParse == nil {
		expiresAt = parsed
	}
	credentials := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"","expiresAt":%d,"scopes":["user:inference","user:profile"]}}`,
		access, expiresAt.UnixMilli())
	credentialsPath := filepath.Join(dir, ".credentials.json")
	if errWrite := os.WriteFile(credentialsPath, []byte(credentials), 0o600); errWrite != nil {
		return errWrite
	}
	// Remove the written credential afterwards; the dir keeps only the CLI's device identity.
	defer func() { _ = os.Remove(credentialsPath) }()
	return runCLI(ctx, auth, dir, binary, claudePingArgs(),
		[]string{"CLAUDE_CONFIG_DIR=" + dir}, access)
}

// sendCodex runs `codex exec` with an isolated CODEX_HOME holding this account's auth.json.
// The refresh token is left empty so the CLI cannot rotate the proxy's stored credential.
func sendCodex(ctx context.Context, auth *coreauth.Auth, cfg config.WindowRefreshConfig) error {
	access, idToken := metaString(auth, "access_token"), metaString(auth, "id_token")
	if access == "" {
		return fmt.Errorf("codex account has no access token")
	}
	if idToken == "" {
		// The Codex CLI rejects auth.json unless id_token is a JWT.
		return fmt.Errorf("codex account has no id_token")
	}
	binary, errBinary := resolveBinary(cfg.CodexBinaryPath, "codex")
	if errBinary != nil {
		return errBinary
	}
	home, errTemp := os.MkdirTemp("", "cliproxy-window-*")
	if errTemp != nil {
		return errTemp
	}
	defer func() { _ = os.RemoveAll(home) }()
	authJSON := fmt.Sprintf(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":%q,"access_token":%q,"refresh_token":"","account_id":%q},"last_refresh":%q}`,
		idToken, access, metaString(auth, "account_id"), time.Now().UTC().Format(time.RFC3339))
	if errWrite := os.WriteFile(filepath.Join(home, "auth.json"), []byte(authJSON), 0o600); errWrite != nil {
		return errWrite
	}
	return runCLI(ctx, auth, home, binary, codexPingArgs(),
		[]string{"CODEX_HOME=" + home}, access, idToken)
}

// runCLI runs a CLI with a minimal environment so no other credential or user config leaks in.
func runCLI(ctx context.Context, auth *coreauth.Auth, home, binary string, args, env []string, secrets ...string) error {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = home
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}, env...)
	if proxy := strings.TrimSpace(auth.ProxyURL); proxy != "" {
		cmd.Env = append(cmd.Env, "HTTPS_PROXY="+proxy, "HTTP_PROXY="+proxy)
	}
	out, errRun := cmd.CombinedOutput()
	if errRun == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if len(msg) > 300 {
		msg = msg[:300]
	}
	for _, secret := range secrets {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return fmt.Errorf("%s: %w: %s", auth.Provider, errRun, msg)
}

func claudePingArgs() []string {
	return []string{"-p", pingPrompt, "--no-session-persistence", "--model", claudePingModel, "--effort", pingEffort}
}

func codexPingArgs() []string {
	return []string{"exec", "--skip-git-repo-check", "--ephemeral", "-s", "read-only",
		"-m", codexPingModel, "-c", `model_reasoning_effort="` + pingEffort + `"`, pingPrompt}
}
