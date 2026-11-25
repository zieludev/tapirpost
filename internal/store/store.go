// Package store keeps content in a SQL database through database/sql.
//
// The queries are written once, here. The sqlite and postgres packages open
// the connection, carry the migrations and supply a Dialect with the few
// fragments on which the two databases disagree.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// Dialect describes how one database differs from the SQL in this package.
// Queries are written with "?" placeholders.
type Dialect struct {
	Name string

	// Numbered rewrites the placeholders to $1, $2 and so on.
	Numbered bool

	// LockShared and LockExclusive end the SELECT that reads a content type
	// at the start of a write transaction. They keep the type still until
	// the transaction ends, on databases that do not serialise writers.
	LockShared, LockExclusive string

	// JSONField is an expression that reads the named field from the JSON
	// column col so that it compares as kind. It holds one placeholder,
	// which takes the returned argument.
	JSONField func(col string, kind content.Kind, name string) (expr string, arg any)

	// JSONValue is the placeholder for a value that is compared with a
	// JSONField of the same kind, with the argument it takes.
	JSONValue func(kind content.Kind, v any) (expr string, arg any)

	// Text and Int are the placeholders for a value compared with a text or
	// integer column.
	Text, Int string

	// Contains is a format with two verbs, haystack then needle, that is
	// true when the first contains the second.
	Contains string

	// RemoveKey is an expression for the JSON column col without one key.
	// It holds one placeholder, which takes the returned argument.
	RemoveKey func(col, name string) (expr string, arg any)

	// Lock, when set, serialises migrations between processes. It returns
	// the function that releases the lock.
	Lock func(ctx context.Context, conn *sql.Conn) (unlock func(), err error)
}

// DB is a content.Store on top of a SQL database.
type DB struct {
	db      *sql.DB
	d       Dialect
	rebound sync.Map // query with "?" -> query with "$n"
	stmts   sync.Map // query -> *sql.Stmt
}

// New wraps an open database. Call Migrate before anything else.
func New(db *sql.DB, d Dialect) *DB {
	return &DB{db: db, d: d}
}

// Close releases the database connections.
func (s *DB) Close() error {
	s.stmts.Range(func(_, stmt any) bool {
		stmt.(*sql.Stmt).Close()
		return true
	})
	return s.db.Close()
}

// prepared runs fixed queries through statements that are prepared once.
// Every delivery request reads a token and a revision; without this the
// database would parse the same two queries again each time.
type prepared struct{ s *DB }

func (p prepared) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if cached, ok := p.s.stmts.Load(query); ok {
		return cached.(*sql.Stmt).QueryRowContext(ctx, args...)
	}
	stmt, err := p.s.db.PrepareContext(ctx, query)
	if err != nil {
		// Run unprepared, the query reports the same problem through the
		// row, which is the only way a caller of this method can see it.
		return p.s.db.QueryRowContext(ctx, query, args...)
	}
	if existing, loaded := p.s.stmts.LoadOrStore(query, stmt); loaded {
		stmt.Close()
		stmt = existing.(*sql.Stmt)
	}
	return stmt.QueryRowContext(ctx, args...)
}

// Ping reports whether the database answers.
func (s *DB) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// q adapts the placeholders of a fixed query to the dialect.
func (s *DB) q(query string) string {
	if !s.d.Numbered {
		return query
	}
	if cached, ok := s.rebound.Load(query); ok {
		return cached.(string)
	}
	out := rebind(query)
	s.rebound.Store(query, out)
	return out
}

// dynamic does the same for a query assembled at run time. Those are not
// cached: their number is not bounded by the code.
func (s *DB) dynamic(query string) string {
	if !s.d.Numbered {
		return query
	}
	return rebind(query)
}

func rebind(query string) string {
	var sb strings.Builder
	sb.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			sb.WriteByte(query[i])
			continue
		}
		n++
		sb.WriteByte('$')
		sb.WriteString(strconv.Itoa(n))
	}
	return sb.String()
}

func (s *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func toMicros(t time.Time) int64 {
	return t.UnixMicro()
}

func fromMicros(us int64) time.Time {
	return time.UnixMicro(us).UTC()
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return content.ErrNotFound
	}
	return err
}

// Content types.

const typeColumns = `id, name, description, fields, schema_version, revision, created_at, updated_at`

func scanType(row interface{ Scan(...any) error }) (content.Type, error) {
	var (
		t                content.Type
		fields           []byte
		created, updated int64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &fields, &t.SchemaVersion, &t.Revision, &created, &updated); err != nil {
		return content.Type{}, notFound(err)
	}
	if err := json.Unmarshal(fields, &t.Fields); err != nil {
		return content.Type{}, fmt.Errorf("decode fields of %q: %w", t.Name, err)
	}
	t.CreatedAt, t.UpdatedAt = fromMicros(created), fromMicros(updated)
	return t, nil
}

// CreateType implements content.Store.
func (s *DB) CreateType(ctx context.Context, t content.Type) error {
	fields, err := json.Marshal(t.Fields)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO content_types (id, name, description, fields, schema_version, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO NOTHING`),
		t.ID, t.Name, t.Description, string(fields), t.SchemaVersion, t.Revision, toMicros(t.CreatedAt), toMicros(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert content type: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("already exists: %w", content.ErrConflict)
	}
	return nil
}

// Type implements content.Store.
func (s *DB) Type(ctx context.Context, name string) (content.Type, error) {
	return s.typeFrom(ctx, prepared{s}, name)
}

func (s *DB) typeFrom(ctx context.Context, from queryer, name string) (content.Type, error) {
	return scanType(from.QueryRowContext(ctx, s.q(`SELECT `+typeColumns+` FROM content_types WHERE name = ?`), name))
}

// Types implements content.Store.
func (s *DB) Types(ctx context.Context) ([]content.Type, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+typeColumns+` FROM content_types ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list content types: %w", err)
	}
	defer rows.Close()
	types := []content.Type{}
	for rows.Next() {
		t, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

// TypeRevision implements content.Store. The counter alone would start over
// when a type is deleted and created again under the same name, and then
// repeat values that caches may still hold. The id of the type, which is new
// every time, keeps the two apart.
func (s *DB) TypeRevision(ctx context.Context, name string) (string, error) {
	var (
		id  string
		rev int64
	)
	err := prepared{s}.QueryRowContext(ctx, s.q(`SELECT id, revision FROM content_types WHERE name = ?`), name).Scan(&id, &rev)
	if err != nil {
		return "", notFound(err)
	}
	return id + "." + strconv.FormatInt(rev, 10), nil
}

// UpdateType implements content.Store. t.SchemaVersion is the version the
// caller read; the stored one must still match.
func (s *DB) UpdateType(ctx context.Context, t content.Type, removedFields []string) (content.Type, error) {
	fields, err := json.Marshal(t.Fields)
	if err != nil {
		return content.Type{}, err
	}
	var updated content.Type
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`
			UPDATE content_types
			SET description = ?, fields = ?, schema_version = schema_version + 1, revision = revision + 1, updated_at = ?
			WHERE name = ? AND schema_version = ?`),
			t.Description, string(fields), toMicros(t.UpdatedAt), t.Name, t.SchemaVersion)
		if err != nil {
			return fmt.Errorf("update content type: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			if _, err := s.typeFrom(ctx, tx, t.Name); err != nil {
				return err
			}
			return fmt.Errorf("changed by someone else: %w", content.ErrConflict)
		}
		for _, name := range removedFields {
			dataExpr, dataArg := s.d.RemoveKey("data", name)
			pubExpr, pubArg := s.d.RemoveKey("published_data", name)
			_, err := tx.ExecContext(ctx, s.q(`UPDATE entries SET data = `+dataExpr+`, published_data = `+pubExpr+` WHERE type = ?`),
				dataArg, pubArg, t.Name)
			if err != nil {
				return fmt.Errorf("remove field %q from entries: %w", name, err)
			}
		}
		updated, err = s.typeFrom(ctx, tx, t.Name)
		return err
	})
	return updated, err
}

// DeleteType implements content.Store. The entries go with the type through
// the foreign key.
func (s *DB) DeleteType(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM content_types WHERE name = ?`), name)
	if err != nil {
		return fmt.Errorf("delete content type: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return content.ErrNotFound
	}
	return nil
}

// lockType reads the schema version of a type inside a write transaction and
// holds the type until the transaction ends.
func (s *DB) lockType(ctx context.Context, tx *sql.Tx, name, lock string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, s.q(`SELECT schema_version FROM content_types WHERE name = ?`+lock), name).Scan(&version)
	return version, notFound(err)
}

func (s *DB) lockTypeAt(ctx context.Context, tx *sql.Tx, name, lock string, schemaVersion int64) error {
	current, err := s.lockType(ctx, tx, name, lock)
	if err != nil {
		return err
	}
	if current != schemaVersion {
		return fmt.Errorf("the content type changed: %w", content.ErrConflict)
	}
	return nil
}

func (s *DB) bumpRevision(ctx context.Context, tx *sql.Tx, name string) error {
	_, err := tx.ExecContext(ctx, s.q(`UPDATE content_types SET revision = revision + 1 WHERE name = ?`), name)
	if err != nil {
		return fmt.Errorf("raise revision: %w", err)
	}
	return nil
}

// Entries.

const entryColumns = `id, type, data, published_data, version, published_version, created_at, updated_at, published_at`

// publishedColumns leaves the draft out: serving published content does not
// need it, and it is half of what the database would send.
const publishedColumns = `id, type, NULL, published_data, version, published_version, created_at, updated_at, published_at`

// scanEntry reads the entry columns, and after them whatever else the query
// selected into more.
func scanEntry(row interface{ Scan(...any) error }, more ...any) (content.Entry, error) {
	var (
		e                content.Entry
		data, published  []byte
		pubVersion       sql.NullInt64
		created, updated int64
		publishedAt      sql.NullInt64
	)
	dest := append([]any{&e.ID, &e.Type, &data, &published, &e.Version, &pubVersion, &created, &updated, &publishedAt}, more...)
	if err := row.Scan(dest...); err != nil {
		return content.Entry{}, notFound(err)
	}
	e.Data, e.Published = data, published
	e.PublishedVersion = pubVersion.Int64
	e.CreatedAt, e.UpdatedAt = fromMicros(created), fromMicros(updated)
	if publishedAt.Valid {
		e.PublishedAt = fromMicros(publishedAt.Int64)
	}
	return e, nil
}

func (s *DB) entryFrom(ctx context.Context, from queryer, typeName, id string) (content.Entry, error) {
	return scanEntry(from.QueryRowContext(ctx, s.q(`SELECT `+entryColumns+` FROM entries WHERE id = ? AND type = ?`), id, typeName))
}

// Entry implements content.Store.
func (s *DB) Entry(ctx context.Context, typeName, id string) (content.Entry, error) {
	return s.entryFrom(ctx, prepared{s}, typeName, id)
}

// CreateEntry implements content.Store.
func (s *DB) CreateEntry(ctx context.Context, e content.Entry, schemaVersion int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockTypeAt(ctx, tx, e.Type, s.d.LockShared, schemaVersion); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q(`
			INSERT INTO entries (id, type, data, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`),
			e.ID, e.Type, string(e.Data), e.Version, toMicros(e.CreatedAt), toMicros(e.UpdatedAt))
		if err != nil {
			return fmt.Errorf("insert entry: %w", err)
		}
		return nil
	})
}

// UpdateEntry implements content.Store.
func (s *DB) UpdateEntry(ctx context.Context, typeName, id string, data json.RawMessage, schemaVersion int64, at time.Time) (content.Entry, error) {
	var e content.Entry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockTypeAt(ctx, tx, typeName, s.d.LockShared, schemaVersion); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`
			UPDATE entries SET data = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND type = ?`),
			string(data), toMicros(at), id, typeName)
		if err != nil {
			return fmt.Errorf("update entry: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return content.ErrNotFound
		}
		e, err = s.entryFrom(ctx, tx, typeName, id)
		return err
	})
	return e, err
}

// PublishEntry implements content.Store.
func (s *DB) PublishEntry(ctx context.Context, typeName, id string, version, schemaVersion int64, at time.Time) (content.Entry, error) {
	var e content.Entry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.lockTypeAt(ctx, tx, typeName, s.d.LockExclusive, schemaVersion); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`
			UPDATE entries SET published_data = data, published_version = version, published_at = ?
			WHERE id = ? AND type = ? AND version = ?`),
			toMicros(at), id, typeName, version)
		if err != nil {
			return fmt.Errorf("publish entry: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			if _, err := s.entryFrom(ctx, tx, typeName, id); err != nil {
				return err
			}
			return fmt.Errorf("the entry changed: %w", content.ErrConflict)
		}
		if err := s.bumpRevision(ctx, tx, typeName); err != nil {
			return err
		}
		e, err = s.entryFrom(ctx, tx, typeName, id)
		return err
	})
	return e, err
}

// UnpublishEntry implements content.Store. Unpublishing an entry that is not
// published changes nothing.
func (s *DB) UnpublishEntry(ctx context.Context, typeName, id string) (content.Entry, error) {
	var e content.Entry
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.lockType(ctx, tx, typeName, s.d.LockExclusive); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`
			UPDATE entries SET published_data = NULL, published_version = NULL, published_at = NULL
			WHERE id = ? AND type = ? AND published_data IS NOT NULL`),
			id, typeName)
		if err != nil {
			return fmt.Errorf("unpublish entry: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 0 {
			if err := s.bumpRevision(ctx, tx, typeName); err != nil {
				return err
			}
		}
		e, err = s.entryFrom(ctx, tx, typeName, id)
		return err
	})
	return e, err
}

// DeleteEntry implements content.Store.
func (s *DB) DeleteEntry(ctx context.Context, typeName, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.lockType(ctx, tx, typeName, s.d.LockExclusive); err != nil {
			return err
		}
		e, err := s.entryFrom(ctx, tx, typeName, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM entries WHERE id = ? AND type = ?`), id, typeName); err != nil {
			return fmt.Errorf("delete entry: %w", err)
		}
		if e.Published != nil {
			return s.bumpRevision(ctx, tx, typeName)
		}
		return nil
	})
}

// Tokens.

const tokenColumns = `id, name, prefix, scopes, types, created_at, expires_at`

func scanToken(row interface{ Scan(...any) error }) (content.Token, error) {
	var (
		t             content.Token
		scopes, types []byte
		created       int64
		expires       sql.NullInt64
	)
	if err := row.Scan(&t.ID, &t.Name, &t.Prefix, &scopes, &types, &created, &expires); err != nil {
		return content.Token{}, notFound(err)
	}
	if err := json.Unmarshal(scopes, &t.Scopes); err != nil {
		return content.Token{}, fmt.Errorf("decode scopes of token %q: %w", t.ID, err)
	}
	if err := json.Unmarshal(types, &t.Types); err != nil {
		return content.Token{}, fmt.Errorf("decode types of token %q: %w", t.ID, err)
	}
	t.CreatedAt = fromMicros(created)
	if expires.Valid {
		at := fromMicros(expires.Int64)
		t.ExpiresAt = &at
	}
	return t, nil
}

// CreateToken implements content.Store.
func (s *DB) CreateToken(ctx context.Context, t content.Token, hash string) error {
	scopes, err := json.Marshal(t.Scopes)
	if err != nil {
		return err
	}
	types, err := json.Marshal(t.Types)
	if err != nil {
		return err
	}
	var expires sql.NullInt64
	if t.ExpiresAt != nil {
		expires = sql.NullInt64{Int64: toMicros(*t.ExpiresAt), Valid: true}
	}
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO tokens (id, name, token_hash, prefix, scopes, types, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		t.ID, t.Name, hash, t.Prefix, string(scopes), string(types), toMicros(t.CreatedAt), expires)
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}
	return nil
}

// TokenByHash implements content.Store.
func (s *DB) TokenByHash(ctx context.Context, hash string) (content.Token, error) {
	return scanToken(prepared{s}.QueryRowContext(ctx, s.q(`SELECT `+tokenColumns+` FROM tokens WHERE token_hash = ?`), hash))
}

// Tokens implements content.Store.
func (s *DB) Tokens(ctx context.Context) ([]content.Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	tokens := []content.Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// DeleteToken implements content.Store.
func (s *DB) DeleteToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM tokens WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete token: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return content.ErrNotFound
	}
	return nil
}
