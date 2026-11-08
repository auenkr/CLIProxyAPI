package config

// WindowRefreshConfig controls pings that start a fresh 5-hour quota window. Pings run the
// provider's installed official CLI (claude, codex) with that account's credentials.
type WindowRefreshConfig struct {
	// ClaudeBinaryPath is the Claude Code binary. Default: "claude" from PATH.
	ClaudeBinaryPath string `yaml:"claude-binary-path,omitempty" json:"claude-binary-path,omitempty"`

	// CodexBinaryPath is the Codex CLI binary. Default: "codex" from PATH.
	CodexBinaryPath string `yaml:"codex-binary-path,omitempty" json:"codex-binary-path,omitempty"`

	// StateDir holds per-account Claude config dirs. Default: the user cache dir.
	StateDir string `yaml:"state-dir,omitempty" json:"state-dir,omitempty"`
}

// WindowRefreshAccount is the per-account window refresh setting, stored in the auth metadata.
type WindowRefreshAccount struct {
	// Enabled turns the automatic ping on for this account.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Anchor is an optional RFC3339 instant. Idle accounts are not started before it. When the
	// provider reports a 5h reset time, that time is used instead.
	Anchor string `yaml:"anchor,omitempty" json:"anchor,omitempty"`
}
