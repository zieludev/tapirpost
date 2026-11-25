// Package sqlite opens the SQLite backend: one file, no server, no cgo.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"

	_ "github.com/ncruces/go-sqlite3/driver" // registers the "sqlite3" driver

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens the database file at path, creating it if needed, and brings
// its schema up to date. maxConns limits the connection pool.
func Open(ctx context.Context, path string, maxConns int) (*store.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite: the database path is empty")
	}
	// WAL lets readers work while one writer writes. NORMAL is the durability
	// SQLite recommends with WAL: a power cut can lose the last transactions
	// but cannot corrupt the file. A writer waits up to busy_timeout for its
	// turn instead of failing at once.
	params := url.Values{}
	params.Add("_pragma", "busy_timeout(5000)")
	params.Add("_pragma", "journal_mode(WAL)")
	params.Add("_pragma", "synchronous(NORMAL)")
	params.Add("_pragma", "foreign_keys(ON)")
	// Write transactions take the write lock when they begin. A transaction
	// that started as a reader cannot wait for the lock later; it fails.
	params.Add("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Opaque: url.PathEscape(path), RawQuery: params.Encode()}).String()

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if maxConns > 0 {
		db.SetMaxOpenConns(maxConns)
		db.SetMaxIdleConns(maxConns)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}

	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := store.New(db, dialect)
	if err := s.Migrate(ctx, files); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	return s, nil
}

var dialect = store.Dialect{
	Name: "sqlite",
	Text: "?",
	Int:  "?",
	// json_extract returns the SQL value of a JSON scalar: text for a string,
	// an integer or a real for a number, 1 or 0 for a boolean. Text compares
	// byte by byte, which is the order the Kind constants promise.
	JSONField: func(col string, _ content.Kind, name string) (string, any) {
		return "json_extract(" + col + ", ?)", "$." + name
	},
	JSONValue: func(_ content.Kind, v any) (string, any) {
		if b, ok := v.(bool); ok {
			if b {
				return "?", 1
			}
			return "?", 0
		}
		return "?", v
	},
	Contains: "instr(%s, %s) > 0",
	RemoveKey: func(col, name string) (string, any) {
		return "json_remove(" + col + ", ?)", "$." + name
	},
}
