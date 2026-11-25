// Package postgres opens the PostgreSQL backend.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is an arbitrary key for pg_advisory_lock; every TapirPost
// process uses the same one so that only one migrates at a time.
const migrationLock int64 = 0x7461706972706f73 // "tapirpos" in ASCII

// Open connects to the database named by a postgres:// URL and brings its
// schema up to date. maxConns limits the connection pool.
func Open(ctx context.Context, dsn string, maxConns int) (*store.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := store.New(db, dialect)
	if err := s.Migrate(ctx, files); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return s, nil
}

var dialect = store.Dialect{
	Name:          "postgres",
	Numbered:      true,
	LockShared:    " FOR SHARE",
	LockExclusive: " FOR UPDATE",
	Text:          "?::text",
	Int:           "?::bigint",
	// Strings are read as text and compared byte by byte ("C"), the same
	// order SQLite uses, instead of the order of the database locale.
	//
	// Numbers and booleans are compared as jsonb, without a cast. A cast such
	// as (data->>'n')::bigint would raise an error on a row of another
	// content type that keeps text under the same key, and nothing promises
	// that the type = $1 condition is evaluated first.
	JSONField: func(col string, kind content.Kind, name string) (string, any) {
		if kind == content.KindString {
			return "(" + col + ` ->> ?::text) COLLATE "C"`, name
		}
		return "(" + col + " -> ?::text)", name
	},
	JSONValue: func(kind content.Kind, v any) (string, any) {
		switch kind {
		case content.KindInteger:
			return "to_jsonb(?::bigint)", v
		case content.KindNumber:
			return "to_jsonb(?::double precision)", v
		case content.KindBoolean:
			return "to_jsonb(?::boolean)", v
		}
		return "?::text", v
	},
	Contains: "strpos(%s, %s) > 0",
	RemoveKey: func(col, name string) (string, any) {
		return col + " - ?::text", name
	},
	Lock: func(ctx context.Context, conn *sql.Conn) (func(), error) {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
			return nil, err
		}
		return func() {
			// Not the caller's context: the lock has to go even when that
			// one is already cancelled. Closing the connection would drop
			// it as well, but the connection returns to the pool.
			conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
		}, nil
	},
}
