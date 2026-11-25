// Package httpapi is the HTTP face of TapirPost: a management API for content
// types, entries and tokens, and a delivery API for published content.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// Cache keeps encoded delivery responses. Implementations are safe for
// concurrent use, and Get returns a slice that the caller only reads.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Options configures an API.
type Options struct {
	Service *content.Service
	Logger  *slog.Logger

	// Cache holds delivery responses for CacheTTL. Nil turns caching off.
	Cache    Cache
	CacheTTL time.Duration

	// PublicDelivery serves published content without a token.
	PublicDelivery bool
	// DeliveryCacheControl is sent as Cache-Control with published content.
	DeliveryCacheControl string

	MaxBodyBytes   int64
	RequestTimeout time.Duration
	Version        string
}

// API serves the HTTP API. It is an http.Handler.
type API struct {
	opts     Options
	svc      *content.Service
	log      *slog.Logger
	mux      *http.ServeMux
	routes   []route
	draining atomic.Bool
}

type route struct {
	method string
	path   string
	// scope is what a token needs for the route. Empty means anyone.
	scope content.Scope
	// delivery routes need no token when PublicDelivery is set.
	delivery bool
	handle   func(w http.ResponseWriter, r *http.Request) error
}

// New returns an API with the defaults filled in.
func New(opts Options) *API {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 15 * time.Second
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = 5 * time.Minute
	}
	if opts.DeliveryCacheControl == "" {
		opts.DeliveryCacheControl = "no-cache"
	}
	a := &API{opts: opts, svc: opts.Service, log: opts.Logger, mux: http.NewServeMux()}

	a.routes = []route{
		{method: "GET", path: "/healthz", handle: a.healthz},
		{method: "GET", path: "/readyz", handle: a.readyz},
		{method: "GET", path: "/openapi.json", handle: a.openAPI},

		{method: "GET", path: "/v1/content/{type}", scope: content.ScopeRead, delivery: true, handle: a.deliverList},
		{method: "GET", path: "/v1/content/{type}/{id}", scope: content.ScopeRead, delivery: true, handle: a.deliverEntry},

		{method: "GET", path: "/v1/types", scope: content.ScopeRead, handle: a.listTypes},
		{method: "POST", path: "/v1/types", scope: content.ScopeAdmin, handle: a.createType},
		{method: "GET", path: "/v1/types/{type}", scope: content.ScopeRead, handle: a.getType},
		{method: "PUT", path: "/v1/types/{type}", scope: content.ScopeAdmin, handle: a.updateType},
		{method: "DELETE", path: "/v1/types/{type}", scope: content.ScopeAdmin, handle: a.deleteType},

		{method: "GET", path: "/v1/types/{type}/entries", scope: content.ScopeWrite, handle: a.listEntries},
		{method: "POST", path: "/v1/types/{type}/entries", scope: content.ScopeWrite, handle: a.createEntry},
		{method: "GET", path: "/v1/types/{type}/entries/{id}", scope: content.ScopeWrite, handle: a.getEntry},
		{method: "PUT", path: "/v1/types/{type}/entries/{id}", scope: content.ScopeWrite, handle: a.updateEntry},
		{method: "DELETE", path: "/v1/types/{type}/entries/{id}", scope: content.ScopeWrite, handle: a.deleteEntry},
		{method: "POST", path: "/v1/types/{type}/entries/{id}/publish", scope: content.ScopeWrite, handle: a.publishEntry},
		{method: "POST", path: "/v1/types/{type}/entries/{id}/unpublish", scope: content.ScopeWrite, handle: a.unpublishEntry},

		{method: "GET", path: "/v1/tokens", scope: content.ScopeAdmin, handle: a.listTokens},
		{method: "POST", path: "/v1/tokens", scope: content.ScopeAdmin, handle: a.createToken},
		{method: "DELETE", path: "/v1/tokens/{id}", scope: content.ScopeAdmin, handle: a.deleteToken},
	}
	for _, rt := range a.routes {
		a.mux.HandleFunc(rt.method+" "+rt.path, a.guard(rt))
	}
	return a
}

// Drain makes the readiness probe fail, so that a load balancer stops
// sending requests before the server shuts down.
func (a *API) Drain() {
	a.draining.Store(true)
}

// requestInfo carries what the token check learned to the handler and to the
// access log.
type requestInfo struct {
	token  content.Token
	authed bool
}

type requestInfoKey struct{}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := &requestInfo{}
	ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestInfoKey{}, info), a.opts.RequestTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			a.log.ErrorContext(ctx, "panic in handler", "panic", p, "stack", string(debug.Stack()))
			if !rec.wrote {
				a.fail(rec, r, errInternal)
			}
		}
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			level = slog.LevelDebug
		}
		a.log.Log(ctx, level, "request",
			"method", r.Method, "path", r.URL.Path, "status", rec.status,
			"bytes", rec.bytes, "duration", time.Since(start), "token", info.token.ID)
	}()

	rec.Header().Set("X-Content-Type-Options", "nosniff")
	// Nothing may be kept by a cache on the way unless a handler says so:
	// responses carry drafts, and one of them a token secret. Published
	// content replaces this with the configured policy.
	rec.Header().Set("Cache-Control", "no-store")
	if _, pattern := a.mux.Handler(r); pattern == "" {
		a.unmatched(rec, r)
		return
	}
	a.mux.ServeHTTP(rec, r)
}

// unmatched answers requests no route takes. The mux knows whether the path
// exists under another method, but it says so in plain text; this asks it
// and repeats the answer as JSON.
func (a *API) unmatched(w http.ResponseWriter, r *http.Request) {
	probe := &probeWriter{header: http.Header{}}
	a.mux.ServeHTTP(probe, r)
	if probe.status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", probe.header.Get("Allow"))
		a.fail(w, r, &apiError{status: http.StatusMethodNotAllowed, code: "method_not_allowed", message: "the method " + r.Method + " is not allowed here"})
		return
	}
	a.fail(w, r, &apiError{status: http.StatusNotFound, code: "not_found", message: "no such route"})
}

// probeWriter records the status and headers of a response and drops the
// body.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return len(b), nil
}

// guard wraps a route with the token check and the error handling.
func (a *API) guard(rt route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if rt.scope != "" && !(rt.delivery && a.opts.PublicDelivery) {
			if err := a.authorize(r, rt.scope); err != nil {
				a.fail(w, r, err)
				return
			}
		}
		if err := rt.handle(w, r); err != nil {
			a.fail(w, r, err)
		}
	}
}

func (a *API) authorize(r *http.Request, scope content.Scope) error {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	secret = strings.TrimSpace(secret)
	if !ok || !strings.EqualFold(scheme, "Bearer") || secret == "" {
		return errNoToken
	}
	token, err := a.svc.Authenticate(r.Context(), secret)
	if err != nil {
		return err
	}
	if info, ok := r.Context().Value(requestInfoKey{}).(*requestInfo); ok {
		info.token, info.authed = token, true
	}
	if !token.Allows(scope, r.PathValue("type")) {
		return errForbidden
	}
	return nil
}

// statusRecorder remembers what was sent, for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wrote {
		s.status, s.wrote = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (a *API) healthz(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": a.opts.Version})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) error {
	if a.draining.Load() {
		return writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.svc.Ping(ctx); err != nil {
		a.log.WarnContext(r.Context(), "readiness: the database does not answer", "error", err)
		return writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "database unavailable"})
	}
	return writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
