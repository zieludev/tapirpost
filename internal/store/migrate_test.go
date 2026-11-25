package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	_ "github.com/ncruces/go-sqlite3/driver"
)

func TestRebind(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"SELECT 1":                         "SELECT 1",
		"SELECT * FROM t WHERE a = ?":      "SELECT * FROM t WHERE a = $1",
		"INSERT INTO t VALUES (?, ?, ?)":   "INSERT INTO t VALUES ($1, $2, $3)",
		"a = ?::text AND b IN (?,?) OR ?":  "a = $1::text AND b IN ($2,$3) OR $4",
		"?????????? ?":                     "$1$2$3$4$5$6$7$8$9$10 $11",
		"(data ->> ?::text) COLLATE \"C\"": "(data ->> $1::text) COLLATE \"C\"",
	}
	for in, want := range cases {
		if got := rebind(in); got != want {
			t.Errorf("rebind(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestPlaceholdersFollowTheDialect(t *testing.T) {
	const query = "SELECT a FROM t WHERE b = ? AND c = ?"
	plain := New(nil, Dialect{})
	numbered := New(nil, Dialect{Numbered: true})
	if got := plain.q(query); got != query {
		t.Errorf("unnumbered: %q", got)
	}
	for range 2 {
		if got := numbered.q(query); got != "SELECT a FROM t WHERE b = $1 AND c = $2" {
			t.Errorf("numbered: %q", got)
		}
		if got := numbered.dynamic(query); got != "SELECT a FROM t WHERE b = $1 AND c = $2" {
			t.Errorf("numbered, dynamic: %q", got)
		}
	}
}

func openTestDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, Dialect{Name: "sqlite"})
}

func tables(t *testing.T, s *DB) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name <> 'schema_migrations' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func applied(t *testing.T, s *DB) []string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), `SELECT name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestMigrate(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "test.db"))
	first := fstest.MapFS{
		"0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER PRIMARY KEY);\nCREATE TABLE one_more (id INTEGER PRIMARY KEY);")},
	}
	for range 2 {
		if err := s.Migrate(t.Context(), first); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	if got := strings.Join(tables(t, s), ","); got != "one,one_more" {
		t.Fatalf("tables after the first migration: %s", got)
	}

	// Out of order on purpose: files are applied by version, not by name.
	more := fstest.MapFS{
		"0001_one.sql":    first["0001_one.sql"],
		"0010_three.sql":  {Data: []byte("CREATE TABLE three (id INTEGER PRIMARY KEY, two_id INTEGER REFERENCES two (id));")},
		"0002_two.sql":    {Data: []byte("CREATE TABLE two (id INTEGER PRIMARY KEY);")},
		"README.md":       {Data: []byte("not a migration")},
		"notes/0003.sql":  {Data: []byte("not in the top directory")},
		"0001_one.sql.bk": {Data: []byte("not a migration either")},
	}
	if err := s.Migrate(t.Context(), more); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got := strings.Join(applied(t, s), ","); got != "0001_one.sql,0002_two.sql,0010_three.sql" {
		t.Fatalf("applied migrations: %s", got)
	}
	if got := strings.Join(tables(t, s), ","); got != "one,one_more,three,two" {
		t.Fatalf("tables: %s", got)
	}
}

func TestMigrateRollsBackAFailedFile(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "test.db"))
	files := fstest.MapFS{
		"0001_good.sql": {Data: []byte("CREATE TABLE good (id INTEGER PRIMARY KEY);")},
		"0002_bad.sql":  {Data: []byte("CREATE TABLE half (id INTEGER PRIMARY KEY);\nCREATE TABLE good (id INTEGER PRIMARY KEY);")},
		"0003_late.sql": {Data: []byte("CREATE TABLE late (id INTEGER PRIMARY KEY);")},
	}
	err := s.Migrate(t.Context(), files)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("Migrate with a broken file: %v", err)
	}
	// The first statement of the broken file must be gone with the rest of
	// it, and nothing after the broken file may have run.
	if got := strings.Join(tables(t, s), ","); got != "good" {
		t.Fatalf("tables after a failed migration: %s", got)
	}
	if got := strings.Join(applied(t, s), ","); got != "0001_good.sql" {
		t.Fatalf("applied migrations: %s", got)
	}

	files["0002_bad.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE half (id INTEGER PRIMARY KEY);")}
	if err := s.Migrate(t.Context(), files); err != nil {
		t.Fatalf("Migrate after the fix: %v", err)
	}
	if got := strings.Join(tables(t, s), ","); got != "good,half,late" {
		t.Fatalf("tables after the fix: %s", got)
	}
}

func TestMigrateRefusesANewerDatabase(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "test.db"))
	newer := fstest.MapFS{
		"0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER PRIMARY KEY);")},
		"0002_two.sql": {Data: []byte("CREATE TABLE two (id INTEGER PRIMARY KEY);")},
	}
	if err := s.Migrate(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"0001_one.sql": newer["0001_one.sql"]}
	err := s.Migrate(t.Context(), older)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("an older program accepted a newer database: %v", err)
	}
}

func TestMigrateRejectsBadFiles(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"no files":         {},
		"no version":       {"init.sql": {Data: []byte("SELECT 1;")}},
		"no description":   {"0001.sql": {Data: []byte("SELECT 1;")}},
		"version zero":     {"0000_init.sql": {Data: []byte("SELECT 1;")}},
		"negative version": {"-1_init.sql": {Data: []byte("SELECT 1;")}},
		"same version":     {"0001_a.sql": {Data: []byte("SELECT 1;")}, "1_b.sql": {Data: []byte("SELECT 1;")}},
	}
	for name, files := range cases {
		s := openTestDB(t, filepath.Join(t.TempDir(), "test.db"))
		if err := s.Migrate(t.Context(), files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestMigrateFromSeveralProcesses starts the same migrations from several
// connections at once, as replicas that start together do.
func TestMigrateFromSeveralProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	files := fstest.MapFS{
		"0001_one.sql": {Data: []byte("CREATE TABLE one (id INTEGER PRIMARY KEY);")},
		"0002_two.sql": {Data: []byte("CREATE TABLE two (id INTEGER PRIMARY KEY);\nINSERT INTO two (id) VALUES (1);")},
	}
	// The first run creates the bookkeeping table, so that the race below is
	// about the migrations themselves.
	if err := openTestDB(t, path).Migrate(t.Context(), fstest.MapFS{"0001_one.sql": files["0001_one.sql"]}); err != nil {
		t.Fatal(err)
	}

	const processes = 6
	var wg sync.WaitGroup
	errs := make(chan error, processes)
	for range processes {
		s := openTestDB(t, path)
		wg.Go(func() { errs <- s.Migrate(t.Context(), files) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}

	s := openTestDB(t, path)
	if got := strings.Join(applied(t, s), ","); got != "0001_one.sql,0002_two.sql" {
		t.Fatalf("applied migrations: %s", got)
	}
	var rows int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM two`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the second migration ran %d times, %v", rows, err)
	}
}
