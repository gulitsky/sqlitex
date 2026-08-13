# sqlitex

[![Go Reference](https://pkg.go.dev/badge/github.com/gulitsky/sqlitex/v2.svg)](https://pkg.go.dev/github.com/gulitsky/sqlitex/v2)
[![Go Version](https://img.shields.io/github/go-mod/go-version/gulitsky/sqlitex)](go.mod)
[![License](https://img.shields.io/github/license/gulitsky/sqlitex)](LICENSE)

A `database/sql` wrapper for SQLite that sets up WAL, sane pragmas, and
role-appropriate pooling, migrates the schema from a single declaration of it,
and keeps the WAL in check with a background maintenance loop.

It exists because a usable SQLite setup in Go is not `sql.Open` — it is a
dozen pragmas that have to run on *every* pooled connection, a writer pool
limited to one connection so transaction upgrades cannot deadlock, a separate
reader pool that can actually use all cores, something that checkpoints the
WAL so it does not grow without bound, and a way to change a table definition
in a database whose `ALTER TABLE` mostly cannot.

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
//go:embed schema.sql
var schema string

db, err := sqlitex.Open(ctx, "sqlite", "app.db",
	sqlitex.WithBusyTimeout(3*time.Second),
	sqlitex.WithWALAutoCheckpoint(0), // leave all checkpointing to Maintain
)
if err != nil {
	return err
}
defer db.Close()

// Bring the schema in line with schema.sql, or fail before serving anything.
if err := db.Migrate(ctx, schema); err != nil {
	return err
}

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

// The free functions take any *sql.DB, including one you opened yourself.
err = sqlitex.Migrate(ctx, mem, schema)
err = sqlitex.Maintain(ctx, rw)
```

`DB` has exported fields, so a pair can also be assembled by hand when the two
pools need settings that differ beyond the built-in per-role defaults.

## Declarative migrations

You keep one file — the schema you want — and `Migrate` works out how to get
there:

```go
//go:embed schema.sql
var schema string

err := db.Migrate(ctx, schema)
```

There are no numbered migration files and no table recording which ones have
run. The schema the database holds is the only state, so there is nothing that
can disagree with it about what has been applied — a restored backup, a
database three versions behind, and one built this morning all converge to the
same place. Run against a database that already matches, `Migrate` finds
nothing to do and changes nothing.

### How it works

The declaration is executed into a throwaway in-memory database on the same
driver, and what SQLite stored there is compared with what it stored in yours.
Both sides went through the same parser, so this package never parses SQL —
including the statement rewrites a rebuild needs, which come from asking SQLite
to rename a table and reading back the text it wrote.

Changed tables are replaced by [the procedure SQLite
documents](https://www.sqlite.org/lang_altertable.html#otheralter) for schema
changes it cannot make in place: build the new table beside the old one, copy
the columns they have in common, drop the old one, rename the new one into
place. Views and triggers step aside first, because renaming a table makes
SQLite reparse every one of them and any that reads the table being replaced is
invalid at that moment.

Everything happens on a single connection, inside one transaction, with foreign
key enforcement off — the procedure drops and recreates tables that other
tables point at — and `PRAGMA foreign_key_check` before the commit. Either the
whole migration lands or none of it does. `BEGIN IMMEDIATE` takes the write
lock up front, so several processes starting at once serialize instead of
racing, and whichever arrives second finds the work already done.

### What it does on its own

| change | what happens |
| --- | --- |
| new table, index, view, trigger | created |
| index, view or trigger no longer declared | dropped — they hold no data |
| changed index, view or trigger | dropped and recreated |
| new column, nullable or with a default | table rebuilt, existing rows carried over |
| changed type, constraint, `STRICT`, column order | table rebuilt |
| `AUTOINCREMENT` counter | preserved across the rebuild, so identifiers are not reused |

### What it refuses

A refusal is an error naming what it would have done, with every problem
reported at once rather than one per run:

| situation | why |
| --- | --- |
| a table or column is no longer declared | dropping is indistinguishable from renaming — see below |
| a new `NOT NULL` column has no default and the table has rows | nothing in the declaration says what those rows should hold |
| a virtual table is declared differently | the rebuild procedure has no way to build a second one holding the same content |

`WithAllowDrop` permits the first. The second has to be answered in the
declaration, with a default, or in a premigration. So does the third: a virtual
table can be created and dropped from the declaration, but changing one is a
`DROP` and a `CREATE` whose consequences only you know.

Virtual tables are otherwise handled: an FTS5 index keeps its content in shadow
tables (`docs_data`, `docs_idx` and several more) that look like ordinary
tables in the schema, and the comparison leaves them to the virtual table that
owns them.

### Renames

A schema that renames a column declares exactly what a schema that drops one
and adds another declares. No comparison can tell them apart, so a rename is
performed rather than deduced — by a premigration, which runs before the
comparison and leaves it nothing to find:

```go
err := db.Migrate(ctx, schema, sqlitex.WithPremigration(
	func(ctx context.Context, conn *sql.Conn) error {
		var legacy int
		err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM pragma_table_info('users') WHERE name = 'nickname';`).Scan(&legacy)
		if err != nil || legacy == 0 {
			return err
		}

		_, err = conn.ExecContext(ctx, `ALTER TABLE users RENAME COLUMN nickname TO handle;`)
		return err
	}))
```

It runs on every migration and decides for itself whether there is anything to
do — by asking the database what it holds, the way the example does, not by
remembering what it has already done. That is what keeps the database the only
state. It runs inside the migration's transaction, so returning an error rolls
back everything, and it is also where data that cannot be derived from a schema
change belongs: backfills, splitting a column, re-encoding a format.

### Fixtures

Rows the schema takes for granted — reference tables the rest of it points at —
go in `WithFixtures`, which runs once the schema is in place:

```go
//go:embed fixtures.sql
var fixtures string

err := db.Migrate(ctx, schema, sqlitex.WithFixtures(fixtures))
```

They run on every migration, so they have to converge rather than accumulate:
`INSERT OR IGNORE`, `ON CONFLICT DO NOTHING`, or `ON CONFLICT DO UPDATE`. What
they insert is covered by the foreign key check, so seed data pointing at
nothing fails the migration instead of settling into the database.

The first two forms stop writing once the rows are there, which lets a
migration with nothing else to do skip that check. `ON CONFLICT DO UPDATE`
rewrites its rows every run even when the values are identical, and a database
big enough for the check to be slow will feel it on every startup.

### Seeing the plan first

`Plan` reports the statements a migration would run, in order, and applies
none of them — useful in a deployment check, and the honest answer to "what is
this about to do to my database":

```go
stmts, err := sqlitex.Plan(ctx, db.RW, schema)
for _, stmt := range stmts {
	fmt.Println(stmt)
}
```

```sql
DROP VIEW "members";
CREATE TABLE "sqlitex_new_users" (id INTEGER PRIMARY KEY, email TEXT NOT NULL, tier TEXT NOT NULL DEFAULT 'free');
INSERT INTO "sqlitex_new_users" ("id", "email") SELECT "id", "email" FROM "users";
DROP TABLE "users";
ALTER TABLE "sqlitex_new_users" RENAME TO "users";
CREATE UNIQUE INDEX idx_users_email ON users(email);
CREATE VIEW members AS SELECT email FROM users;
```

A premigration is the one thing `Plan` does run, inside a transaction it then
rolls back, because the plan for a database it has yet to touch is not the plan
`Migrate` would apply.

### Other tools' tables

`WithIgnore` takes LIKE patterns for objects the comparison should not see in
either direction — for the tables something else keeps in the same database,
which `WithAllowDrop` would otherwise offer to remove:

```go
err := db.Migrate(ctx, schema, sqlitex.WithAllowDrop(), sqlitex.WithIgnore(`\_litestream\_%`))
```

Objects belonging to an ignored table are ignored with it. Since they are never
touched, they are also not stepped aside during a rebuild, so an ignored view
that reads a declared table will fail one.

### Limits worth knowing before you adopt this

- **Reformatting the declaration rebuilds the table.** The comparison is
  textual, normalized only in how the table name is written, so a new comment
  or changed indentation inside a `CREATE TABLE` reads as a change. It costs a
  rebuild, not correctness, and it happens once: afterwards the stored text
  matches again.
- **A rebuild copies the table and rebuilds its indexes.** On a large table
  that is not instant, and it holds the write lock while it runs.
- **Two versions of an application on one file will fight.** Each pulls the
  schema toward its own declaration, on every start. Migrate from one process,
  or do not overlap deployments.
- **Views and triggers are dropped and recreated whenever any table is
  rebuilt**, even ones unrelated to it. They hold no data, so this is cheap,
  but it does mean their definitions come from the declaration and nowhere
  else. The same goes for a trigger on a view that is replaced: dropping a view
  takes its `INSTEAD OF` triggers with it, so they come back from the
  declaration too.
- **Migration needs SQLite 3.37 or newer** (November 2021), for
  `pragma_table_list`, which is how the shadow tables of a virtual table are
  told apart from ordinary ones. The pooling and maintenance in this package
  have no such requirement.

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

For opening — `Open`, `OpenReadOnly`, `OpenReadWrite`, `OpenMemory`:

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

For migrating — `Migrate` and `Plan`:

| option | effect |
| --- | --- |
| `WithPremigration(fn)` | runs before the comparison; where renames and data changes live |
| `WithFixtures(sql)` | runs after the schema is in place; rows that have to exist |
| `WithAllowDrop()` | permits dropping tables and columns no longer declared |
| `WithIgnore(patterns...)` | leaves matching objects out of the comparison |
| `WithMigrationLogger(l)` | where migration events go; `slog.Default` otherwise |

For maintaining — `Maintain`:

| option | effect |
| --- | --- |
| `WithCheckpointPeriod(d)` | how often to checkpoint; `0` disables |
| `WithOptimizePeriod(d)` | how often to run `PRAGMA optimize`; `0` disables |
| `WithLogger(l)` | where maintenance events go; `slog.Default` otherwise |
| `WithDatabaseName(s)` | the name to log, skipping the query that resolves it |

`WithLogger` and `WithMigrationLogger` do the same thing for different
operations. They are two names because Go has no overloading and each belongs
to a different set of options.

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
`Maintain`, and every option keep their v1 signatures. `DB`, `Migrate` and
`Plan` are new.

## License

[MIT](LICENSE)
