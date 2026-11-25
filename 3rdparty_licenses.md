# Third-party components

Components this repository depends on or refers to. None of their code is
copied into the repository: the Go modules are fetched by the Go toolchain,
and the rest is used by CI or to build the container image.

| source | name | license type | license link |
| --- | --- | --- | --- |
| https://github.com/golang/go | Go (toolchain and standard library, compiled into the binary) | BSD-3-Clause | https://github.com/golang/go/blob/master/LICENSE |
| https://github.com/ncruces/go-sqlite3 | github.com/ncruces/go-sqlite3 (SQLite driver) | MIT | https://github.com/ncruces/go-sqlite3/blob/main/LICENSE |
| https://github.com/ncruces/go-sqlite3-wasm | github.com/ncruces/go-sqlite3-wasm/v6 (SQLite translated to Go, required by the SQLite driver) | MIT-0 | https://github.com/ncruces/go-sqlite3-wasm/blob/main/LICENSE |
| https://sqlite.org | SQLite (contained in github.com/ncruces/go-sqlite3-wasm/v6) | blessing | https://sqlite.org/copyright.html |
| https://github.com/ncruces/julianday | github.com/ncruces/julianday (required by the SQLite driver) | MIT | https://github.com/ncruces/julianday/blob/main/LICENSE |
| https://github.com/jackc/pgx | github.com/jackc/pgx/v5 (PostgreSQL driver) | MIT | https://github.com/jackc/pgx/blob/master/LICENSE |
| https://github.com/jackc/pgpassfile | github.com/jackc/pgpassfile (required by pgx) | MIT | https://github.com/jackc/pgpassfile/blob/master/LICENSE |
| https://github.com/jackc/pgservicefile | github.com/jackc/pgservicefile (required by pgx) | MIT | https://github.com/jackc/pgservicefile/blob/master/LICENSE |
| https://github.com/jackc/puddle | github.com/jackc/puddle/v2 (required by pgx) | MIT | https://github.com/jackc/puddle/blob/master/LICENSE |
| https://github.com/redis/go-redis | github.com/redis/go-redis/v9 (Redis-protocol client) | BSD-2-Clause | https://github.com/redis/go-redis/blob/master/LICENSE |
| https://github.com/cespare/xxhash | github.com/cespare/xxhash/v2 (required by go-redis) | MIT | https://github.com/cespare/xxhash/blob/main/LICENSE.txt |
| https://github.com/uber-go/atomic | go.uber.org/atomic (required by go-redis) | MIT | https://github.com/uber-go/atomic/blob/master/LICENSE.txt |
| https://github.com/golang/sync | golang.org/x/sync (required by pgx) | BSD-3-Clause | https://github.com/golang/sync/blob/master/LICENSE |
| https://github.com/golang/sys | golang.org/x/sys (required by the SQLite driver and go-redis) | BSD-3-Clause | https://github.com/golang/sys/blob/master/LICENSE |
| https://github.com/golang/text | golang.org/x/text (required by pgx) | BSD-3-Clause | https://github.com/golang/text/blob/master/LICENSE |
| https://github.com/alicebob/miniredis | github.com/alicebob/miniredis/v2 (tests only: a Redis-protocol server inside the test process) | MIT | https://github.com/alicebob/miniredis/blob/master/LICENSE |
| https://github.com/yuin/gopher-lua | github.com/yuin/gopher-lua (tests only, required by miniredis) | MIT | https://github.com/yuin/gopher-lua/blob/master/LICENSE |
| https://github.com/dominikh/go-tools | honnef.co/go/tools (staticcheck, run by CI and `make check`) | MIT | https://github.com/dominikh/go-tools/blob/master/LICENSE |
| https://github.com/actions/checkout | actions/checkout (CI only) | MIT | https://github.com/actions/checkout/blob/main/LICENSE |
| https://github.com/actions/setup-go | actions/setup-go (CI only) | MIT | https://github.com/actions/setup-go/blob/main/LICENSE |
| https://github.com/postgres/postgres | PostgreSQL (container image `postgres`, a service in CI) | PostgreSQL | https://www.postgresql.org/about/licence/ |
| https://github.com/valkey-io/valkey | Valkey (container image `valkey/valkey`, a service in CI) | BSD-3-Clause | https://github.com/valkey-io/valkey/blob/unstable/COPYING |
| https://github.com/docker-library/golang | Go container image `golang` (build stage of the Dockerfile) | BSD-3-Clause | https://github.com/docker-library/golang/blob/master/LICENSE |

The table lists every module in `go.mod`. The module graph behind them holds
more modules that are needed neither to build nor to test this repository
(test dependencies of dependencies); none of those is under a copyleft
licence either.
