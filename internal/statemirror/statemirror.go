// Package statemirror lets file-backed runtime state (usage counters, cost
// buckets) be mirrored to a remote store such as PostgreSQL. The local file
// stays the working copy; the mirror is seeded from it and restores it on start.
package statemirror

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
)

// Mirror stores opaque state blobs by key.
type Mirror interface {
	// LoadState returns the blob for key; ok is false when none is stored.
	LoadState(ctx context.Context, key string) (data []byte, ok bool, err error)
	// SaveState stores the blob for key.
	SaveState(ctx context.Context, key string, data []byte) error
}

var defaultMirror atomic.Pointer[Mirror]

// SetDefault registers the process-wide mirror. A nil mirror disables mirroring.
func SetDefault(m Mirror) {
	if m == nil {
		defaultMirror.Store(nil)
		return
	}
	defaultMirror.Store(&m)
}

// Default returns the registered mirror, or nil.
func Default() Mirror {
	if p := defaultMirror.Load(); p != nil {
		return *p
	}
	return nil
}

// Restore writes the mirrored blob for key to path when one exists. When the
// mirror holds nothing yet, an existing local file seeds it instead.
func Restore(ctx context.Context, m Mirror, key, path string) error {
	if m == nil || path == "" {
		return nil
	}
	data, ok, err := m.LoadState(ctx, key)
	if err != nil {
		return err
	}
	if ok {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, data, 0o600)
	}
	local, errRead := os.ReadFile(path)
	if errRead != nil || len(local) == 0 {
		return nil
	}
	return m.SaveState(ctx, key, local)
}
