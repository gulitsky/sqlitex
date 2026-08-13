# sqlitex

[![Go Reference](https://pkg.go.dev/badge/github.com/gulitsky/sqlitex/v2.svg)](https://pkg.go.dev/github.com/gulitsky/sqlitex/v2)
[![Go Version](https://img.shields.io/github/go-mod/go-version/gulitsky/sqlitex)](go.mod)
[![License](https://img.shields.io/github/license/gulitsky/sqlitex)](LICENSE)

A `database/sql` wrapper for SQLite that sets up WAL, sane pragmas, and
role-appropriate pooling, and keeps the WAL in check with a background
maintenance loop.

It exists because a usable SQLite setup in Go is not `sql.Open` — it is a
dozen pragmas that have to run on *every* pooled connection, a writer pool
limited to one connection so transaction upgrades cannot deadlock, a separate
reader pool that can actually use all cores, and something that checkpoints the
WAL so it does not grow without bound.

Written for [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), but the
driver name is a parameter — any driver registered under `database/sql` that
understands SQLite file URIs will do.

Requires Go 1.26.5 or newer.

## Install

```bash
go get github.com/gulitsky/sqlitex/v2
```

## Quick start

`Open` returns a matched pair of pools over one file: a single-connection
writer and a concurrent reader.

```go
db, err := sqlitex.Open(ctx, "sqlite", "app.db",
	sqlitex.WithBusyTimeout(3*time.Second),
	sqlitex.WithWALAutoCheckpoint(0), // leave all checkpointing to Maintain
)
if err != nil {
	return err
}
defer db.Close()

go func() {
	if err := db.Maintain(ctx); err != nil {
		logger.Error("maintenance stopped", "error", err)
	}
}()

_, err = db.RW.ExecContext(ctx, "INSERT INTO items (name) VALUES (?);", name)

var count int
err = db.RO.QueryRowContext(ctx, "SELECT count(*) FROM items;").Scan(&count)
```

`RW` and `RO` are plain `*sql.DB`, so everything in `database/sql` works as
usual — pick `RW` for statements that write, `RO` for the ones that only read.

Opening the pair in this order matters and is why `Open` takes a context: the
pools are lazy, and a read-only connection can create neither the database file
nor its WAL. `Open` forces the read-write pool to connect first, so the
read-only pool always finds something to attach to.

## Single-role pools

The pair is a convenience, not a requirement. A read replica, a write-only
worker, and a test on an in-memory database each want just one pool:

```go
ro, err := sqlitex.OpenReadOnly("sqlite", "app.db")
rw, err := sqlitex.OpenReadWrite("sqlite", "app.db")
mem, err := sqlitex.OpenMemory("sqlite") // a fresh, distinct database per call

err = sqlitex.Maintain(ctx, rw) // the free function takes any *sql.DB
```

`DB` has exported fields, so a pair can also be assembled by hand when the two
pools need settings that differ beyond the built-in per-role defaults.

## Maintenance

`Maintain` runs until the context is canceled, doing two things on a timer:

| task | default period | option |
| --- | --- | --- |
| `PRAGMA wal_checkpoint(PASSIVE)` | 1 minute | `WithCheckpointPeriod` |
| `PRAGMA optimize` | 4 hours | `WithOptimizePeriod` |

A zero period disables that task. On cancellation it runs a final
`wal_checkpoint(TRUNCATE)` — with its own uncancelable context, so shutdown
does not leave the WAL behind — and only then returns.

While it runs it takes ownership of checkpointing: it sets
`wal_autocheckpoint=0` and restores the previous value on the way out, so a
database that outlives its maintenance loop keeps checkpointing on its own.
Since that pragma is per-connection, it is reasserted before every checkpoint,
in case the pool has since replaced the connection. To silence automatic
checkpoints across the entire pool, pass `WithWALAutoCheckpoint(0)` at open
time.

Events go to `slog.Default()` unless `WithLogger` says otherwise, tagged with a
`database` attribute. Pools opened through `Open` know their own path; anything
else resolves the name with one `PRAGMA database_list` query at startup, which
`WithDatabaseName` skips.

## What gets configured

Applied to every connection of every pool:

| pragma | value |
| --- | --- |
| `journal_mode` | `WAL` |
| `synchronous` | `NORMAL` |
| `foreign_keys` | `on` |
| `busy_timeout` | `5000` |
| `temp_store` | `MEMORY` |
| `mmap_size` | 32 GiB (clamped to the address space on 32-bit systems) |

And per role:

| | read-write | read-only |
| --- | --- | --- |
| URI mode | `rwc` | `ro` |
| transaction lock | `immediate` | `deferred` |
| `cache_size` | `-64000` (≈62 MiB) | `-16000` (≈16 MiB) |
| also sets | `analysis_limit`, `optimize` | `query_only=yes` |
| max connections | 1 | `GOMAXPROCS`, clamped to 2–8 |
| idle timeout | 1 hour | 15 minutes |

The single write connection is deliberate: it serializes writers in Go, where
they wait politely, instead of in SQLite, where a deferred transaction that
upgrades to a write can only fail with `SQLITE_BUSY`. `_txlock=immediate` takes
the write lock up front for the same reason.

Pragmas are applied through a custom `driver.Connector`, so they run when each
connection is created rather than on whichever connection happened to serve a
setup query — a pool that grows later stays configured.

## Options

| option | effect |
| --- | --- |
| `WithBusyTimeout(d)` | `busy_timeout`, how long a blocked statement waits |
| `WithCacheSizeKiB(n)` | `cache_size`, per connection |
| `WithMemoryMapSize(n)` | `mmap_size`, in bytes |
| `WithWALAutoCheckpoint(n)` | `wal_autocheckpoint`, in pages; `0` disables |
| `WithPragma(name, value)` | anything else |

Options passed to `Open` apply to both pools. The per-role defaults above still
differ, so overriding one of them — `cache_size`, say — overrides it for both;
use the single-role constructors when the two need genuinely different values.

`WithPragma` interpolates its arguments into a `PRAGMA name = value;`
statement, because SQLite cannot bind pragma parameters. Characters that could
end the statement are rejected, but it is still a place for trusted
compile-time values only — never route user input into it.

## Migrating from v1

The import path gains a `/v2`, and one function changed:

```go
// v1
db, err := sqlitex.Open("sqlite", "file:app.db?mode=rwc", "PRAGMA foreign_keys = on;")

// v2 — the name now belongs to the pool pair; per-connection statements are
// an implementation detail of the constructors above.
db, err := sqlitex.Open(ctx, "sqlite", "app.db", sqlitex.WithPragma("foreign_keys", "on"))
```

Everything else is unchanged: `OpenReadOnly`, `OpenReadWrite`, `OpenMemory`,
`Maintain`, and every option keep their v1 signatures.

## License

[MIT](LICENSE)
