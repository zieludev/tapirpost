// Package config reads the settings of TapirPost from command line flags and
// from the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// EnvPrefix starts the name of every environment variable TapirPost reads.
const EnvPrefix = "TAPIRPOST_"

// The kinds of database a URL can name.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

// The values of the cache setting, besides a redis:// or rediss:// URL.
const (
	CacheOff    = "off"
	CacheMemory = "memory"
)

// Server is the configuration of the serve command.
type Server struct {
	Addr string

	Database         string
	DatabaseMaxConns int

	Cache         string
	CacheTTL      time.Duration
	CacheMaxBytes int64

	AdminToken           string
	PublicDelivery       bool
	DeliveryCacheControl string

	MaxBodyBytes    int64
	RequestTimeout  time.Duration
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration

	Log Log
}

// Log says where the diagnostics go in which shape.
type Log struct {
	Format string
	Level  slog.Level
}

// EnvName returns the environment variable that sets a flag: cache-ttl is
// read from TAPIRPOST_CACHE_TTL.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// FromEnv gives the flags of fs the values their environment variables hold,
// if any: all flags, or just the named ones. Call it before fs.Parse: what is
// on the command line then wins.
func FromEnv(fs *flag.FlagSet, getenv func(string) string, only ...string) error {
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if len(only) > 0 && !slices.Contains(only, f.Name) {
			return
		}
		value := getenv(EnvName(f.Name))
		if value == "" {
			return
		}
		if err := f.Value.Set(value); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", EnvName(f.Name), value, err))
		}
	})
	return errors.Join(errs...)
}

// Usage prints the flags of fs together with their environment variables.
func Usage(fs *flag.FlagSet, w io.Writer) {
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(w, "  --%s, %s\n    \t%s", f.Name, EnvName(f.Name), f.Usage)
		if f.DefValue != "" && f.DefValue != "false" {
			fmt.Fprintf(w, " (default %s)", f.DefValue)
		}
		fmt.Fprintln(w)
	})
}

// LogFlags adds the logging flags to fs.
func LogFlags(fs *flag.FlagSet, l *Log) {
	fs.StringVar(&l.Format, "log-format", "text", "log format: text or json")
	fs.TextVar(&l.Level, "log-level", slog.LevelInfo, "log level: debug, info, warn or error")
}

// DatabaseFlag adds the database flag to fs.
func DatabaseFlag(fs *flag.FlagSet, database *string) {
	fs.StringVar(database, "db", "sqlite:tapirpost.db", "database: sqlite:<file> or a postgres:// URL")
}

// ParseServer reads the configuration of the serve command. Help and errors
// about the flags are written to output.
func ParseServer(args []string, getenv func(string) string, output io.Writer) (Server, error) {
	var c Server
	fs := flag.NewFlagSet("tapirpost serve", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "Usage: tapirpost serve [flags]\n\nEvery flag can also be set through the environment variable next to it.")
		fmt.Fprintln(output)
		Usage(fs, output)
	}

	fs.StringVar(&c.Addr, "addr", ":8080", "address to listen on")
	DatabaseFlag(fs, &c.Database)
	fs.IntVar(&c.DatabaseMaxConns, "db-max-conns", 8, "most database connections kept open")
	fs.StringVar(&c.Cache, "cache", CacheOff, "cache for published content: off, memory, or a redis:// URL of a Redis or Valkey server")
	fs.DurationVar(&c.CacheTTL, "cache-ttl", 5*time.Minute, "how long a cached response is kept")
	fs.Int64Var(&c.CacheMaxBytes, "cache-max-bytes", 64<<20, "size of the memory cache in bytes")
	fs.StringVar(&c.AdminToken, "admin-token", "", "a token with every permission, at least 32 characters; prefer the environment variable, a flag shows up in the process list")
	fs.BoolVar(&c.PublicDelivery, "public-delivery", false, "serve published content without a token")
	fs.StringVar(&c.DeliveryCacheControl, "delivery-cache-control", "no-cache", "Cache-Control header sent with published content")
	fs.Int64Var(&c.MaxBodyBytes, "max-body-bytes", 1<<20, "largest request body accepted, in bytes")
	fs.DurationVar(&c.RequestTimeout, "request-timeout", 15*time.Second, "time a request may take")
	fs.DurationVar(&c.ShutdownDelay, "shutdown-delay", 0, "time between failing the readiness probe and closing the listener")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 20*time.Second, "time running requests get to finish on shutdown")
	LogFlags(fs, &c.Log)

	if err := FromEnv(fs, getenv); err != nil {
		return Server{}, err
	}
	if err := fs.Parse(args); err != nil {
		return Server{}, err
	}
	if fs.NArg() > 0 {
		return Server{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := c.validate(); err != nil {
		return Server{}, err
	}
	return c, nil
}

func (c Server) validate() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}
	if _, _, err := SplitDatabase(c.Database); err != nil {
		errs = append(errs, err)
	}
	if c.DatabaseMaxConns < 1 {
		fail("db-max-conns must be at least 1")
	}
	if c.Cache != CacheOff && c.Cache != CacheMemory && !IsRedisURL(c.Cache) {
		fail("cache must be off, memory, or a redis:// or rediss:// URL, not %q", c.Cache)
	}
	if c.CacheTTL <= 0 {
		fail("cache-ttl must be positive")
	}
	if c.CacheMaxBytes < 1 {
		fail("cache-max-bytes must be positive")
	}
	if c.MaxBodyBytes < 1 {
		fail("max-body-bytes must be positive")
	}
	if c.RequestTimeout <= 0 {
		fail("request-timeout must be positive")
	}
	if c.ShutdownDelay < 0 {
		fail("shutdown-delay must not be negative")
	}
	if c.ShutdownTimeout <= 0 {
		fail("shutdown-timeout must be positive")
	}
	if strings.ContainsAny(c.DeliveryCacheControl, "\r\n") {
		fail("delivery-cache-control must be a single line")
	}
	if err := c.Log.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate reports a logging configuration that cannot be used.
func (l Log) Validate() error {
	if l.Format != "text" && l.Format != "json" {
		return fmt.Errorf("log-format must be text or json, not %q", l.Format)
	}
	return nil
}

// IsRedisURL reports whether s names a Redis-protocol server.
func IsRedisURL(s string) bool {
	return strings.HasPrefix(s, "redis://") || strings.HasPrefix(s, "rediss://")
}

// SplitDatabase tells which kind of database a setting names and what to
// open: the file of a SQLite database, or the URL of a PostgreSQL one.
func SplitDatabase(database string) (kind, target string, err error) {
	switch {
	case strings.HasPrefix(database, "postgres://"), strings.HasPrefix(database, "postgresql://"):
		return Postgres, database, nil
	case strings.HasPrefix(database, "sqlite:"):
		if path := strings.TrimPrefix(database, "sqlite:"); path != "" {
			return SQLite, path, nil
		}
	}
	return "", "", errors.New("db must be sqlite:<file> or a postgres:// URL")
}
