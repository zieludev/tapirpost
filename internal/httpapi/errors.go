package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/zieludev/tapirpost/internal/content"
)

// apiError is an error that already knows how it looks on the wire.
type apiError struct {
	status  int
	code    string
	message string
	details []content.Issue
}

func (e *apiError) Error() string { return e.message }

var (
	errNoToken   = &apiError{status: http.StatusUnauthorized, code: "unauthorized", message: "send a token as: Authorization: Bearer <token>"}
	errBadToken  = &apiError{status: http.StatusUnauthorized, code: "unauthorized", message: "the token is not valid"}
	errForbidden = &apiError{status: http.StatusForbidden, code: "forbidden", message: "the token does not allow this"}
	errInternal  = &apiError{status: http.StatusInternalServerError, code: "internal", message: "something went wrong on the server"}
)

func badRequest(message string, details ...content.Issue) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "bad_request", message: message, details: details}
}

// classify is the one place where errors from the rest of the program are
// given an HTTP status.
func classify(err error) *apiError {
	var (
		api        *apiError
		validation *content.ValidationError
		tooLarge   *http.MaxBytesError
	)
	switch {
	case errors.As(err, &api):
		return api
	case errors.As(err, &validation):
		return &apiError{status: http.StatusUnprocessableEntity, code: "validation_failed", message: "the request did not pass validation", details: validation.Issues}
	case errors.As(err, &tooLarge):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: "payload_too_large", message: "the request body is too large"}
	case errors.Is(err, content.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: "not_found", message: err.Error()}
	case errors.Is(err, content.ErrConflict):
		return &apiError{status: http.StatusConflict, code: "conflict", message: err.Error()}
	case errors.Is(err, content.ErrInvalidToken):
		return errBadToken
	case errors.Is(err, context.DeadlineExceeded):
		return &apiError{status: http.StatusGatewayTimeout, code: "timeout", message: "the request took too long"}
	case errors.Is(err, context.Canceled):
		return &apiError{status: http.StatusServiceUnavailable, code: "cancelled", message: "the request was cancelled"}
	}
	return nil
}

type errorBody struct {
	Error errorJSON `json:"error"`
}

type errorJSON struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details []content.Issue `json:"details,omitempty"`
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	api := classify(err)
	if api == nil {
		a.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		api = errInternal
	}
	if api.status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="tapirpost"`)
	}
	writeJSON(w, api.status, errorBody{Error: errorJSON{Code: api.code, Message: api.message, Details: api.details}})
}

// encode renders v as one line of JSON. Unlike json.Marshal it leaves <, >
// and & alone: the output is never embedded in HTML here, and content reads
// better unescaped.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	body, err := encode(v)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
	return nil
}
