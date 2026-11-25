// Package cachetest holds the conformance suite every cache has to pass.
package cachetest

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Cache is what the suite tests: the interface the HTTP API consumes.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// Run runs the suite. open is called once per test and returns an empty
// cache that can hold at least 4 MiB, together with a function that lets
// the given amount of time pass for that cache.
func Run(t *testing.T, open func(t *testing.T) (c Cache, advance func(time.Duration))) {
	tests := []struct {
		name string
		fn   func(t *testing.T, c Cache, advance func(time.Duration))
	}{
		{"Miss", testMiss},
		{"SetGet", testSetGet},
		{"Overwrite", testOverwrite},
		{"Values", testValues},
		{"SetCopies", testSetCopies},
		{"Expiry", testExpiry},
		{"OverwriteRestartsExpiry", testOverwriteRestartsExpiry},
		{"TTLMustBePositive", testTTLMustBePositive},
		{"Concurrent", testConcurrent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, advance := open(t)
			tc.fn(t, c, advance)
		})
	}
}

func mustSet(t *testing.T, c Cache, key string, value []byte, ttl time.Duration) {
	t.Helper()
	if err := c.Set(t.Context(), key, value, ttl); err != nil {
		t.Fatalf("Set(%q): %v", key, err)
	}
}

func wantValue(t *testing.T, c Cache, key string, want []byte) {
	t.Helper()
	got, ok, err := c.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if !ok {
		t.Fatalf("Get(%q): missing, want %d bytes", key, len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get(%q): got %d bytes %.40q, want %d bytes %.40q", key, len(got), got, len(want), want)
	}
}

func wantMiss(t *testing.T, c Cache, key string) {
	t.Helper()
	got, ok, err := c.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if ok {
		t.Fatalf("Get(%q): got %.40q, want a miss", key, got)
	}
}

func testMiss(t *testing.T, c Cache, _ func(time.Duration)) {
	wantMiss(t, c, "never set")
	wantMiss(t, c, "")
}

func testSetGet(t *testing.T, c Cache, _ func(time.Duration)) {
	mustSet(t, c, "a", []byte("alpha"), time.Minute)
	mustSet(t, c, "b", []byte("beta"), time.Minute)
	wantValue(t, c, "a", []byte("alpha"))
	wantValue(t, c, "b", []byte("beta"))
	wantValue(t, c, "a", []byte("alpha"))
	wantMiss(t, c, "A")
	wantMiss(t, c, "a ")
}

func testOverwrite(t *testing.T, c Cache, _ func(time.Duration)) {
	mustSet(t, c, "k", []byte("a long first value"), time.Minute)
	mustSet(t, c, "k", []byte("second"), time.Minute)
	wantValue(t, c, "k", []byte("second"))
}

func testValues(t *testing.T, c Cache, _ func(time.Duration)) {
	every := make([]byte, 256)
	for i := range every {
		every[i] = byte(i)
	}
	values := map[string][]byte{
		"empty":                {},
		"every byte":           every,
		"one mebibyte":         bytes.Repeat([]byte("0123456789abcdef"), 1<<16),
		"key: with {odd} *?[]": []byte("odd key"),
		"klucz zażółć 日本":      []byte("unicode key"),
	}
	for key, value := range values {
		mustSet(t, c, key, value, time.Minute)
	}
	for key, value := range values {
		wantValue(t, c, key, value)
	}
}

func testSetCopies(t *testing.T, c Cache, _ func(time.Duration)) {
	value := []byte("original")
	mustSet(t, c, "k", value, time.Minute)
	copy(value, "CHANGED!")
	wantValue(t, c, "k", []byte("original"))
}

func testExpiry(t *testing.T, c Cache, advance func(time.Duration)) {
	mustSet(t, c, "short", []byte("gone soon"), 2*time.Second)
	mustSet(t, c, "long", []byte("stays"), time.Hour)
	advance(time.Second)
	wantValue(t, c, "short", []byte("gone soon"))
	advance(1500 * time.Millisecond)
	wantMiss(t, c, "short")
	wantValue(t, c, "long", []byte("stays"))
}

func testOverwriteRestartsExpiry(t *testing.T, c Cache, advance func(time.Duration)) {
	mustSet(t, c, "k", []byte("first"), 2*time.Second)
	advance(1500 * time.Millisecond)
	mustSet(t, c, "k", []byte("second"), 2*time.Second)
	advance(1500 * time.Millisecond)
	wantValue(t, c, "k", []byte("second"))
	advance(time.Second)
	wantMiss(t, c, "k")
}

func testTTLMustBePositive(t *testing.T, c Cache, _ func(time.Duration)) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		if err := c.Set(t.Context(), "k", []byte("forever"), ttl); err == nil {
			t.Fatalf("Set with a time to live of %s succeeded", ttl)
		}
	}
	wantMiss(t, c, "k")
}

func testConcurrent(t *testing.T, c Cache, _ func(time.Duration)) {
	const workers, keys, rounds = 8, 16, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			for r := range rounds {
				key := fmt.Sprintf("key-%d", (w+r)%keys)
				want := []byte("value of " + key)
				if err := c.Set(t.Context(), key, want, time.Minute); err != nil {
					errs <- err
					return
				}
				got, ok, err := c.Get(t.Context(), key)
				if err != nil {
					errs <- err
					return
				}
				// Every writer stores the same value under a key, so a
				// hit can only ever be that value.
				if ok && !bytes.Equal(got, want) {
					errs <- fmt.Errorf("Get(%q) returned %q", key, got)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
