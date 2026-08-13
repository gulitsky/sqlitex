package sqlitex_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

func recorder() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	return logger, &buf
}

func TestMigrateLogsWhatItDid(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY);`)

	logger, log := recorder()

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE INDEX idx_users_email ON users(email);
`

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithMigrationLogger(logger)); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	out := log.String()
	for _, want := range []string{
		`level=INFO`,
		`msg="database schema migrated"`,
		`database=:memory:`,
		`statements=5`,
		`msg="applying statement"`,
		`CREATE INDEX idx_users_email`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}
}

// A migration with nothing to do is not an event worth an info line on every
// startup.
func TestMigrateIsQuietWhenUpToDate(t *testing.T) {
	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY);`

	db := setup(t, schema)

	logger, log := recorder()

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithMigrationLogger(logger)); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	out := log.String()
	if strings.Contains(out, "level=INFO") {
		t.Errorf("a migration with nothing to do logged at info level:\n%s", out)
	}
	if !strings.Contains(out, `msg="database schema is up to date"`) {
		t.Errorf("log does not say the schema was already current:\n%s", out)
	}
}

// Tables another tool keeps in the same database survive a migration that is
// otherwise allowed to drop whatever is not declared.
func TestMigrateIgnoresMatchingObjects(t *testing.T) {
	statements := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE _litestream_seq (id INTEGER PRIMARY KEY, seq INTEGER);`,
		`CREATE TABLE _litestream_lock (id INTEGER);`,
		`CREATE INDEX idx_litestream ON _litestream_seq(seq);`,
		`INSERT INTO _litestream_seq (seq) VALUES (7);`,
	}

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY);`

	// Without the option, they are exactly what WithAllowDrop drops.
	db := setup(t, statements...)

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithAllowDrop()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name LIKE '\_litestream%' ESCAPE '\';`); got != 0 {
		t.Fatalf("%d objects survived without WithIgnore, want them dropped", got)
	}

	// With it, they are none of the migration's business.
	ignored := setup(t, statements...)

	err := sqlitex.Migrate(t.Context(), ignored, schema,
		sqlitex.WithAllowDrop(),
		sqlitex.WithIgnore(`\_litestream\_%`),
	)
	if err != nil {
		t.Fatalf("Migrate with WithIgnore failed: %v", err)
	}

	if got := scalar[int](t, ignored, `SELECT count(*) FROM _litestream_seq;`); got != 1 {
		t.Errorf("rows in the ignored table = %d, want 1", got)
	}
	// The index belongs to an ignored table, so it is ignored with it.
	if got := scalar[int](t, ignored, `SELECT count(*) FROM sqlite_schema WHERE name = 'idx_litestream';`); got != 1 {
		t.Error("the index on an ignored table was dropped")
	}
	if got := scalar[int](t, ignored, `SELECT count(*) FROM sqlite_schema WHERE name = '_litestream_lock';`); got != 1 {
		t.Error("an ignored table was dropped")
	}

	// And the migration still has nothing to say about them afterwards.
	got, err := sqlitex.Plan(t.Context(), ignored, schema,
		sqlitex.WithAllowDrop(),
		sqlitex.WithIgnore(`\_litestream\_%`),
	)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("plan =\n%s\nwant nothing to do", strings.Join(got, "\n"))
	}
}

// The underscore is a LIKE wildcard, and the escape that makes it literal has
// to reach SQLite.
func TestMigrateIgnorePatternsEscape(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE axbxc (id INTEGER PRIMARY KEY);`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY);`

	// "a_b_c" with the underscores escaped matches only the literal name.
	if err := sqlitex.Migrate(t.Context(), db, schema,
		sqlitex.WithAllowDrop(),
		sqlitex.WithIgnore(`a\_b\_c`),
	); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'axbxc';`); got != 0 {
		t.Error("an escaped underscore matched an arbitrary character")
	}
}

func TestMigrateRejectsEmptyIgnorePattern(t *testing.T) {
	db := setup(t)

	if _, err := sqlitex.Plan(t.Context(), db, `CREATE TABLE t (x TEXT);`, sqlitex.WithIgnore("  ")); err == nil {
		t.Error("expected an empty ignore pattern to be rejected")
	}
}

func TestMigrateRejectsNilLogger(t *testing.T) {
	db := setup(t)

	if err := sqlitex.Migrate(t.Context(), db, `CREATE TABLE t (x TEXT);`, sqlitex.WithMigrationLogger(nil)); err == nil {
		t.Error("expected a nil logger to be rejected")
	}
}
