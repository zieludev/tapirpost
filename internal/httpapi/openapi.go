package httpapi

import (
	_ "embed"
	"net/http"
	"strconv"
)

// The description is written by hand and kept honest by a test that compares
// its paths with the route table.
//
//go:embed openapi.json
var openAPIDocument []byte

func (a *API) openAPI(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(openAPIDocument)))
	w.WriteHeader(http.StatusOK)
	w.Write(openAPIDocument)
	return nil
}
