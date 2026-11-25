package cache

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/zieludev/tapirpost/internal/cache/cachetest"
)

// fakeClock is a clock that only moves when a test says so.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestMemory(maxBytes int64) (*Memory, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := NewMemory(maxBytes)
	m.now = clock.now
	return m, clock
}

func TestMemoryConformance(t *testing.T) {
	cachetest.Run(t, func(t *testing.T) (cachetest.Cache, func(time.Duration)) {
		m, clock := newTestMemory(8 << 20)
		return m, clock.advance
	})
}

func value(n int) []byte {
	return bytes.Repeat([]byte("x"), n)
}

func TestMemoryEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := t.Context()
	const valueSize = 1000
	itemSize := int64(len("k0") + valueSize + itemOverhead)
	m, _ := newTestMemory(3 * itemSize)

	for i := range 3 {
		if err := m.Set(ctx, fmt.Sprintf("k%d", i), value(valueSize), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if m.Len() != 3 || m.Bytes() != 3*itemSize {
		t.Fatalf("after three values: %d values, %d bytes", m.Len(), m.Bytes())
	}

	// k0 was stored first, but reading it makes k1 the one unused longest.
	if _, ok, _ := m.Get(ctx, "k0"); !ok {
		t.Fatal("k0 is missing")
	}
	if err := m.Set(ctx, "k3", value(valueSize), time.Minute); err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]bool{"k0": true, "k1": false, "k2": true, "k3": true} {
		if _, ok, _ := m.Get(ctx, key); ok != want {
			t.Errorf("%s present: got %v, want %v", key, ok, want)
		}
	}
	if m.Len() != 3 || m.Bytes() != 3*itemSize {
		t.Fatalf("after eviction: %d values, %d bytes", m.Len(), m.Bytes())
	}
}

func TestMemoryStaysWithinItsBudget(t *testing.T) {
	ctx := t.Context()
	const budget = 64 << 10
	m, _ := newTestMemory(budget)
	for i := range 500 {
		if err := m.Set(ctx, fmt.Sprintf("key-%d", i), value(100+i*7), time.Minute); err != nil {
			t.Fatal(err)
		}
		if m.Bytes() > budget {
			t.Fatalf("after %d values the cache holds %d bytes, over its budget of %d", i+1, m.Bytes(), budget)
		}
	}
	if _, ok, _ := m.Get(ctx, "key-499"); !ok {
		t.Fatal("the newest value was evicted")
	}
	if _, ok, _ := m.Get(ctx, "key-0"); ok {
		t.Fatal("the oldest value is still there")
	}
}

func TestMemoryRefusesWhatCannotFit(t *testing.T) {
	ctx := t.Context()
	m, _ := newTestMemory(1000)
	if err := m.Set(ctx, "k", value(100), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ctx, "k", value(5000), time.Minute); err != nil {
		t.Fatal(err)
	}
	// The old value must not survive a newer one that was too large to
	// keep: it would be served as if it were current.
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("an outdated value outlived its replacement")
	}
	if m.Len() != 0 || m.Bytes() != 0 {
		t.Fatalf("left behind %d values, %d bytes", m.Len(), m.Bytes())
	}
}

func TestMemoryAccountsForReplacedAndExpiredValues(t *testing.T) {
	ctx := t.Context()
	m, clock := newTestMemory(1 << 20)
	if err := m.Set(ctx, "k", value(1000), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.Set(ctx, "k", value(10), time.Minute); err != nil {
		t.Fatal(err)
	}
	if want := int64(len("k") + 10 + itemOverhead); m.Bytes() != want {
		t.Fatalf("after replacing: %d bytes, want %d", m.Bytes(), want)
	}
	clock.advance(time.Minute)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("the value outlived its time to live")
	}
	if m.Len() != 0 || m.Bytes() != 0 {
		t.Fatalf("an expired value still takes %d bytes", m.Bytes())
	}
}
