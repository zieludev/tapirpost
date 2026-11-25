package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/zieludev/tapirpost/internal/cache/cachetest"
	"github.com/zieludev/tapirpost/internal/testenv"
)

// TestRedisConformance runs against miniredis, a Redis-protocol server that
// lives in the test process, so it needs nothing installed.
func TestRedisConformance(t *testing.T) {
	cachetest.Run(t, func(t *testing.T) (cachetest.Cache, func(time.Duration)) {
		server := miniredis.RunT(t)
		c, err := NewRedis("redis://" + server.Addr())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c, server.FastForward
	})
}

// prefixed keeps the keys of one test apart from whatever else is on a
// shared server.
type prefixed struct {
	cache  *Redis
	prefix string
}

func (p prefixed) Get(ctx context.Context, key string) ([]byte, bool, error) {
	return p.cache.Get(ctx, p.prefix+key)
}

func (p prefixed) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return p.cache.Set(ctx, p.prefix+key, value, ttl)
}

// TestRedisConformanceRealServer runs against the server named by
// TAPIRPOST_TEST_REDIS_URL, where time passes by waiting.
func TestRedisConformanceRealServer(t *testing.T) {
	url := testenv.Service(t, testenv.RedisURL)
	cachetest.Run(t, func(t *testing.T) (cachetest.Cache, func(time.Duration)) {
		c, err := NewRedis(url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if err := c.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
		suffix := make([]byte, 8)
		rand.Read(suffix)
		return prefixed{cache: c, prefix: "tapirpost-test:" + hex.EncodeToString(suffix) + ":"}, time.Sleep
	})
}

func TestRedisRejectsBadURL(t *testing.T) {
	for _, url := range []string{"", "localhost:6379", "http://localhost:6379", "redis://localhost:notaport"} {
		if _, err := NewRedis(url); err == nil {
			t.Errorf("NewRedis(%q) succeeded", url)
		}
	}
}

func TestRedisLeavesAFailingServerAlone(t *testing.T) {
	ctx := t.Context()
	server := miniredis.RunT(t)
	c, err := NewRedis("redis://" + server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	clock := &fakeClock{t: time.Now()}
	c.now = clock.now

	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	server.Close()
	if err := c.Ping(ctx); err == nil {
		t.Fatal("Ping succeeded without a server")
	}
	if _, _, err := c.Get(ctx, "k"); err == nil {
		t.Fatal("Get succeeded without a server")
	}
	// From here on the server is not asked: no error, and no waiting for a
	// timeout either.
	start := time.Now()
	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("Get while paused: %v, %v", ok, err)
	}
	if err := c.Set(ctx, "k", []byte("v2"), time.Minute); err != nil {
		t.Fatalf("Set while paused: %v", err)
	}
	if elapsed := time.Since(start); elapsed > redisIOTimeout/2 {
		t.Fatalf("calls while paused took %s", elapsed)
	}

	if err := server.Restart(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("Get before the pause ended: %v, %v", ok, err)
	}
	clock.advance(redisPause)
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || string(got) != "v" {
		t.Fatalf("Get after the pause: %q, %v, %v", got, ok, err)
	}
}

func TestRedisDoesNotPauseWhenTheCallerGivesUp(t *testing.T) {
	server := miniredis.RunT(t)
	c, err := NewRedis("redis://" + server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Set(t.Context(), "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := c.Get(cancelled, "k"); err == nil {
		t.Fatal("Get with a cancelled context succeeded")
	}
	if got, ok, err := c.Get(t.Context(), "k"); err != nil || !ok || string(got) != "v" {
		t.Fatalf("Get after a cancelled call: %q, %v, %v", got, ok, err)
	}
}
