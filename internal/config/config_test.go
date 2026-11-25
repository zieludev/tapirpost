package config

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(name string) string { return m[name] }
}

func TestDefaults(t *testing.T) {
	got, err := ParseServer(nil, env(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := Server{
		Addr:                 ":8080",
		Database:             "sqlite:tapirpost.db",
		DatabaseMaxConns:     8,
		Cache:                "off",
		CacheTTL:             5 * time.Minute,
		CacheMaxBytes:        64 << 20,
		DeliveryCacheControl: "no-cache",
		MaxBodyBytes:         1 << 20,
		RequestTimeout:       15 * time.Second,
		ShutdownTimeout:      20 * time.Second,
		Log:                  Log{Format: "text", Level: slog.LevelInfo},
	}
	if got != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", got, want)
	}
}

func TestFlagsWinOverTheEnvironment(t *testing.T) {
	environment := env(
		"TAPIRPOST_ADDR", "127.0.0.1:9000",
		"TAPIRPOST_DB", "postgres://user:secret@db/tapirpost",
		"TAPIRPOST_CACHE", "memory",
		"TAPIRPOST_CACHE_TTL", "30s",
		"TAPIRPOST_PUBLIC_DELIVERY", "true",
		"TAPIRPOST_ADMIN_TOKEN", "from-the-environment-0123456789abcdef",
		"TAPIRPOST_LOG_LEVEL", "debug",
		"TAPIRPOST_LOG_FORMAT", "json",
		"TAPIRPOST_DELIVERY_CACHE_CONTROL", "public, max-age=60",
		"UNRELATED_CACHE", "redis://nowhere",
	)
	got, err := ParseServer([]string{"--cache-ttl", "2m", "-db-max-conns=3", "--cache", "redis://cache:6379/1"}, environment, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]bool{
		"addr from the environment":            got.Addr == "127.0.0.1:9000",
		"db from the environment":              got.Database == "postgres://user:secret@db/tapirpost",
		"cache from the flag":                  got.Cache == "redis://cache:6379/1",
		"cache-ttl from the flag":              got.CacheTTL == 2*time.Minute,
		"db-max-conns from the flag":           got.DatabaseMaxConns == 3,
		"public-delivery from the environment": got.PublicDelivery,
		"admin-token from the environment":     got.AdminToken == "from-the-environment-0123456789abcdef",
		"log-level from the environment":       got.Log.Level == slog.LevelDebug,
		"log-format from the environment":      got.Log.Format == "json",
		"header from the environment":          got.DeliveryCacheControl == "public, max-age=60",
		"untouched settings keep the default":  got.MaxBodyBytes == 1<<20,
	}
	for what, ok := range checks {
		if !ok {
			t.Errorf("%s: %+v", what, got)
		}
	}
}

func TestInvalidSettings(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{"unknown flag", []string{"--nope"}, nil, "nope"},
		{"stray argument", []string{"serve"}, nil, "unexpected argument"},
		{"unknown database", []string{"--db", "mysql://localhost/tapirpost"}, nil, "db must be"},
		{"sqlite without a file", []string{"--db", "sqlite:"}, nil, "db must be"},
		{"a bare file name", []string{"--db", "tapirpost.db"}, nil, "db must be"},
		{"no connections", []string{"--db-max-conns", "0"}, nil, "db-max-conns"},
		{"unknown cache", []string{"--cache", "memcached://localhost"}, nil, "cache must be"},
		{"cache on", []string{"--cache", "on"}, nil, "cache must be"},
		{"no cache ttl", []string{"--cache-ttl", "0"}, nil, "cache-ttl"},
		{"negative cache ttl", []string{"--cache-ttl", "-1s"}, nil, "cache-ttl"},
		{"cache ttl without a unit", []string{"--cache-ttl", "300"}, nil, "cache-ttl"},
		{"empty cache", []string{"--cache-max-bytes", "0"}, nil, "cache-max-bytes"},
		{"no body", []string{"--max-body-bytes", "0"}, nil, "max-body-bytes"},
		{"no time for requests", []string{"--request-timeout", "0s"}, nil, "request-timeout"},
		{"negative delay", []string{"--shutdown-delay", "-1s"}, nil, "shutdown-delay"},
		{"no time to shut down", []string{"--shutdown-timeout", "0s"}, nil, "shutdown-timeout"},
		{"header injection", []string{"--delivery-cache-control", "no-cache\r\nX-Evil: 1"}, nil, "delivery-cache-control"},
		{"unknown log format", []string{"--log-format", "xml"}, nil, "log-format"},
		{"unknown log level", []string{"--log-level", "loud"}, nil, "log-level"},
		{"bad value in the environment", nil, []string{"TAPIRPOST_CACHE_TTL", "soon"}, "TAPIRPOST_CACHE_TTL"},
		{"bad boolean in the environment", nil, []string{"TAPIRPOST_PUBLIC_DELIVERY", "maybe"}, "TAPIRPOST_PUBLIC_DELIVERY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := ParseServer(tc.args, env(tc.env...), &out)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error()+out.String(), tc.want) {
				t.Fatalf("the error does not mention %q: %v; output: %s", tc.want, err, out.String())
			}
		})
	}

	t.Run("every problem is reported", func(t *testing.T) {
		_, err := ParseServer([]string{"--db", "nope", "--cache", "nope", "--log-format", "nope"}, env(), &bytes.Buffer{})
		for _, want := range []string{"db must be", "cache must be", "log-format"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not mention %q: %v", want, err)
			}
		}
	})
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	_, err := ParseServer([]string{"--help"}, env(), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"--cache-ttl, TAPIRPOST_CACHE_TTL", "--db, TAPIRPOST_DB", "(default 5m0s)", "--admin-token, TAPIRPOST_ADMIN_TOKEN"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the help does not contain %q:\n%s", want, out.String())
		}
	}
}

func TestSplitDatabase(t *testing.T) {
	cases := []struct {
		in, kind, target string
	}{
		{"sqlite:tapirpost.db", SQLite, "tapirpost.db"},
		{"sqlite:/var/lib/tapirpost/data.db", SQLite, "/var/lib/tapirpost/data.db"},
		{"sqlite:dir with spaces/a:b.db", SQLite, "dir with spaces/a:b.db"},
		{"postgres://u:p@host:5432/db?sslmode=require", Postgres, "postgres://u:p@host:5432/db?sslmode=require"},
		{"postgresql://host/db", Postgres, "postgresql://host/db"},
		{"sqlite:", "", ""},
		{"", "", ""},
		{"file:tapirpost.db", "", ""},
		{"POSTGRES://host/db", "", ""},
	}
	for _, tc := range cases {
		kind, target, err := SplitDatabase(tc.in)
		if kind != tc.kind || target != tc.target || (err != nil) != (tc.kind == "") {
			t.Errorf("SplitDatabase(%q): got %q, %q, %v", tc.in, kind, target, err)
		}
	}

	// A setting that is refused may still hold a password, so the error must
	// not repeat it.
	_, _, err := SplitDatabase("mysql://admin:hunter2@db.internal/cms")
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "db.internal") {
		t.Fatalf("error for a refused setting: %v", err)
	}
}

func TestEnvName(t *testing.T) {
	for flagName, want := range map[string]string{"db": "TAPIRPOST_DB", "cache-max-bytes": "TAPIRPOST_CACHE_MAX_BYTES", "addr": "TAPIRPOST_ADDR"} {
		if got := EnvName(flagName); got != want {
			t.Errorf("EnvName(%q): got %q, want %q", flagName, got, want)
		}
	}
}
