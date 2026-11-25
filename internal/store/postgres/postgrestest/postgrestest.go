// Package postgrestest gives tests a PostgreSQL store of their own.
package postgrestest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"testing"

	"github.com/zieludev/tapirpost/internal/store"
	"github.com/zieludev/tapirpost/internal/store/postgres"
	"github.com/zieludev/tapirpost/internal/testenv"
)

// Open returns a store in a fresh schema of the database named by
// TAPIRPOST_TEST_POSTGRES_URL and drops the schema when the test ends. Tests are
// skipped when the variable is not set.
func Open(t testing.TB) *store.DB {
	t.Helper()
	base := testenv.Service(t, testenv.PostgresURL)

	suffix := make([]byte, 8)
	rand.Read(suffix)
	schema := "tapirpost_test_" + hex.EncodeToString(suffix)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatalf("postgres: create schema: %v", err)
	}
	t.Cleanup(func() {
		// The test context is already cancelled during cleanup.
		admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("postgres: %s is not a URL: %v", testenv.PostgresURL, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := postgres.Open(t.Context(), u.String(), 8)
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
