package httpapi

// Routes lists the routes as "METHOD /path", for the test that compares them
// with the OpenAPI description.
func (a *API) Routes() []string {
	out := make([]string, len(a.routes))
	for i, rt := range a.routes {
		out[i] = rt.method + " " + rt.path
	}
	return out
}
