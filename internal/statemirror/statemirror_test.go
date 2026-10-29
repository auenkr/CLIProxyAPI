package statemirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type memMirror map[string][]byte

func (m memMirror) LoadState(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := m[key]
	return v, ok, nil
}

func (m memMirror) SaveState(_ context.Context, key string, data []byte) error {
	m[key] = data
	return nil
}

func TestRestoreSeedsMirrorFromLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.state")
	if err := os.WriteFile(path, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := memMirror{}
	if err := Restore(context.Background(), m, "k", path); err != nil {
		t.Fatal(err)
	}
	if string(m["k"]) != "local" {
		t.Fatalf("mirror not seeded: %q", m["k"])
	}
}

func TestRestorePrefersMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "s.state")
	m := memMirror{"k": []byte("remote")}
	if err := Restore(context.Background(), m, "k", path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "remote" {
		t.Fatalf("got %q err %v", got, err)
	}
}
