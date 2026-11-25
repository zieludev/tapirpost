// Command bench measures the delivery API of a tapirpost binary over HTTP.
//
// For every combination of database and cache it starts the binary on a free
// local port, fills it with entries through the management API, sends
// requests from a number of connections for a fixed time and prints what it
// measured as a Markdown table. The load generator runs on the same machine
// as the server and competes with it for the CPU, so the numbers say how the
// configurations compare, not what a server does on hardware of its own.
//
//	go build -o tapirpost ./cmd/tapirpost
//	go run ./tools/bench -binary ./tapirpost
//	go run ./tools/bench -binary ./tapirpost -postgres postgres://user:pass@127.0.0.1:5432/bench
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const typeName = "bench_article"

var categories = []string{"news", "sport", "culture", "science", "travel"}

type options struct {
	binary      string
	postgres    string
	entries     int
	duration    time.Duration
	warmup      time.Duration
	connections int
	verbose     bool
}

func main() {
	var o options
	flag.StringVar(&o.binary, "binary", "./tapirpost", "the tapirpost binary to measure")
	flag.StringVar(&o.postgres, "postgres", "", "URL of an empty PostgreSQL database; adds the PostgreSQL runs")
	flag.IntVar(&o.entries, "entries", 1000, "published entries to create")
	flag.DurationVar(&o.duration, "duration", 10*time.Second, "how long each measurement runs")
	flag.DurationVar(&o.warmup, "warmup", 2*time.Second, "how long requests are sent before the measurement starts")
	flag.IntVar(&o.connections, "connections", 32, "connections that send requests at the same time")
	flag.BoolVar(&o.verbose, "verbose", false, "show the output of the server")
	flag.Parse()

	if err := run(context.Background(), o); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	binary, err := filepath.Abs(o.binary)
	if err != nil {
		return err
	}
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("the binary to measure: %w (build it with: go build -o tapirpost ./cmd/tapirpost)", err)
	}
	o.binary = binary

	dir, err := os.MkdirTemp("", "tapirpost-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	databases := []struct{ name, url string }{{"SQLite", "sqlite:" + filepath.Join(dir, "bench.db")}}
	if o.postgres != "" {
		databases = append(databases, struct{ name, url string }{"PostgreSQL", o.postgres})
	}

	fmt.Printf("%s, %d logical CPUs, %s/%s, %s\n", cpuModel(), runtime.NumCPU(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("%d published entries, %d connections, %s per measurement after %s of warm-up\n\n", o.entries, o.connections, o.duration, o.warmup)
	fmt.Println("| database | cache | request | requests/s | p50 | p95 | p99 | max | errors |")
	fmt.Println("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |")

	for _, db := range databases {
		for _, cache := range []string{"off", "memory"} {
			results, err := measure(ctx, o, db.url, cache)
			if err != nil {
				return fmt.Errorf("%s, cache %s: %w", db.name, cache, err)
			}
			for _, r := range results {
				fmt.Printf("| %s | %s | %s | %.0f | %s | %s | %s | %s | %d |\n",
					db.name, cache, r.name, r.perSecond, short(r.p50), short(r.p95), short(r.p99), short(r.max), r.errors)
			}
		}
	}
	return nil
}

type result struct {
	name               string
	perSecond          float64
	p50, p95, p99, max time.Duration
	errors             int64
}

// measure runs one server configuration through every request kind.
func measure(ctx context.Context, o options, database, cache string) ([]result, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	rand.Read(secret)
	admin := hex.EncodeToString(secret)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := exec.CommandContext(ctx, o.binary, "serve")
	server.Env = append(os.Environ(),
		fmt.Sprintf("TAPIRPOST_ADDR=127.0.0.1:%d", port),
		"TAPIRPOST_DB="+database,
		"TAPIRPOST_CACHE="+cache,
		"TAPIRPOST_ADMIN_TOKEN="+admin,
	)
	// The server logs every request, as it does in production. The log goes
	// nowhere here, so its formatting is measured and a terminal is not.
	if o.verbose {
		server.Stderr = os.Stderr
	}
	server.Cancel = func() error { return server.Process.Signal(syscall.SIGTERM) }
	server.WaitDelay = 15 * time.Second
	if err := server.Start(); err != nil {
		return nil, err
	}
	defer func() {
		cancel()
		server.Wait()
	}()

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        o.connections * 2,
			MaxIdleConnsPerHost: o.connections * 2,
			DisableCompression:  true,
		},
	}
	api := &api{client: client, base: base, token: admin}

	if err := api.waitUntilReady(ctx); err != nil {
		return nil, err
	}
	ids, err := api.seed(ctx, o.entries)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	var created struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := api.call(ctx, "POST", "/v1/tokens", map[string]any{"name": "bench", "scopes": []string{"read"}}, &created); err != nil {
		return nil, err
	}
	reader := created.Secret

	// Fifteen different listings: five filters, three pages each.
	var lists []string
	for _, category := range categories {
		for _, offset := range []int{0, 20, 40} {
			lists = append(lists, fmt.Sprintf("%s/v1/content/%s?filter[category]=%s&sort=-views&limit=20&offset=%d", base, typeName, category, offset))
		}
	}
	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = base + "/v1/content/" + typeName + "/" + id
	}

	var results []result
	for _, kind := range []struct {
		name string
		urls []string
	}{
		{"list: filter, sort, page of 20", lists},
		{"one entry by id", entries},
	} {
		load(ctx, client, reader, kind.urls, o.connections, o.warmup)
		r := load(ctx, client, reader, kind.urls, o.connections, o.duration)
		r.name = kind.name
		results = append(results, r)
	}

	// Leave a shared database as it was found.
	if err := api.call(ctx, "DELETE", "/v1/types/"+typeName, nil, nil); err != nil {
		return nil, err
	}
	if err := api.call(ctx, "DELETE", "/v1/tokens/"+created.ID, nil, nil); err != nil {
		return nil, err
	}
	return results, nil
}

// load sends GET requests from several connections until the time is up.
func load(ctx context.Context, client *http.Client, token string, urls []string, connections int, d time.Duration) result {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	var (
		wg     sync.WaitGroup
		failed atomic.Int64
		all    = make([][]time.Duration, connections)
	)
	start := time.Now()
	for c := range connections {
		wg.Go(func() {
			var took []time.Duration
			for i := c * 7919; ctx.Err() == nil; i++ {
				req, err := http.NewRequestWithContext(ctx, "GET", urls[i%len(urls)], nil)
				if err != nil {
					failed.Add(1)
					continue
				}
				req.Header.Set("Authorization", "Bearer "+token)
				began := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					// The request that the deadline cut off is not an error
					// of the server.
					if ctx.Err() == nil {
						failed.Add(1)
					}
					continue
				}
				_, err = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK {
					if ctx.Err() == nil {
						failed.Add(1)
					}
					continue
				}
				took = append(took, time.Since(began))
			}
			all[c] = took
		})
	}
	wg.Wait()
	elapsed := time.Since(start)

	merged := slices.Concat(all...)
	slices.Sort(merged)
	r := result{errors: failed.Load()}
	if len(merged) == 0 {
		return r
	}
	at := func(q float64) time.Duration { return merged[min(len(merged)-1, int(q*float64(len(merged))))] }
	r.perSecond = float64(len(merged)) / elapsed.Seconds()
	r.p50, r.p95, r.p99, r.max = at(0.50), at(0.95), at(0.99), merged[len(merged)-1]
	return r
}

type api struct {
	client *http.Client
	base   string
	token  string
}

func (a *api) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(raw))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (a *api) waitUntilReady(ctx context.Context) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, "GET", a.base+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := a.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the server did not become ready; run with -verbose to see why")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// seed creates the content type and n published entries of about 600 bytes,
// and returns their ids.
func (a *api) seed(ctx context.Context, n int) ([]string, error) {
	// A run that was interrupted may have left the type behind.
	a.call(ctx, "DELETE", "/v1/types/"+typeName, nil, nil)
	err := a.call(ctx, "POST", "/v1/types", map[string]any{
		"name": typeName,
		"fields": []map[string]any{
			{"name": "title", "type": "string", "required": true},
			{"name": "body", "type": "text"},
			{"name": "category", "type": "enum", "values": categories},
			{"name": "views", "type": "integer"},
			{"name": "released", "type": "datetime"},
		},
	}, nil)
	if err != nil {
		return nil, err
	}

	body := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 9)
	ids := make([]string, n)
	errs := make(chan error, 8)
	next := make(chan int)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range next {
				var entry struct {
					ID string `json:"id"`
				}
				err := a.call(ctx, "POST", "/v1/types/"+typeName+"/entries", map[string]any{"fields": map[string]any{
					"title":    fmt.Sprintf("Article number %d", i),
					"body":     body,
					"category": categories[i%len(categories)],
					"views":    (i * 7919) % 10007,
					"released": fmt.Sprintf("2026-%02d-%02dT12:00:00Z", 1+i%12, 1+i%28),
				}}, &entry)
				if err == nil {
					err = a.call(ctx, "POST", "/v1/types/"+typeName+"/entries/"+entry.ID+"/publish", nil, nil)
				}
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					continue
				}
				ids[i] = entry.ID
			}
		})
	}
	for i := range n {
		next <- i
	}
	close(next)
	wg.Wait()
	select {
	case err := <-errs:
		return nil, err
	default:
		return ids, nil
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown CPU"
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if name, value, ok := strings.Cut(scanner.Text(), ":"); ok && strings.TrimSpace(name) == "model name" {
			return strings.TrimSpace(value)
		}
	}
	return "unknown CPU"
}

// short prints a duration with three significant digits.
func short(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2f s", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.2f ms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%d µs", d.Microseconds())
	}
}
