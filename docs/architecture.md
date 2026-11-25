# Architecture

TapirPost is one process: an HTTP server in front of a SQL database, with an
optional cache for what it serves to readers. This page explains how the code
is laid out, which way the dependencies point and why a few things are the way
they are.

## Layout

```
cmd/tapirpost/          main: reads the configuration and wires everything by hand
internal/config/        flags and environment variables
internal/content/       the domain: types, fields, entries, tokens, queries, the service
internal/store/         content.Store on database/sql, written once for both databases
internal/store/sqlite/      opens SQLite, its migrations and dialect
internal/store/postgres/    opens PostgreSQL, its migrations and dialect
internal/store/storetest/   the conformance suite every store has to pass
internal/cache/         the memory cache and the Redis-protocol cache
internal/cache/cachetest/   the conformance suite every cache has to pass
internal/httpapi/       routes, tokens, errors, the delivery cache, openapi.json
internal/testenv/       finds PostgreSQL and Valkey for the tests that need them
tools/bench/            the HTTP load generator behind the numbers in the README
deploy/kubernetes/      an example Deployment and Service
```

Packages are named after what they provide, not after a layer. There is no
`models`, `services` or `utils`.

## Dependencies

```
cmd/tapirpost --> config
              --> httpapi --> content
              --> store/sqlite, store/postgres --> store --> content
              --> cache
```

`content` imports nothing from this module. It declares the `Store` interface
it needs, and `store` implements it. `httpapi` declares the two-method `Cache`
interface it needs, and `cache` implements it without knowing about HTTP. The
interfaces live with their consumer, so the domain never depends on a database
driver and the HTTP layer never depends on a cache client.

Nothing is injected by a framework. `main` builds the store, the cache, the
service and the API in that order and passes each to the next.

## The domain

A content type is a named list of typed fields. An entry of a type has two
copies of its values:

- the draft (`Data`), which editors change;
- the published copy (`Published`), which the delivery API serves.

Publishing copies the draft over the published copy. Editing a published entry
therefore never changes what readers see until it is published again, and
unpublishing removes the published copy while the draft stays.

A draft may be unfinished: required fields are only enforced on publish. Every
other rule (types, lengths, ranges, patterns, unknown fields) is enforced on
every write, so what is stored always fits the schema it was written under.

Values are stored as one JSON object per copy, in canonical form: known fields
only, no nulls, timestamps in UTC at a fixed width, keys sorted. A fixed-width
timestamp compares as text in the same order as time, which is what lets both
databases filter and sort on it without parsing it.

### Two races, closed with version numbers

The service validates in Go and then writes. Between the two, someone else may
change what the validation relied on:

- the content type may change (a field is removed, or becomes required);
- the draft may change while it is being published.

Every write therefore names the schema version it was validated against, and a
publish also names the entry version it validated. The store compares them
inside the write transaction and answers `ErrConflict` when they have moved.
The client gets a 409 and retries. In PostgreSQL the transaction holds a row
lock on the content type for this; SQLite serialises writers anyway.

## The store

`internal/store` contains every query once, written against `database/sql`.
The two databases differ in a handful of fragments, which a `Dialect` supplies:

| | SQLite | PostgreSQL |
| --- | --- | --- |
| placeholders | `?` | `$1`, `$2`, ... |
| entry values | JSON as text | `jsonb` |
| read a field | `json_extract` | `->>`, or `->` for numbers and booleans |
| string order | byte order (the default) | byte order (`COLLATE "C"`) |
| remove a field | `json_remove` | the `-` operator |
| lock a type during a write | not needed, writers are serialised | `FOR SHARE` / `FOR UPDATE` |
| serialise migrations | the write lock | `pg_advisory_lock` |

Two details in that table are there on purpose:

- Strings compare byte by byte in both databases. Otherwise the same query
  could return another order depending on the locale of a PostgreSQL cluster.
- In PostgreSQL numbers and booleans are compared as `jsonb`, without a cast.
  A cast such as `(data->>'n')::bigint` raises an error when it meets text,
  and all entries share one table: a row of another content type may keep
  text under the same key, and nothing promises that the `type = $1` condition
  is evaluated before the cast.

There is no ORM and no query builder dependency. Listing queries are assembled
from fragments in `entries.go`; values, field names included, are always bound
as parameters.

Filtering and sorting read every entry of the type, because the values live
inside JSON. That is a deliberate first step: it needs no schema change per
content type. Indexes for chosen fields are the obvious next one.

### Migrations

Each backend carries numbered SQL files, embedded in the binary. On start the
store applies the ones the database has not seen, each in a transaction
together with the record of having run. Several processes may start at once:
PostgreSQL takes an advisory lock, and on both databases the check "has this
one run?" happens inside the transaction. A database that a newer version has
already migrated is refused instead of being used with the wrong schema.

### One suite for both

`storetest` is written against `content.Store` only. The SQLite and PostgreSQL
packages both run it, so a difference between the databases shows up as a
failing test and not as a surprise in production. It covers the listing
semantics in detail: every operator per field kind, missing values, ordering
of ties, pages past the end, and rows of another type that hold text where
this type holds numbers.

## The cache

The delivery API can keep its encoded responses in a cache. The requirement
was that it never serves something older than the last publish, and the design
makes that a property of the keys rather than of an invalidation step that
could be forgotten or lost:

1. Every content type has a revision. Every change to what is published
   (publish, unpublish, deleting a published entry, changing the type) changes
   the revision in the same database transaction as the change itself.
2. The revision is part of every cache key.
3. A request reads the revision from the database first, then looks in the
   cache, and on a miss reads the content and stores the response under the
   revision it read at the start.

After a publish there is no key left under which an old response could be
found. This holds for one process and for many, with a cache per process or
one shared server, because the revision comes from the database they share.
The time to live only decides how long unreachable leftovers take up space.

The order in step 3 matters. If a request read the content first and the
revision afterwards, a publish in between would let it store old content under
the new revision. `TestCacheRaceWithPublish` holds a request at exactly that
point to prove the order is right.

A revision is the id of the type followed by a counter. The counter alone
would start over when a type is deleted and created again under its old name,
and then repeat values the cache may still hold.

What it costs: one primary key lookup per request, even on a hit. What it
saves: the listing query and the JSON encoding.

What is outside the argument: anything that makes the database itself go back
in time. A read replica that lags behind, a restore from a backup, or a copy
of the database that shares a cache server with its original.

A cache that fails does not fail requests. Errors are logged and the response
comes from the database. After an error the Redis-protocol cache is left alone
for a second, so that an outage costs one timeout and not one per request.

## HTTP

`net/http` with the routing patterns of the standard library, no router
dependency. Handlers return an error, and one function (`classify` in
`errors.go`) turns errors from the rest of the program into a status and a
JSON body. Errors nobody classified become a 500 with a fixed message; their
text goes to the log only.

Request bodies are limited in size and decoded strictly: an unknown property
is an error, so a typo is not silently ignored. Listings refuse unknown query
parameters for the same reason, and because every distinct parameter would
otherwise take a cache entry of its own.

Every request runs under a deadline, carried by `context.Context` into every
database and cache call.

Shutdown follows what a load balancer needs: on SIGTERM the readiness probe
fails first while requests are still served (`--shutdown-delay`), then the
listener closes and the requests in flight get `--shutdown-timeout` to finish.

`openapi.json` is written by hand. A test compares its paths and methods with
the route table, in both directions, and follows every `$ref`.

## What is deliberately not here

- No admin interface. The API is the product in this version.
- No user accounts or sessions: tokens only.
- No references between entries, no media, no localisation, no version history.
- No GraphQL.
- No plugin system and no scripting.
