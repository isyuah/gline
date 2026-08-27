package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

const migrationLockID int64 = 0x474c494e45

type migration struct {
	Name     string
	SQL      []byte
	Checksum [sha256.Size]byte
}

// Migrate applies immutable *.up.sql files in lexical order. It serializes
// concurrent migrators with a PostgreSQL advisory lock and rejects edits to an
// already-applied migration by comparing SHA-256 checksums.
func Migrate(ctx context.Context, db *sql.DB, source fs.FS) error {
	if db == nil || source == nil {
		return fmt.Errorf("postgres: nil migration dependency")
	}
	migrations, err := loadMigrations(source)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    name text PRIMARY KEY,
    checksum bytea NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	for _, m := range migrations {
		var applied []byte
		err := conn.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE name = $1`, m.Name,
		).Scan(&applied)
		switch {
		case err == nil:
			if !bytes.Equal(applied, m.Checksum[:]) {
				return fmt.Errorf("%w: %s", ErrMigrationDrift, m.Name)
			}
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("read migration %s: %w", m.Name, err)
		}

		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Name, err)
		}
		if _, err = tx.ExecContext(ctx, string(m.SQL)); err == nil {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (name, checksum) VALUES ($1, $2)`,
				m.Name, m.Checksum[:],
			)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
	}
	return nil
}

func loadMigrations(source fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(source, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		body, err := fs.ReadFile(source, path.Clean(entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, fmt.Errorf("migration %s is empty", entry.Name())
		}
		result = append(result, migration{Name: entry.Name(), SQL: body, Checksum: sha256.Sum256(body)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	for i := range result {
		if i > 0 && result[i-1].Name == result[i].Name {
			return nil, fmt.Errorf("duplicate migration %s", result[i].Name)
		}
		if i > 0 && result[i-1].Name[:4] == result[i].Name[:4] {
			return nil, fmt.Errorf("duplicate migration version %s", result[i].Name[:4])
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("no .up.sql migrations found")
	}
	return result, nil
}

func checksumString(sum [sha256.Size]byte) string { return hex.EncodeToString(sum[:]) }
