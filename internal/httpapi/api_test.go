package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/httpapi"
)

func TestAuthorization(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	h.createType(articleSpec)
	h.createType(map[string]any{"name": "page", "fields": []map[string]any{{"name": "title", "type": "string"}}})
	h.published("article", map[string]any{"title": "Hello"})

	revoked := h.newToken([]string{"admin"})
	listed := decode[struct {
		Items []tokenResponse `json:"items"`
	}](t, h.admin("GET", "/v1/tokens", nil))
	wantStatus(t, h.admin("DELETE", "/v1/tokens/"+listed.Items[len(listed.Items)-1].ID, nil), http.StatusNoContent)

	// An expired token cannot be made through the API, which refuses expiry
	// dates in the past, so this one goes straight into the store.
	const expired = "tapirpost_expired-token-for-the-tests"
	sum := sha256.Sum256([]byte(expired))
	past := time.Now().Add(-time.Minute)
	err := h.store.CreateToken(t.Context(), content.Token{
		ID: "expired", Name: "expired", Scopes: []content.Scope{content.ScopeAdmin}, Types: []string{},
		CreatedAt: past.Add(-time.Hour), ExpiresAt: &past,
	}, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}

	tokens := map[string]string{
		"none":           "",
		"unknown":        "tapirpost_this-token-does-not-exist",
		"not ours":       "sk-something-else",
		"revoked":        revoked,
		"expired":        expired,
		"read":           h.newToken([]string{"read"}),
		"write":          h.newToken([]string{"write"}),
		"read and write": h.newToken([]string{"read", "write"}),
		"admin":          h.newToken([]string{"admin"}),
		"root":           rootToken,
		"read page":      h.newToken([]string{"read"}, "page"),
		"write page":     h.newToken([]string{"write"}, "page"),
		"write article":  h.newToken([]string{"write"}, "article"),
	}

	entry := map[string]any{"fields": map[string]any{"title": "New"}}
	requests := []struct {
		name   string
		method string
		path   string
		body   any
		ok     int
		// allowed lists the tokens that get through. Everyone else gets
		// 401 without a valid token and 403 with one.
		allowed []string
	}{
		{"deliver list", "GET", "/v1/content/article", nil, 200, []string{"read", "write", "read and write", "admin", "root", "write article"}},
		{"deliver entry", "GET", "/v1/content/article/missing", nil, 404, []string{"read", "write", "read and write", "admin", "root", "write article"}},
		{"list types", "GET", "/v1/types", nil, 200, []string{"read", "write", "read and write", "admin", "root", "read page", "write page", "write article"}},
		{"get type", "GET", "/v1/types/article", nil, 200, []string{"read", "write", "read and write", "admin", "root", "write article"}},
		{"create type", "POST", "/v1/types", map[string]any{"name": "!"}, 422, []string{"admin", "root"}},
		{"update type", "PUT", "/v1/types/article", map[string]any{"fields": []any{}}, 422, []string{"admin", "root"}},
		{"delete type", "DELETE", "/v1/types/missing", nil, 404, []string{"admin", "root"}},
		{"list entries", "GET", "/v1/types/article/entries", nil, 200, []string{"write", "read and write", "admin", "root", "write article"}},
		{"create entry", "POST", "/v1/types/article/entries", entry, 201, []string{"write", "read and write", "admin", "root", "write article"}},
		{"get entry", "GET", "/v1/types/article/entries/missing", nil, 404, []string{"write", "read and write", "admin", "root", "write article"}},
		{"update entry", "PUT", "/v1/types/article/entries/missing", entry, 404, []string{"write", "read and write", "admin", "root", "write article"}},
		{"delete entry", "DELETE", "/v1/types/article/entries/missing", nil, 404, []string{"write", "read and write", "admin", "root", "write article"}},
		{"publish", "POST", "/v1/types/article/entries/missing/publish", nil, 404, []string{"write", "read and write", "admin", "root", "write article"}},
		{"unpublish", "POST", "/v1/types/article/entries/missing/unpublish", nil, 404, []string{"write", "read and write", "admin", "root", "write article"}},
		{"list tokens", "GET", "/v1/tokens", nil, 200, []string{"admin", "root"}},
		{"create token", "POST", "/v1/tokens", map[string]any{"name": ""}, 422, []string{"admin", "root"}},
		{"delete token", "DELETE", "/v1/tokens/missing", nil, 404, []string{"admin", "root"}},
	}
	invalid := []string{"none", "unknown", "not ours", "revoked", "expired"}

	for _, req := range requests {
		for name, token := range tokens {
			t.Run(req.name+"/"+name, func(t *testing.T) {
				rec := h.do(req.method, req.path, token, req.body)
				switch {
				case slices.Contains(req.allowed, name):
					wantStatus(t, rec, req.ok)
				case slices.Contains(invalid, name):
					wantError(t, rec, http.StatusUnauthorized, "unauthorized")
					if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
						t.Fatalf("WWW-Authenticate: got %q", got)
					}
				default:
					wantError(t, rec, http.StatusForbidden, "forbidden")
				}
			})
		}
	}

	t.Run("probes need no token", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz", "/openapi.json"} {
			wantStatus(t, h.do("GET", path, "", nil), http.StatusOK)
		}
	})

	t.Run("the scheme is case insensitive and must be bearer", func(t *testing.T) {
		wantStatus(t, h.do("GET", "/v1/types", "", nil, "Authorization", "bearer "+rootToken), http.StatusOK)
		wantError(t, h.do("GET", "/v1/types", "", nil, "Authorization", "Basic "+rootToken), http.StatusUnauthorized, "unauthorized")
		wantError(t, h.do("GET", "/v1/types", "", nil, "Authorization", rootToken), http.StatusUnauthorized, "unauthorized")
	})

	t.Run("a limited token only sees its types", func(t *testing.T) {
		list := decode[struct {
			Items []content.Type `json:"items"`
		}](t, h.do("GET", "/v1/types", tokens["read page"], nil))
		if len(list.Items) != 1 || list.Items[0].Name != "page" {
			t.Fatalf("types visible to a token for page: %+v", list.Items)
		}
	})

	t.Run("a secret is never listed", func(t *testing.T) {
		body := h.admin("GET", "/v1/tokens", nil).Body.String()
		for name, secret := range tokens {
			if secret != "" && strings.Contains(body, secret) {
				t.Fatalf("the token list contains the secret of %q", name)
			}
		}
		if strings.Contains(body, "token_hash") || strings.Contains(body, `"secret"`) {
			t.Fatalf("the token list leaks: %s", body)
		}
	})
}

func TestTokenRules(t *testing.T) {
	h := newHarness(t, sqliteStore(t))

	rec := h.admin("POST", "/v1/tokens", map[string]any{
		"name": "  Website  ", "scopes": []string{"read"}, "types": []string{"article"},
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	wantStatus(t, rec, http.StatusCreated)
	token := decode[tokenResponse](t, rec)
	if token.Name != "Website" || !strings.HasPrefix(token.Secret, "tapirpost_") || !strings.HasPrefix(token.Secret, token.Prefix) || len(token.Prefix) >= len(token.Secret)/2 {
		t.Fatalf("token: %+v", token)
	}
	if token.ExpiresAt == nil || !reflect.DeepEqual(token.Types, []string{"article"}) || !reflect.DeepEqual(token.Scopes, []string{"read"}) {
		t.Fatalf("token: %+v", token)
	}
	if got := rec.Header().Get("Location"); got != "/v1/tokens/"+token.ID {
		t.Fatalf("Location: got %q", got)
	}

	bad := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"no name", map[string]any{"scopes": []string{"read"}}, "name"},
		{"no scopes", map[string]any{"name": "x"}, "scopes"},
		{"unknown scope", map[string]any{"name": "x", "scopes": []string{"root"}}, "scopes"},
		{"repeated scope", map[string]any{"name": "x", "scopes": []string{"read", "read"}}, "scopes"},
		{"limited admin", map[string]any{"name": "x", "scopes": []string{"admin"}, "types": []string{"article"}}, "types"},
		{"bad type name", map[string]any{"name": "x", "scopes": []string{"read"}, "types": []string{"Not A Name"}}, "types"},
		{"expiry in the past", map[string]any{"name": "x", "scopes": []string{"read"}, "expires_at": "2020-01-01T00:00:00Z"}, "expires_at"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, h.admin("POST", "/v1/tokens", tc.body), http.StatusUnprocessableEntity, "validation_failed", tc.field)
		})
	}
}

func TestPublicDelivery(t *testing.T) {
	h := newHarness(t, sqliteStore(t), func(o *httpapi.Options) { o.PublicDelivery = true })
	h.createType(articleSpec)
	id := h.published("article", map[string]any{"title": "Open"})

	for _, path := range []string{"/v1/content/article", "/v1/content/article/" + id} {
		rec := h.do("GET", path, "", nil)
		wantStatus(t, rec, http.StatusOK)
		if vary := rec.Header().Get("Vary"); vary != "" {
			t.Fatalf("Vary on public content: got %q", vary)
		}
	}
	// A token that would be refused is not even looked at.
	wantStatus(t, h.do("GET", "/v1/content/article", "garbage", nil), http.StatusOK)
	wantError(t, h.do("GET", "/v1/types", "", nil), http.StatusUnauthorized, "unauthorized")
	wantError(t, h.do("GET", "/v1/types/article/entries", "", nil), http.StatusUnauthorized, "unauthorized")
}

func TestTypes(t *testing.T) {
	h := newHarness(t, sqliteStore(t))

	rec := h.admin("POST", "/v1/types", articleSpec)
	wantStatus(t, rec, http.StatusCreated)
	if got := rec.Header().Get("Location"); got != "/v1/types/article" {
		t.Fatalf("Location: got %q", got)
	}
	created := decode[content.Type](t, rec)
	if created.Name != "article" || created.SchemaVersion != 1 || len(created.Fields) != 8 || created.CreatedAt.IsZero() {
		t.Fatalf("created type: %+v", created)
	}
	if strings.Contains(rec.Body.String(), `"id"`) {
		t.Fatalf("the internal id of a type is exposed: %s", rec.Body)
	}

	wantError(t, h.admin("POST", "/v1/types", articleSpec), http.StatusConflict, "conflict")

	got := decode[content.Type](t, h.admin("GET", "/v1/types/article", nil))
	if !reflect.DeepEqual(got, created) {
		t.Fatalf("GET after POST:\n got %+v\nwant %+v", got, created)
	}
	wantError(t, h.admin("GET", "/v1/types/missing", nil), http.StatusNotFound, "not_found")
	wantError(t, h.admin("GET", "/v1/types/Not%20A%20Name", nil), http.StatusNotFound, "not_found")

	h.createType(map[string]any{"name": "aardvark", "fields": []map[string]any{{"name": "n", "type": "integer"}}})
	list := decode[struct {
		Items []content.Type `json:"items"`
	}](t, h.admin("GET", "/v1/types", nil))
	if len(list.Items) != 2 || list.Items[0].Name != "aardvark" || list.Items[1].Name != "article" {
		t.Fatalf("list: %+v", list.Items)
	}

	t.Run("update", func(t *testing.T) {
		id := h.published("article", map[string]any{"title": "Kept", "body": "Dropped", "views": 3})

		rec := h.admin("PUT", "/v1/types/article", map[string]any{
			"description": "Shorter",
			"fields": []map[string]any{
				{"name": "title", "type": "string", "required": true},
				{"name": "views", "type": "integer"},
				{"name": "summary", "type": "text"},
			},
		})
		wantStatus(t, rec, http.StatusOK)
		updated := decode[content.Type](t, rec)
		if updated.Description != "Shorter" || updated.SchemaVersion != 2 || len(updated.Fields) != 3 || updated.Revision <= created.Revision {
			t.Fatalf("updated type: %+v", updated)
		}

		entry := decode[entryResponse](t, h.admin("GET", "/v1/types/article/entries/"+id, nil))
		want := map[string]any{"title": "Kept", "views": float64(3)}
		if !reflect.DeepEqual(entry.Fields, want) || !reflect.DeepEqual(entry.PublishedFields, want) {
			t.Fatalf("entry after its field was removed: %+v", entry)
		}

		wantError(t, h.admin("PUT", "/v1/types/article", map[string]any{
			"fields": []map[string]any{{"name": "title", "type": "integer"}},
		}), http.StatusUnprocessableEntity, "validation_failed", "fields")
		wantError(t, h.admin("PUT", "/v1/types/article", map[string]any{
			"name": "post", "fields": []map[string]any{{"name": "title", "type": "string"}},
		}), http.StatusUnprocessableEntity, "validation_failed", "name")
		wantError(t, h.admin("PUT", "/v1/types/missing", map[string]any{
			"fields": []map[string]any{{"name": "title", "type": "string"}},
		}), http.StatusNotFound, "not_found")
	})

	t.Run("delete", func(t *testing.T) {
		wantStatus(t, h.admin("DELETE", "/v1/types/aardvark", nil), http.StatusNoContent)
		wantError(t, h.admin("DELETE", "/v1/types/aardvark", nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/types/aardvark", nil), http.StatusNotFound, "not_found")
	})
}

func TestTypeValidation(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	field := func(f map[string]any) map[string]any {
		return map[string]any{"name": "thing", "fields": []map[string]any{f}}
	}
	many := make([]map[string]any, content.MaxFields+1)
	for i := range many {
		many[i] = map[string]any{"name": "f" + strings.Repeat("x", i), "type": "string"}
	}

	cases := []struct {
		name  string
		spec  map[string]any
		field string
	}{
		{"no name", map[string]any{"fields": []map[string]any{{"name": "a", "type": "string"}}}, "name"},
		{"uppercase name", map[string]any{"name": "Article", "fields": []map[string]any{{"name": "a", "type": "string"}}}, "name"},
		{"name with a dash", map[string]any{"name": "blog-post", "fields": []map[string]any{{"name": "a", "type": "string"}}}, "name"},
		{"name too long", map[string]any{"name": strings.Repeat("a", 64), "fields": []map[string]any{{"name": "a", "type": "string"}}}, "name"},
		{"no fields", map[string]any{"name": "thing"}, "fields"},
		{"too many fields", map[string]any{"name": "thing", "fields": many}, "fields"},
		{"description too long", map[string]any{"name": "thing", "description": strings.Repeat("d", content.MaxDescriptionLen+1), "fields": []map[string]any{{"name": "a", "type": "string"}}}, "description"},
		{"bad field name", field(map[string]any{"name": "Title", "type": "string"}), "fields[0].name"},
		{"reserved field name", field(map[string]any{"name": "published_at", "type": "datetime"}), "fields[0].name"},
		{"unknown field type", field(map[string]any{"name": "a", "type": "uuid"}), "fields[0].type"},
		{"length rule on a number", field(map[string]any{"name": "a", "type": "integer", "max_length": 3}), "fields[0]"},
		{"range rule on a string", field(map[string]any{"name": "a", "type": "string", "min": 1}), "fields[0]"},
		{"values on a string", field(map[string]any{"name": "a", "type": "string", "values": []string{"x"}}), "fields[0].values"},
		{"enum without values", field(map[string]any{"name": "a", "type": "enum"}), "fields[0].values"},
		{"enum with a repeated value", field(map[string]any{"name": "a", "type": "enum", "values": []string{"x", "x"}}), "fields[0].values"},
		{"enum with an empty value", field(map[string]any{"name": "a", "type": "enum", "values": []string{""}}), "fields[0].values"},
		{"min above max", field(map[string]any{"name": "a", "type": "number", "min": 2, "max": 1}), "fields[0].min"},
		{"min_length above max_length", field(map[string]any{"name": "a", "type": "string", "min_length": 5, "max_length": 2}), "fields[0].min_length"},
		{"negative min_length", field(map[string]any{"name": "a", "type": "string", "min_length": -1}), "fields[0].min_length"},
		{"zero max_length", field(map[string]any{"name": "a", "type": "string", "max_length": 0}), "fields[0].max_length"},
		{"broken pattern", field(map[string]any{"name": "a", "type": "string", "pattern": "("}), "fields[0].pattern"},
		{"repeated field", map[string]any{"name": "thing", "fields": []map[string]any{{"name": "a", "type": "string"}, {"name": "a", "type": "text"}}}, "fields[1].name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, h.admin("POST", "/v1/types", tc.spec), http.StatusUnprocessableEntity, "validation_failed", tc.field)
		})
	}

	t.Run("every problem is reported at once", func(t *testing.T) {
		wantError(t, h.admin("POST", "/v1/types", map[string]any{
			"name": "Bad Name", "fields": []map[string]any{{"name": "id", "type": "string"}, {"name": "ok", "type": "nope"}},
		}), http.StatusUnprocessableEntity, "validation_failed", "name", "fields[0].name", "fields[1].type")
	})

	t.Run("nothing was created", func(t *testing.T) {
		list := decode[struct {
			Items []content.Type `json:"items"`
		}](t, h.admin("GET", "/v1/types", nil))
		if len(list.Items) != 0 {
			t.Fatalf("types after only invalid requests: %+v", list.Items)
		}
	})
}

func TestEntries(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	h.createType(articleSpec)

	// A draft may be unfinished: the required title is missing.
	rec := h.admin("POST", "/v1/types/article/entries", map[string]any{"fields": map[string]any{"views": 5}})
	wantStatus(t, rec, http.StatusCreated)
	draft := decode[entryResponse](t, rec)
	if draft.Status != "draft" || draft.Version != 1 || draft.PublishedAt != nil || draft.PublishedFields != nil || draft.Type != "article" {
		t.Fatalf("new draft: %+v", draft)
	}
	if got := rec.Header().Get("Location"); got != "/v1/types/article/entries/"+draft.ID {
		t.Fatalf("Location: got %q", got)
	}
	path := "/v1/types/article/entries/" + draft.ID

	wantError(t, h.admin("POST", path+"/publish", nil), http.StatusUnprocessableEntity, "validation_failed", "title")

	full := map[string]any{
		"title": "Hello", "body": "First line\nSecond line", "views": 5, "rating": 4.5, "featured": true,
		"released": "2026-01-31T13:00:00+01:00", "status": "live", "meta": map[string]any{"tags": []any{"a", "b"}},
	}
	rec = h.admin("PUT", path, map[string]any{"fields": full})
	wantStatus(t, rec, http.StatusOK)
	updated := decode[entryResponse](t, rec)
	if updated.Status != "draft" || updated.Version != 2 || updated.UpdatedAt.Before(draft.UpdatedAt) {
		t.Fatalf("updated draft: %+v", updated)
	}
	if got := updated.Fields["released"]; got != "2026-01-31T12:00:00.000000Z" {
		t.Fatalf("a timestamp is stored in UTC at a fixed width: got %v", got)
	}

	rec = h.admin("POST", path+"/publish", nil)
	wantStatus(t, rec, http.StatusOK)
	published := decode[entryResponse](t, rec)
	if published.Status != "published" || published.PublishedAt == nil || !reflect.DeepEqual(published.PublishedFields, published.Fields) {
		t.Fatalf("published entry: %+v", published)
	}

	h.updateEntry("article", draft.ID, map[string]any{"title": "Hello again"})
	changed := decode[entryResponse](t, h.admin("GET", path, nil))
	if changed.Status != "changed" || changed.Fields["title"] != "Hello again" || changed.PublishedFields["title"] != "Hello" || changed.Version != 3 {
		t.Fatalf("entry with an unpublished edit: %+v", changed)
	}
	delivered := decode[publishedEntry](t, h.admin("GET", "/v1/content/article/"+draft.ID, nil))
	if delivered.Fields["title"] != "Hello" || delivered.Fields["views"] != float64(5) {
		t.Fatalf("readers see the edit before it is published: %+v", delivered)
	}

	rec = h.admin("POST", path+"/unpublish", nil)
	wantStatus(t, rec, http.StatusOK)
	unpublished := decode[entryResponse](t, rec)
	if unpublished.Status != "draft" || unpublished.PublishedAt != nil || unpublished.PublishedFields != nil || unpublished.Fields["title"] != "Hello again" {
		t.Fatalf("unpublished entry: %+v", unpublished)
	}
	wantStatus(t, h.admin("POST", path+"/unpublish", nil), http.StatusOK)
	wantError(t, h.admin("GET", "/v1/content/article/"+draft.ID, nil), http.StatusNotFound, "not_found")

	wantStatus(t, h.admin("DELETE", path, nil), http.StatusNoContent)
	wantError(t, h.admin("GET", path, nil), http.StatusNotFound, "not_found")
	wantError(t, h.admin("DELETE", path, nil), http.StatusNotFound, "not_found")
	wantError(t, h.admin("PUT", path, map[string]any{"fields": full}), http.StatusNotFound, "not_found")
	wantError(t, h.admin("POST", path+"/publish", nil), http.StatusNotFound, "not_found")
	wantError(t, h.admin("POST", "/v1/types/missing/entries", map[string]any{"fields": map[string]any{}}), http.StatusNotFound, "not_found")
	wantError(t, h.admin("GET", "/v1/types/missing/entries", nil), http.StatusNotFound, "not_found")
}

func TestEntryValidation(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	h.createType(map[string]any{
		"name": "thing",
		"fields": []map[string]any{
			{"name": "code", "type": "string", "min_length": 2, "max_length": 4, "pattern": "^[A-Z]+$"},
			{"name": "notes", "type": "text"},
			{"name": "count", "type": "integer", "min": 1, "max": 10},
			{"name": "ratio", "type": "number", "min": 0, "max": 1},
			{"name": "active", "type": "boolean"},
			{"name": "at", "type": "datetime"},
			{"name": "kind", "type": "enum", "values": []string{"a", "b"}},
			{"name": "extra", "type": "json"},
		},
	})

	bad := []struct {
		name   string
		fields string
		field  string
	}{
		{"unknown field", `{"nope": 1}`, "nope"},
		{"string given a number", `{"code": 12}`, "code"},
		{"string too short", `{"code": "A"}`, "code"},
		{"string too long", `{"code": "ABCDE"}`, "code"},
		{"string length counts characters, not bytes", `{"code": "ŻŻŻŻŻ"}`, "code"},
		{"string against its pattern", `{"code": "abc"}`, "code"},
		{"string with a line break", `{"code": "AB\nC"}`, "code"},
		{"string with a NUL", `{"notes": "a\u0000b"}`, "notes"},
		{"integer given a string", `{"count": "3"}`, "count"},
		{"integer given a fraction", `{"count": 1.5}`, "count"},
		{"integer given an exponent", `{"count": 1e1}`, "count"},
		{"integer below its minimum", `{"count": 0}`, "count"},
		{"integer above its maximum", `{"count": 11}`, "count"},
		{"integer beyond 64 bits", `{"count": 9223372036854775808}`, "count"},
		{"number given a string", `{"ratio": "0.5"}`, "ratio"},
		{"number above its maximum", `{"ratio": 1.01}`, "ratio"},
		{"number beyond a float", `{"ratio": 1e999}`, "ratio"},
		{"boolean given a string", `{"active": "true"}`, "active"},
		{"boolean given a number", `{"active": 1}`, "active"},
		{"datetime given a date", `{"at": "2026-01-31"}`, "at"},
		{"datetime given a number", `{"at": 1767225600}`, "at"},
		{"datetime that does not exist", `{"at": "2026-02-30T00:00:00Z"}`, "at"},
		{"enum given another value", `{"kind": "c"}`, "kind"},
		{"enum given a number", `{"kind": 1}`, "kind"},
		{"json with a NUL", `{"extra": {"deep": ["a\u0000"]}}`, "extra"},
		{"an array instead of an object", `[1, 2]`, "fields"},
		{"a string instead of an object", `"fields"`, "fields"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, h.admin("POST", "/v1/types/thing/entries", `{"fields": `+tc.fields+`}`), http.StatusUnprocessableEntity, "validation_failed", tc.field)
		})
	}

	t.Run("every problem is reported at once", func(t *testing.T) {
		wantError(t, h.admin("POST", "/v1/types/thing/entries", `{"fields": {"code": 1, "count": "x", "nope": true}}`),
			http.StatusUnprocessableEntity, "validation_failed", "code", "count", "nope")
	})

	t.Run("accepted values are stored in one canonical form", func(t *testing.T) {
		rec := h.admin("POST", "/v1/types/thing/entries", `{"fields": {
			"code": "ABCD", "notes": "<b>&amp;</b>\n\ttab", "count": 10, "ratio": 1e-1, "active": false,
			"at": "2026-06-30T23:59:59.123456789-05:00", "kind": "b", "notes": "last one wins",
			"extra": {"n": 1.50, "big": 12345678901234567890, "list": [null, {"x": []}]}
		}}`)
		wantStatus(t, rec, http.StatusCreated)
		got := decode[entryResponse](t, rec).Fields
		want := map[string]any{
			"code": "ABCD", "notes": "last one wins", "count": float64(10), "ratio": 0.1, "active": false,
			"at": "2026-07-01T04:59:59.123456Z", "kind": "b",
			"extra": map[string]any{"n": 1.5, "big": float64(12345678901234567890), "list": []any{nil, map[string]any{"x": []any{}}}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stored fields:\n got %v\nwant %v", got, want)
		}
		if !strings.Contains(rec.Body.String(), `"big":12345678901234567890`) {
			t.Fatalf("a number inside a json field lost digits: %s", rec.Body)
		}
	})

	t.Run("null removes a value", func(t *testing.T) {
		id := h.createEntry("thing", map[string]any{"code": "AB", "count": 3})
		h.updateEntry("thing", id, map[string]any{"code": nil, "count": 4, "extra": nil})
		got := decode[entryResponse](t, h.admin("GET", "/v1/types/thing/entries/"+id, nil)).Fields
		if want := map[string]any{"count": float64(4)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("fields after setting null: got %v, want %v", got, want)
		}
	})

	t.Run("html is not escaped on the way out", func(t *testing.T) {
		id := h.createEntry("thing", map[string]any{"notes": "<p>Tom & Jerry</p>"})
		body := h.admin("GET", "/v1/types/thing/entries/"+id, nil).Body.String()
		if !strings.Contains(body, `<p>Tom & Jerry</p>`) {
			t.Fatalf("content was escaped: %s", body)
		}
	})
}

func seedArticles(h *harness) map[string]string {
	h.t.Helper()
	h.createType(articleSpec)
	ids := map[string]string{}
	for _, a := range []map[string]any{
		{"title": "Alpha", "views": 10, "rating": 4.5, "featured": true, "status": "live", "released": "2026-01-01T00:00:00Z"},
		{"title": "beta", "views": 0, "rating": 0.5, "featured": false, "status": "live", "released": "2026-02-01T00:00:00Z"},
		{"title": "Gamma", "views": 25, "rating": 3, "featured": true, "status": "archived"},
		{"title": "delta", "views": 10, "meta": map[string]any{"k": "v"}},
	} {
		ids[a["title"].(string)] = h.published("article", a)
	}
	ids["draft"] = h.createEntry("article", map[string]any{"title": "Unpublished", "views": 10})
	return ids
}

func TestDeliveryQueries(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	ids := seedArticles(h)

	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{"delta", "Gamma", "beta", "Alpha"}},
		{"?filter[title]=Alpha", []string{"Alpha"}},
		{"?filter[title][eq]=Alpha", []string{"Alpha"}},
		{"?filter[title][ne]=Alpha", []string{"delta", "Gamma", "beta"}},
		{"?filter[title][in]=Alpha,beta", []string{"beta", "Alpha"}},
		{"?filter[title][contains]=a", []string{"delta", "Gamma", "beta", "Alpha"}},
		{"?filter[title][contains]=mm", []string{"Gamma"}},
		{"?filter[views][gte]=10", []string{"delta", "Gamma", "Alpha"}},
		{"?filter[views][gt]=10", []string{"Gamma"}},
		{"?filter[views][lt]=10", []string{"beta"}},
		{"?filter[views][lte]=10&filter[views][gt]=0", []string{"delta", "Alpha"}},
		{"?filter[views]=10&filter[featured]=true", []string{"Alpha"}},
		{"?filter[rating][gte]=3", []string{"Gamma", "Alpha"}},
		{"?filter[featured]=false", []string{"beta"}},
		{"?filter[featured][ne]=true", []string{"delta", "beta"}},
		{"?filter[status][in]=live,archived", []string{"Gamma", "beta", "Alpha"}},
		{"?filter[released][gte]=2026-01-15T00:00:00Z", []string{"beta"}},
		{"?filter[released][lt]=2026-01-15T01:00:00%2B01:00", []string{"Alpha"}},
		{"?filter[id]=" + ids["Gamma"], []string{"Gamma"}},
		{"?filter[id]=" + ids["draft"], []string{}},
		{"?filter[published_at][gt]=2000-01-01T00:00:00Z", []string{"delta", "Gamma", "beta", "Alpha"}},
		{"?filter[created_at][lt]=2000-01-01T00:00:00Z", []string{}},
		{"?sort=title", []string{"Alpha", "Gamma", "beta", "delta"}},
		{"?sort=-title", []string{"delta", "beta", "Gamma", "Alpha"}},
		{"?sort=-views,title", []string{"Gamma", "Alpha", "delta", "beta"}},
		{"?sort=rating", []string{"beta", "Gamma", "Alpha", "delta"}},
		{"?sort=-rating", []string{"Alpha", "Gamma", "beta", "delta"}},
		{"?sort=published_at", []string{"Alpha", "beta", "Gamma", "delta"}},
		{"?sort=title&limit=2", []string{"Alpha", "Gamma"}},
		{"?sort=title&limit=2&offset=2", []string{"beta", "delta"}},
		{"?sort=title&limit=2&offset=4", []string{}},
		{"?sort=%20title%20,%20", []string{"Alpha", "Gamma", "beta", "delta"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := h.titles(tc.query)
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a page says where it is", func(t *testing.T) {
		page := decode[publishedPage](t, h.admin("GET", "/v1/content/article?sort=title&limit=3&offset=1", nil))
		if page.Total != 4 || page.Limit != 3 || page.Offset != 1 || len(page.Items) != 3 {
			t.Fatalf("page: total %d, limit %d, offset %d, %d items", page.Total, page.Limit, page.Offset, len(page.Items))
		}
		page = decode[publishedPage](t, h.admin("GET", "/v1/content/article", nil))
		if page.Limit != content.DefaultLimit || page.Offset != 0 || page.Total != 4 {
			t.Fatalf("default page: %+v", page)
		}
	})

	t.Run("an entry has the published shape", func(t *testing.T) {
		rec := h.admin("GET", "/v1/content/article/"+ids["delta"], nil)
		wantStatus(t, rec, http.StatusOK)
		entry := decode[publishedEntry](t, rec)
		want := map[string]any{"title": "delta", "views": float64(10), "meta": map[string]any{"k": "v"}}
		if entry.ID != ids["delta"] || entry.Type != "article" || entry.PublishedAt.IsZero() || !reflect.DeepEqual(entry.Fields, want) {
			t.Fatalf("entry: %+v", entry)
		}
	})

	t.Run("not found", func(t *testing.T) {
		wantError(t, h.admin("GET", "/v1/content/article/"+ids["draft"], nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/article/nope", nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/missing", nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/missing/nope", nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/Not%20A%20Name", nil), http.StatusNotFound, "not_found")
	})

	bad := []struct {
		query string
		param string
	}{
		{"?page=2", "page"},
		{"?filter=x", "filter"},
		{"?filter[title", "filter[title"},
		{"?filter[nope]=1", "filter[nope]"},
		{"?filter[meta]=1", "filter[meta]"},
		{"?filter[title][like]=a", "filter[title]"},
		{"?filter[featured][gt]=true", "filter[featured]"},
		{"?filter[views][contains]=1", "filter[views]"},
		{"?filter[views]=ten", "filter[views]"},
		{"?filter[views][in]=1,x", "filter[views]"},
		{"?filter[rating]=NaN", "filter[rating]"},
		{"?filter[featured]=yes", "filter[featured]"},
		{"?filter[released]=yesterday", "filter[released]"},
		{"?filter[released][contains]=2026", "filter[released]"},
		{"?filter[published_at]=today", "filter[published_at]"},
		{"?filter[updated_at][gt]=2000-01-01T00:00:00Z", "filter[updated_at]"},
		{"?sort=nope", "sort"},
		{"?sort=meta", "sort"},
		{"?sort=updated_at", "sort"},
		{"?sort=title,title", "sort"},
		{"?sort=", "sort"},
		{"?sort=title&sort=views", "sort"},
		{"?sort=title,views,rating,status", "sort"},
		{"?limit=0", "limit"},
		{"?limit=101", "limit"},
		{"?limit=ten", "limit"},
		{"?limit=1&limit=2", "limit"},
		{"?offset=-1", "offset"},
		{"?offset=1.5", "offset"},
	}
	for _, tc := range bad {
		t.Run("refuses "+tc.query, func(t *testing.T) {
			wantError(t, h.admin("GET", "/v1/content/article"+tc.query, nil), http.StatusBadRequest, "bad_request", tc.param)
		})
	}

	t.Run("refuses too many filters", func(t *testing.T) {
		query := "?filter[views][in]=" + strings.Repeat("1,", content.MaxInValues) + "1"
		wantError(t, h.admin("GET", "/v1/content/article"+query, nil), http.StatusBadRequest, "bad_request", "filter[views]")
		query = "?"
		for i := range content.MaxFilters + 1 {
			query += "filter[views][ne]=" + strings.Repeat("1", i+1) + "&"
		}
		wantError(t, h.admin("GET", "/v1/content/article"+query, nil), http.StatusBadRequest, "bad_request", "filter")
	})

	t.Run("an entry takes no query parameters", func(t *testing.T) {
		wantError(t, h.admin("GET", "/v1/content/article/"+ids["Alpha"]+"?fields=title", nil), http.StatusBadRequest, "bad_request")
	})
}

func TestManagementQueries(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	ids := seedArticles(h)
	h.updateEntry("article", ids["Alpha"], map[string]any{"title": "Alpha edited", "views": 11})

	titles := func(query string) []string {
		t.Helper()
		rec := h.admin("GET", "/v1/types/article/entries"+query, nil)
		wantStatus(t, rec, http.StatusOK)
		page := decode[entryPage](t, rec)
		out := make([]string, len(page.Items))
		for i, item := range page.Items {
			out[i], _ = item.Fields["title"].(string)
		}
		return out
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{"Unpublished", "delta", "Gamma", "beta", "Alpha edited"}},
		{"?filter[title]=Alpha", []string{}},
		{"?filter[title]=Alpha%20edited", []string{"Alpha edited"}},
		{"?filter[views]=10&sort=title", []string{"Unpublished", "delta"}},
		{"?sort=-updated_at&limit=1", []string{"Alpha edited"}},
		{"?sort=published_at&limit=2", []string{"Alpha edited", "beta"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got := titles(tc.query)
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("statuses", func(t *testing.T) {
		page := decode[entryPage](t, h.admin("GET", "/v1/types/article/entries?sort=title", nil))
		got := map[string]string{}
		for _, item := range page.Items {
			got[item.Fields["title"].(string)] = item.Status
		}
		want := map[string]string{"Alpha edited": "changed", "Gamma": "published", "Unpublished": "draft", "beta": "published", "delta": "published"}
		if !reflect.DeepEqual(got, want) || page.Total != 5 {
			t.Fatalf("statuses: got %v (total %d), want %v", got, page.Total, want)
		}
	})

	wantError(t, h.admin("GET", "/v1/types/article/entries?filter[nope]=1", nil), http.StatusBadRequest, "bad_request", "filter[nope]")
	wantError(t, h.admin("GET", "/v1/types/article/entries?page=1", nil), http.StatusBadRequest, "bad_request", "page")
}

func TestConditionalRequests(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	h.createType(articleSpec)
	id := h.published("article", map[string]any{"title": "One"})

	for _, path := range []string{"/v1/content/article", "/v1/content/article/" + id} {
		t.Run(path, func(t *testing.T) {
			first := h.admin("GET", path, nil)
			wantStatus(t, first, http.StatusOK)
			tag := first.Header().Get("ETag")
			if len(tag) < 10 || !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
				t.Fatalf("ETag: got %q", tag)
			}
			if got := first.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("Cache-Control: got %q", got)
			}
			if got := first.Header().Get("Vary"); got != "Authorization" {
				t.Fatalf("Vary: got %q", got)
			}
			if got := first.Header().Get("Content-Length"); got == "" || got == "0" {
				t.Fatalf("Content-Length: got %q", got)
			}

			again := h.admin("GET", path, nil)
			if again.Header().Get("ETag") != tag || again.Body.String() != first.Body.String() {
				t.Fatal("the same content got another ETag or body")
			}

			for _, header := range []string{tag, "W/" + tag, `"other", ` + tag, `"other",W/` + tag + ` , "more"`, "*"} {
				rec := h.do("GET", path, rootToken, nil, "If-None-Match", header)
				wantStatus(t, rec, http.StatusNotModified)
				if rec.Body.Len() != 0 {
					t.Fatalf("a 304 has a body: %s", rec.Body)
				}
				if rec.Header().Get("ETag") != tag || rec.Header().Get("Cache-Control") != "no-cache" {
					t.Fatalf("a 304 lacks its validators: %v", rec.Header())
				}
			}
			for _, header := range []string{`"other"`, strings.Trim(tag, `"`), `W/"other", "more"`} {
				wantStatus(t, h.do("GET", path, rootToken, nil, "If-None-Match", header), http.StatusOK)
			}
		})
	}

	t.Run("the ETag follows the content", func(t *testing.T) {
		path := "/v1/content/article/" + id
		before := h.admin("GET", path, nil).Header().Get("ETag")

		h.updateEntry("article", id, map[string]any{"title": "Two"})
		if got := h.admin("GET", path, nil).Header().Get("ETag"); got != before {
			t.Fatal("an unpublished edit changed the ETag")
		}

		h.publish("article", id)
		rec := h.do("GET", path, rootToken, nil, "If-None-Match", before)
		wantStatus(t, rec, http.StatusOK)
		if rec.Header().Get("ETag") == before {
			t.Fatal("new content kept the old ETag")
		}
	})

	t.Run("HEAD answers without a body", func(t *testing.T) {
		rec := h.admin("HEAD", "/v1/content/article/"+id, nil)
		wantStatus(t, rec, http.StatusOK)
		if rec.Header().Get("ETag") == "" {
			t.Fatal("HEAD has no ETag")
		}
	})

	t.Run("Cache-Control can be configured", func(t *testing.T) {
		h := newHarness(t, sqliteStore(t), func(o *httpapi.Options) { o.DeliveryCacheControl = "public, max-age=60" })
		h.createType(articleSpec)
		if got := h.admin("GET", "/v1/content/article", nil).Header().Get("Cache-Control"); got != "public, max-age=60" {
			t.Fatalf("Cache-Control: got %q", got)
		}
	})

	t.Run("everything else is not to be stored", func(t *testing.T) {
		secret := h.admin("POST", "/v1/tokens", map[string]any{"name": "x", "scopes": []string{"read"}})
		wantStatus(t, secret, http.StatusCreated)
		responses := map[string]string{
			"a token secret":        secret.Header().Get("Cache-Control"),
			"the management API":    h.admin("GET", "/v1/types/article/entries", nil).Header().Get("Cache-Control"),
			"an error":              h.admin("GET", "/v1/types/missing", nil).Header().Get("Cache-Control"),
			"a delivery error":      h.admin("GET", "/v1/content/article/missing", nil).Header().Get("Cache-Control"),
			"a refused request":     h.do("GET", "/v1/content/article", "", nil).Header().Get("Cache-Control"),
			"an unknown route":      h.admin("GET", "/nope", nil).Header().Get("Cache-Control"),
			"a probe":               h.do("GET", "/readyz", "", nil).Header().Get("Cache-Control"),
			"the API description":   h.do("GET", "/openapi.json", "", nil).Header().Get("Cache-Control"),
			"a wrong method":        h.admin("PATCH", "/v1/types", nil).Header().Get("Cache-Control"),
			"a bad delivery query":  h.admin("GET", "/v1/content/article?nope=1", nil).Header().Get("Cache-Control"),
			"a missing type's list": h.admin("GET", "/v1/content/missing", nil).Header().Get("Cache-Control"),
		}
		for what, got := range responses {
			if got != "no-store" {
				t.Errorf("Cache-Control of %s: got %q, want no-store", what, got)
			}
		}
	})
}

func TestRequestRules(t *testing.T) {
	h := newHarness(t, sqliteStore(t), func(o *httpapi.Options) { o.MaxBodyBytes = 512 })
	h.createType(map[string]any{"name": "note", "fields": []map[string]any{{"name": "text", "type": "text"}}})
	const entries = "/v1/types/note/entries"

	t.Run("body too large", func(t *testing.T) {
		wantStatus(t, h.admin("POST", entries, `{"fields": {"text": "`+strings.Repeat("a", 400)+`"}}`), http.StatusCreated)
		wantError(t, h.admin("POST", entries, `{"fields": {"text": "`+strings.Repeat("a", 600)+`"}}`), http.StatusRequestEntityTooLarge, "payload_too_large")
	})

	t.Run("content type", func(t *testing.T) {
		const body = `{"fields": {"text": "x"}}`
		wantStatus(t, h.do("POST", entries, rootToken, body, "Content-Type", "application/json; charset=utf-8"), http.StatusCreated)
		wantError(t, h.do("POST", entries, rootToken, body, "Content-Type", "text/plain"), http.StatusUnsupportedMediaType, "unsupported_media_type")
		wantError(t, h.do("POST", entries, rootToken, body, "Content-Type", "application/x-www-form-urlencoded"), http.StatusUnsupportedMediaType, "unsupported_media_type")
		wantError(t, h.do("POST", entries, rootToken, body, "Content-Type", ""), http.StatusUnsupportedMediaType, "unsupported_media_type")
	})

	t.Run("malformed bodies", func(t *testing.T) {
		for name, body := range map[string]string{
			"not json":         `fields=text`,
			"cut short":        `{"fields": {"text": "x"`,
			"unknown property": `{"fields": {"text": "x"}, "publish": true}`,
			"two values":       `{"fields": {"text": "x"}} {"fields": {}}`,
			"wrong shape":      `{"fields": "text"}`,
			"no fields":        `{}`,
			"null":             `null`,
			"empty":            ``,
		} {
			rec := h.admin("POST", entries, body)
			if name == "wrong shape" {
				wantError(t, rec, http.StatusUnprocessableEntity, "validation_failed", "fields")
				continue
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400; body: %s", name, rec.Code, rec.Body)
				continue
			}
			wantError(t, rec, http.StatusBadRequest, "bad_request")
		}
		wantError(t, h.admin("POST", "/v1/types", `{"name": "x", "fields": [{"name": "a", "type": "string", "unique": true}]}`), http.StatusBadRequest, "bad_request")
	})

	t.Run("unknown routes", func(t *testing.T) {
		for _, path := range []string{"/", "/v1", "/v1/nope", "/v2/types", "/v1/types/note/entries/x/publish/now", "/healthz/x"} {
			wantError(t, h.admin("GET", path, nil), http.StatusNotFound, "not_found")
		}
	})

	t.Run("wrong method", func(t *testing.T) {
		cases := map[string][]string{
			"PATCH /v1/types/note":                 {"DELETE", "GET", "HEAD", "PUT"},
			"DELETE /v1/types":                     {"GET", "HEAD", "POST"},
			"POST /v1/content/note":                {"GET", "HEAD"},
			"GET /v1/types/note/entries/x/publish": {"POST"},
			"POST /healthz":                        {"GET", "HEAD"},
		}
		for request, allow := range cases {
			method, path, _ := strings.Cut(request, " ")
			rec := h.admin(method, path, nil)
			wantError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed")
			got := strings.Split(rec.Header().Get("Allow"), ", ")
			sort.Strings(got)
			if !reflect.DeepEqual(got, allow) {
				t.Errorf("%s: Allow: got %v, want %v", request, got, allow)
			}
		}
	})

	t.Run("every response says it is not to be sniffed", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/v1/types", "/nope", "/v1/content/note"} {
			if got := h.admin("GET", path, nil).Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("%s: X-Content-Type-Options: got %q", path, got)
			}
		}
	})
}

func TestProbes(t *testing.T) {
	s := sqliteStore(t)
	h := newHarness(t, s)

	rec := h.do("GET", "/healthz", "", nil)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[map[string]string](t, rec); got["status"] != "ok" || got["version"] != "test" {
		t.Fatalf("healthz: %v", got)
	}
	wantStatus(t, h.do("GET", "/readyz", "", nil), http.StatusOK)

	h.api.Drain()
	rec = h.do("GET", "/readyz", "", nil)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if got := decode[map[string]string](t, rec); got["status"] != "draining" {
		t.Fatalf("readyz while draining: %v", got)
	}
	// Draining only tells the load balancer to look elsewhere. Requests that
	// still arrive are served.
	wantStatus(t, h.do("GET", "/healthz", "", nil), http.StatusOK)
	wantStatus(t, h.admin("GET", "/v1/types", nil), http.StatusOK)

	t.Run("without a database", func(t *testing.T) {
		broken := sqliteStore(t)
		h := newHarness(t, broken)
		broken.(interface{ Close() error }).Close()
		wantStatus(t, h.do("GET", "/readyz", "", nil), http.StatusServiceUnavailable)
		wantStatus(t, h.do("GET", "/healthz", "", nil), http.StatusOK)
		wantError(t, h.admin("GET", "/v1/types", nil), http.StatusInternalServerError, "internal")
	})
}

// hookStore is a store that tests can make slow or broken.
type hookStore struct {
	content.Store
	types func(ctx context.Context) ([]content.Type, error)
}

func (s *hookStore) Types(ctx context.Context) ([]content.Type, error) {
	if s.types != nil {
		return s.types(ctx)
	}
	return s.Store.Types(ctx)
}

func TestTimeout(t *testing.T) {
	s := &hookStore{Store: sqliteStore(t)}
	s.types = func(ctx context.Context) ([]content.Type, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h := newHarness(t, s, func(o *httpapi.Options) { o.RequestTimeout = 30 * time.Millisecond })

	start := time.Now()
	wantError(t, h.admin("GET", "/v1/types", nil), http.StatusGatewayTimeout, "timeout")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the request was only given up after %s", elapsed)
	}
}

func TestPanicIsContained(t *testing.T) {
	s := &hookStore{Store: sqliteStore(t)}
	s.types = func(context.Context) ([]content.Type, error) { panic("boom") }
	h := newHarness(t, s)

	rec := h.admin("GET", "/v1/types", nil)
	body := wantError(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(body.Error.Message, "boom") {
		t.Fatalf("the panic value reached the client: %s", rec.Body)
	}
	wantStatus(t, h.do("GET", "/healthz", "", nil), http.StatusOK)
}

func TestInternalErrorsAreNotExposed(t *testing.T) {
	s := &hookStore{Store: sqliteStore(t)}
	s.types = func(context.Context) ([]content.Type, error) {
		return nil, errors.New("dial tcp 10.1.2.3:5432: password authentication failed for user tapirpost")
	}
	h := newHarness(t, s)
	rec := h.admin("GET", "/v1/types", nil)
	wantError(t, rec, http.StatusInternalServerError, "internal")
	if strings.Contains(rec.Body.String(), "10.1.2.3") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("an internal error reached the client: %s", rec.Body)
	}
}

func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	h := newHarness(t, sqliteStore(t))
	rec := h.do("GET", "/openapi.json", "", nil)
	wantStatus(t, rec, http.StatusOK)

	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Fatalf("openapi version: got %q", doc.OpenAPI)
	}

	var described []string
	operations := map[string]bool{}
	for path, methods := range doc.Paths {
		for method, op := range methods {
			described = append(described, strings.ToUpper(method)+" "+path)
			id, _ := op["operationId"].(string)
			if id == "" || operations[id] {
				t.Errorf("%s %s: operationId %q is empty or used twice", method, path, id)
			}
			operations[id] = true
			if responses, _ := op["responses"].(map[string]any); len(responses) == 0 {
				t.Errorf("%s %s: no responses", method, path)
			}
		}
	}
	routes := h.api.Routes()
	sort.Strings(described)
	sort.Strings(routes)
	if !reflect.DeepEqual(described, routes) {
		t.Fatalf("the description and the routes differ:\ndescribed %v\n   routes %v", described, routes)
	}

	// Every $ref has to point at something that exists.
	var tree any
	if err := json.Unmarshal(rec.Body.Bytes(), &tree); err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				node := tree
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					next, ok := node.(map[string]any)[part]
					if !ok {
						t.Errorf("dangling reference %s", ref)
						break
					}
					node = next
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(tree)
}
