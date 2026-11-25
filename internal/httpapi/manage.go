package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// decode reads a JSON request body into dst and refuses anything it does not
// understand, so that a typo in a field name is an error and not a no-op.
func (a *API) decode(w http.ResponseWriter, r *http.Request, dst any) error {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return &apiError{status: http.StatusUnsupportedMediaType, code: "unsupported_media_type", message: "send the body as Content-Type: application/json"}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, a.opts.MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return badRequest("the body is not the expected JSON: " + err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequest("the body must hold a single JSON value")
	}
	return nil
}

func (a *API) token(r *http.Request) (content.Token, bool) {
	info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo)
	if !ok || !info.authed {
		return content.Token{}, false
	}
	return info.token, true
}

// Content types.

type listJSON[T any] struct {
	Items []T `json:"items"`
}

func (a *API) listTypes(w http.ResponseWriter, r *http.Request) error {
	types, err := a.svc.Types(r.Context())
	if err != nil {
		return err
	}
	visible := make([]content.Type, 0, len(types))
	token, _ := a.token(r)
	for _, t := range types {
		if token.Allows(content.ScopeRead, t.Name) {
			visible = append(visible, t)
		}
	}
	return writeJSON(w, http.StatusOK, listJSON[content.Type]{Items: visible})
}

func (a *API) createType(w http.ResponseWriter, r *http.Request) error {
	var spec content.TypeSpec
	if err := a.decode(w, r, &spec); err != nil {
		return err
	}
	t, err := a.svc.CreateType(r.Context(), spec)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/types/"+t.Name)
	return writeJSON(w, http.StatusCreated, t)
}

func (a *API) getType(w http.ResponseWriter, r *http.Request) error {
	t, err := a.svc.Type(r.Context(), r.PathValue("type"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) updateType(w http.ResponseWriter, r *http.Request) error {
	var spec content.TypeSpec
	if err := a.decode(w, r, &spec); err != nil {
		return err
	}
	t, err := a.svc.UpdateType(r.Context(), r.PathValue("type"), spec)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, t)
}

func (a *API) deleteType(w http.ResponseWriter, r *http.Request) error {
	if err := a.svc.DeleteType(r.Context(), r.PathValue("type")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// Entries.

type entryJSON struct {
	ID              string          `json:"id"`
	Type            string          `json:"type"`
	Status          content.Status  `json:"status"`
	Version         int64           `json:"version"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	PublishedAt     *time.Time      `json:"published_at"`
	Fields          json.RawMessage `json:"fields"`
	PublishedFields json.RawMessage `json:"published_fields"`
}

func toEntryJSON(e content.Entry) entryJSON {
	out := entryJSON{
		ID:              e.ID,
		Type:            e.Type,
		Status:          e.Status(),
		Version:         e.Version,
		CreatedAt:       e.CreatedAt,
		UpdatedAt:       e.UpdatedAt,
		Fields:          e.Data,
		PublishedFields: e.Published,
	}
	if e.Published != nil {
		out.PublishedAt = &e.PublishedAt
	} else {
		out.PublishedFields = json.RawMessage("null")
	}
	return out
}

type entryRequest struct {
	Fields json.RawMessage `json:"fields"`
}

func (a *API) decodeEntry(w http.ResponseWriter, r *http.Request) (json.RawMessage, error) {
	var req entryRequest
	if err := a.decode(w, r, &req); err != nil {
		return nil, err
	}
	if req.Fields == nil {
		return nil, badRequest(`the body must look like {"fields": {...}}`)
	}
	return req.Fields, nil
}

type pageJSON[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

func (a *API) listEntries(w http.ResponseWriter, r *http.Request) error {
	t, err := a.svc.Type(r.Context(), r.PathValue("type"))
	if err != nil {
		return err
	}
	params, err := parseListParams(r.URL.Query())
	if err != nil {
		return err
	}
	page, err := a.svc.ListEntries(r.Context(), t, params)
	if err != nil {
		return queryError(err)
	}
	out := pageJSON[entryJSON]{Items: make([]entryJSON, len(page.Entries)), Total: page.Total, Limit: page.Limit, Offset: page.Offset}
	for i, e := range page.Entries {
		out.Items[i] = toEntryJSON(e)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *API) createEntry(w http.ResponseWriter, r *http.Request) error {
	fields, err := a.decodeEntry(w, r)
	if err != nil {
		return err
	}
	e, err := a.svc.CreateEntry(r.Context(), r.PathValue("type"), fields)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/types/"+e.Type+"/entries/"+e.ID)
	return writeJSON(w, http.StatusCreated, toEntryJSON(e))
}

func (a *API) getEntry(w http.ResponseWriter, r *http.Request) error {
	e, err := a.svc.Entry(r.Context(), r.PathValue("type"), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toEntryJSON(e))
}

func (a *API) updateEntry(w http.ResponseWriter, r *http.Request) error {
	fields, err := a.decodeEntry(w, r)
	if err != nil {
		return err
	}
	e, err := a.svc.UpdateEntry(r.Context(), r.PathValue("type"), r.PathValue("id"), fields)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toEntryJSON(e))
}

func (a *API) deleteEntry(w http.ResponseWriter, r *http.Request) error {
	if err := a.svc.DeleteEntry(r.Context(), r.PathValue("type"), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *API) publishEntry(w http.ResponseWriter, r *http.Request) error {
	e, err := a.svc.PublishEntry(r.Context(), r.PathValue("type"), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toEntryJSON(e))
}

func (a *API) unpublishEntry(w http.ResponseWriter, r *http.Request) error {
	e, err := a.svc.UnpublishEntry(r.Context(), r.PathValue("type"), r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, toEntryJSON(e))
}

// Tokens.

type createdTokenJSON struct {
	content.Token
	// Secret is shown this once. Only its hash is stored.
	Secret string `json:"secret"`
}

func (a *API) listTokens(w http.ResponseWriter, r *http.Request) error {
	tokens, err := a.svc.Tokens(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, listJSON[content.Token]{Items: tokens})
}

func (a *API) createToken(w http.ResponseWriter, r *http.Request) error {
	var spec content.TokenSpec
	if err := a.decode(w, r, &spec); err != nil {
		return err
	}
	token, secret, err := a.svc.CreateToken(r.Context(), spec)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/tokens/"+token.ID)
	return writeJSON(w, http.StatusCreated, createdTokenJSON{Token: token, Secret: secret})
}

func (a *API) deleteToken(w http.ResponseWriter, r *http.Request) error {
	if err := a.svc.DeleteToken(r.Context(), r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
