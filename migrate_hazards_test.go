package sqlitex_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

// Dropping a view takes its INSTEAD OF triggers with it, so a plan that drops
// both would fail on the second one — and a view that is merely replaced still
// has to put its triggers back.
func TestMigrateViewTriggersSurviveARebuild(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`,
		`CREATE VIEW members AS SELECT id FROM users;`,
		`CREATE TRIGGER members_insert INSTEAD OF INSERT ON members BEGIN SELECT 1; END;`,
	)

	// A table change cycles every view and trigger.
	const rebuilt = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tier TEXT);
CREATE VIEW members AS SELECT id FROM users;
CREATE TRIGGER members_insert INSTEAD OF INSERT ON members BEGIN SELECT 1; END;
`

	if err := sqlitex.Migrate(t.Context(), db, rebuilt); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'members_insert';`); got != 1 {
		t.Error("the trigger on the view did not come back after a table rebuild")
	}

	// And so does replacing the view on its own, with no table touched.
	const replaced = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, tier TEXT);
CREATE VIEW members AS SELECT id, email FROM users;
CREATE TRIGGER members_insert INSTEAD OF INSERT ON members BEGIN SELECT 1; END;
`

	if err := sqlitex.Migrate(t.Context(), db, replaced); err != nil {
		t.Fatalf("Migrate over a changed view failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'members_insert';`); got != 1 {
		t.Error("replacing a view dropped the trigger that belongs to it")
	}

	if got := plan(t, db, replaced); len(got) != 0 {
		t.Errorf("plan after migrating =\n%s\nwant nothing to do", strings.Join(got, "\n"))
	}
}

// SQLite matches identifiers case-insensitively, so a declaration that spells
// one differently is not declaring a different object.
func TestMigrateMatchesIdentifiersCaseInsensitively(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, Name TEXT);`,
		`INSERT INTO users (Name) VALUES ('kept');`,
	)

	// Same table, same column, different spelling — plus one real change so
	// that the rebuild actually runs and has to carry the column over.
	const schema = `CREATE TABLE Users (id INTEGER PRIMARY KEY, name TEXT, tier TEXT);`

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithAllowDrop()); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[string](t, db, `SELECT name FROM users;`); got != "kept" {
		t.Errorf("column value = %q, want a case-only difference to carry the data over", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM users;`); got != 1 {
		t.Errorf("rows = %d, want the table not to have been dropped and recreated", got)
	}
}

// A virtual table keeps its content in shadow tables that look like ordinary
// ones. They belong to it and are none of the migration's business.
func TestMigrateHandlesVirtualTables(t *testing.T) {
	db := setup(t)

	const schema = `
CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT);
CREATE VIRTUAL TABLE docs USING fts5(body);
`

	if err := sqlitex.Migrate(t.Context(), db, schema); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO docs (body) VALUES ('hello world');`); err != nil {
		t.Fatalf("write to the virtual table: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM docs WHERE docs MATCH 'hello';`); got != 1 {
		t.Errorf("full-text search returns %d rows, want 1", got)
	}

	// Converges, rather than offering to create the shadow tables again.
	if got := plan(t, db, schema); len(got) != 0 {
		t.Errorf("plan after migrating =\n%s\nwant nothing to do", strings.Join(got, "\n"))
	}

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithAllowDrop()); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM docs;`); got != 1 {
		t.Error("the shadow tables of the virtual table were disturbed")
	}
}

// The rebuild procedure does not apply to a virtual table, so a changed one is
// refused rather than attempted.
func TestMigrateRefusesToRebuildAVirtualTable(t *testing.T) {
	db := setup(t, `CREATE VIRTUAL TABLE docs USING fts5(body);`)

	const schema = `CREATE VIRTUAL TABLE docs USING fts5(body, title);`

	_, err := sqlitex.Plan(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected a changed virtual table to be refused")
	}
	if !strings.Contains(err.Error(), "virtual table") {
		t.Errorf("refusal does not explain itself: %v", err)
	}
}

// A premigration is arbitrary code, and code panics. The connection it panics
// on must not go back to the pool mid-transaction.
func TestMigratePremigrationPanicLeavesNoOpenTransaction(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY);`)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate")
			}
		}()

		_ = sqlitex.Migrate(t.Context(), db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`,
			sqlitex.WithPremigration(func(ctx context.Context, conn *sql.Conn) error {
				if _, err := conn.ExecContext(ctx, `CREATE TABLE scratch (x TEXT);`); err != nil {
					return err
				}

				panic("premigration exploded")
			}))
	}()

	// The transaction was rolled back rather than left open on the connection.
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'scratch';`); got != 0 {
		t.Error("the panicking premigration left its work behind")
	}

	// Foreign key enforcement came back, which it cannot do inside an open
	// transaction, and the pool is usable again.
	if got := scalar[int](t, db, `PRAGMA foreign_keys;`); got != 1 {
		t.Errorf("foreign_keys = %d after a panic, want 1", got)
	}
	if err := sqlitex.Migrate(t.Context(), db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`); err != nil {
		t.Errorf("the pool is unusable after a panic: %v", err)
	}
}
