package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer that the server goroutine and the test can
// both touch.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func noEnv(string) string { return "" }

func envOf(pairs ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(name string) string { return m[name] }
}

// command runs tapirpost to completion and returns its exit code and output.
func command(t *testing.T, getenv func(string) string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(t.Context(), args, getenv, &out, &errOut, nil)
	return code, out.String(), errOut.String()
}

// server is a running "tapirpost serve".
type server struct {
	t      *testing.T
	url    string
	stop   context.CancelFunc
	done   chan int
	stderr *syncBuffer
}

func startServer(t *testing.T, getenv func(string) string, args ...string) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &server{t: t, stop: cancel, done: make(chan int, 1), stderr: &syncBuffer{}}
	addr := make(chan net.Addr, 1)
	go func() {
		s.done <- run(ctx, append([]string{"serve", "--addr", "127.0.0.1:0"}, args...), getenv, io.Discard, s.stderr, func(a net.Addr) { addr <- a })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop")
		}
	})
	select {
	case a := <-addr:
		s.url = "http://" + a.String()
	case code := <-s.done:
		s.done <- code
		t.Fatalf("the server exited with %d before listening: %s", code, s.stderr)
	case <-time.After(10 * time.Second):
		t.Fatalf("the server did not start: %s", s.stderr)
	}
	return s
}

// call sends a request and returns the status, the headers and the decoded
// JSON body.
func (s *server) call(method, path, token string, body any) (int, http.Header, map[string]any) {
	s.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.url+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			s.t.Fatalf("%s %s answered with something that is not JSON: %q", method, path, raw)
		}
	}
	return resp.StatusCode, resp.Header, decoded
}

func (s *server) want(status int, method, path, token string, body any) map[string]any {
	s.t.Helper()
	got, _, decoded := s.call(method, path, token, body)
	if got != status {
		s.t.Fatalf("%s %s: got %d, want %d; body: %v", method, path, got, status, decoded)
	}
	return decoded
}

func TestCommands(t *testing.T) {
	code, stdout, _ := command(t, noEnv, "version")
	if code != 0 || strings.TrimSpace(stdout) != version {
		t.Fatalf("version: %d, %q", code, stdout)
	}
	code, stdout, _ = command(t, noEnv, "help")
	if code != 0 || !strings.Contains(stdout, "tapirpost token create") {
		t.Fatalf("help: %d, %q", code, stdout)
	}
	code, _, stderr := command(t, noEnv, "frobnicate")
	if code != 2 || !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Fatalf("unknown command: %d, %q", code, stderr)
	}
	for _, args := range [][]string{{"serve", "--help"}, {"--help"}, {"migrate", "--help"}, {"token", "create", "--help"}} {
		code, _, stderr := command(t, noEnv, args...)
		if code != 0 || !strings.Contains(stderr, "Usage: tapirpost") || !strings.Contains(stderr, "TAPIRPOST_DB") {
			t.Errorf("%v: %d, %q", args, code, stderr)
		}
	}
}

func TestBadConfigurationStopsTheServer(t *testing.T) {
	db := "sqlite:" + filepath.Join(t.TempDir(), "tapirpost.db")
	cases := []struct {
		name string
		env  func(string) string
		args []string
		want string
	}{
		{"unknown database", noEnv, []string{"serve", "--db", "mysql://localhost/x"}, "db must be"},
		{"unknown flag", noEnv, []string{"serve", "--nope"}, "nope"},
		{"short admin token", envOf("TAPIRPOST_ADMIN_TOKEN", "short"), []string{"serve", "--db", db, "--addr", "127.0.0.1:0"}, "at least 32 characters"},
		{"database directory missing", noEnv, []string{"serve", "--db", "sqlite:" + filepath.Join(t.TempDir(), "missing", "tapirpost.db")}, "sqlite"},
		{"unreachable postgres", noEnv, []string{"serve", "--db", "postgres://tapirpost@127.0.0.1:1/tapirpost?connect_timeout=2"}, "postgres: connect"},
		{"bad cache url", noEnv, []string{"serve", "--db", db, "--cache", "redis://localhost:notaport"}, "cache"},
		{"address in use", noEnv, nil, "address already in use"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if tc.name == "address in use" {
				taken, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer taken.Close()
				args = []string{"serve", "--db", db, "--addr", taken.Addr().String()}
			}
			code, _, stderr := command(t, tc.env, args...)
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit code %d, stderr %q; want 1 and %q", code, stderr, tc.want)
			}
		})
	}
}

func TestMigrate(t *testing.T) {
	db := "sqlite:" + filepath.Join(t.TempDir(), "tapirpost.db")
	for range 2 {
		code, _, stderr := command(t, noEnv, "migrate", "--db", db)
		if code != 0 || !strings.Contains(stderr, "up to date") {
			t.Fatalf("migrate: %d, %q", code, stderr)
		}
	}
	code, _, stderr := command(t, envOf("TAPIRPOST_DB", db, "TAPIRPOST_LOG_FORMAT", "json"), "migrate")
	if code != 0 || !strings.Contains(stderr, `"msg":"the database schema is up to date"`) {
		t.Fatalf("migrate configured through the environment: %d, %q", code, stderr)
	}
	code, _, stderr = command(t, noEnv, "migrate", "--db", "nope")
	if code != 1 || !strings.Contains(stderr, "db must be") {
		t.Fatalf("migrate with a bad database: %d, %q", code, stderr)
	}
}

func TestTokenCreate(t *testing.T) {
	db := "sqlite:" + filepath.Join(t.TempDir(), "tapirpost.db")

	bad := map[string][]string{
		"no subcommand":  {"token"},
		"no name":        {"token", "create", "--db", db, "--scopes", "admin"},
		"no scopes":      {"token", "create", "--db", db, "--name", "x"},
		"unknown scope":  {"token", "create", "--db", db, "--name", "x", "--scopes", "root"},
		"limited admin":  {"token", "create", "--db", db, "--name", "x", "--scopes", "admin", "--types", "article"},
		"already over":   {"token", "create", "--db", db, "--name", "x", "--scopes", "read", "--expires-in", "-1h"},
		"stray argument": {"token", "create", "--db", db, "--name", "x", "--scopes", "read", "extra"},
	}
	for name, args := range bad {
		code, stdout, stderr := command(t, noEnv, args...)
		if code != 1 || stdout != "" || stderr == "" {
			t.Errorf("%s: exit code %d, stdout %q, stderr %q", name, code, stdout, stderr)
		}
	}

	// The settings of the server must not leak into this command: only the
	// database is read from the environment.
	environment := envOf("TAPIRPOST_DB", db, "TAPIRPOST_NAME", "from the environment", "TAPIRPOST_SCOPES", "admin")
	if code, stdout, _ := command(t, environment, "token", "create"); code != 1 || stdout != "" {
		t.Fatalf("a token was created from environment variables alone: %d, %q", code, stdout)
	}

	code, stdout, stderr := command(t, environment, "token", "create", "--name", "site", "--scopes", "read, write", "--types", "article,page", "--expires-in", "24h")
	secret := strings.TrimSpace(stdout)
	if code != 0 || !strings.HasPrefix(secret, "tapirpost_") || strings.ContainsAny(secret, " \n") {
		t.Fatalf("token create: %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if strings.Contains(stderr, secret) || !strings.Contains(stderr, "site") {
		t.Fatalf("stderr of token create: %q", stderr)
	}

	s := startServer(t, environment)
	s.want(http.StatusOK, "GET", "/v1/types", secret, nil)
	s.want(http.StatusForbidden, "GET", "/v1/tokens", secret, nil)
	s.want(http.StatusForbidden, "GET", "/v1/types/other/entries", secret, nil)
	s.want(http.StatusNotFound, "GET", "/v1/types/article/entries", secret, nil)
}

// TestServe is the path of the README: create a token, start the server,
// define a type, write, publish and read.
func TestServe(t *testing.T) {
	for _, cache := range []string{"off", "memory"} {
		t.Run("cache "+cache, func(t *testing.T) {
			db := "sqlite:" + filepath.Join(t.TempDir(), "tapirpost.db")
			code, stdout, stderr := command(t, noEnv, "token", "create", "--db", db, "--name", "me", "--scopes", "admin")
			if code != 0 {
				t.Fatalf("token create: %d, %q", code, stderr)
			}
			admin := strings.TrimSpace(stdout)

			s := startServer(t, noEnv, "--db", db, "--cache", cache, "--log-format", "json")
			if health := s.want(http.StatusOK, "GET", "/healthz", "", nil); health["status"] != "ok" || health["version"] != version {
				t.Fatalf("healthz: %v", health)
			}
			s.want(http.StatusOK, "GET", "/readyz", "", nil)
			s.want(http.StatusUnauthorized, "GET", "/v1/types", "", nil)
			if doc := s.want(http.StatusOK, "GET", "/openapi.json", "", nil); doc["openapi"] == nil {
				t.Fatalf("openapi.json: %v", doc)
			}

			s.want(http.StatusCreated, "POST", "/v1/types", admin, map[string]any{
				"name":   "article",
				"fields": []map[string]any{{"name": "title", "type": "string", "required": true}, {"name": "body", "type": "text"}},
			})
			entry := s.want(http.StatusCreated, "POST", "/v1/types/article/entries", admin, map[string]any{
				"fields": map[string]any{"title": "Hello", "body": "First post."},
			})
			id := entry["id"].(string)
			reader := s.want(http.StatusCreated, "POST", "/v1/tokens", admin, map[string]any{"name": "site", "scopes": []string{"read"}})["secret"].(string)

			s.want(http.StatusNotFound, "GET", "/v1/content/article/"+id, reader, nil)
			s.want(http.StatusOK, "POST", "/v1/types/article/entries/"+id+"/publish", admin, nil)
			s.want(http.StatusForbidden, "POST", "/v1/types/article/entries/"+id+"/publish", reader, nil)

			wantCache := map[string][]string{"off": {"", ""}, "memory": {"MISS", "HIT"}}[cache]
			for i, state := range wantCache {
				status, headers, page := s.call("GET", "/v1/content/article?filter[title]=Hello", reader, nil)
				if status != http.StatusOK || page["total"] != float64(1) {
					t.Fatalf("read %d: %d, %v", i+1, status, page)
				}
				if got := headers.Get("X-Cache"); got != state {
					t.Fatalf("read %d: X-Cache %q, want %q", i+1, got, state)
				}
				if headers.Get("ETag") == "" || headers.Get("Cache-Control") != "no-cache" {
					t.Fatalf("read %d: headers %v", i+1, headers)
				}
			}

			s.want(http.StatusOK, "POST", "/v1/types/article/entries/"+id+"/unpublish", admin, nil)
			if page := s.want(http.StatusOK, "GET", "/v1/content/article", reader, nil); page["total"] != float64(0) {
				t.Fatalf("after unpublishing: %v", page)
			}

			s.stop()
			if code := <-s.done; code != 0 {
				t.Fatalf("exit code after a signal: %d; %s", code, s.stderr)
			}
			s.done <- 0
			logs := s.stderr.String()
			for _, want := range []string{`"msg":"listening"`, `"msg":"stopped"`, `"path":"/v1/types"`, `"status":201`} {
				if !strings.Contains(logs, want) {
					t.Errorf("the log lacks %s:\n%s", want, logs)
				}
			}
			if strings.Contains(logs, admin) || strings.Contains(logs, reader) {
				t.Fatal("a token secret was logged")
			}
		})
	}
}

func TestServeWithAnAdminTokenFromTheEnvironment(t *testing.T) {
	const admin = "an-admin-token-from-a-kubernetes-secret"
	environment := envOf(
		"TAPIRPOST_DB", "sqlite:"+filepath.Join(t.TempDir(), "tapirpost.db"),
		"TAPIRPOST_ADMIN_TOKEN", admin,
		"TAPIRPOST_PUBLIC_DELIVERY", "true",
		"TAPIRPOST_DELIVERY_CACHE_CONTROL", "public, max-age=30",
	)
	s := startServer(t, environment)
	s.want(http.StatusCreated, "POST", "/v1/types", admin, map[string]any{
		"name": "page", "fields": []map[string]any{{"name": "title", "type": "string"}},
	})
	status, headers, _ := s.call("GET", "/v1/content/page", "", nil)
	if status != http.StatusOK || headers.Get("Cache-Control") != "public, max-age=30" {
		t.Fatalf("public delivery: %d, %v", status, headers)
	}
	if logs := s.stderr.String(); strings.Contains(logs, admin) {
		t.Fatalf("the admin token was logged:\n%s", logs)
	}
}

func TestServeWarnsWhenNobodyCanGetIn(t *testing.T) {
	s := startServer(t, noEnv, "--db", "sqlite:"+filepath.Join(t.TempDir(), "tapirpost.db"))
	if logs := s.stderr.String(); !strings.Contains(logs, "tapirpost token create") {
		t.Fatalf("no hint about the missing token:\n%s", logs)
	}
}

// TestShutdownOrder checks what a load balancer relies on: after the signal
// the readiness probe fails while requests are still served, and only then
// does the server go away.
func TestShutdownOrder(t *testing.T) {
	s := startServer(t, noEnv, "--db", "sqlite:"+filepath.Join(t.TempDir(), "tapirpost.db"), "--shutdown-delay", "1500ms")
	s.want(http.StatusOK, "GET", "/readyz", "", nil)

	s.stop()
	deadline := time.Now().Add(time.Second)
	for {
		status, _, _ := s.call("GET", "/readyz", "", nil)
		if status == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the readiness probe still answers %d after the signal", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.want(http.StatusOK, "GET", "/healthz", "", nil)

	select {
	case code := <-s.done:
		s.done <- code
		if code != 0 {
			t.Fatalf("exit code %d; %s", code, s.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
	if _, err := http.Get(s.url + "/healthz"); err == nil {
		t.Fatal("the server still answers after it stopped")
	}
}

func TestRequestsInFlightFinishBeforeTheServerStops(t *testing.T) {
	const admin = "an-admin-token-from-a-kubernetes-secret"
	s := startServer(t, envOf("TAPIRPOST_ADMIN_TOKEN", admin), "--db", "sqlite:"+filepath.Join(t.TempDir(), "tapirpost.db"))

	// A request whose body arrives slowly keeps its handler busy for as long
	// as the client takes. The server has to wait for it, not cut it off.
	const body = `{"name":"page","fields":[{"name":"title","type":"string"}]}`
	conn, err := net.Dial("tcp", strings.TrimPrefix(s.url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /v1/types HTTP/1.1\r\nHost: tapirpost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", admin, len(body), body[:20])
	time.Sleep(200 * time.Millisecond)

	s.stop()
	time.Sleep(300 * time.Millisecond)
	select {
	case code := <-s.done:
		t.Fatalf("the server stopped with %d while a request was in flight", code)
	default:
	}

	fmt.Fprint(conn, body[20:])
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil || !strings.HasPrefix(string(reply), "HTTP/1.1 201 Created") || !strings.Contains(string(reply), `"name":"page"`) {
		t.Fatalf("the request in flight got %q, %v", reply, err)
	}
	if code := <-s.done; code != 0 {
		t.Fatalf("exit code %d; %s", code, s.stderr)
	}
	s.done <- 0
}
