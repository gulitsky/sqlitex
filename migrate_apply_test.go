package sqlitex_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

func TestMigrateCreatesAndConverges(t *testing.T) {
	db := setup(t)

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;
CREATE INDEX idx_users_email ON users(email);
`

	if err := sqlitex.Migrate(t.Context(), db, schema); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO users (email) VALUES ('a@example.com');`); err != nil {
		t.Fatalf("insert into the migrated table: %v", err)
	}

	// Running it again has nothing to do, and says so by succeeding.
	if err := sqlitex.Migrate(t.Context(), db, schema); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}

	if got := scalar[int](t, db, `SELECT count(*) FROM users;`); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}

	// A change carries the data across.
	const changed = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL, tier TEXT NOT NULL DEFAULT 'free') STRICT;
CREATE INDEX idx_users_email ON users(email);
`

	if err := sqlitex.Migrate(t.Context(), db, changed); err != nil {
		t.Fatalf("Migrate to the changed schema failed: %v", err)
	}

	if got := scalar[string](t, db, `SELECT email FROM users;`); got != "a@example.com" {
		t.Errorf("email = %q, want it to survive the rebuild", got)
	}
	if got := scalar[string](t, db, `SELECT tier FROM users;`); got != "free" {
		t.Errorf("tier = %q, want the declared default", got)
	}

	if plan := plan(t, db, changed); len(plan) != 0 {
		t.Errorf("plan after migrating =\n%s\nwant nothing to do", strings.Join(plan, "\n"))
	}
}

// Everything or nothing: a statement that fails takes the whole migration with
// it, including the table rebuild that had already succeeded.
func TestMigrateIsAtomic(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`,
		`INSERT INTO users (email) VALUES ('duplicate'), ('duplicate');`,
	)

	// The rebuild for the new column succeeds; the unique index over the
	// duplicates that follow it cannot.
	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tier TEXT);
CREATE UNIQUE INDEX idx_users_email ON users(email);
`

	err := sqlitex.Migrate(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected the unique index over duplicate values to fail")
	}
	if !strings.Contains(err.Error(), "idx_users_email") {
		t.Errorf("error does not name the statement that failed: %v", err)
	}

	// The database is exactly as it was.
	if got := scalar[int](t, db, `SELECT count(*) FROM users;`); got != 2 {
		t.Errorf("rows = %d, want both still there", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM pragma_table_info('users') WHERE name = 'tier';`); got != 0 {
		t.Error("the rolled-back rebuild left its new column behind")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'sqlitex_new_%';`); got != 0 {
		t.Error("the rolled-back rebuild left its temporary table behind")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'idx_users_email';`); got != 0 {
		t.Error("the failed index exists")
	}
}

// Enforcement is off only for the duration, and comes back either way.
func TestMigrateRestoresForeignKeys(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`)

	if got := scalar[int](t, db, `PRAGMA foreign_keys;`); got != 1 {
		t.Fatalf("foreign_keys = %d before migrating, want the pool default", got)
	}

	if err := sqlitex.Migrate(t.Context(), db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tier TEXT);`); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `PRAGMA foreign_keys;`); got != 1 {
		t.Errorf("foreign_keys = %d after a migration, want 1", got)
	}

	// And after one that fails.
	if err := sqlitex.Migrate(t.Context(), db, `CREATE TABLE users (`); err == nil {
		t.Fatal("expected an invalid declaration to fail")
	}
	if got := scalar[int](t, db, `PRAGMA foreign_keys;`); got != 1 {
		t.Errorf("foreign_keys = %d after a failed migration, want 1", got)
	}
}

// A migration that would orphan rows is caught before it commits, by the check
// that stands in for the enforcement switched off around it.
func TestMigrateRejectsBrokenForeignKeys(t *testing.T) {
	db := setup(t,
		`CREATE TABLE orgs (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, org INTEGER REFERENCES orgs(id));`,
		`INSERT INTO orgs (id) VALUES (1);`,
		`INSERT INTO users (org) VALUES (1);`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, org INTEGER REFERENCES orgs(id));`

	err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithAllowDrop())
	if err == nil {
		t.Fatal("expected dropping a table that is still referenced to be rejected")
	}
	if !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("error does not explain itself: %v", err)
	}

	if got := scalar[int](t, db, `SELECT count(*) FROM orgs;`); got != 1 {
		t.Error("the rejected migration dropped the table anyway")
	}
}

func TestDBMigrate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close() //nolint:errcheck // asserted by TestClose

	const schema = `CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL) STRICT;`

	if err := db.Migrate(t.Context(), schema); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if _, err := db.RW.ExecContext(t.Context(), `INSERT INTO items (name) VALUES ('widget');`); err != nil {
		t.Fatalf("write to the migrated database: %v", err)
	}

	// The read-only pool sees the migrated schema through its own connections.
	var name string
	if err := db.RO.QueryRowContext(t.Context(), `SELECT name FROM items;`).Scan(&name); err != nil {
		t.Fatalf("read from the migrated database: %v", err)
	}
	if name != "widget" {
		t.Errorf("name = %q", name)
	}

	if err := db.Migrate(t.Context(), schema); err != nil {
		t.Errorf("second Migrate failed: %v", err)
	}
}

// Two pools over one file, migrating at once: BEGIN IMMEDIATE serializes them,
// and whichever arrives second finds the work already done.
func TestMigrateConcurrently(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;
CREATE INDEX idx_users_email ON users(email);
`

	const writers = 4

	errs := make(chan error, writers)
	for range writers {
		go func() {
			db, err := sqlitex.OpenReadWrite("sqlite", dbPath)
			if err != nil {
				errs <- err
				return
			}
			defer db.Close() //nolint:errcheck // asserted by TestClose

			errs <- sqlitex.Migrate(t.Context(), db, schema)
		}()
	}

	for range writers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Migrate failed: %v", err)
		}
	}

	db, err := sqlitex.OpenReadWrite("sqlite", dbPath)
	if err != nil {
		t.Fatalf("OpenReadWrite failed: %v", err)
	}
	defer db.Close() //nolint:errcheck // asserted by TestClose

	// Exactly one of each, not four attempts worth of leftovers.
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name IN ('users', 'idx_users_email');`); got != 2 {
		t.Errorf("schema objects = %d, want 2", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'sqlitex_new_%';`); got != 0 {
		t.Error("a temporary table survived")
	}
}

func scalar[T any](t *testing.T, db *sql.DB, query string) T {
	t.Helper()

	var v T
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}

	return v
}

// Everything at once: a rebuilt table under a view, a trigger, an index that
// changes, a foreign key pointing in from a table that is not touched, an
// AUTOINCREMENT counter that must not go backwards, and rows that must come
// out the other side.
func TestMigrateEndToEnd(t *testing.T) {
	db := setup(t,
		`CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT);`,
		`CREATE TABLE users (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			org   INTEGER REFERENCES orgs(id),
			email TEXT NOT NULL
		);`,
		`CREATE INDEX idx_users_email ON users(email);`,
		`CREATE VIEW members AS SELECT email FROM users;`,
		`CREATE TRIGGER stamp AFTER INSERT ON orgs BEGIN SELECT 1; END;`,
		`INSERT INTO orgs (name) VALUES ('acme');`,
		`INSERT INTO users (org, email) VALUES (1, 'a@example.com'), (1, 'b@example.com'), (1, 'c@example.com');`,
		`DELETE FROM users WHERE id > 1;`,
	)

	const schema = `
CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT);
CREATE TABLE users (
			id    INTEGER PRIMARY KEY AUTOINCREMENT,
			org   INTEGER REFERENCES orgs(id),
			email TEXT NOT NULL,
			tier  TEXT NOT NULL DEFAULT 'free'
		);
CREATE UNIQUE INDEX idx_users_email ON users(email);
CREATE VIEW members AS SELECT email FROM users;
CREATE TRIGGER stamp AFTER INSERT ON orgs BEGIN SELECT 1; END;
`

	if err := sqlitex.Migrate(t.Context(), db, schema); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[int](t, db, `SELECT count(*) FROM users;`); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
	if got := scalar[string](t, db, `SELECT tier FROM users;`); got != "free" {
		t.Errorf("tier = %q, want the declared default", got)
	}

	// The high-water mark did not go backwards, so identifiers that were handed
	// out are not handed out again.
	if got := scalar[int](t, db, `SELECT seq FROM sqlite_sequence WHERE name = 'users';`); got != 3 {
		t.Errorf("sequence = %d, want 3", got)
	}

	// The rebuild kept its own foreign key and left the table pointing at it
	// alone.
	if got := scalar[string](t, db, `SELECT sql FROM sqlite_schema WHERE name = 'users';`); !strings.Contains(got, "REFERENCES orgs(id)") {
		t.Errorf("rebuilt table lost its foreign key: %s", got)
	}
	if got := scalar[string](t, db, `SELECT sql FROM sqlite_schema WHERE name = 'orgs';`); strings.Contains(got, "sqlitex_new") {
		t.Errorf("rebuild leaked its temporary name into another table: %s", got)
	}

	for _, name := range []string{"members", "stamp", "idx_users_email"} {
		if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = '`+name+`';`); got != 1 {
			t.Errorf("%q did not come back", name)
		}
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM members;`); got != 1 {
		t.Errorf("view returns %d rows, want 1", got)
	}

	if got := plan(t, db, schema); len(got) != 0 {
		t.Errorf("plan after migrating =\n%s\nwant nothing to do", strings.Join(got, "\n"))
	}
}

func TestMigrateDrops(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT, email TEXT);`,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY);`,
		`CREATE INDEX idx_sessions ON sessions(id);`,
		`INSERT INTO users (nickname, email) VALUES ('nick', 'a@example.com');`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithAllowDrop()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[string](t, db, `SELECT email FROM users;`); got != "a@example.com" {
		t.Errorf("email after dropping a column = %q", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name IN ('sessions', 'idx_sessions');`); got != 0 {
		t.Errorf("%d undeclared objects survived", got)
	}
}
