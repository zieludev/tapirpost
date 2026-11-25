package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zieludev/tapirpost/internal/content"
)

// The delivery API serves published content and can cache its responses.
//
// A cached response is never stale. Every change to the published content of
// a type changes the type's revision in the same database transaction, the
// revision is part of the cache key, and a request reads the revision from
// the database before it looks at the cache. After a publish there is simply
// no key left under which the old response could be found, for this process
// or for any other one that shares the database. The time to live only
// decides how long the unreachable leftovers take up space.
//
// The price is one primary key lookup per request. What the cache saves is
// the listing query, the count and the JSON encoding.
//
// Two things are outside this argument, because they make the database
// itself go back in time: a read replica that lags behind, and a restore
// from a backup, which brings old revisions back into use.

type deliveryJSON struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	PublishedAt time.Time       `json:"published_at"`
	Fields      json.RawMessage `json:"fields"`
}

func toDeliveryJSON(e content.Entry) deliveryJSON {
	return deliveryJSON{ID: e.ID, Type: e.Type, PublishedAt: e.PublishedAt, Fields: e.Published}
}

func (a *API) deliverList(w http.ResponseWriter, r *http.Request) error {
	typeName := r.PathValue("type")
	// Values.Encode sorts by key, so the same parameters in another order
	// share a cache entry.
	return a.deliver(w, r, typeName, "list?"+r.URL.Query().Encode(), func() ([]byte, error) {
		t, err := a.svc.Type(r.Context(), typeName)
		if err != nil {
			return nil, err
		}
		params, err := parseListParams(r.URL.Query())
		if err != nil {
			return nil, err
		}
		page, err := a.svc.ListPublished(r.Context(), t, params)
		if err != nil {
			return nil, queryError(err)
		}
		out := pageJSON[deliveryJSON]{Items: make([]deliveryJSON, len(page.Entries)), Total: page.Total, Limit: page.Limit, Offset: page.Offset}
		for i, e := range page.Entries {
			out.Items[i] = toDeliveryJSON(e)
		}
		return encode(out)
	})
}

func (a *API) deliverEntry(w http.ResponseWriter, r *http.Request) error {
	typeName, id := r.PathValue("type"), r.PathValue("id")
	if len(r.URL.Query()) > 0 {
		return badRequest("this route takes no query parameters")
	}
	return a.deliver(w, r, typeName, "entry/"+id, func() ([]byte, error) {
		e, err := a.svc.PublishedEntry(r.Context(), typeName, id)
		if err != nil {
			return nil, err
		}
		return encode(toDeliveryJSON(e))
	})
}

// etagLen is the length of the quoted, hex-encoded validator that starts
// every cached value.
const etagLen = 2 + 32

// cacheKeyPrefix marks the keys of TapirPost on a cache server that others
// use as well.
const cacheKeyPrefix = "tapirpost:"

// deliver answers with the response that build produces, taking it from the
// cache when it is there. request identifies the response within the type.
func (a *API) deliver(w http.ResponseWriter, r *http.Request, typeName, request string, build func() ([]byte, error)) error {
	ctx := r.Context()
	if a.opts.Cache == nil {
		body, err := build()
		if err != nil {
			return err
		}
		a.writeDelivery(w, r, etag(body), body, "")
		return nil
	}

	// The revision is read before the content. A publish that lands in
	// between is harmless then: the response is stored under the older
	// revision, where no later request will look.
	revision, err := a.svc.TypeRevision(ctx, typeName)
	if err != nil {
		return err
	}
	// The revision starts with the id of the type, which is unique, so the
	// key needs no namespace. The one exception is a copy of a whole
	// database: it has the ids of its original, and the two must not share
	// a cache server.
	sum := sha256.Sum256([]byte(request))
	key := cacheKeyPrefix + revision + ":" + hex.EncodeToString(sum[:])

	cached, ok, err := a.opts.Cache.Get(ctx, key)
	if err != nil {
		a.log.WarnContext(ctx, "cache read failed, serving from the database", "error", err)
	} else if ok && len(cached) >= etagLen {
		a.writeDelivery(w, r, string(cached[:etagLen]), cached[etagLen:], "HIT")
		return nil
	}

	body, err := build()
	if err != nil {
		return err
	}
	tag := etag(body)
	value := make([]byte, 0, etagLen+len(body))
	value = append(append(value, tag...), body...)
	if err := a.opts.Cache.Set(ctx, key, value, a.opts.CacheTTL); err != nil {
		a.log.WarnContext(ctx, "cache write failed", "error", err)
	}
	a.writeDelivery(w, r, tag, body, "MISS")
	return nil
}

func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

func (a *API) writeDelivery(w http.ResponseWriter, r *http.Request, tag string, body []byte, cacheState string) {
	h := w.Header()
	h.Set("ETag", tag)
	h.Set("Cache-Control", a.opts.DeliveryCacheControl)
	if !a.opts.PublicDelivery {
		h.Set("Vary", "Authorization")
	}
	if cacheState != "" {
		h.Set("X-Cache", cacheState)
	}
	if etagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body)
}

// etagMatches implements the weak comparison If-None-Match asks for: a W/
// prefix on either side is ignored, and "*" matches anything.
func etagMatches(header, tag string) bool {
	if header == "" {
		return false
	}
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}
