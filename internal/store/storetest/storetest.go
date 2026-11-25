// Package storetest holds the conformance suite every content.Store has to
// pass. The backends run the same tests, so that what works on one works on
// the other.
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// Run runs the suite. open is called once per test and must return a store
// without any content in it.
func Run(t *testing.T, open func(t *testing.T) content.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s content.Store)
	}{
		{"Ping", testPing},
		{"Types", testTypes},
		{"TypeUpdateConflict", testTypeUpdateConflict},
		{"EntryLifecycle", testEntryLifecycle},
		{"EntryGuards", testEntryGuards},
		{"Revision", testRevision},
		{"RevisionIsNeverReused", testRevisionIsNeverReused},
		{"RemoveFields", testRemoveFields},
		{"DeleteTypeCascades", testDeleteTypeCascades},
		{"JSONRoundTrip", testJSONRoundTrip},
		{"List", testList},
		{"ListDrafts", testListDrafts},
		{"Tokens", testTokens},
		{"ConcurrentPublish", testConcurrentPublish},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, open(t))
		})
	}
}

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func at(seconds int) time.Time {
	return base.Add(time.Duration(seconds) * time.Second)
}

// articleType returns the type most tests use, with a new id on every call,
// the way the service creates types.
func articleType() content.Type {
	return content.Type{
		ID:          content.NewID(),
		Name:        "article",
		Description: "News and notes",
		Fields: []content.Field{
			{Name: "title", Type: content.FieldString, Required: true, MaxLength: ptr(200)},
			{Name: "body", Type: content.FieldText},
			{Name: "views", Type: content.FieldInteger, Min: ptr(-100.0)},
			{Name: "rating", Type: content.FieldNumber},
			{Name: "featured", Type: content.FieldBoolean},
			{Name: "released", Type: content.FieldDateTime},
			{Name: "status", Type: content.FieldEnum, Values: []string{"live", "archived", "draft"}},
			{Name: "meta", Type: content.FieldJSON},
		},
		SchemaVersion: 1,
		Revision:      1,
		CreatedAt:     at(0),
		UpdatedAt:     at(0),
	}
}

func ptr[T any](v T) *T { return &v }

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustCreateType(t *testing.T, s content.Store, ct content.Type) {
	t.Helper()
	if err := s.CreateType(t.Context(), ct); err != nil {
		t.Fatalf("CreateType(%q): %v", ct.Name, err)
	}
}

func mustCreateEntry(t *testing.T, s content.Store, typeName, id string, data any, createdAt time.Time) content.Entry {
	t.Helper()
	e := content.Entry{
		ID:        id,
		Type:      typeName,
		Data:      mustJSON(t, data),
		Version:   1,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	if err := s.CreateEntry(t.Context(), e, 1); err != nil {
		t.Fatalf("CreateEntry(%q): %v", id, err)
	}
	return e
}

func mustPublish(t *testing.T, s content.Store, typeName, id string, when time.Time) content.Entry {
	t.Helper()
	e, err := s.Entry(t.Context(), typeName, id)
	if err != nil {
		t.Fatalf("Entry(%q): %v", id, err)
	}
	p, err := s.PublishEntry(t.Context(), typeName, id, e.Version, 1, when)
	if err != nil {
		t.Fatalf("PublishEntry(%q): %v", id, err)
	}
	return p
}

func revision(t *testing.T, s content.Store, name string) string {
	t.Helper()
	rev, err := s.TypeRevision(t.Context(), name)
	if err != nil {
		t.Fatalf("TypeRevision(%q): %v", name, err)
	}
	if rev == "" {
		t.Fatalf("TypeRevision(%q) is empty", name)
	}
	return rev
}

// revisionCounter is the number behind a revision, for tests that count how
// often it moved.
func revisionCounter(t *testing.T, s content.Store, name string) int64 {
	t.Helper()
	ct, err := s.Type(t.Context(), name)
	if err != nil {
		t.Fatalf("Type(%q): %v", name, err)
	}
	return ct.Revision
}

// canonical turns JSON into a form in which two documents are equal exactly
// when they mean the same: backends may order keys and print numbers
// differently.
func canonical(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return normalizeNumbers(t, v)
}

func normalizeNumbers(t *testing.T, v any) any {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		r, ok := new(big.Rat).SetString(x.String())
		if !ok {
			t.Fatalf("invalid number %q", x)
		}
		return "number:" + r.RatString()
	case []any:
		for i := range x {
			x[i] = normalizeNumbers(t, x[i])
		}
	case map[string]any:
		for k := range x {
			x[k] = normalizeNumbers(t, x[k])
		}
	}
	return v
}

func wantJSON(t *testing.T, what string, got []byte, want any) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nothing, want %s", what, mustJSON(t, want))
	}
	if g, w := canonical(t, got), canonical(t, mustJSON(t, want)); !reflect.DeepEqual(g, w) {
		t.Fatalf("%s:\n got %s\nwant %s", what, got, mustJSON(t, want))
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: got error %v, want %v", what, err, want)
	}
}

func testPing(t *testing.T, s content.Store) {
	if err := s.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Ping(ctx); err == nil {
		t.Fatal("Ping with a cancelled context succeeded")
	}
}

func testTypes(t *testing.T, s content.Store) {
	ctx := t.Context()

	if types, err := s.Types(ctx); err != nil || len(types) != 0 {
		t.Fatalf("Types on an empty store: %v, %v", types, err)
	}
	_, err := s.Type(ctx, "article")
	wantErr(t, "Type before create", err, content.ErrNotFound)
	_, err = s.TypeRevision(ctx, "article")
	wantErr(t, "TypeRevision before create", err, content.ErrNotFound)

	want := articleType()
	mustCreateType(t, s, want)
	wantErr(t, "second CreateType", s.CreateType(ctx, want), content.ErrConflict)

	got, err := s.Type(ctx, "article")
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Type:\n got %+v\nwant %+v", got, want)
	}

	page := content.Type{
		ID: content.NewID(), Name: "page", Fields: []content.Field{{Name: "slug", Type: content.FieldString}},
		SchemaVersion: 1, Revision: 1, CreatedAt: at(1), UpdatedAt: at(1),
	}
	mustCreateType(t, s, page)
	types, err := s.Types(ctx)
	if err != nil {
		t.Fatalf("Types: %v", err)
	}
	if len(types) != 2 || types[0].Name != "article" || types[1].Name != "page" {
		t.Fatalf("Types: got %+v, want article then page", types)
	}

	change := got
	change.Description = "Changed"
	change.Fields = append([]content.Field{}, got.Fields[:2]...)
	change.UpdatedAt = at(5)
	updated, err := s.UpdateType(ctx, change, nil)
	if err != nil {
		t.Fatalf("UpdateType: %v", err)
	}
	change.SchemaVersion, change.Revision = 2, 2
	if !reflect.DeepEqual(updated, change) {
		t.Fatalf("UpdateType:\n got %+v\nwant %+v", updated, change)
	}

	if err := s.DeleteType(ctx, "page"); err != nil {
		t.Fatalf("DeleteType: %v", err)
	}
	wantErr(t, "second DeleteType", s.DeleteType(ctx, "page"), content.ErrNotFound)
	_, err = s.Type(ctx, "page")
	wantErr(t, "Type after delete", err, content.ErrNotFound)
}

func testTypeUpdateConflict(t *testing.T, s content.Store) {
	ctx := t.Context()
	ct := articleType()
	mustCreateType(t, s, ct)

	first := ct
	first.Description = "first"
	if _, err := s.UpdateType(ctx, first, nil); err != nil {
		t.Fatalf("UpdateType: %v", err)
	}
	// Still carries schema version 1, which the first update replaced.
	second := ct
	second.Description = "second"
	_, err := s.UpdateType(ctx, second, nil)
	wantErr(t, "UpdateType with a stale schema version", err, content.ErrConflict)

	missing := ct
	missing.Name = "missing"
	_, err = s.UpdateType(ctx, missing, nil)
	wantErr(t, "UpdateType of a missing type", err, content.ErrNotFound)

	got, err := s.Type(ctx, "article")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "first" || got.SchemaVersion != 2 {
		t.Fatalf("after the conflict: got description %q at schema version %d, want %q at 2", got.Description, got.SchemaVersion, "first")
	}
}

func testEntryLifecycle(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())

	draft := map[string]any{"title": "Hello", "views": 1}
	created := mustCreateEntry(t, s, "article", "e1", draft, at(10))

	got, err := s.Entry(ctx, "article", "e1")
	if err != nil {
		t.Fatalf("Entry: %v", err)
	}
	wantJSON(t, "draft after create", got.Data, draft)
	if got.Published != nil || got.Status() != content.StatusDraft || got.Version != 1 || got.PublishedVersion != 0 {
		t.Fatalf("after create: %+v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) || !got.UpdatedAt.Equal(created.UpdatedAt) || !got.PublishedAt.IsZero() {
		t.Fatalf("timestamps after create: %+v", got)
	}

	published, err := s.PublishEntry(ctx, "article", "e1", 1, 1, at(20))
	if err != nil {
		t.Fatalf("PublishEntry: %v", err)
	}
	wantJSON(t, "published copy", published.Published, draft)
	if published.Status() != content.StatusPublished || published.PublishedVersion != 1 || !published.PublishedAt.Equal(at(20)) {
		t.Fatalf("after publish: %+v", published)
	}

	edited := map[string]any{"title": "Hello again", "views": 2}
	updated, err := s.UpdateEntry(ctx, "article", "e1", mustJSON(t, edited), 1, at(30))
	if err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}
	wantJSON(t, "draft after update", updated.Data, edited)
	wantJSON(t, "published copy after a draft update", updated.Published, draft)
	if updated.Status() != content.StatusChanged || updated.Version != 2 || updated.PublishedVersion != 1 {
		t.Fatalf("after update: %+v", updated)
	}
	if !updated.UpdatedAt.Equal(at(30)) || !updated.CreatedAt.Equal(at(10)) || !updated.PublishedAt.Equal(at(20)) {
		t.Fatalf("timestamps after update: %+v", updated)
	}

	republished, err := s.PublishEntry(ctx, "article", "e1", 2, 1, at(40))
	if err != nil {
		t.Fatalf("second PublishEntry: %v", err)
	}
	wantJSON(t, "published copy after the second publish", republished.Published, edited)
	if republished.Status() != content.StatusPublished || !republished.PublishedAt.Equal(at(40)) {
		t.Fatalf("after the second publish: %+v", republished)
	}

	unpublished, err := s.UnpublishEntry(ctx, "article", "e1")
	if err != nil {
		t.Fatalf("UnpublishEntry: %v", err)
	}
	wantJSON(t, "draft after unpublish", unpublished.Data, edited)
	if unpublished.Published != nil || unpublished.Status() != content.StatusDraft || unpublished.PublishedVersion != 0 || !unpublished.PublishedAt.IsZero() {
		t.Fatalf("after unpublish: %+v", unpublished)
	}

	if err := s.DeleteEntry(ctx, "article", "e1"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
	_, err = s.Entry(ctx, "article", "e1")
	wantErr(t, "Entry after delete", err, content.ErrNotFound)
}

func testEntryGuards(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())
	other := articleType()
	other.Name = "other"
	mustCreateType(t, s, other)
	mustCreateEntry(t, s, "article", "e1", map[string]any{"title": "One"}, at(1))

	entry := content.Entry{ID: "e2", Type: "missing", Data: json.RawMessage(`{}`), Version: 1, CreatedAt: at(2), UpdatedAt: at(2)}
	wantErr(t, "CreateEntry in a missing type", s.CreateEntry(ctx, entry, 1), content.ErrNotFound)
	entry.Type = "article"
	wantErr(t, "CreateEntry with a stale schema version", s.CreateEntry(ctx, entry, 7), content.ErrConflict)

	_, err := s.Entry(ctx, "other", "e1")
	wantErr(t, "Entry through another type", err, content.ErrNotFound)
	_, err = s.Entry(ctx, "article", "nope")
	wantErr(t, "Entry that does not exist", err, content.ErrNotFound)

	data := json.RawMessage(`{"title":"Two"}`)
	_, err = s.UpdateEntry(ctx, "article", "nope", data, 1, at(3))
	wantErr(t, "UpdateEntry that does not exist", err, content.ErrNotFound)
	_, err = s.UpdateEntry(ctx, "other", "e1", data, 1, at(3))
	wantErr(t, "UpdateEntry through another type", err, content.ErrNotFound)
	_, err = s.UpdateEntry(ctx, "missing", "e1", data, 1, at(3))
	wantErr(t, "UpdateEntry in a missing type", err, content.ErrNotFound)
	_, err = s.UpdateEntry(ctx, "article", "e1", data, 7, at(3))
	wantErr(t, "UpdateEntry with a stale schema version", err, content.ErrConflict)

	_, err = s.PublishEntry(ctx, "article", "nope", 1, 1, at(4))
	wantErr(t, "PublishEntry that does not exist", err, content.ErrNotFound)
	_, err = s.PublishEntry(ctx, "article", "e1", 9, 1, at(4))
	wantErr(t, "PublishEntry with a stale entry version", err, content.ErrConflict)
	_, err = s.PublishEntry(ctx, "article", "e1", 1, 7, at(4))
	wantErr(t, "PublishEntry with a stale schema version", err, content.ErrConflict)
	_, err = s.PublishEntry(ctx, "missing", "e1", 1, 1, at(4))
	wantErr(t, "PublishEntry in a missing type", err, content.ErrNotFound)

	_, err = s.UnpublishEntry(ctx, "article", "nope")
	wantErr(t, "UnpublishEntry that does not exist", err, content.ErrNotFound)
	_, err = s.UnpublishEntry(ctx, "missing", "e1")
	wantErr(t, "UnpublishEntry in a missing type", err, content.ErrNotFound)

	wantErr(t, "DeleteEntry that does not exist", s.DeleteEntry(ctx, "article", "nope"), content.ErrNotFound)
	wantErr(t, "DeleteEntry through another type", s.DeleteEntry(ctx, "other", "e1"), content.ErrNotFound)
	wantErr(t, "DeleteEntry in a missing type", s.DeleteEntry(ctx, "missing", "e1"), content.ErrNotFound)

	got, err := s.Entry(ctx, "article", "e1")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "draft after the refused calls", got.Data, map[string]any{"title": "One"})
	if got.Version != 1 || got.Published != nil {
		t.Fatalf("the refused calls changed the entry: %+v", got)
	}
}

// testRevision pins down the contract the delivery cache relies on: the
// revision moves on every change of published content and on nothing else.
func testRevision(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())
	other := articleType()
	other.Name = "other"
	mustCreateType(t, s, other)

	rev := revision(t, s, "article")
	seen := map[string]bool{rev: true}
	step := func(what string, moves bool) {
		t.Helper()
		now := revision(t, s, "article")
		switch {
		case moves && now == rev:
			t.Fatalf("%s: the revision stayed at %s", what, now)
		case moves && seen[now]:
			t.Fatalf("%s: the revision went back to %s, which was used before", what, now)
		case !moves && now != rev:
			t.Fatalf("%s: the revision moved from %s to %s", what, rev, now)
		}
		rev, seen[now] = now, true
	}

	mustCreateEntry(t, s, "article", "e1", map[string]any{"title": "One"}, at(1))
	step("creating a draft", false)

	if _, err := s.UpdateEntry(ctx, "article", "e1", json.RawMessage(`{"title":"Two"}`), 1, at(2)); err != nil {
		t.Fatal(err)
	}
	step("editing a draft", false)

	if _, err := s.PublishEntry(ctx, "article", "e1", 2, 1, at(3)); err != nil {
		t.Fatal(err)
	}
	step("publishing", true)

	if _, err := s.UpdateEntry(ctx, "article", "e1", json.RawMessage(`{"title":"Three"}`), 1, at(4)); err != nil {
		t.Fatal(err)
	}
	step("editing the draft of a published entry", false)

	if _, err := s.PublishEntry(ctx, "article", "e1", 3, 1, at(5)); err != nil {
		t.Fatal(err)
	}
	step("publishing again", true)

	if _, err := s.PublishEntry(ctx, "article", "e1", 9, 1, at(5)); !errors.Is(err, content.ErrConflict) {
		t.Fatalf("stale publish: %v", err)
	}
	step("a refused publish", false)

	if _, err := s.UnpublishEntry(ctx, "article", "e1"); err != nil {
		t.Fatal(err)
	}
	step("unpublishing", true)

	if _, err := s.UnpublishEntry(ctx, "article", "e1"); err != nil {
		t.Fatal(err)
	}
	step("unpublishing an unpublished entry", false)

	if err := s.DeleteEntry(ctx, "article", "e1"); err != nil {
		t.Fatal(err)
	}
	step("deleting a draft", false)

	mustCreateEntry(t, s, "article", "e2", map[string]any{"title": "Four"}, at(6))
	mustPublish(t, s, "article", "e2", at(7))
	step("publishing a second entry", true)

	if err := s.DeleteEntry(ctx, "article", "e2"); err != nil {
		t.Fatal(err)
	}
	step("deleting a published entry", true)

	mustCreateEntry(t, s, "other", "o1", map[string]any{"title": "Elsewhere"}, at(8))
	mustPublish(t, s, "other", "o1", at(9))
	step("publishing in another type", false)

	ct, err := s.Type(ctx, "article")
	if err != nil {
		t.Fatal(err)
	}
	ct.Description = "changed"
	if _, err := s.UpdateType(ctx, ct, nil); err != nil {
		t.Fatal(err)
	}
	step("changing the type", true)
}

// testRevisionIsNeverReused covers a type that is deleted and created again
// under its old name. Its counter starts over, and a cache may still hold
// responses of the first life: the revisions of the second must not collide
// with them.
func testRevisionIsNeverReused(t *testing.T, s content.Store) {
	seen := map[string]string{}
	record := func(when string) {
		t.Helper()
		rev := revision(t, s, "article")
		if earlier, dup := seen[rev]; dup {
			t.Fatalf("the revision %s %s was already used %s", rev, when, earlier)
		}
		seen[rev] = when
	}

	for _, life := range []string{"in the first life", "in the second life"} {
		mustCreateType(t, s, articleType())
		record(life + ", when created")
		mustCreateEntry(t, s, "article", "e1", map[string]any{"title": "One"}, at(1))
		mustPublish(t, s, "article", "e1", at(2))
		record(life + ", after one publish")
		mustPublish(t, s, "article", "e1", at(3))
		record(life + ", after two publishes")
		if err := s.DeleteType(t.Context(), "article"); err != nil {
			t.Fatalf("DeleteType: %v", err)
		}
	}
}

func testRemoveFields(t *testing.T, s content.Store) {
	ctx := t.Context()
	ct := articleType()
	mustCreateType(t, s, ct)
	other := articleType()
	other.Name = "other"
	mustCreateType(t, s, other)

	full := map[string]any{"title": "Keep", "body": "Drop me", "views": 3, "meta": map[string]any{"body": "nested stays"}}
	mustCreateEntry(t, s, "article", "e1", full, at(1))
	mustPublish(t, s, "article", "e1", at(2))
	mustCreateEntry(t, s, "article", "e2", map[string]any{"title": "No body"}, at(3))
	mustCreateEntry(t, s, "other", "o1", full, at(4))

	ct.Fields = []content.Field{ct.Fields[0], ct.Fields[7]}
	if _, err := s.UpdateType(ctx, ct, []string{"body", "views"}); err != nil {
		t.Fatalf("UpdateType: %v", err)
	}

	want := map[string]any{"title": "Keep", "meta": map[string]any{"body": "nested stays"}}
	e1, err := s.Entry(ctx, "article", "e1")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "draft after removing fields", e1.Data, want)
	wantJSON(t, "published copy after removing fields", e1.Published, want)

	e2, err := s.Entry(ctx, "article", "e2")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "entry without the removed fields", e2.Data, map[string]any{"title": "No body"})
	if e2.Published != nil {
		t.Fatalf("removing fields published a draft: %s", e2.Published)
	}

	o1, err := s.Entry(ctx, "other", "o1")
	if err != nil {
		t.Fatal(err)
	}
	wantJSON(t, "entry of another type", o1.Data, full)
}

func testDeleteTypeCascades(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())
	other := articleType()
	other.Name = "other"
	mustCreateType(t, s, other)
	mustCreateEntry(t, s, "article", "e1", map[string]any{"title": "Gone"}, at(1))
	mustPublish(t, s, "article", "e1", at(2))
	mustCreateEntry(t, s, "other", "o1", map[string]any{"title": "Stays"}, at(3))

	if err := s.DeleteType(ctx, "article"); err != nil {
		t.Fatalf("DeleteType: %v", err)
	}
	_, err := s.TypeRevision(ctx, "article")
	wantErr(t, "TypeRevision after delete", err, content.ErrNotFound)

	// Under the same name again, the old entries must not come back.
	mustCreateType(t, s, articleType())
	_, err = s.Entry(ctx, "article", "e1")
	wantErr(t, "entry of a deleted type", err, content.ErrNotFound)
	entries, total, err := s.Entries(ctx, "article", content.Query{Limit: 10}, false)
	if err != nil || total != 0 || len(entries) != 0 {
		t.Fatalf("entries of a recreated type: %v, %d, %v", entries, total, err)
	}
	if _, err := s.Entry(ctx, "other", "o1"); err != nil {
		t.Fatalf("entry of another type: %v", err)
	}
}

func testJSONRoundTrip(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())

	docs := map[string]map[string]any{
		"empty":   {},
		"unicode": {"title": "Zażółć gęślą jaźń, 日本語, \U0001F600", "body": "line one\nline two\ttabbed"},
		"quoting": {"title": `quote " backslash \ slash / apostrophe '`, "body": "<script>&amp;</script> percent % question ? dollar $1"},
		"numbers": {"views": json.Number("-9007199254740993"), "rating": json.Number("0.1")},
		"extreme": {"views": json.Number("9223372036854775807"), "rating": json.Number("1e+25")},
		"nested": {"meta": map[string]any{
			"list":   []any{1, "two", true, nil, map[string]any{"deep": []any{}}},
			"object": map[string]any{"a": map[string]any{"b": map[string]any{"c": "d"}}},
			"zero":   0, "false": false, "blank": "",
		}},
	}
	i := 0
	for name, doc := range docs {
		i++
		id := fmt.Sprintf("e%d", i)
		mustCreateEntry(t, s, "article", id, doc, at(i))
		got, err := s.Entry(ctx, "article", id)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantJSON(t, name+" draft", got.Data, doc)
		published := mustPublish(t, s, "article", id, at(100+i))
		wantJSON(t, name+" published", published.Published, doc)
	}
}

func jsonField(name string, kind content.Kind) content.FieldRef {
	return content.FieldRef{Name: name, Kind: kind}
}

func systemField(name string, kind content.Kind) content.FieldRef {
	return content.FieldRef{Name: name, System: true, Kind: kind}
}

var (
	fTitle     = jsonField("title", content.KindString)
	fViews     = jsonField("views", content.KindInteger)
	fRating    = jsonField("rating", content.KindNumber)
	fFeatured  = jsonField("featured", content.KindBoolean)
	fReleased  = jsonField("released", content.KindString)
	fStatus    = jsonField("status", content.KindString)
	fID        = systemField("id", content.KindString)
	fCreated   = systemField("created_at", content.KindTime)
	fUpdated   = systemField("updated_at", content.KindTime)
	fPublished = systemField("published_at", content.KindTime)
)

func where(field content.FieldRef, op content.Op, values ...any) content.Filter {
	return content.Filter{Field: field, Op: op, Values: values}
}

func asc(field content.FieldRef) content.Sort  { return content.Sort{Field: field} }
func desc(field content.FieldRef) content.Sort { return content.Sort{Field: field, Desc: true} }

// publishOrder is deliberately not the order of the ids, so that sorting by
// published_at can be told apart from sorting by id.
var publishOrder = []string{"a03", "a01", "a08", "a02", "a06", "a05", "a04"}

func publishedAt(id string) time.Time {
	for i, p := range publishOrder {
		if p == id {
			return at(100 + i)
		}
	}
	panic("not published: " + id)
}

// seedList creates eight articles. a07 is never published. a08 is published
// and then edited, so its draft and its published copy differ.
func seedList(t *testing.T, s content.Store) {
	t.Helper()
	mustCreateType(t, s, articleType())

	// Same keys, other kinds of values: a store must not trip over these
	// rows while it filters or sorts articles.
	note := content.Type{
		ID:   content.NewID(),
		Name: "note",
		Fields: []content.Field{
			{Name: "title", Type: content.FieldInteger},
			{Name: "views", Type: content.FieldString},
			{Name: "rating", Type: content.FieldString},
			{Name: "featured", Type: content.FieldString},
		},
		SchemaVersion: 1, Revision: 1, CreatedAt: at(0), UpdatedAt: at(0),
	}
	mustCreateType(t, s, note)
	mustCreateEntry(t, s, "note", "n01", map[string]any{"title": 10, "views": "many", "rating": "high", "featured": "yes"}, at(1))
	mustPublish(t, s, "note", "n01", at(2))

	articles := []struct {
		id   string
		data map[string]any
	}{
		{"a01", map[string]any{"title": "Alpha", "views": 10, "rating": 4.5, "featured": true, "released": "2026-01-01T00:00:00.000000Z", "status": "live"}},
		{"a02", map[string]any{"title": "beta", "views": -5, "rating": 0.1, "featured": false, "released": "2026-01-02T12:30:00.500000Z", "status": "live"}},
		{"a03", map[string]any{"title": "Gamma ray", "views": json.Number("9007199254740993"), "rating": 1000, "featured": true, "released": "2025-12-31T23:59:59.999999Z", "status": "archived"}},
		{"a04", map[string]any{"title": "delta", "views": 10, "rating": -1.5, "status": "draft"}},
		{"a05", map[string]any{"title": "Émile", "views": 0, "rating": 0, "featured": false}},
		{"a06", map[string]any{"views": 3}},
		{"a07", map[string]any{"title": "alpha", "views": 10, "rating": 4.5, "featured": true}},
		{"a08", map[string]any{"title": "Zeta", "views": 7}},
	}
	for i, a := range articles {
		mustCreateEntry(t, s, "article", a.id, a.data, at(10+i))
	}
	for _, id := range publishOrder {
		mustPublish(t, s, "article", id, publishedAt(id))
	}
	edited := mustJSON(t, map[string]any{"title": "Zeta 2", "views": 8})
	if _, err := s.UpdateEntry(t.Context(), "article", "a08", edited, 1, at(200)); err != nil {
		t.Fatalf("UpdateEntry(a08): %v", err)
	}
}

type listCase struct {
	name      string
	q         content.Query
	want      []string
	wantTotal int // 0 means len(want)
}

func runList(t *testing.T, s content.Store, published bool, cases []listCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.q
			if q.Limit == 0 {
				q.Limit = 50
			}
			entries, total, err := s.Entries(t.Context(), "article", q, published)
			if err != nil {
				t.Fatalf("Entries: %v", err)
			}
			got := make([]string, len(entries))
			for i, e := range entries {
				got[i] = e.ID
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("entries: got %v, want %v", got, tc.want)
			}
			wantTotal := tc.wantTotal
			if wantTotal == 0 {
				wantTotal = len(tc.want)
			}
			if total != wantTotal {
				t.Errorf("total: got %d, want %d", total, wantTotal)
			}
		})
	}
}

func testList(t *testing.T, s content.Store) {
	seedList(t, s)
	allNewestFirst := []string{"a08", "a06", "a05", "a04", "a03", "a02", "a01"}

	runList(t, s, true, []listCase{
		{name: "default order is newest first", want: allNewestFirst},

		{name: "string eq", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "Alpha")}}, want: []string{"a01"}},
		{name: "string eq is case sensitive and skips drafts", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "alpha")}}, want: []string{}},
		{name: "string eq sees the published copy", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "Zeta")}}, want: []string{"a08"}},
		{name: "string eq does not see the draft", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "Zeta 2")}}, want: []string{}},
		{name: "string ne keeps entries without the field", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpNe, "Alpha")}}, want: []string{"a08", "a06", "a05", "a04", "a03", "a02"}},
		{name: "string in", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpIn, "Alpha", "beta", "nobody")}}, want: []string{"a02", "a01"}},
		{name: "string contains", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpContains, "a")}}, want: []string{"a08", "a04", "a03", "a02", "a01"}},
		{name: "string contains is case sensitive", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpContains, "A")}}, want: []string{"a01"}},
		{name: "string contains takes wildcards literally", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpContains, "%")}}, want: []string{}},
		{name: "string contains with a space", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpContains, "a r")}}, want: []string{"a03"}},
		{name: "string gt is byte order", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpGt, "a")}}, want: []string{"a05", "a04", "a02"}},
		{name: "string lte is byte order", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpLte, "Zeta")}}, want: []string{"a08", "a03", "a01"}},
		{name: "enum in", q: content.Query{Filters: []content.Filter{where(fStatus, content.OpIn, "live", "archived")}}, want: []string{"a03", "a02", "a01"}},

		{name: "integer eq", q: content.Query{Filters: []content.Filter{where(fViews, content.OpEq, int64(10))}}, want: []string{"a04", "a01"}},
		{name: "integer ne", q: content.Query{Filters: []content.Filter{where(fViews, content.OpNe, int64(10))}}, want: []string{"a08", "a06", "a05", "a03", "a02"}},
		{name: "integer gt", q: content.Query{Filters: []content.Filter{where(fViews, content.OpGt, int64(10))}}, want: []string{"a03"}},
		{name: "integer gte", q: content.Query{Filters: []content.Filter{where(fViews, content.OpGte, int64(10))}}, want: []string{"a04", "a03", "a01"}},
		{name: "integer lt", q: content.Query{Filters: []content.Filter{where(fViews, content.OpLt, int64(0))}}, want: []string{"a02"}},
		{name: "integer lte", q: content.Query{Filters: []content.Filter{where(fViews, content.OpLte, int64(0))}}, want: []string{"a05", "a02"}},
		{name: "integer in", q: content.Query{Filters: []content.Filter{where(fViews, content.OpIn, int64(3), int64(7))}}, want: []string{"a08", "a06"}},
		{name: "integer beyond 2^53 is exact", q: content.Query{Filters: []content.Filter{where(fViews, content.OpEq, int64(9007199254740993))}}, want: []string{"a03"}},
		{name: "integer next to it does not match", q: content.Query{Filters: []content.Filter{where(fViews, content.OpEq, int64(9007199254740992))}}, want: []string{}},

		{name: "number gte", q: content.Query{Filters: []content.Filter{where(fRating, content.OpGte, 0.1)}}, want: []string{"a03", "a02", "a01"}},
		{name: "number lt", q: content.Query{Filters: []content.Filter{where(fRating, content.OpLt, 0.0)}}, want: []string{"a04"}},
		{name: "number eq whole", q: content.Query{Filters: []content.Filter{where(fRating, content.OpEq, 1000.0)}}, want: []string{"a03"}},
		{name: "number eq fraction", q: content.Query{Filters: []content.Filter{where(fRating, content.OpEq, 4.5)}}, want: []string{"a01"}},
		{name: "number eq zero", q: content.Query{Filters: []content.Filter{where(fRating, content.OpEq, 0.0)}}, want: []string{"a05"}},
		{name: "number in", q: content.Query{Filters: []content.Filter{where(fRating, content.OpIn, -1.5, 0.1)}}, want: []string{"a04", "a02"}},

		{name: "boolean eq true", q: content.Query{Filters: []content.Filter{where(fFeatured, content.OpEq, true)}}, want: []string{"a03", "a01"}},
		{name: "boolean eq false", q: content.Query{Filters: []content.Filter{where(fFeatured, content.OpEq, false)}}, want: []string{"a05", "a02"}},
		{name: "boolean ne keeps entries without the field", q: content.Query{Filters: []content.Filter{where(fFeatured, content.OpNe, true)}}, want: []string{"a08", "a06", "a05", "a04", "a02"}},

		{name: "datetime gte", q: content.Query{Filters: []content.Filter{where(fReleased, content.OpGte, "2026-01-01T00:00:00.000000Z")}}, want: []string{"a02", "a01"}},
		{name: "datetime lt", q: content.Query{Filters: []content.Filter{where(fReleased, content.OpLt, "2026-01-01T00:00:00.000000Z")}}, want: []string{"a03"}},

		{name: "id eq", q: content.Query{Filters: []content.Filter{where(fID, content.OpEq, "a03")}}, want: []string{"a03"}},
		{name: "id in", q: content.Query{Filters: []content.Filter{where(fID, content.OpIn, "a01", "a05", "a07", "n01")}}, want: []string{"a05", "a01"}},
		{name: "created_at lt", q: content.Query{Filters: []content.Filter{where(fCreated, content.OpLt, at(12))}}, want: []string{"a02", "a01"}},
		{name: "created_at eq", q: content.Query{Filters: []content.Filter{where(fCreated, content.OpEq, at(12))}}, want: []string{"a03"}},
		{name: "published_at gte", q: content.Query{Filters: []content.Filter{where(fPublished, content.OpGte, publishedAt("a02"))}}, want: []string{"a06", "a05", "a04", "a02"}},

		{name: "filters combine with and", q: content.Query{Filters: []content.Filter{where(fViews, content.OpEq, int64(10)), where(fFeatured, content.OpEq, true)}}, want: []string{"a01"}},
		{name: "a field can be filtered twice", q: content.Query{Filters: []content.Filter{where(fViews, content.OpGte, int64(0)), where(fViews, content.OpLt, int64(10))}}, want: []string{"a08", "a06", "a05"}},

		{name: "sort string asc, missing last", q: content.Query{Sort: []content.Sort{asc(fTitle)}}, want: []string{"a01", "a03", "a08", "a02", "a04", "a05", "a06"}},
		{name: "sort string desc, missing last", q: content.Query{Sort: []content.Sort{desc(fTitle)}}, want: []string{"a05", "a04", "a02", "a08", "a03", "a01", "a06"}},
		{name: "sort integer asc, ties by id", q: content.Query{Sort: []content.Sort{asc(fViews)}}, want: []string{"a02", "a05", "a06", "a08", "a01", "a04", "a03"}},
		{name: "sort integer desc, ties by id", q: content.Query{Sort: []content.Sort{desc(fViews)}}, want: []string{"a03", "a01", "a04", "a08", "a06", "a05", "a02"}},
		{name: "sort number asc, missing last", q: content.Query{Sort: []content.Sort{asc(fRating)}}, want: []string{"a04", "a05", "a02", "a01", "a03", "a06", "a08"}},
		{name: "sort number desc, missing last", q: content.Query{Sort: []content.Sort{desc(fRating)}}, want: []string{"a03", "a01", "a02", "a05", "a04", "a06", "a08"}},
		{name: "sort boolean asc, missing last", q: content.Query{Sort: []content.Sort{asc(fFeatured)}}, want: []string{"a02", "a05", "a01", "a03", "a04", "a06", "a08"}},
		{name: "sort datetime desc, missing last", q: content.Query{Sort: []content.Sort{desc(fReleased)}}, want: []string{"a02", "a01", "a03", "a04", "a05", "a06", "a08"}},
		{name: "sort by two keys", q: content.Query{Sort: []content.Sort{desc(fViews), desc(fTitle)}}, want: []string{"a03", "a04", "a01", "a08", "a06", "a05", "a02"}},
		{name: "sort published_at asc", q: content.Query{Sort: []content.Sort{asc(fPublished)}}, want: publishOrder},
		{name: "sort published_at desc", q: content.Query{Sort: []content.Sort{desc(fPublished)}}, want: []string{"a04", "a05", "a06", "a02", "a08", "a01", "a03"}},
		{name: "sort created_at asc", q: content.Query{Sort: []content.Sort{asc(fCreated)}}, want: []string{"a01", "a02", "a03", "a04", "a05", "a06", "a08"}},
		{name: "sort id asc", q: content.Query{Sort: []content.Sort{asc(fID)}}, want: []string{"a01", "a02", "a03", "a04", "a05", "a06", "a08"}},
		{name: "filter and sort", q: content.Query{Filters: []content.Filter{where(fViews, content.OpLte, int64(10))}, Sort: []content.Sort{desc(fRating)}}, want: []string{"a01", "a02", "a05", "a04", "a06", "a08"}},

		{name: "first page", q: content.Query{Sort: []content.Sort{asc(fViews)}, Limit: 3}, want: []string{"a02", "a05", "a06"}, wantTotal: 7},
		{name: "second page", q: content.Query{Sort: []content.Sort{asc(fViews)}, Limit: 3, Offset: 3}, want: []string{"a08", "a01", "a04"}, wantTotal: 7},
		{name: "last page", q: content.Query{Sort: []content.Sort{asc(fViews)}, Limit: 3, Offset: 6}, want: []string{"a03"}, wantTotal: 7},
		{name: "page past the end", q: content.Query{Sort: []content.Sort{asc(fViews)}, Limit: 3, Offset: 7}, want: []string{}, wantTotal: 7},
		{name: "full page counts the rest", q: content.Query{Limit: 7}, want: allNewestFirst, wantTotal: 7},
		{name: "page of a filtered result", q: content.Query{Filters: []content.Filter{where(fViews, content.OpGte, int64(3))}, Sort: []content.Sort{asc(fViews)}, Limit: 2, Offset: 1}, want: []string{"a08", "a01"}, wantTotal: 5},
	})

	t.Run("published entries carry the published copy only", func(t *testing.T) {
		q := content.Query{Filters: []content.Filter{where(fID, content.OpEq, "a08")}, Limit: 1}
		entries, _, err := s.Entries(t.Context(), "article", q, true)
		if err != nil || len(entries) != 1 {
			t.Fatalf("Entries: %v, %v", entries, err)
		}
		wantJSON(t, "published copy", entries[0].Published, map[string]any{"title": "Zeta", "views": 7})
		if !entries[0].PublishedAt.Equal(publishedAt("a08")) || !entries[0].CreatedAt.Equal(at(17)) {
			t.Fatalf("timestamps: %+v", entries[0])
		}
	})

	t.Run("a missing type has no entries", func(t *testing.T) {
		entries, total, err := s.Entries(t.Context(), "missing", content.Query{Limit: 10}, true)
		if err != nil || total != 0 || len(entries) != 0 {
			t.Fatalf("Entries: %v, %d, %v", entries, total, err)
		}
	})
}

func testListDrafts(t *testing.T, s content.Store) {
	seedList(t, s)

	runList(t, s, false, []listCase{
		{name: "default order is newest first", want: []string{"a08", "a07", "a06", "a05", "a04", "a03", "a02", "a01"}},
		{name: "string eq sees the draft", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "Zeta 2")}}, want: []string{"a08"}},
		{name: "string eq does not see the published copy", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "Zeta")}}, want: []string{}},
		{name: "unpublished entries are listed", q: content.Query{Filters: []content.Filter{where(fTitle, content.OpEq, "alpha")}}, want: []string{"a07"}},
		{name: "integer eq on drafts", q: content.Query{Filters: []content.Filter{where(fViews, content.OpEq, int64(10))}}, want: []string{"a07", "a04", "a01"}},
		{name: "sort string asc on drafts", q: content.Query{Sort: []content.Sort{asc(fTitle)}}, want: []string{"a01", "a03", "a08", "a07", "a02", "a04", "a05", "a06"}},
		{name: "sort updated_at desc", q: content.Query{Sort: []content.Sort{desc(fUpdated)}, Limit: 2}, want: []string{"a08", "a07"}, wantTotal: 8},
		{name: "updated_at gte", q: content.Query{Filters: []content.Filter{where(fUpdated, content.OpGte, at(200))}}, want: []string{"a08"}},
		{name: "published_at sorts unpublished entries last", q: content.Query{Sort: []content.Sort{asc(fPublished)}}, want: append(append([]string{}, publishOrder...), "a07")},
	})

	t.Run("drafts carry both copies", func(t *testing.T) {
		q := content.Query{Filters: []content.Filter{where(fID, content.OpEq, "a08")}, Limit: 1}
		entries, _, err := s.Entries(t.Context(), "article", q, false)
		if err != nil || len(entries) != 1 {
			t.Fatalf("Entries: %v, %v", entries, err)
		}
		wantJSON(t, "draft", entries[0].Data, map[string]any{"title": "Zeta 2", "views": 8})
		wantJSON(t, "published copy", entries[0].Published, map[string]any{"title": "Zeta", "views": 7})
		if entries[0].Status() != content.StatusChanged {
			t.Fatalf("status: got %s, want %s", entries[0].Status(), content.StatusChanged)
		}
	})
}

func testTokens(t *testing.T, s content.Store) {
	ctx := t.Context()

	if tokens, err := s.Tokens(ctx); err != nil || len(tokens) != 0 {
		t.Fatalf("Tokens on an empty store: %v, %v", tokens, err)
	}
	_, err := s.TokenByHash(ctx, "nope")
	wantErr(t, "TokenByHash before create", err, content.ErrNotFound)

	expires := at(3600)
	reader := content.Token{
		ID: "t1", Name: "Website", Prefix: "tapirpost_abc123",
		Scopes: []content.Scope{content.ScopeRead}, Types: []string{"article", "page"},
		CreatedAt: at(1), ExpiresAt: &expires,
	}
	admin := content.Token{
		ID: "t2", Name: "Ops", Prefix: "tapirpost_def456",
		Scopes: []content.Scope{content.ScopeAdmin}, Types: []string{},
		CreatedAt: at(2),
	}
	if err := s.CreateToken(ctx, reader, "hash-1"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.CreateToken(ctx, admin, "hash-2"); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := s.CreateToken(ctx, content.Token{ID: "t3", Name: "Twin", Scopes: admin.Scopes, Types: []string{}, CreatedAt: at(3)}, "hash-2"); err == nil {
		t.Fatal("CreateToken accepted a hash that is already taken")
	}

	got, err := s.TokenByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("TokenByHash: %v", err)
	}
	if !reflect.DeepEqual(got, reader) {
		t.Fatalf("TokenByHash:\n got %+v\nwant %+v", got, reader)
	}

	tokens, err := s.Tokens(ctx)
	if err != nil {
		t.Fatalf("Tokens: %v", err)
	}
	if !reflect.DeepEqual(tokens, []content.Token{reader, admin}) {
		t.Fatalf("Tokens:\n got %+v\nwant %+v", tokens, []content.Token{reader, admin})
	}

	if err := s.DeleteToken(ctx, "t1"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	wantErr(t, "second DeleteToken", s.DeleteToken(ctx, "t1"), content.ErrNotFound)
	_, err = s.TokenByHash(ctx, "hash-1")
	wantErr(t, "TokenByHash after delete", err, content.ErrNotFound)
	if _, err := s.TokenByHash(ctx, "hash-2"); err != nil {
		t.Fatalf("the other token is gone too: %v", err)
	}
}

// testConcurrentPublish checks that no revision is lost when publishes race:
// a lost one would leave a stale cache entry reachable.
func testConcurrentPublish(t *testing.T, s content.Store) {
	ctx := t.Context()
	mustCreateType(t, s, articleType())
	const writers, rounds = 8, 5
	for w := range writers {
		mustCreateEntry(t, s, "article", fmt.Sprintf("e%d", w), map[string]any{"title": "start"}, at(w))
	}
	before := revisionCounter(t, s, "article")

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := range writers {
		wg.Go(func() {
			id := fmt.Sprintf("e%d", w)
			for r := range rounds {
				data := json.RawMessage(fmt.Sprintf(`{"title":"writer %d round %d"}`, w, r))
				e, err := s.UpdateEntry(ctx, "article", id, data, 1, at(1000+r))
				if err != nil {
					errs <- fmt.Errorf("UpdateEntry(%s): %w", id, err)
					return
				}
				if _, err := s.PublishEntry(ctx, "article", id, e.Version, 1, at(1000+r)); err != nil {
					errs <- fmt.Errorf("PublishEntry(%s): %w", id, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if got, want := revisionCounter(t, s, "article"), before+writers*rounds; got != want {
		t.Fatalf("revision counter after %d publishes: got %d, want %d", writers*rounds, got, want)
	}
	entries, total, err := s.Entries(ctx, "article", content.Query{Limit: 50}, true)
	if err != nil || total != writers || len(entries) != writers {
		t.Fatalf("published entries: %d of %d, %v", len(entries), total, err)
	}
	for _, e := range entries {
		if e.Version != rounds+1 || e.PublishedVersion != e.Version {
			t.Fatalf("entry %s ended at version %d, published %d", e.ID, e.Version, e.PublishedVersion)
		}
	}
}
