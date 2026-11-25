package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

type migration struct {
	version int64
	name    string
	sql     string
}

// Migrate applies the migrations in files that the database has not seen
// yet, in order. A file is named <version>_<description>.sql and runs in one
// transaction together with the record of having run.
//
// It is safe to call from several processes at once, and it refuses a
// database that a newer version of the program has already migrated.
func (s *DB) Migrate(ctx context.Context, files fs.FS) error {
	migrations, err := readMigrations(files)
	if err != nil {
		return err
	}

	// One connection for the whole run: a lock taken by the dialect belongs
	// to the session, so everything has to happen on that session.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Close()

	if s.d.Lock != nil {
		unlock, err := s.d.Lock(ctx, conn)
		if err != nil {
			return fmt.Errorf("migrate: lock: %w", err)
		}
		defer unlock()
	}

	_, err = conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at BIGINT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	var newest sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&newest); err != nil {
		return fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	if known := migrations[len(migrations)-1].version; newest.Int64 > known {
		return fmt.Errorf("migrate: the database is at schema version %d, this program only knows up to %d; use a newer version", newest.Int64, known)
	}

	for _, m := range migrations {
		if err := s.apply(ctx, conn, m); err != nil {
			return fmt.Errorf("migrate: %s: %w", m.name, err)
		}
	}
	return nil
}

func (s *DB) apply(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Checked inside the transaction: on a database without Lock, another
	// process may have applied this migration while we waited for ours.
	var applied int
	err = tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`), m.version).Scan(&applied)
	if err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`),
		m.version, m.name, toMicros(time.Now()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func readMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if len(names) == 0 {
		return nil, errors.New("migrate: no migration files")
	}
	migrations := make([]migration, 0, len(names))
	seen := make(map[int64]string, len(names))
	for _, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil || version < 1 {
			return nil, fmt.Errorf("migrate: %q is not named <version>_<description>.sql", name)
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrate: %q and %q share version %d", other, name, version)
		}
		seen[version] = name
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	return migrations, nil
}
