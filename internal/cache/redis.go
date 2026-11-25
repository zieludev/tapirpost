package cache

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// A cache that answers slowly is worse than no cache, so the client gives up
// quickly and does not retry: the caller reads from the database instead.
const (
	redisDialTimeout = 500 * time.Millisecond
	redisIOTimeout   = 250 * time.Millisecond
	// redisPause is how long the server is left alone after an error, so
	// that an outage costs one timeout and not one per request.
	redisPause = time.Second
)

// Redis is a cache on a server that speaks the Redis protocol, such as
// Valkey or Redis. It can be shared by several processes.
type Redis struct {
	client      *redis.Client
	pausedUntil atomic.Int64 // unix nanoseconds
	now         func() time.Time
}

// NewRedis returns a cache on the server named by a redis:// or rediss://
// URL. It does not connect until the cache is used.
func NewRedis(rawURL string) (*Redis, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = redisDialTimeout
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = redisIOTimeout
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = redisIOTimeout
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = -1
	}
	return &Redis{client: redis.NewClient(opts), now: time.Now}, nil
}

// Get returns the value stored under key. While the server is being left
// alone after an error, every key is reported as missing.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if r.paused() {
		return nil, false, nil
	}
	value, err := r.client.Get(ctx, key).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, false, nil
	case err != nil:
		r.pause(ctx)
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}
	return value, true, nil
}

// Set stores value under key for ttl. While the server is being left alone
// after an error, nothing is stored.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return errTTL
	}
	if r.paused() {
		return nil
	}
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		r.pause(ctx)
		return fmt.Errorf("cache: set: %w", err)
	}
	return nil
}

// Ping reports whether the server answers.
func (r *Redis) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: ping: %w", err)
	}
	return nil
}

// Close releases the connections to the server.
func (r *Redis) Close() error {
	return r.client.Close()
}

func (r *Redis) paused() bool {
	return r.now().UnixNano() < r.pausedUntil.Load()
}

// pause starts the quiet period, unless the error came from the caller
// giving up: that says nothing about the server.
func (r *Redis) pause(ctx context.Context) {
	if ctx.Err() == nil {
		r.pausedUntil.Store(r.now().Add(redisPause).UnixNano())
	}
}
