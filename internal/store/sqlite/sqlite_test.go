package sqlite_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/store/sqlite"
	"github.com/zieludev/tapirpost/internal/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) content.Store {
		// A directory name with characters that mean something in a URI.
		path := filepath.Join(t.TempDir(), "a b?c#d%20", "tapirpost.db")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		s, err := sqlite.Open(t.Context(), path, 4)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestOpen(t *testing.T) {
	if _, err := sqlite.Open(t.Context(), "", 4); err == nil {
		t.Error("an empty path was accepted")
	}
	if _, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "missing", "tapirpost.db"), 4); err == nil {
		t.Error("a path in a directory that does not exist was accepted")
	}

	// Opening twice is what a restart does: the schema is already there and
	// what was stored is still there.
	path := filepath.Join(t.TempDir(), "tapirpost.db")
	first, err := sqlite.Open(t.Context(), path, 4)
	if err != nil {
		t.Fatal(err)
	}
	ct := content.Type{
		ID: content.NewID(), Name: "note", Fields: []content.Field{{Name: "text", Type: content.FieldText}},
		SchemaVersion: 1, Revision: 1,
	}
	if err := first.CreateType(t.Context(), ct); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := sqlite.Open(t.Context(), path, 4)
	if err != nil {
		t.Fatalf("opening an existing database: %v", err)
	}
	got, err := second.Type(t.Context(), "note")
	if err != nil || got.ID != ct.ID {
		second.Close()
		t.Fatalf("after reopening: %+v, %v", got, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	// A clean shutdown leaves one file behind, not the write-ahead log.
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("%s was left behind after closing", filepath.Base(path)+suffix)
		}
	}
}
