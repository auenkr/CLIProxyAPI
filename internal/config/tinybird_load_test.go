package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsObservabilityTinybird(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlText := "observability:\n  tinybird:\n    enable: true\n    host: https://example.test\n    token: tok\n    datasource: ds\n"
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := TinybirdConfig{Enable: true, Host: "https://example.test", Token: "tok", Datasource: "ds"}
	if cfg.Tinybird != want {
		t.Fatalf("Tinybird = %+v, want %+v", cfg.Tinybird, want)
	}
}
