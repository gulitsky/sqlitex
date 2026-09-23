package sqlitex_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
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

// requireFTS5 skips a test on a driver built without the module it declares a
// virtual table with, which mattn/go-sqlite3 is unless -tags sqlite_fts5 is
// passed. Nothing here is about FTS5 itself; it is just a virtual table that
// both drivers can be made to have.
func requireFTS5(t *testing.T, db *sql.DB) {
	t.Helper()

	if _, err := db.Exec(`CREATE VIRTUAL TABLE fts5_probe USING fts5(x);`); err != nil {
		t.Skipf("driver has no fts5 module: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE fts5_probe;`); err != nil {
		t.Fatalf("drop the probe table: %v", err)
	}
}

// A virtual table keeps its content in shadow tables that look like ordinary
// ones. They belong to it and are none of the migration's business.
func TestMigrateHandlesVirtualTables(t *testing.T) {
	db := setup(t)
	requireFTS5(t, db)

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
	db := setup(t)
	requireFTS5(t, db)

	if _, err := db.Exec(`CREATE VIRTUAL TABLE docs USING fts5(body);`); err != nil {
		t.Fatalf("create the virtual table: %v", err)
	}

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

// sqlite_sequence records the spelling a table was created with, not the one
// the declaration uses, so a rebuild that looks the row up case-sensitively
// starts the counter over and hands out identifiers the old table has used.
func TestMigrateKeepsTheSequenceAcrossACaseOnlyRename(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT);`,
		`INSERT INTO users (name) VALUES ('a'), ('b'), ('c');`,
		`DELETE FROM users;`,
	)

	// Declared under a different case, with one real change so the rebuild runs.
	const schema = `CREATE TABLE Users (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, tier TEXT);`

	if err := sqlitex.Migrate(t.Context(), db, schema); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[int](t, db, `SELECT seq FROM sqlite_sequence WHERE name = 'users' COLLATE NOCASE;`); got != 3 {
		t.Errorf("sequence = %d after a case-only rename, want 3", got)
	}

	if _, err := db.Exec(`INSERT INTO users (name) VALUES ('d');`); err != nil {
		t.Fatalf("insert after the rebuild failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT id FROM users;`); got != 4 {
		t.Errorf("id = %d, want 4 rather than one the deleted rows already used", got)
	}
}

// total_changes does not count the rows DDL touches, so a premigration that
// only alters tables leaves both counters at zero. The foreign key check must
// still run: the schema it changed is exactly what the check is there for.
func TestMigrateChecksForeignKeysAfterADDLOnlyPremigration(t *testing.T) {
	db := setup(t,
		`CREATE TABLE parent (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE child (id INTEGER PRIMARY KEY);`,
		`INSERT INTO child (id) VALUES (1);`,
	)

	// The declaration already matches what the premigration leaves behind, so
	// the plan is empty and nothing but the ALTER changes anything.
	const schema = `
CREATE TABLE parent (id INTEGER PRIMARY KEY);
CREATE TABLE child (id INTEGER PRIMARY KEY, pid INTEGER NOT NULL DEFAULT 99 REFERENCES parent(id));
`

	err := sqlitex.Migrate(t.Context(), db, schema,
		sqlitex.WithPremigration(func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx,
				`ALTER TABLE child ADD COLUMN pid INTEGER NOT NULL DEFAULT 99 REFERENCES parent(id);`)

			return err
		}))
	if err == nil {
		t.Fatal("Migrate committed a premigration that left a dangling reference")
	}
	if !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("error = %v, want it to name the foreign key violation", err)
	}

	// And the whole transaction went back, the premigration with it.
	if got := scalar[int](t, db, `SELECT count(*) FROM pragma_table_info('child') WHERE name = 'pid';`); got != 0 {
		t.Error("the rolled back premigration left its column behind")
	}
}

// A name taken by an object of another type is still declared. Refusing to
// drop the table it currently names must not claim otherwise.
func TestMigrateRefusalNamesADeclarationOfAnotherType(t *testing.T) {
	db := setup(t, `CREATE TABLE report (id INTEGER PRIMARY KEY);`)

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY);
CREATE VIEW report AS SELECT id FROM users;
`

	_, err := sqlitex.Plan(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected replacing a table with a view to be refused without WithAllowDrop")
	}
	if strings.Contains(err.Error(), "is not declared") {
		t.Errorf("refusal = %v, want it not to claim a declared name is undeclared", err)
	}
	if !strings.Contains(err.Error(), "declared as a view") {
		t.Errorf("refusal = %v, want it to say the name is declared as a view", err)
	}
}
