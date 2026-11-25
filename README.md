# TapirPost

[![CI](https://github.com/zieludev/tapirpost/actions/workflows/ci.yml/badge.svg)](https://github.com/zieludev/tapirpost/actions/workflows/ci.yml)

TapirPost is a small headless CMS written in Go. It is one binary with an HTTP
API and nothing else: you describe your content types through the API, write
entries, publish them, and your site or app reads what is published.

- Content types with typed, validated fields, created and changed at run time.
- Entries have a draft and a published copy. Editing never changes what
  readers see until you publish.
- API tokens with scopes, optionally limited to some content types.
- A delivery API with filters, sorting, paging and ETags.
- An optional cache, in memory or on Redis or Valkey, that does not serve
  stale content: [how](#caching).
- SQLite by default, with nothing to set up, or PostgreSQL.

This is version 0.1. There is no admin interface yet, the API is all there is.
[What is tested and what is not](#status) is spelled out below.

## Quick start

You need Go 1.27 or newer.

```bash
git clone https://github.com/zieludev/tapirpost.git
cd tapirpost
go build -o tapirpost ./cmd/tapirpost

export TOKEN=$(./tapirpost token create --name me --scopes admin)
./tapirpost serve --addr 127.0.0.1:8080
```

`token create` writes a token into `tapirpost.db` in the current directory and
prints its secret, once. `serve` then says where it listens. Without `--addr`
that is port 8080 on every interface of the machine:

```
time=2026-11-25T15:35:35.560+01:00 level=INFO msg=listening addr=127.0.0.1:8080 version=0.1.0 database=sqlite cache=off public_delivery=false
```

In another terminal (with the same `TOKEN`), create a content type:

```bash
curl -s localhost:8080/v1/types \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{
  "name": "article",
  "fields": [
    {"name": "title", "type": "string", "required": true, "max_length": 120},
    {"name": "body", "type": "text"},
    {"name": "category", "type": "enum", "values": ["news", "notes"]},
    {"name": "views", "type": "integer", "min": 0}
  ]
}'
```

Write a draft. The answer carries its id:

```bash
curl -s localhost:8080/v1/types/article/entries \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"fields": {"title": "Hello", "body": "First post.", "category": "news", "views": 3}}'
```

```json
{"id":"01a21322-2919-7d1d-96c4-1061845c483f","type":"article","status":"draft","version":1,"created_at":"2026-11-25T14:35:35.577819Z","updated_at":"2026-11-25T14:35:35.577819Z","published_at":null,"fields":{"body":"First post.","category":"news","title":"Hello","views":3},"published_fields":null}
```

Readers do not see a draft (`GET /v1/content/article/<id>` answers 404).
Publish it:

```bash
curl -s -X POST localhost:8080/v1/types/article/entries/01a21322-2919-7d1d-96c4-1061845c483f/publish \
  -H "Authorization: Bearer $TOKEN"
```

And read it the way a website would. `-g` stops curl from treating the square
brackets as a pattern:

```bash
curl -sg 'localhost:8080/v1/content/article?filter[category]=news&sort=-views' \
  -H "Authorization: Bearer $TOKEN"
```

```json
{"items":[{"id":"01a21322-2919-7d1d-96c4-1061845c483f","type":"article","published_at":"2026-11-25T14:35:35.602597Z","fields":{"body":"First post.","category":"news","title":"Hello","views":3}}],"total":1,"limit":20,"offset":0}
```

A website should not carry an admin token. Make it one that can only read:

```bash
curl -s localhost:8080/v1/tokens \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name": "website", "scopes": ["read"], "types": ["article"]}'
```

The output above is what these commands printed on my machine.

## Content types and fields

A content type has a name and a list of fields. Names are lowercase letters,
digits and underscores, starting with a letter.

| type | holds | rules you can add |
| --- | --- | --- |
| `string` | one line of text | `min_length`, `max_length`, `pattern` |
| `text` | text of any shape | `min_length`, `max_length`, `pattern` |
| `integer` | a 64-bit integer | `min`, `max` |
| `number` | a 64-bit float | `min`, `max` |
| `boolean` | `true` or `false` | |
| `datetime` | an RFC 3339 timestamp, stored in UTC | |
| `enum` | one of `values` | |
| `json` | any JSON value; cannot be filtered or sorted on | |

Every field can be `required`. A draft may leave required fields out, because
a draft is allowed to be unfinished; publishing it is refused until they are
there. Every other rule holds for drafts too. Unknown fields are refused and
`null` removes a value. All problems of a request are reported at once:

```json
{"error":{"code":"validation_failed","message":"the request did not pass validation","details":[{"field":"colour","message":"is not a field of \"article\""},{"field":"title","message":"must be a string"},{"field":"views","message":"must be at least 0"}]}}
```

`PUT /v1/types/<name>` replaces the fields of a type. You can add fields,
change their rules and remove them. Removing a field deletes its values from
every entry, drafts and published copies alike. The type of an existing field
cannot change: remove the field and add it again.

## Drafts and publishing

An entry has a status:

| status | meaning |
| --- | --- |
| `draft` | not published |
| `published` | readers see exactly what the draft says |
| `changed` | published, and the draft has newer edits |

`PUT .../entries/<id>` replaces the draft, `POST .../publish` copies the draft
to the published copy, `POST .../unpublish` withdraws it and keeps the draft.
`GET .../entries/<id>` shows both copies as `fields` and `published_fields`.

## The API

| route | what it does | scope |
| --- | --- | --- |
| `GET /v1/content/<type>` | list published entries | `read` |
| `GET /v1/content/<type>/<id>` | one published entry | `read` |
| `GET /v1/types` | list content types | `read` |
| `POST /v1/types` | create a content type | `admin` |
| `GET /v1/types/<type>` | one content type | `read` |
| `PUT /v1/types/<type>` | replace its description and fields | `admin` |
| `DELETE /v1/types/<type>` | delete it with all of its entries | `admin` |
| `GET /v1/types/<type>/entries` | list entries with their drafts | `write` |
| `POST /v1/types/<type>/entries` | create a draft | `write` |
| `GET /v1/types/<type>/entries/<id>` | one entry, draft and published copy | `write` |
| `PUT /v1/types/<type>/entries/<id>` | replace the draft | `write` |
| `DELETE /v1/types/<type>/entries/<id>` | delete the entry | `write` |
| `POST /v1/types/<type>/entries/<id>/publish` | publish the draft | `write` |
| `POST /v1/types/<type>/entries/<id>/unpublish` | withdraw the published copy | `write` |
| `GET /v1/tokens` | list tokens | `admin` |
| `POST /v1/tokens` | create a token | `admin` |
| `DELETE /v1/tokens/<id>` | revoke a token | `admin` |
| `GET /healthz`, `GET /readyz` | probes | none |
| `GET /openapi.json` | this API as OpenAPI 3.1 | none |

The full description, with every parameter and response, is
[`openapi.json`](internal/httpapi/openapi.json), which the server also serves.
A test keeps it in step with the routes.

Bodies are JSON and must be sent as `Content-Type: application/json`. Unknown
properties are an error, not ignored. Errors always look like the one above:
a `code`, a `message`, and `details` when a field can be named.

### Tokens and permissions

Send a token as `Authorization: Bearer <secret>`.

| scope | allows |
| --- | --- |
| `read` | published content, and looking at content types |
| `write` | everything `read` allows, and entries: drafts, publishing, deleting |
| `admin` | everything, including content types and tokens |

A `read` or `write` token can be limited to some content types with `types`.
A token can have an expiry date (`expires_at`). Secrets are not stored: the
database holds a SHA-256 hash, and a secret is shown once, when the token is
made.

There are two ways to get the first token: `tapirpost token create`, which
writes straight to the database, or the `TAPIRPOST_ADMIN_TOKEN` variable,
which gives the server one admin token from its configuration (at least 32
characters; meant for a Kubernetes Secret).

With `--public-delivery` the two delivery routes need no token at all.

### Filtering, sorting and paging

Both listings (`/v1/content/<type>` and `/v1/types/<type>/entries`) take:

| parameter | example | |
| --- | --- | --- |
| `filter[field]` | `filter[category]=news` | equals |
| `filter[field][op]` | `filter[views][gte]=10` | `eq`, `ne`, `lt`, `lte`, `gt`, `gte`, `in`, `contains` |
| `sort` | `sort=-views,title` | up to three fields, `-` for descending |
| `limit` | `limit=50` | 1 to 100, 20 by default |
| `offset` | `offset=100` | |

Several filters must all hold. `in` takes values separated by commas. `ne`
also keeps entries that have no value for the field. Entries without a value
sort last in both directions. Strings compare byte by byte, so comparisons are
case sensitive and `Z` sorts before `a`. Without `sort` the newest entries
come first.

Besides the fields of the type you can use `id`, `created_at` and
`published_at`, and `updated_at` on the management listing. Unknown parameters
and unknown fields are refused with a 400 instead of being ignored.

## Caching

```bash
tapirpost serve --cache memory                       # in this process
tapirpost serve --cache redis://localhost:6379/0     # Redis or Valkey, shared
```

The cache holds the finished JSON of delivery responses. It does not serve
stale content, and that does not rest on remembering to invalidate anything.
Every content type has a revision in the database. Publishing, unpublishing,
deleting a published entry and changing the type all change it, in the same
transaction as the change itself. The revision is part of every cache key, and
every request reads it from the database before it looks at the cache. After
a publish, the old responses are simply not asked for any more; they expire on
their own (`--cache-ttl`, five minutes by default).

That makes the memory cache correct with several replicas on one PostgreSQL
too: they read the same revision. The price is one small query per request
even on a hit. What the cache saves is the listing query and the encoding,
which is where the time goes: see the [numbers](#benchmarks).

Three things are outside that guarantee, because they turn the database
itself back in time: reading from a replica that lags behind, restoring a
backup (clear the cache, or wait one TTL), and letting a copy of a database
share a cache server with its original.

If the cache server is down, requests are served from the database and the
error is logged. Responses say `X-Cache: HIT` or `MISS`.

Independently of the cache, delivery responses carry an `ETag`, and a request
with a matching `If-None-Match` gets a `304` without a body. The default
`Cache-Control` is `no-cache`: a client may keep a response but has to
revalidate it. `--delivery-cache-control` changes that, for instance to
`public, max-age=60` behind a CDN, and then the CDN decides how stale a page
can be. Everything that is not published content is sent with `no-store`.

## Configuration

Every flag of `tapirpost serve` can also be set through the environment
variable next to it. A flag wins. There is no configuration file.

| flag | variable | default | |
| --- | --- | --- | --- |
| `--addr` | `TAPIRPOST_ADDR` | `:8080` | address to listen on |
| `--db` | `TAPIRPOST_DB` | `sqlite:tapirpost.db` | `sqlite:<file>` or a `postgres://` URL |
| `--db-max-conns` | `TAPIRPOST_DB_MAX_CONNS` | `8` | most database connections kept open |
| `--cache` | `TAPIRPOST_CACHE` | `off` | `off`, `memory`, or a `redis://` or `rediss://` URL |
| `--cache-ttl` | `TAPIRPOST_CACHE_TTL` | `5m` | how long a cached response is kept |
| `--cache-max-bytes` | `TAPIRPOST_CACHE_MAX_BYTES` | `67108864` | size of the memory cache |
| `--admin-token` | `TAPIRPOST_ADMIN_TOKEN` | | an admin token from the configuration; use the variable, a flag shows up in the process list |
| `--public-delivery` | `TAPIRPOST_PUBLIC_DELIVERY` | `false` | serve published content without a token |
| `--delivery-cache-control` | `TAPIRPOST_DELIVERY_CACHE_CONTROL` | `no-cache` | `Cache-Control` of published content |
| `--max-body-bytes` | `TAPIRPOST_MAX_BODY_BYTES` | `1048576` | largest request body |
| `--request-timeout` | `TAPIRPOST_REQUEST_TIMEOUT` | `15s` | time a request may take |
| `--shutdown-delay` | `TAPIRPOST_SHUTDOWN_DELAY` | `0s` | time between failing the readiness probe and closing the listener |
| `--shutdown-timeout` | `TAPIRPOST_SHUTDOWN_TIMEOUT` | `20s` | time running requests get to finish |
| `--log-format` | `TAPIRPOST_LOG_FORMAT` | `text` | `text` or `json` |
| `--log-level` | `TAPIRPOST_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

`tapirpost migrate` brings the database schema up to date and exits. The
server does the same when it starts, so you only need it if you want the
migration to be a step of its own.

## Databases

SQLite is the default: one file, created on first start, through a driver in
pure Go, so the binary stays static and needs no C library. PostgreSQL is one
setting away:

```bash
tapirpost serve --db 'postgres://user:password@localhost:5432/tapirpost?sslmode=disable'
```

Both sit behind the same interface and the same test suite, which pins down
the details that usually differ: how strings sort, where missing values go,
what a page past the end returns.

Entries are stored as JSON, as text in SQLite and as `jsonb` in PostgreSQL.
Filtering and sorting therefore read every entry of the type. That is fine for
thousands of entries and will not be for millions; indexes for chosen fields
are a next step. Listings with the cache on are not affected.

MySQL is not supported. Its usual Go driver is under the MPL-2.0, and this
project keeps to permissively licensed dependencies.

## In a container and on Kubernetes

The [`Dockerfile`](Dockerfile) builds an image that contains the binary and
nothing else. It runs as an unprivileged user, keeps the SQLite file in
`/data`, and has no CA certificates: to verify the TLS certificate of a
database, mount a CA bundle and point `SSL_CERT_FILE` at it.

[`deploy/kubernetes/tapirpost.yaml`](deploy/kubernetes/tapirpost.yaml) is a
Deployment with two replicas on PostgreSQL and a Service. What the server
brings for that:

- all settings come from the environment, secrets included;
- `/healthz` is the liveness probe, `/readyz` the readiness probe, which also
  checks the database;
- on SIGTERM it fails the readiness probe first and keeps serving for
  `--shutdown-delay`, then stops accepting and lets running requests finish;
- replicas that start together migrate the database one at a time;
- logs go to standard error, as JSON with `--log-format json`.

## Benchmarks

Measured on the laptop this was built on: Intel Core Ultra 7 155H, 22 logical
CPUs, 30 GB of RAM, Linux 7.0, Go 1.27.3. SQLite, 1000 published entries of
about 600 bytes, every request with a token.

One request at a time, in the test process, from the handler down to the
database (`make bench`):

| request | cache off | cache in memory |
| --- | ---: | ---: |
| list: filter, sort, page of 20 | 1.84 ms | 20 µs |
| one entry by id | 23 µs | 16 µs |

Over HTTP against the built binary, 32 connections for 10 seconds
(`make bench-http`):

| request | cache | requests/s | p50 | p99 |
| --- | --- | ---: | ---: | ---: |
| list: filter, sort, page of 20 | off | 2029 | 14.57 ms | 36.47 ms |
| list: filter, sort, page of 20 | memory | 27167 | 1.02 ms | 3.24 ms |
| one entry by id | off | 30691 | 915 µs | 2.79 ms |
| one entry by id | memory | 42760 | 594 µs | 2.34 ms |

How to read this. The uncached listing is slow for what it does, about 2 µs
per entry of the type, because it filters and sorts inside JSON without an
index. The cache takes that away. In the HTTP run the load generator shares
the CPU with the server, so those numbers compare the configurations with
each other; they are not what a server does on a machine of its own.

I have not measured PostgreSQL or the Redis-protocol cache yet, and I have not
compared TapirPost with any other CMS: I only publish numbers I measured the
same way.

## Status

| | |
| --- | --- |
| SQLite backend | The whole test suite runs against it, with the race detector. |
| Memory cache | The same, including the tests that publish and unpublish while readers keep the cache warm. |
| Redis-protocol cache | Tested against a Redis-protocol server that runs inside the test process, and in CI against Valkey 9. |
| PostgreSQL backend | The same suite as SQLite runs against PostgreSQL 18 in CI, with the race detector. Not benchmarked yet. |
| Dockerfile, Kubernetes manifest | Written, not built or applied yet. |

Not there yet:

- an admin interface;
- references between entries, media files, localisation, version history;
- webhooks on publish;
- indexes for filtered and sorted fields;
- user accounts or single sign-on (there are only tokens);
- CORS headers, for browsers that call the delivery API directly;
- metrics.

## Development

```bash
make test         # go test -race ./...
make check        # gofmt, go vet, staticcheck
make bench        # the Go benchmarks
make bench-http   # builds the binary and runs the HTTP load
```

The tests that need PostgreSQL or a Redis-protocol server are skipped unless
you point them at one:

```bash
export TAPIRPOST_TEST_POSTGRES_URL='postgres://user:password@127.0.0.1:5432/tapirpost_test?sslmode=disable'
export TAPIRPOST_TEST_REDIS_URL='redis://127.0.0.1:6379/0'
```

Every PostgreSQL test works in a schema of its own and drops it afterwards.

[docs/architecture.md](docs/architecture.md) explains how the code is laid
out and why.

## Licence

[MIT](LICENSE). What this builds on is listed in
[3rdparty_licenses.md](3rdparty_licenses.md).
