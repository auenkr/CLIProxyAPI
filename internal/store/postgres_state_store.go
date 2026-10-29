package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const defaultStateTable = "state_store"

func (s *PostgresStore) stateTable() string {
	name := s.cfg.StateTable
	if name == "" {
		name = defaultStateTable
	}
	return s.fullTableName(name)
}

func (s *PostgresStore) ensureStateTable(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			key TEXT PRIMARY KEY,
			content BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)
	`, s.stateTable())); err != nil {
		return fmt.Errorf("postgres store: create state table: %w", err)
	}
	return nil
}

// LoadState returns the stored runtime state blob for key.
func (s *PostgresStore) LoadState(ctx context.Context, key string) ([]byte, bool, error) {
	var content []byte
	query := fmt.Sprintf("SELECT content FROM %s WHERE key = $1", s.stateTable())
	err := s.db.QueryRowContext(ctx, query, key).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres store: load state %s: %w", key, err)
	}
	return content, true, nil
}

// SaveState upserts the runtime state blob for key.
func (s *PostgresStore) SaveState(ctx context.Context, key string, data []byte) error {
	query := fmt.Sprintf(`
		INSERT INTO %s (key, content) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
	`, s.stateTable())
	if _, err := s.db.ExecContext(ctx, query, key, data); err != nil {
		return fmt.Errorf("postgres store: save state %s: %w", key, err)
	}
	return nil
}
