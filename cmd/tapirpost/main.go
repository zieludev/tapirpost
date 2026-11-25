// Command tapirpost runs TapirPost, a small headless CMS: one binary that
// serves a management API and a delivery API on top of SQLite or PostgreSQL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zieludev/tapirpost/internal/cache"
	"github.com/zieludev/tapirpost/internal/config"
	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/httpapi"
	"github.com/zieludev/tapirpost/internal/store"
	"github.com/zieludev/tapirpost/internal/store/postgres"
	"github.com/zieludev/tapirpost/internal/store/sqlite"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `TapirPost is a small headless CMS.

Usage:
  tapirpost [serve] [flags]        run the server
  tapirpost migrate [flags]        bring the database schema up to date and exit
  tapirpost token create [flags]   create an API token and print its secret
  tapirpost version                print the version

Run a command with --help to see its flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal the default handling is back, so that a second
	// one ends a shutdown that takes too long.
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil))
}

// run is main without the process: tests call it with their own arguments,
// environment and streams. listening, when set, is told the address the
// server ended up on.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, listening func(net.Addr)) int {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	var err error
	switch command {
	case "serve":
		err = serve(ctx, args, getenv, stderr, listening)
	case "migrate":
		err = migrate(ctx, args, getenv, stderr)
	case "token":
		err = token(ctx, args, getenv, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
	case "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "tapirpost: unknown command %q\n\n%s", command, usage)
		return 2
	}
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	default:
		fmt.Fprintln(stderr, "tapirpost:", err)
		return 1
	}
}

func newLogger(l config.Log, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: l.Level}
	if l.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// openStore opens the database a setting names and migrates it.
func openStore(ctx context.Context, database string, maxConns int) (*store.DB, string, error) {
	kind, target, err := config.SplitDatabase(database)
	if err != nil {
		return nil, "", err
	}
	if kind == config.Postgres {
		db, err := postgres.Open(ctx, target, maxConns)
		return db, kind, err
	}
	db, err := sqlite.Open(ctx, target, maxConns)
	return db, kind, err
}

// openCache returns the cache a setting names, nil for none, and what to
// call when the server stops.
func openCache(c config.Server) (httpapi.Cache, func(), error) {
	switch {
	case c.Cache == config.CacheMemory:
		return cache.NewMemory(c.CacheMaxBytes), func() {}, nil
	case config.IsRedisURL(c.Cache):
		r, err := cache.NewRedis(c.Cache)
		if err != nil {
			return nil, nil, err
		}
		return r, func() { r.Close() }, nil
	}
	return nil, func() {}, nil
}

func serve(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer, listening func(net.Addr)) error {
	cfg, err := config.ParseServer(args, getenv, stderr)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, stderr)

	db, kind, err := openStore(ctx, cfg.Database, cfg.DatabaseMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	responses, closeCache, err := openCache(cfg)
	if err != nil {
		return err
	}
	defer closeCache()

	svc, err := content.NewService(db, cfg.AdminToken)
	if err != nil {
		return err
	}
	if cfg.AdminToken == "" {
		if tokens, err := svc.Tokens(ctx); err == nil && len(tokens) == 0 {
			log.Warn("there is no token yet, so nobody can use the API; create one with: tapirpost token create --name me --scopes admin")
		}
	}

	api := httpapi.New(httpapi.Options{
		Service:              svc,
		Logger:               log,
		Cache:                responses,
		CacheTTL:             cfg.CacheTTL,
		PublicDelivery:       cfg.PublicDelivery,
		DeliveryCacheControl: cfg.DeliveryCacheControl,
		MaxBodyBytes:         cfg.MaxBodyBytes,
		RequestTimeout:       cfg.RequestTimeout,
		Version:              version,
	})

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 10 * time.Second,
		// The handlers stop at RequestTimeout on their own. These limits sit
		// behind that one, for clients that send or read too slowly.
		ReadTimeout:    cfg.RequestTimeout + 10*time.Second,
		WriteTimeout:   cfg.RequestTimeout + 10*time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	cacheKind := cfg.Cache
	if config.IsRedisURL(cacheKind) {
		// The URL may carry a password.
		cacheKind = "redis"
	}
	log.Info("listening", "addr", listener.Addr().String(), "version", version, "database", kind, "cache", cacheKind, "public_delivery", cfg.PublicDelivery)
	if listening != nil {
		listening(listener.Addr())
	}

	failed := make(chan error, 1)
	go func() { failed <- server.Serve(listener) }()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
	}

	// Shutting down in the order a load balancer needs: first say "not
	// ready" and keep serving, so that it stops sending requests, and only
	// then close the listener and let the requests in flight finish.
	log.Info("shutting down", "delay", cfg.ShutdownDelay, "timeout", cfg.ShutdownTimeout)
	api.Drain()
	time.Sleep(cfg.ShutdownDelay)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Info("stopped")
	return nil
}

func migrate(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	var (
		database string
		logging  config.Log
	)
	fs := flag.NewFlagSet("tapirpost migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: tapirpost migrate [flags]\n\nBrings the database schema up to date and exits. The server does the same\nwhen it starts; this command is for doing it as a step of its own.\n\n")
		config.Usage(fs, stderr)
	}
	config.DatabaseFlag(fs, &database)
	config.LogFlags(fs, &logging)
	if err := parse(fs, args, getenv); err != nil {
		return err
	}
	if err := logging.Validate(); err != nil {
		return err
	}

	db, kind, err := openStore(ctx, database, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	newLogger(logging, stderr).Info("the database schema is up to date", "database", kind)
	return nil
}

func token(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: tapirpost token create [flags]")
	}
	var (
		database, name, scopes, types string
		expiresIn                     time.Duration
	)
	fs := flag.NewFlagSet("tapirpost token create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: tapirpost token create [flags]\n\nCreates an API token in the database and prints its secret, which cannot\nbe shown again. The server does not have to be running. The database can\nalso be set through TAPIRPOST_DB.\n\n")
		fs.PrintDefaults()
	}
	config.DatabaseFlag(fs, &database)
	fs.StringVar(&name, "name", "", "what the token is for")
	fs.StringVar(&scopes, "scopes", "", "comma-separated scopes: read, write, admin")
	fs.StringVar(&types, "types", "", "comma-separated content types the token is limited to; empty for all")
	fs.DurationVar(&expiresIn, "expires-in", 0, "how long the token is valid, for example 720h; 0 for no expiry")
	if err := parse(fs, args[1:], getenv, "db"); err != nil {
		return err
	}

	spec := content.TokenSpec{Name: name, Types: splitList(types)}
	for _, scope := range splitList(scopes) {
		spec.Scopes = append(spec.Scopes, content.Scope(scope))
	}
	if expiresIn != 0 {
		at := time.Now().Add(expiresIn)
		spec.ExpiresAt = &at
	}

	db, _, err := openStore(ctx, database, 1)
	if err != nil {
		return err
	}
	defer db.Close()
	svc, err := content.NewService(db, "")
	if err != nil {
		return err
	}
	created, secret, err := svc.CreateToken(ctx, spec)
	if err != nil {
		return err
	}
	// Only the secret goes to standard output, so that it can be captured
	// with TOKEN=$(tapirpost token create ...).
	fmt.Fprintf(stderr, "created token %s (%s) with scopes %v; this secret is not shown again:\n", created.ID, created.Name, created.Scopes)
	fmt.Fprintln(stdout, secret)
	return nil
}

// parse reads the flags of a command. envFlags names the flags that may also
// come from the environment; none means all of them.
func parse(fs *flag.FlagSet, args []string, getenv func(string) string, envFlags ...string) error {
	if err := config.FromEnv(fs, getenv, envFlags...); err != nil {
		return err
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
