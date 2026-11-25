package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/httpapi"
	"github.com/zieludev/tapirpost/internal/store/sqlite"
)

const rootToken = "root-token-for-the-tests-0123456789"

// harness is an API on top of a real store, driven without a network.
type harness struct {
	t     testing.TB
	api   *httpapi.API
	svc   *content.Service
	store content.Store
}

func sqliteStore(t testing.TB) content.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "tapirpost.db"), 4)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newHarness(t testing.TB, s content.Store, configure ...func(*httpapi.Options)) *harness {
	t.Helper()
	svc, err := content.NewService(s, rootToken)
	if err != nil {
		t.Fatal(err)
	}
	opts := httpapi.Options{Service: svc, Version: "test"}
	for _, fn := range configure {
		fn(&opts)
	}
	return &harness{t: t, api: httpapi.New(opts), svc: svc, store: s}
}

// do sends one request. A string body is sent as it is, anything else as
// JSON.
func (h *harness) do(method, target, token string, body any, headers ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *bytes.Reader
	switch b := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequestWithContext(h.t.Context(), method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.api.ServeHTTP(rec, req)
	return rec
}

func (h *harness) admin(method, target string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.do(method, target, rootToken, body)
}

func wantStatus(t testing.TB, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status: got %d, want %d; body: %s", rec.Code, want, rec.Body)
	}
}

type errorResponse struct {
	Error struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details []content.Issue `json:"details"`
	} `json:"error"`
}

// wantError checks the status, the error code and that the error names every
// one of the given fields.
func wantError(t testing.TB, rec *httptest.ResponseRecorder, status int, code string, fields ...string) errorResponse {
	t.Helper()
	wantStatus(t, rec, status)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type of an error: got %q", ct)
	}
	body := decode[errorResponse](t, rec)
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("error: got code %q and message %q, want code %q; body: %s", body.Error.Code, body.Error.Message, code, rec.Body)
	}
	for _, field := range fields {
		found := false
		for _, issue := range body.Error.Details {
			found = found || issue.Field == field
		}
		if !found {
			t.Fatalf("error details do not mention %q: %s", field, rec.Body)
		}
	}
	return body
}

func decode[T any](t testing.TB, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %T: %v; body: %s", v, err, rec.Body)
	}
	return v
}

type entryResponse struct {
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	Status          string         `json:"status"`
	Version         int64          `json:"version"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
	PublishedAt     *time.Time     `json:"published_at"`
	Fields          map[string]any `json:"fields"`
	PublishedFields map[string]any `json:"published_fields"`
}

type entryPage struct {
	Items  []entryResponse `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

type publishedEntry struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	PublishedAt time.Time      `json:"published_at"`
	Fields      map[string]any `json:"fields"`
}

type publishedPage struct {
	Items  []publishedEntry `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

type tokenResponse struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Scopes    []string   `json:"scopes"`
	Types     []string   `json:"types"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	Secret    string     `json:"secret"`
}

var articleSpec = map[string]any{
	"name":        "article",
	"description": "News and notes",
	"fields": []map[string]any{
		{"name": "title", "type": "string", "required": true, "max_length": 80},
		{"name": "body", "type": "text"},
		{"name": "views", "type": "integer", "min": 0},
		{"name": "rating", "type": "number"},
		{"name": "featured", "type": "boolean"},
		{"name": "released", "type": "datetime"},
		{"name": "status", "type": "enum", "values": []string{"live", "archived"}},
		{"name": "meta", "type": "json"},
	},
}

func (h *harness) createType(spec any) {
	h.t.Helper()
	wantStatus(h.t, h.admin("POST", "/v1/types", spec), http.StatusCreated)
}

func (h *harness) createEntry(typeName string, fields map[string]any) string {
	h.t.Helper()
	rec := h.admin("POST", "/v1/types/"+typeName+"/entries", map[string]any{"fields": fields})
	wantStatus(h.t, rec, http.StatusCreated)
	return decode[entryResponse](h.t, rec).ID
}

func (h *harness) updateEntry(typeName, id string, fields map[string]any) {
	h.t.Helper()
	wantStatus(h.t, h.admin("PUT", "/v1/types/"+typeName+"/entries/"+id, map[string]any{"fields": fields}), http.StatusOK)
}

func (h *harness) publish(typeName, id string) {
	h.t.Helper()
	wantStatus(h.t, h.admin("POST", "/v1/types/"+typeName+"/entries/"+id+"/publish", nil), http.StatusOK)
}

func (h *harness) unpublish(typeName, id string) {
	h.t.Helper()
	wantStatus(h.t, h.admin("POST", "/v1/types/"+typeName+"/entries/"+id+"/unpublish", nil), http.StatusOK)
}

// published creates an entry and publishes it.
func (h *harness) published(typeName string, fields map[string]any) string {
	h.t.Helper()
	id := h.createEntry(typeName, fields)
	h.publish(typeName, id)
	return id
}

// newToken creates a token through the API and returns its secret.
func (h *harness) newToken(scopes []string, types ...string) string {
	h.t.Helper()
	rec := h.admin("POST", "/v1/tokens", map[string]any{"name": strings.Join(scopes, "+"), "scopes": scopes, "types": types})
	wantStatus(h.t, rec, http.StatusCreated)
	return decode[tokenResponse](h.t, rec).Secret
}

// titles fetches a delivery listing and returns the titles in order.
func (h *harness) titles(query string) []string {
	h.t.Helper()
	rec := h.admin("GET", "/v1/content/article"+query, nil)
	wantStatus(h.t, rec, http.StatusOK)
	page := decode[publishedPage](h.t, rec)
	titles := make([]string, len(page.Items))
	for i, item := range page.Items {
		titles[i], _ = item.Fields["title"].(string)
	}
	return titles
}
