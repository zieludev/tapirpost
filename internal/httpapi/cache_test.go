package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/zieludev/tapirpost/internal/cache"
	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/httpapi"
	"github.com/zieludev/tapirpost/internal/store/postgres/postgrestest"
	"github.com/zieludev/tapirpost/internal/testenv"
)

// The tests in this file run for every combination of store and cache. The
// combinations that need a real PostgreSQL or a real Redis-protocol server
// are skipped unless the environment points at one.

type storeKind struct {
	name string
	open func(t *testing.T) content.Store
}

var storeKinds = []storeKind{
	{"sqlite", func(t *testing.T) content.Store { return sqliteStore(t) }},
	{"postgres", func(t *testing.T) content.Store { return postgrestest.Open(t) }},
}

type cacheKind struct {
	name string
	open func(t *testing.T) httpapi.Cache
}

func miniredisCache(t *testing.T) (*cache.Redis, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	c, err := cache.NewRedis("redis://" + server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, server
}

var cacheKinds = []cacheKind{
	{"off", func(*testing.T) httpapi.Cache { return nil }},
	{"memory", func(*testing.T) httpapi.Cache { return cache.NewMemory(8 << 20) }},
	{"redis protocol", func(t *testing.T) httpapi.Cache {
		c, _ := miniredisCache(t)
		return c
	}},
	{"real cache server", func(t *testing.T) httpapi.Cache {
		c, err := cache.NewRedis(testenv.Service(t, testenv.RedisURL))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if err := c.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
		return c
	}},
}

func withCache(c httpapi.Cache) func(*httpapi.Options) {
	return func(o *httpapi.Options) { o.Cache = c }
}

// forEachSetup runs fn against every store and cache combination.
func forEachSetup(t *testing.T, fn func(t *testing.T, h *harness, cached bool)) {
	for _, sk := range storeKinds {
		for _, ck := range cacheKinds {
			t.Run(sk.name+"/"+ck.name, func(t *testing.T) {
				c := ck.open(t)
				fn(t, newHarness(t, sk.open(t), withCache(c)), c != nil)
			})
		}
	}
}

// reader checks what the delivery API serves and how the cache took part.
type reader struct {
	*harness
	cached bool
}

// list reads a listing twice. With a cache the first read must come from the
// database when the content just changed (fresh) and from the cache when it
// did not, and the second read must come from the cache and be identical.
func (r reader) list(query string, fresh bool, want ...string) {
	r.t.Helper()
	path := "/v1/content/article" + query
	first := r.admin("GET", path, nil)
	wantStatus(r.t, first, http.StatusOK)
	second := r.admin("GET", path, nil)
	wantStatus(r.t, second, http.StatusOK)
	r.wantCacheStates(first, second, fresh)

	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		page := decode[publishedPage](r.t, rec)
		got := make([]string, len(page.Items))
		for j, item := range page.Items {
			got[j], _ = item.Fields["title"].(string)
		}
		if len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
			r.t.Fatalf("read %d of %s: got %v, want %v", i+1, path, got, want)
		}
		if page.Total != len(want) {
			r.t.Fatalf("read %d of %s: total %d, want %d", i+1, path, page.Total, len(want))
		}
	}
}

// entry reads one entry twice, like list. An empty title means the entry
// must not be there.
func (r reader) entry(id string, fresh bool, title string) {
	r.t.Helper()
	path := "/v1/content/article/" + id
	first := r.admin("GET", path, nil)
	second := r.admin("GET", path, nil)
	if title == "" {
		wantError(r.t, first, http.StatusNotFound, "not_found")
		wantError(r.t, second, http.StatusNotFound, "not_found")
		return
	}
	wantStatus(r.t, first, http.StatusOK)
	wantStatus(r.t, second, http.StatusOK)
	r.wantCacheStates(first, second, fresh)
	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if got := decode[publishedEntry](r.t, rec).Fields["title"]; got != title {
			r.t.Fatalf("read %d of %s: got title %v, want %q", i+1, path, got, title)
		}
	}
}

func (r reader) wantCacheStates(first, second *httptest.ResponseRecorder, fresh bool) {
	r.t.Helper()
	wantFirst, wantSecond := "", ""
	if r.cached {
		wantFirst, wantSecond = "HIT", "HIT"
		if fresh {
			wantFirst = "MISS"
		}
	}
	if got := first.Header().Get("X-Cache"); got != wantFirst {
		r.t.Fatalf("X-Cache of the first read: got %q, want %q", got, wantFirst)
	}
	if got := second.Header().Get("X-Cache"); got != wantSecond {
		r.t.Fatalf("X-Cache of the second read: got %q, want %q", got, wantSecond)
	}
	if first.Body.String() != second.Body.String() || first.Header().Get("ETag") != second.Header().Get("ETag") {
		r.t.Fatalf("the two reads differ:\n%s\n%s", first.Body, second.Body)
	}
}

// TestCacheNeverServesStaleContent walks through every way published content
// can change and checks, after each step, that readers see the new state at
// once: with no cache, and with each kind of cache warm with the old state.
func TestCacheNeverServesStaleContent(t *testing.T) {
	forEachSetup(t, func(t *testing.T, h *harness, cached bool) {
		r := reader{harness: h, cached: cached}
		const byTitle = "?sort=title"
		const onlyOne = "?filter[title][contains]=One"

		h.createType(articleSpec)
		r.list("", true)
		r.list(byTitle, true)

		one := h.createEntry("article", map[string]any{"title": "One", "views": 1})
		r.list(byTitle, false)
		r.entry(one, false, "")

		h.publish("article", one)
		r.list(byTitle, true, "One")
		r.list(onlyOne, true, "One")
		r.entry(one, true, "One")

		// A draft edit changes nothing for readers, and need not cost them
		// their cache.
		h.updateEntry("article", one, map[string]any{"title": "One, edited", "views": 2})
		r.list(byTitle, false, "One")
		r.entry(one, false, "One")

		h.publish("article", one)
		r.list(byTitle, true, "One, edited")
		r.list(onlyOne, true, "One, edited")
		r.entry(one, true, "One, edited")

		two := h.published("article", map[string]any{"title": "Two", "views": 20})
		r.list(byTitle, true, "One, edited", "Two")
		r.list(onlyOne, true, "One, edited")
		r.entry(one, true, "One, edited")
		r.entry(two, true, "Two")

		h.unpublish("article", one)
		r.list(byTitle, true, "Two")
		r.list(onlyOne, true)
		r.entry(one, false, "")
		r.entry(two, true, "Two")

		// Unpublishing what is not published changes nothing.
		h.unpublish("article", one)
		r.list(byTitle, false, "Two")

		h.publish("article", one)
		r.list(byTitle, true, "One, edited", "Two")
		r.entry(one, true, "One, edited")

		// Deleting a draft is invisible to readers, deleting a published
		// entry is not.
		draft := h.createEntry("article", map[string]any{"title": "Never published"})
		wantStatus(t, h.admin("DELETE", "/v1/types/article/entries/"+draft, nil), http.StatusNoContent)
		r.list(byTitle, false, "One, edited", "Two")

		wantStatus(t, h.admin("DELETE", "/v1/types/article/entries/"+two, nil), http.StatusNoContent)
		r.list(byTitle, true, "One, edited")
		r.entry(two, false, "")
		r.entry(one, true, "One, edited")

		// Removing a field removes its values from what is published.
		before := decode[publishedEntry](t, h.admin("GET", "/v1/content/article/"+one, nil))
		if before.Fields["views"] != float64(2) {
			t.Fatalf("views before the field was removed: %v", before.Fields)
		}
		wantStatus(t, h.admin("PUT", "/v1/types/article", map[string]any{
			"fields": []map[string]any{{"name": "title", "type": "string", "required": true}},
		}), http.StatusOK)
		r.list(byTitle, true, "One, edited")
		r.entry(one, true, "One, edited")
		after := decode[publishedEntry](t, h.admin("GET", "/v1/content/article/"+one, nil))
		if _, still := after.Fields["views"]; still {
			t.Fatalf("a removed field is still served: %v", after.Fields)
		}

		// A type that is deleted and created again starts from nothing,
		// although its revision counter starts over as well.
		wantStatus(t, h.admin("DELETE", "/v1/types/article", nil), http.StatusNoContent)
		wantError(t, h.admin("GET", "/v1/content/article"+byTitle, nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/article/"+one, nil), http.StatusNotFound, "not_found")

		h.createType(articleSpec)
		r.list(byTitle, true)
		r.list("", true)
		r.entry(one, false, "")
		reborn := h.published("article", map[string]any{"title": "Second life"})
		r.list(byTitle, true, "Second life")
		r.list("", true, "Second life")
		h.updateEntry("article", reborn, map[string]any{"title": "Second life, edited"})
		h.publish("article", reborn)
		r.list(byTitle, true, "Second life, edited")
		r.entry(reborn, true, "Second life, edited")
	})
}

// TestCacheAcrossProcesses stands for several replicas behind a load
// balancer: two APIs with a cache each (or one shared cache server) on one
// database. What one of them publishes, the other serves at once.
func TestCacheAcrossProcesses(t *testing.T) {
	for _, sk := range storeKinds {
		for _, shared := range []bool{false, true} {
			name := sk.name + "/a memory cache each"
			if shared {
				name = sk.name + "/one cache server"
			}
			t.Run(name, func(t *testing.T) {
				s := sk.open(t)
				var first, second httpapi.Cache = cache.NewMemory(8 << 20), cache.NewMemory(8 << 20)
				if shared {
					c, _ := miniredisCache(t)
					first, second = c, c
				}
				a := newHarness(t, s, withCache(first))
				b := reader{harness: newHarness(t, s, withCache(second)), cached: true}

				a.createType(articleSpec)
				b.list("", true)

				one := a.published("article", map[string]any{"title": "One"})
				b.list("", true, "One")
				b.entry(one, true, "One")

				a.updateEntry("article", one, map[string]any{"title": "One, edited"})
				a.publish("article", one)
				b.list("", true, "One, edited")
				b.entry(one, true, "One, edited")

				a.unpublish("article", one)
				b.list("", true)
				b.entry(one, false, "")

				// And the other way round. With one cache server the second
				// process finds what the first one stored; with a cache each
				// it has to ask the database once itself.
				two := b.published("article", map[string]any{"title": "Two"})
				b.list("", true, "Two")
				b.entry(two, true, "Two")
				reader{harness: a, cached: true}.list("", !shared, "Two")
				reader{harness: a, cached: true}.entry(two, !shared, "Two")
			})
		}
	}
}

// TestCacheKeysDoNotCollide puts two databases with the same type name, the
// same entry and the same number of changes behind one cache.
func TestCacheKeysDoNotCollide(t *testing.T) {
	shared := cache.NewMemory(8 << 20)
	a := newHarness(t, sqliteStore(t), withCache(shared))
	b := newHarness(t, sqliteStore(t), withCache(shared))

	for i, h := range []*harness{a, b} {
		h.createType(articleSpec)
		h.published("article", map[string]any{"title": fmt.Sprintf("From database %d", i+1)})
	}
	for range 2 {
		if got := a.titles(""); !reflect.DeepEqual(got, []string{"From database 1"}) {
			t.Fatalf("database 1 serves %v", got)
		}
		if got := b.titles(""); !reflect.DeepEqual(got, []string{"From database 2"}) {
			t.Fatalf("database 2 serves %v", got)
		}
	}
}

func TestCacheKeyIgnoresParameterOrder(t *testing.T) {
	h := newHarness(t, sqliteStore(t), withCache(cache.NewMemory(8<<20)))
	seedArticles(h)

	state := func(query string) string {
		t.Helper()
		rec := h.admin("GET", "/v1/content/article"+query, nil)
		wantStatus(t, rec, http.StatusOK)
		return rec.Header().Get("X-Cache")
	}
	if got := state("?sort=title&limit=2&filter[views][gte]=10"); got != "MISS" {
		t.Fatalf("first request: %s", got)
	}
	if got := state("?filter[views][gte]=10&limit=2&sort=title"); got != "HIT" {
		t.Fatalf("the same parameters in another order: %s", got)
	}
	if got := state("?filter%5Bviews%5D%5Bgte%5D=10&limit=2&sort=title"); got != "HIT" {
		t.Fatalf("the same parameters, percent-encoded: %s", got)
	}
	for _, other := range []string{"?sort=title&limit=3&filter[views][gte]=10", "?sort=-title&limit=2&filter[views][gte]=10", "?sort=title&limit=2&filter[views][gte]=11", "?sort=title&limit=2"} {
		if got := state(other); got != "MISS" {
			t.Fatalf("a different request %s: %s", other, got)
		}
	}
}

func TestCacheServesConditionalRequests(t *testing.T) {
	h := newHarness(t, sqliteStore(t), withCache(cache.NewMemory(8<<20)))
	h.createType(articleSpec)
	id := h.published("article", map[string]any{"title": "One"})

	first := h.admin("GET", "/v1/content/article/"+id, nil)
	tag := first.Header().Get("ETag")
	if first.Header().Get("X-Cache") != "MISS" || tag == "" {
		t.Fatalf("first read: %v", first.Header())
	}
	rec := h.do("GET", "/v1/content/article/"+id, rootToken, nil, "If-None-Match", tag)
	wantStatus(t, rec, http.StatusNotModified)
	if rec.Header().Get("X-Cache") != "HIT" || rec.Header().Get("ETag") != tag || rec.Body.Len() != 0 {
		t.Fatalf("revalidation from the cache: %v, body %q", rec.Header(), rec.Body)
	}

	h.updateEntry("article", id, map[string]any{"title": "Two"})
	h.publish("article", id)
	rec = h.do("GET", "/v1/content/article/"+id, rootToken, nil, "If-None-Match", tag)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[publishedEntry](t, rec).Fields["title"]; got != "Two" {
		t.Fatalf("a client holding the old ETag got %v", got)
	}
}

func TestCacheDoesNotKeepErrors(t *testing.T) {
	memory := cache.NewMemory(8 << 20)
	h := newHarness(t, sqliteStore(t), withCache(memory))
	h.createType(articleSpec)
	draft := h.createEntry("article", map[string]any{"title": "Soon"})

	for range 2 {
		wantError(t, h.admin("GET", "/v1/content/article/"+draft, nil), http.StatusNotFound, "not_found")
		wantError(t, h.admin("GET", "/v1/content/article?nope=1", nil), http.StatusBadRequest, "bad_request")
		wantError(t, h.admin("GET", "/v1/content/article?filter[nope]=1", nil), http.StatusBadRequest, "bad_request")
		wantError(t, h.admin("GET", "/v1/content/missing", nil), http.StatusNotFound, "not_found")
	}
	if memory.Len() != 0 {
		t.Fatalf("the cache holds %d values after only failed requests", memory.Len())
	}
}

func TestCacheChecksTheTokenFirst(t *testing.T) {
	h := newHarness(t, sqliteStore(t), withCache(cache.NewMemory(8<<20)))
	h.createType(articleSpec)
	id := h.published("article", map[string]any{"title": "Members only"})
	other := h.newToken([]string{"read"}, "page")

	for _, path := range []string{"/v1/content/article", "/v1/content/article/" + id} {
		wantStatus(t, h.admin("GET", path, nil), http.StatusOK)
		if got := h.admin("GET", path, nil).Header().Get("X-Cache"); got != "HIT" {
			t.Fatalf("%s is not cached: %q", path, got)
		}
		wantError(t, h.do("GET", path, "", nil), http.StatusUnauthorized, "unauthorized")
		wantError(t, h.do("GET", path, "tapirpost_unknown", nil), http.StatusUnauthorized, "unauthorized")
		wantError(t, h.do("GET", path, other, nil), http.StatusForbidden, "forbidden")
	}
}

// brokenCache fails every call, like a cache server that is down.
type brokenCache struct {
	gets, sets atomic.Int64
}

func (c *brokenCache) Get(context.Context, string) ([]byte, bool, error) {
	c.gets.Add(1)
	return nil, false, errors.New("cache down")
}

func (c *brokenCache) Set(context.Context, string, []byte, time.Duration) error {
	c.sets.Add(1)
	return errors.New("cache down")
}

// garbageCache answers every key with something that cannot be a response.
type garbageCache struct{}

func (garbageCache) Get(context.Context, string) ([]byte, bool, error) {
	return []byte("?"), true, nil
}

func (garbageCache) Set(context.Context, string, []byte, time.Duration) error { return nil }

func TestCacheFailuresDoNotFailRequests(t *testing.T) {
	check := func(t *testing.T, h *harness) {
		t.Helper()
		h.createType(articleSpec)
		id := h.published("article", map[string]any{"title": "One"})
		for range 2 {
			if got := h.titles(""); !reflect.DeepEqual(got, []string{"One"}) {
				t.Fatalf("list: %v", got)
			}
			wantStatus(t, h.admin("GET", "/v1/content/article/"+id, nil), http.StatusOK)
		}
		h.updateEntry("article", id, map[string]any{"title": "Two"})
		h.publish("article", id)
		if got := h.titles(""); !reflect.DeepEqual(got, []string{"Two"}) {
			t.Fatalf("list after a change: %v", got)
		}
	}

	t.Run("errors", func(t *testing.T) {
		broken := &brokenCache{}
		check(t, newHarness(t, sqliteStore(t), withCache(broken)))
		if broken.gets.Load() == 0 || broken.sets.Load() == 0 {
			t.Fatalf("the cache was not even asked: %d gets, %d sets", broken.gets.Load(), broken.sets.Load())
		}
	})

	t.Run("garbage", func(t *testing.T) {
		check(t, newHarness(t, sqliteStore(t), withCache(garbageCache{})))
	})

	t.Run("a server that goes away and comes back", func(t *testing.T) {
		c, server := miniredisCache(t)
		h := newHarness(t, sqliteStore(t), withCache(c))
		h.createType(articleSpec)
		id := h.published("article", map[string]any{"title": "One"})
		if got := h.titles(""); !reflect.DeepEqual(got, []string{"One"}) {
			t.Fatalf("list: %v", got)
		}

		server.Close()
		h.updateEntry("article", id, map[string]any{"title": "Two"})
		h.publish("article", id)
		for range 3 {
			if got := h.titles(""); !reflect.DeepEqual(got, []string{"Two"}) {
				t.Fatalf("list without the cache server: %v", got)
			}
		}

		// The old response is still on the server when it returns. It must
		// stay unreachable.
		if err := server.Restart(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			rec := h.admin("GET", "/v1/content/article", nil)
			wantStatus(t, rec, http.StatusOK)
			if got := decode[publishedPage](t, rec).Items[0].Fields["title"]; got != "Two" {
				t.Fatalf("after the cache server came back: %v", got)
			}
			if rec.Header().Get("X-Cache") == "HIT" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the cache was not used again after the server came back")
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// gateStore lets a test hold a listing between the moment it has read the
// content and the moment it returns it.
type gateStore struct {
	content.Store
	armed    atomic.Bool
	reached  chan struct{}
	released chan struct{}
}

func (s *gateStore) Entries(ctx context.Context, typeName string, q content.Query, published bool) ([]content.Entry, int, error) {
	entries, total, err := s.Store.Entries(ctx, typeName, q, published)
	if published && s.armed.CompareAndSwap(true, false) {
		close(s.reached)
		<-s.released
	}
	return entries, total, err
}

// TestCacheRaceWithPublish is the case the order in deliver exists for. A
// reader has fetched the old content and has not stored it yet when a publish
// completes. Whatever the reader stores afterwards must not be served to
// anyone who asks after the publish.
func TestCacheRaceWithPublish(t *testing.T) {
	for _, ck := range cacheKinds[1:3] {
		t.Run(ck.name, func(t *testing.T) {
			gate := &gateStore{Store: sqliteStore(t), reached: make(chan struct{}), released: make(chan struct{})}
			h := newHarness(t, gate, withCache(ck.open(t)))
			h.createType(articleSpec)
			h.published("article", map[string]any{"title": "One"})

			gate.armed.Store(true)
			slow := make(chan *httptest.ResponseRecorder, 1)
			go func() { slow <- h.admin("GET", "/v1/content/article?sort=title", nil) }()

			<-gate.reached
			h.published("article", map[string]any{"title": "Two"})
			close(gate.released)

			// The slow reader started before the publish finished, so the
			// old state is a correct answer for it.
			rec := <-slow
			wantStatus(t, rec, http.StatusOK)
			if page := decode[publishedPage](t, rec); len(page.Items) != 1 || page.Items[0].Fields["title"] != "One" {
				t.Fatalf("the slow reader got %s", rec.Body)
			}
			for i := range 3 {
				if got := h.titles("?sort=title"); !reflect.DeepEqual(got, []string{"One", "Two"}) {
					t.Fatalf("read %d after the publish: got %v", i+1, got)
				}
			}
		})
	}
}

// TestCacheUnderConcurrentWrites has writers publish, republish and unpublish
// while readers keep every cache entry warm. Each writer checks its own entry
// right after each of its writes returns: from that moment on no reader may
// get the previous state.
func TestCacheUnderConcurrentWrites(t *testing.T) {
	forEachSetup(t, func(t *testing.T, h *harness, _ bool) {
		const writers, rounds, readers = 4, 6, 4
		h.createType(articleSpec)

		ctx, stop := context.WithCancel(t.Context())
		var background sync.WaitGroup
		for range readers {
			background.Go(func() {
				for ctx.Err() == nil {
					for _, path := range []string{"/v1/content/article", "/v1/content/article?sort=title", "/v1/content/article?filter[views][gte]=0&limit=100"} {
						if rec := h.admin("GET", path, nil); rec.Code != http.StatusOK {
							t.Errorf("background read of %s: %d %s", path, rec.Code, rec.Body)
							return
						}
					}
				}
			})
		}

		var wg sync.WaitGroup
		for w := range writers {
			wg.Go(func() {
				fail := func(format string, args ...any) {
					t.Errorf("writer %d: "+format, append([]any{w}, args...)...)
				}
				rec := h.admin("POST", "/v1/types/article/entries", map[string]any{"fields": map[string]any{"title": "start", "views": w}})
				if rec.Code != http.StatusCreated {
					fail("create: %d %s", rec.Code, rec.Body)
					return
				}
				id := decode[entryResponse](t, rec).ID
				entryPath := "/v1/types/article/entries/" + id
				mine := "/v1/content/article?limit=100&filter[id]=" + id

				// seen is what readers must get for this entry right now.
				seen := func(want string) bool {
					one := h.admin("GET", "/v1/content/article/"+id, nil)
					list := h.admin("GET", mine, nil)
					all := h.admin("GET", "/v1/content/article?limit=100", nil)
					if list.Code != http.StatusOK || all.Code != http.StatusOK {
						fail("list: %d, %d", list.Code, all.Code)
						return false
					}
					var inAll []string
					for _, item := range decode[publishedPage](t, all).Items {
						if item.ID == id {
							inAll = append(inAll, item.Fields["title"].(string))
						}
					}
					var inList []string
					for _, item := range decode[publishedPage](t, list).Items {
						inList = append(inList, item.Fields["title"].(string))
					}
					if want == "" {
						if one.Code != http.StatusNotFound || len(inList) != 0 || len(inAll) != 0 {
							fail("unpublished, but readers get %d, %v and %v", one.Code, inList, inAll)
							return false
						}
						return true
					}
					if one.Code != http.StatusOK {
						fail("published %q, but the entry answers %d", want, one.Code)
						return false
					}
					got := decode[publishedEntry](t, one).Fields["title"]
					if got != want || !slices.Equal(inList, []string{want}) || !slices.Equal(inAll, []string{want}) {
						fail("published %q, but readers get %v, %v and %v", want, got, inList, inAll)
						return false
					}
					return true
				}

				for r := range rounds {
					title := fmt.Sprintf("writer %d round %d", w, r)
					if rec := h.admin("PUT", entryPath, map[string]any{"fields": map[string]any{"title": title, "views": w}}); rec.Code != http.StatusOK {
						fail("update: %d %s", rec.Code, rec.Body)
						return
					}
					if rec := h.admin("POST", entryPath+"/publish", nil); rec.Code != http.StatusOK {
						fail("publish: %d %s", rec.Code, rec.Body)
						return
					}
					if !seen(title) {
						return
					}
					if r%2 == 1 {
						if rec := h.admin("POST", entryPath+"/unpublish", nil); rec.Code != http.StatusOK {
							fail("unpublish: %d %s", rec.Code, rec.Body)
							return
						}
						if !seen("") {
							return
						}
					}
				}
			})
		}
		wg.Wait()
		stop()
		background.Wait()
	})
}
