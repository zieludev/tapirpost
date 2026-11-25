package content_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/store/sqlite"
)

const rootToken = "a-root-token-that-is-long-enough-123"

func newService(t *testing.T, wrap func(content.Store) content.Store) *content.Service {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "tapirpost.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var s content.Store = db
	if wrap != nil {
		s = wrap(s)
	}
	svc, err := content.NewService(s, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

var noteSpec = content.TypeSpec{
	Name:   "note",
	Fields: []content.Field{{Name: "title", Type: content.FieldString, Required: true}, {Name: "text", Type: content.FieldText}},
}

func TestRootToken(t *testing.T) {
	for _, short := range []string{"x", strings.Repeat("a", content.MinRootTokenLen-1)} {
		if _, err := content.NewService(nil, short); err == nil {
			t.Errorf("a root token of %d characters was accepted", len(short))
		}
	}

	svc := newService(t, nil)
	token, err := svc.Authenticate(t.Context(), rootToken)
	if err != nil {
		t.Fatalf("Authenticate with the root token: %v", err)
	}
	if token.ID != content.RootTokenID || !token.Allows(content.ScopeAdmin, "") {
		t.Fatalf("root token: %+v", token)
	}
	for _, secret := range []string{"", rootToken + "x", rootToken[:len(rootToken)-1], strings.ToUpper(rootToken)} {
		if _, err := svc.Authenticate(t.Context(), secret); !errors.Is(err, content.ErrInvalidToken) {
			t.Errorf("Authenticate(%q): got %v, want ErrInvalidToken", secret, err)
		}
	}

	// Without a root token nothing but stored tokens gets in, the empty
	// string included.
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "tapirpost.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	closed, err := content.NewService(db, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"", rootToken} {
		if _, err := closed.Authenticate(t.Context(), secret); !errors.Is(err, content.ErrInvalidToken) {
			t.Errorf("without a root token, Authenticate(%q): got %v", secret, err)
		}
	}
}

func TestStoredTokens(t *testing.T) {
	svc := newService(t, nil)
	ctx := t.Context()

	expires := time.Now().Add(time.Hour)
	token, secret, err := svc.CreateToken(ctx, content.TokenSpec{Name: "site", Scopes: []content.Scope{content.ScopeRead}, Types: []string{"note"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != token.ID || !got.Allows(content.ScopeRead, "note") || got.Allows(content.ScopeRead, "page") || got.Allows(content.ScopeWrite, "note") {
		t.Fatalf("token: %+v", got)
	}

	_, other, err := svc.CreateToken(ctx, content.TokenSpec{Name: "other", Scopes: []content.Scope{content.ScopeAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	if other == secret {
		t.Fatal("two tokens share a secret")
	}

	if err := svc.DeleteToken(ctx, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, secret); !errors.Is(err, content.ErrInvalidToken) {
		t.Fatalf("a deleted token still works: %v", err)
	}
	if _, err := svc.Authenticate(ctx, other); err != nil {
		t.Fatalf("deleting one token broke another: %v", err)
	}
	if err := svc.DeleteToken(ctx, token.ID); !errors.Is(err, content.ErrNotFound) {
		t.Fatalf("deleting a token twice: %v", err)
	}
}

func TestPublishNeedsACompleteEntry(t *testing.T) {
	svc := newService(t, nil)
	ctx := t.Context()
	if _, err := svc.CreateType(ctx, noteSpec); err != nil {
		t.Fatal(err)
	}

	draft, err := svc.CreateEntry(ctx, "note", json.RawMessage(`{"text": "no title yet"}`))
	if err != nil {
		t.Fatalf("an unfinished draft was refused: %v", err)
	}
	var validation *content.ValidationError
	if _, err := svc.PublishEntry(ctx, "note", draft.ID); !errors.As(err, &validation) {
		t.Fatalf("publishing an unfinished draft: got %v", err)
	}
	if _, err := svc.PublishedEntry(ctx, "note", draft.ID); !errors.Is(err, content.ErrNotFound) {
		t.Fatalf("a refused publish left something published: %v", err)
	}

	if _, err := svc.UpdateEntry(ctx, "note", draft.ID, json.RawMessage(`{"title": "Now", "text": "done"}`)); err != nil {
		t.Fatal(err)
	}
	published, err := svc.PublishEntry(ctx, "note", draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.Status() != content.StatusPublished || string(published.Published) != `{"text":"done","title":"Now"}` {
		t.Fatalf("published entry: %+v", published)
	}

	// A new required field does not unpublish what is already out, but the
	// entry cannot be published again until it has a value for it.
	spec := noteSpec
	spec.Fields = append(append([]content.Field{}, noteSpec.Fields...), content.Field{Name: "author", Type: content.FieldString, Required: true})
	if _, err := svc.UpdateType(ctx, "note", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishedEntry(ctx, "note", draft.ID); err != nil {
		t.Fatalf("a new required field unpublished an entry: %v", err)
	}
	if _, err := svc.PublishEntry(ctx, "note", draft.ID); !errors.As(err, &validation) {
		t.Fatalf("publishing without the new required field: got %v", err)
	}
}

// racingStore changes the content type right before an entry write reaches
// the database, as a second administrator would.
type racingStore struct {
	content.Store
	change func(ctx context.Context)
}

func (s *racingStore) race(ctx context.Context) {
	if change := s.change; change != nil {
		s.change = nil
		change(ctx)
	}
}

func (s *racingStore) CreateEntry(ctx context.Context, e content.Entry, schemaVersion int64) error {
	s.race(ctx)
	return s.Store.CreateEntry(ctx, e, schemaVersion)
}

func (s *racingStore) UpdateEntry(ctx context.Context, typeName, id string, data json.RawMessage, schemaVersion int64, at time.Time) (content.Entry, error) {
	s.race(ctx)
	return s.Store.UpdateEntry(ctx, typeName, id, data, schemaVersion, at)
}

func (s *racingStore) PublishEntry(ctx context.Context, typeName, id string, version, schemaVersion int64, at time.Time) (content.Entry, error) {
	s.race(ctx)
	return s.Store.PublishEntry(ctx, typeName, id, version, schemaVersion, at)
}

// TestWritesDoNotOutrunTheSchema checks that an entry validated against one
// version of a type is not stored once the type has moved on: it would hold
// values nobody checked against the new fields.
func TestWritesDoNotOutrunTheSchema(t *testing.T) {
	var racing *racingStore
	svc := newService(t, func(s content.Store) content.Store {
		racing = &racingStore{Store: s}
		return racing
	})
	ctx := t.Context()
	if _, err := svc.CreateType(ctx, noteSpec); err != nil {
		t.Fatal(err)
	}
	entry, err := svc.CreateEntry(ctx, "note", json.RawMessage(`{"title": "One", "text": "kept"}`))
	if err != nil {
		t.Fatal(err)
	}

	withoutText := content.TypeSpec{Name: "note", Fields: noteSpec.Fields[:1]}
	dropText := func(ctx context.Context) {
		if _, err := svc.UpdateType(ctx, "note", withoutText); err != nil {
			t.Errorf("UpdateType: %v", err)
		}
	}
	restore := func() {
		t.Helper()
		if _, err := svc.UpdateType(ctx, "note", noteSpec); err != nil {
			t.Fatal(err)
		}
	}

	racing.change = dropText
	if _, err := svc.CreateEntry(ctx, "note", json.RawMessage(`{"title": "Two", "text": "would be stale"}`)); !errors.Is(err, content.ErrConflict) {
		t.Fatalf("CreateEntry across a schema change: got %v, want ErrConflict", err)
	}
	restore()

	racing.change = dropText
	if _, err := svc.UpdateEntry(ctx, "note", entry.ID, json.RawMessage(`{"title": "One", "text": "would be stale"}`)); !errors.Is(err, content.ErrConflict) {
		t.Fatalf("UpdateEntry across a schema change: got %v, want ErrConflict", err)
	}
	restore()

	racing.change = dropText
	if _, err := svc.PublishEntry(ctx, "note", entry.ID); !errors.Is(err, content.ErrConflict) {
		t.Fatalf("PublishEntry across a schema change: got %v, want ErrConflict", err)
	}
	restore()

	// A draft edit between validating and publishing is the other race.
	racing.change = func(ctx context.Context) {
		if _, err := svc.UpdateEntry(ctx, "note", entry.ID, json.RawMessage(`{"text": "the title is gone"}`)); err != nil {
			t.Errorf("UpdateEntry: %v", err)
		}
	}
	if _, err := svc.PublishEntry(ctx, "note", entry.ID); !errors.Is(err, content.ErrConflict) {
		t.Fatalf("PublishEntry across a draft edit: got %v, want ErrConflict", err)
	}

	// Nothing unchecked got through, and a retry works.
	page, err := svc.ListEntries(ctx, mustType(t, svc, "note"), content.QueryParams{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Entries[0].Published != nil || string(page.Entries[0].Data) != `{"text":"the title is gone"}` {
		t.Fatalf("entries after the refused writes: %+v", page.Entries)
	}
	if _, err := svc.UpdateEntry(ctx, "note", entry.ID, json.RawMessage(`{"title": "One"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishEntry(ctx, "note", entry.ID); err != nil {
		t.Fatalf("publishing after the races: %v", err)
	}
}

func mustType(t *testing.T, svc *content.Service, name string) content.Type {
	t.Helper()
	ct, err := svc.Type(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return ct
}
