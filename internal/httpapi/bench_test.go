package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zieludev/tapirpost/internal/cache"
	"github.com/zieludev/tapirpost/internal/content"
	"github.com/zieludev/tapirpost/internal/httpapi"
	"github.com/zieludev/tapirpost/internal/store/postgres/postgrestest"
)

const benchEntries = 1000

var benchCategories = []string{"news", "sport", "culture", "science", "travel"}

// seedBench fills a type with entries the size of a short article and
// returns their ids.
func seedBench(b *testing.B, h *harness) []string {
	b.Helper()
	ctx := b.Context()
	_, err := h.svc.CreateType(ctx, content.TypeSpec{
		Name: "article",
		Fields: []content.Field{
			{Name: "title", Type: content.FieldString, Required: true},
			{Name: "body", Type: content.FieldText},
			{Name: "category", Type: content.FieldEnum, Values: benchCategories},
			{Name: "views", Type: content.FieldInteger},
			{Name: "released", Type: content.FieldDateTime},
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	body := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 9)
	ids := make([]string, benchEntries)
	for i := range ids {
		fields, err := json.Marshal(map[string]any{
			"title":    fmt.Sprintf("Article number %d", i),
			"body":     body,
			"category": benchCategories[i%len(benchCategories)],
			"views":    (i * 7919) % 10007,
			"released": fmt.Sprintf("2026-%02d-%02dT12:00:00Z", 1+i%12, 1+i%28),
		})
		if err != nil {
			b.Fatal(err)
		}
		e, err := h.svc.CreateEntry(ctx, "article", fields)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := h.svc.PublishEntry(ctx, "article", e.ID); err != nil {
			b.Fatal(err)
		}
		ids[i] = e.ID
	}
	return ids
}

// BenchmarkDelivery measures one delivery request from the handler down to
// the database, without a network: with a token, for a listing (filter, sort
// and a page of 20 out of 1000 entries) and for a single entry, with the
// cache off and with the in-process cache.
//
// The PostgreSQL runs need TAPIRPOST_TEST_POSTGRES_URL and are skipped
// without it.
func BenchmarkDelivery(b *testing.B) {
	stores := []struct {
		name string
		open func(b *testing.B) content.Store
	}{
		{"sqlite", func(b *testing.B) content.Store { return sqliteStore(b) }},
		{"postgres", func(b *testing.B) content.Store { return postgrestest.Open(b) }},
	}
	caches := []struct {
		name string
		open func() httpapi.Cache
	}{
		{"off", func() httpapi.Cache { return nil }},
		{"memory", func() httpapi.Cache { return cache.NewMemory(64 << 20) }},
	}

	for _, s := range stores {
		for _, c := range caches {
			b.Run(s.name+"/cache="+c.name, func(b *testing.B) {
				h := newHarness(b, s.open(b), withCache(c.open()))
				ids := seedBench(b, h)
				token := h.newToken([]string{"read"})

				b.Run("list", func(b *testing.B) {
					// Fifteen different listings, so that a cache has more
					// than one entry to find.
					var targets []string
					for _, category := range benchCategories {
						for _, offset := range []int{0, 20, 40} {
							targets = append(targets, fmt.Sprintf("/v1/content/article?filter[category]=%s&sort=-views&limit=20&offset=%d", category, offset))
						}
					}
					benchRequests(b, h, token, targets)
				})
				b.Run("entry", func(b *testing.B) {
					targets := make([]string, len(ids))
					for i, id := range ids {
						targets[i] = "/v1/content/article/" + id
					}
					benchRequests(b, h, token, targets)
				})
			})
		}
	}
}

func benchRequests(b *testing.B, h *harness, token string, targets []string) {
	b.Helper()
	requests := make([]*http.Request, len(targets))
	for i, target := range targets {
		requests[i] = httptest.NewRequestWithContext(b.Context(), "GET", target, nil)
		requests[i].Header.Set("Authorization", "Bearer "+token)
	}
	// Once through before the clock starts: every response is checked, and a
	// cache is as warm as it would be in a running server.
	var bytes int64
	for _, req := range requests {
		rec := httptest.NewRecorder()
		h.api.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s: %d %s", req.URL, rec.Code, rec.Body)
		}
		bytes += int64(rec.Body.Len())
	}
	b.SetBytes(bytes / int64(len(requests)))
	b.ReportAllocs()

	w := &discardWriter{header: http.Header{}}
	i := 0
	for b.Loop() {
		clear(w.header)
		w.status = 0
		h.api.ServeHTTP(w, requests[i%len(requests)])
		if w.status != http.StatusOK {
			b.Fatalf("%s: %d", requests[i%len(requests)].URL, w.status)
		}
		i++
	}
}

// BenchmarkDeliveryParallel sends the single-entry request from all CPUs at
// once, to show how far the path through the handler and the database
// scales. ns/op is the time per request across all of them.
func BenchmarkDeliveryParallel(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "sqlite/cache=off/entry"
		var c httpapi.Cache
		if cached {
			name, c = "sqlite/cache=memory/entry", cache.NewMemory(64<<20)
		}
		b.Run(name, func(b *testing.B) {
			h := newHarness(b, sqliteStore(b), withCache(c))
			ids := seedBench(b, h)
			token := h.newToken([]string{"read"})
			requests := make([]*http.Request, len(ids))
			for i, id := range ids {
				requests[i] = httptest.NewRequestWithContext(b.Context(), "GET", "/v1/content/article/"+id, nil)
				requests[i].Header.Set("Authorization", "Bearer "+token)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				w := &discardWriter{header: http.Header{}}
				for i := 0; pb.Next(); i++ {
					clear(w.header)
					w.status = 0
					h.api.ServeHTTP(w, requests[i%len(requests)])
					if w.status != http.StatusOK {
						b.Errorf("status %d", w.status)
						return
					}
				}
			})
		})
	}
}

// discardWriter is a ResponseWriter that keeps the status and drops the
// body, so that the benchmark does not measure a recorder.
type discardWriter struct {
	header http.Header
	status int
}

func (w *discardWriter) Header() http.Header { return w.header }

func (w *discardWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *discardWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return len(p), nil
}
