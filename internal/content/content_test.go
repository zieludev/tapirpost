package content

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var uuidV7 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewID(t *testing.T) {
	before := time.Now().UnixMilli()
	const n = 20000
	ids := make([]string, n)
	for i := range ids {
		ids[i] = NewID()
	}
	after := time.Now().UnixMilli()

	for i, id := range ids {
		if !uuidV7.MatchString(id) {
			t.Fatalf("id %d is not a UUIDv7: %q", i, id)
		}
		if i > 0 && id <= ids[i-1] {
			t.Fatalf("id %d does not sort after the one before it: %q, %q", i, ids[i-1], id)
		}
	}

	// The first 48 bits are the time in milliseconds. Ids that had to count
	// past a clock that stood still may run a little ahead of it.
	var ms int64
	for _, c := range strings.ReplaceAll(ids[0], "-", "")[:12] {
		ms = ms<<4 | int64(strings.IndexRune("0123456789abcdef", c))
	}
	if ms < before || ms > after+1 {
		t.Fatalf("the timestamp of %q is %d, outside %d..%d", ids[0], ms, before, after)
	}
}

func TestNewIDConcurrently(t *testing.T) {
	const workers, each = 8, 2000
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[string]bool, workers*each)
	)
	for range workers {
		wg.Go(func() {
			local := make([]string, each)
			for i := range local {
				local[i] = NewID()
			}
			if !slices.IsSorted(local) {
				t.Error("the ids one goroutine got are not in order")
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if seen[id] {
					t.Errorf("duplicate id %q", id)
				}
				seen[id] = true
			}
		})
	}
	wg.Wait()
}

func TestTokenAllows(t *testing.T) {
	token := func(types []string, scopes ...Scope) Token {
		return Token{Scopes: scopes, Types: types}
	}
	cases := []struct {
		name     string
		token    Token
		scope    Scope
		typeName string
		want     bool
	}{
		{"read reads", token(nil, ScopeRead), ScopeRead, "article", true},
		{"read does not write", token(nil, ScopeRead), ScopeWrite, "article", false},
		{"read does not administer", token(nil, ScopeRead), ScopeAdmin, "", false},
		{"write reads", token(nil, ScopeWrite), ScopeRead, "article", true},
		{"write writes", token(nil, ScopeWrite), ScopeWrite, "article", true},
		{"write does not administer", token(nil, ScopeWrite), ScopeAdmin, "", false},
		{"admin reads", token(nil, ScopeAdmin), ScopeRead, "article", true},
		{"admin writes", token(nil, ScopeAdmin), ScopeWrite, "article", true},
		{"admin administers", token(nil, ScopeAdmin), ScopeAdmin, "", true},
		{"no scopes", token(nil), ScopeRead, "article", false},
		{"unknown scope asked", token(nil, ScopeRead, ScopeWrite), Scope("delete"), "article", false},
		{"unknown scope held", token(nil, Scope("delete")), ScopeRead, "article", false},
		{"limited, on its type", token([]string{"article", "page"}, ScopeRead), ScopeRead, "page", true},
		{"limited, on another type", token([]string{"article"}, ScopeRead), ScopeRead, "page", false},
		{"limited, type names are exact", token([]string{"article"}, ScopeRead), ScopeRead, "articles", false},
		{"limited, asked about the scope alone", token([]string{"article"}, ScopeRead), ScopeRead, "", true},
		{"limited write, on another type", token([]string{"article"}, ScopeWrite), ScopeWrite, "page", false},
		{"limited write still cannot administer", token([]string{"article"}, ScopeWrite), ScopeAdmin, "article", false},
		{"an empty list is no limit", token([]string{}, ScopeRead), ScopeRead, "page", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.Allows(tc.scope, tc.typeName); got != tc.want {
				t.Fatalf("Allows(%q, %q): got %v, want %v", tc.scope, tc.typeName, got, tc.want)
			}
		})
	}
}

func TestSecrets(t *testing.T) {
	a, b := newSecret(), newSecret()
	if a == b || !strings.HasPrefix(a, tokenPrefix) || len(a) < 40 {
		t.Fatalf("secrets: %q, %q", a, b)
	}
	first, again := hashSecret(a), hashSecret(strings.Clone(a))
	if first == hashSecret(b) || first != again || len(first) != 64 {
		t.Fatal("hashSecret is not a stable 256-bit hash")
	}
	if strings.Contains(hashSecret(a), a[len(tokenPrefix):]) {
		t.Fatal("the hash contains the secret")
	}
	if !sameSecret(a, a) || sameSecret(a, b) || sameSecret(a, a+"x") || sameSecret("", a) {
		t.Fatal("sameSecret is wrong")
	}
}

func testType() Type {
	return Type{
		Name: "article",
		Fields: []Field{
			{Name: "title", Type: FieldString, Required: true},
			{Name: "body", Type: FieldText},
			{Name: "views", Type: FieldInteger},
			{Name: "rating", Type: FieldNumber},
			{Name: "featured", Type: FieldBoolean},
			{Name: "released", Type: FieldDateTime},
			{Name: "status", Type: FieldEnum, Values: []string{"live", "archived"}},
			{Name: "meta", Type: FieldJSON},
		},
	}
}

func TestNormalize(t *testing.T) {
	ct := testType()

	t.Run("canonical form", func(t *testing.T) {
		in := `{ "views" : 7, "title":"<b>Tom & Jerry</b>", "released":"2026-03-01T10:30:00.5+02:00",
			"meta":{"z":1,"a":[true,null]}, "body":null, "rating": 2.50 }`
		got, err := ct.normalize(json.RawMessage(in), true)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"meta":{"a":[true,null],"z":1},"rating":2.5,"released":"2026-03-01T08:30:00.500000Z","title":"<b>Tom & Jerry</b>","views":7}`
		if string(got) != want {
			t.Fatalf("normalize:\n got %s\nwant %s", got, want)
		}
		again, err := ct.normalize(got, true)
		if err != nil || string(again) != want {
			t.Fatalf("normalizing a canonical document changed it: %s, %v", again, err)
		}
	})

	t.Run("required fields", func(t *testing.T) {
		for _, in := range []string{`{}`, `{"title": null}`, `{"views": 1}`} {
			if _, err := ct.normalize(json.RawMessage(in), false); err != nil {
				t.Errorf("a draft %s was refused: %v", in, err)
			}
			_, err := ct.normalize(json.RawMessage(in), true)
			var validation *ValidationError
			if !errors.As(err, &validation) || len(validation.Issues) != 1 || validation.Issues[0].Field != "title" {
				t.Errorf("publishing %s: got %v, want one issue about title", in, err)
			}
		}
	})

	t.Run("not an object", func(t *testing.T) {
		for _, in := range []string{``, `null`, `[]`, `"x"`, `1`, `{"title": "x"} {}`, `{"title": "x"`, `{title: "x"}`} {
			if _, err := ct.normalize(json.RawMessage(in), false); err == nil {
				t.Errorf("normalize(%q) succeeded", in)
			}
		}
	})

	t.Run("issues come in a fixed order", func(t *testing.T) {
		_, err := ct.normalize(json.RawMessage(`{"zzz": 1, "views": "x", "aaa": 2, "rating": "y"}`), true)
		var validation *ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("got %v", err)
		}
		var fields []string
		for _, issue := range validation.Issues {
			fields = append(fields, issue.Field)
		}
		if want := []string{"aaa", "rating", "title", "views", "zzz"}; !reflect.DeepEqual(fields, want) {
			t.Fatalf("issue order: got %v, want %v", fields, want)
		}
		if !strings.Contains(err.Error(), "title is required") {
			t.Fatalf("error text: %v", err)
		}
	})
}

func TestNormalizeTime(t *testing.T) {
	cases := map[string]string{
		"2026-01-31T12:00:00Z":           "2026-01-31T12:00:00.000000Z",
		"2026-01-31T12:00:00.5Z":         "2026-01-31T12:00:00.500000Z",
		"2026-01-31T12:00:00.123456789Z": "2026-01-31T12:00:00.123456Z",
		"2026-01-31T13:00:00+01:00":      "2026-01-31T12:00:00.000000Z",
		"2026-01-31T23:30:00-02:30":      "2026-02-01T02:00:00.000000Z",
		"0001-01-01T00:00:00Z":           "0001-01-01T00:00:00.000000Z",
		"9999-12-31T23:59:59.999999Z":    "9999-12-31T23:59:59.999999Z",
		"2026-01-31":                     "",
		"2026-01-31 12:00:00":            "",
		"2026-01-31T12:00:00":            "",
		"2026-13-01T00:00:00Z":           "",
		"9999-12-31T23:59:59-01:00":      "",
		"0000-01-01T00:00:00+01:00":      "",
		"":                               "",
		"now":                            "",
		" 2026-01-31T12:00:00Z":          "",
	}
	for in, want := range cases {
		got, err := normalizeTime(in)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("normalizeTime(%q): got %q, %v; want %q", in, got, err, want)
		}
	}

	// The point of the fixed width: text order is time order.
	inOrder := []string{"2026-01-31T12:00:00Z", "2026-01-31T12:00:00.000001Z", "2026-01-31T12:00:00.5Z", "2026-01-31T12:00:01Z", "2026-01-31T14:00:00+01:00"}
	var normalized []string
	for _, in := range inOrder {
		n, err := normalizeTime(in)
		if err != nil {
			t.Fatal(err)
		}
		normalized = append(normalized, n)
	}
	if !slices.IsSorted(normalized) {
		t.Fatalf("normalized timestamps do not sort by time: %v", normalized)
	}
}

func TestTypeQuery(t *testing.T) {
	ct := testType()
	n := func(i int) *int { return &i }

	t.Run("defaults", func(t *testing.T) {
		q, err := ct.Query(QueryParams{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if want := (Query{Limit: DefaultLimit}); !reflect.DeepEqual(q, want) {
			t.Fatalf("got %+v, want %+v", q, want)
		}
	})

	t.Run("values take the kind of their field", func(t *testing.T) {
		q, err := ct.Query(QueryParams{
			Filters: []RawFilter{
				{"title", "eq", "1"},
				{"views", "gte", "-3"},
				{"rating", "lt", "2.5"},
				{"featured", "ne", "false"},
				{"released", "gt", "2026-01-31T13:00:00+01:00"},
				{"status", "in", "live,archived"},
				{"views", "in", "1,2,3"},
				{"published_at", "lte", "2026-01-31T13:00:00+01:00"},
				{"id", "eq", "abc"},
			},
			Sort:   []string{"-published_at", "title"},
			Limit:  n(5),
			Offset: n(10),
		}, true)
		if err != nil {
			t.Fatal(err)
		}
		want := Query{
			Filters: []Filter{
				{FieldRef{Name: "title", Kind: KindString}, OpEq, []any{"1"}},
				{FieldRef{Name: "views", Kind: KindInteger}, OpGte, []any{int64(-3)}},
				{FieldRef{Name: "rating", Kind: KindNumber}, OpLt, []any{2.5}},
				{FieldRef{Name: "featured", Kind: KindBoolean}, OpNe, []any{false}},
				{FieldRef{Name: "released", Kind: KindString}, OpGt, []any{"2026-01-31T12:00:00.000000Z"}},
				{FieldRef{Name: "status", Kind: KindString}, OpIn, []any{"live", "archived"}},
				{FieldRef{Name: "views", Kind: KindInteger}, OpIn, []any{int64(1), int64(2), int64(3)}},
				{FieldRef{Name: "published_at", System: true, Kind: KindTime}, OpLte, []any{time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)}},
				{FieldRef{Name: "id", System: true, Kind: KindString}, OpEq, []any{"abc"}},
			},
			Sort: []Sort{
				{FieldRef{Name: "published_at", System: true, Kind: KindTime}, true},
				{FieldRef{Name: "title", Kind: KindString}, false},
			},
			Limit:  5,
			Offset: 10,
		}
		if !reflect.DeepEqual(q, want) {
			t.Fatalf("got  %+v\nwant %+v", q, want)
		}
	})

	t.Run("what changes without a publish is for drafts only", func(t *testing.T) {
		params := QueryParams{Filters: []RawFilter{{"updated_at", "gt", "2026-01-01T00:00:00Z"}}, Sort: []string{"-updated_at"}}
		if _, err := ct.Query(params, false); err != nil {
			t.Fatalf("drafts: %v", err)
		}
		_, err := ct.Query(params, true)
		var validation *ValidationError
		if !errors.As(err, &validation) || len(validation.Issues) != 2 {
			t.Fatalf("published content: got %v, want two issues", err)
		}
	})

	t.Run("limits", func(t *testing.T) {
		for _, limit := range []int{1, DefaultLimit, MaxLimit} {
			if _, err := ct.Query(QueryParams{Limit: n(limit)}, true); err != nil {
				t.Errorf("limit %d: %v", limit, err)
			}
		}
		for _, params := range []QueryParams{
			{Limit: n(0)}, {Limit: n(-1)}, {Limit: n(MaxLimit + 1)}, {Offset: n(-1)},
		} {
			if _, err := ct.Query(params, true); err == nil {
				t.Errorf("%+v was accepted", params)
			}
		}
	})
}

func TestRemovedOrRetyped(t *testing.T) {
	old := []Field{{Name: "a", Type: FieldString}, {Name: "b", Type: FieldInteger}, {Name: "c", Type: FieldText}}

	removed, err := removedOrRetyped(old, []Field{{Name: "b", Type: FieldInteger, Required: true}, {Name: "d", Type: FieldJSON}})
	if err != nil || !reflect.DeepEqual(removed, []string{"a", "c"}) {
		t.Fatalf("got %v, %v; want [a c]", removed, err)
	}
	removed, err = removedOrRetyped(old, old)
	if err != nil || len(removed) != 0 {
		t.Fatalf("no change: got %v, %v", removed, err)
	}
	if _, err = removedOrRetyped(old, []Field{{Name: "a", Type: FieldText}, {Name: "b", Type: FieldInteger}, {Name: "c", Type: FieldText}}); err == nil {
		t.Fatal("a changed field type was accepted")
	}
}
