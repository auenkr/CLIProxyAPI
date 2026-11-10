package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigReadsObservabilityClickHouse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlText := "observability:\n  clickhouse:\n    enable: true\n    host: ch.example.test\n    port: 8443\n    username: user\n    token: tok\n    database: db\n    table: tbl\n    ingest-url: https://ingest.test\n    ingest-token: itok\n"
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := ClickHouseConfig{Enable: true, Host: "ch.example.test", Port: 8443, Username: "user", Token: "tok", Database: "db", Table: "tbl", IngestURL: "https://ingest.test", IngestToken: "itok"}
	if cfg.ClickHouse != want {
		t.Fatalf("ClickHouse = %+v, want %+v", cfg.ClickHouse, want)
	}
}
