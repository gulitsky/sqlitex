package sqlitex_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

// setup opens an in-memory database and puts it in the given state.
func setup(t *testing.T, statements ...string) *sql.DB {
	t.Helper()

	db, err := sqlitex.OpenMemory("sqlite")
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	return db
}

func plan(t *testing.T, db *sql.DB, schema string) []string {
	t.Helper()

	stmts, err := sqlitex.Plan(t.Context(), db, schema)
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}

	return stmts
}

func TestPlanCreatesEverythingOnAnEmptyDatabase(t *testing.T) {
	db := setup(t)

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;
CREATE INDEX idx_users_email ON users(email);
CREATE VIEW recent AS SELECT id FROM users;
`

	got := plan(t, db, schema)
	want := []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;",
		"CREATE INDEX idx_users_email ON users(email);",
		"CREATE VIEW recent AS SELECT id FROM users;",
	}

	if !slices.Equal(got, want) {
		t.Errorf("plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlanIsEmptyWhenSchemasAgree(t *testing.T) {
	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;
CREATE INDEX idx_users_email ON users(email);
`

	db := setup(t, schema)

	if got := plan(t, db, schema); len(got) != 0 {
		t.Errorf("plan = %q, want nothing to do", got)
	}
}

// A table this package rebuilt is stored with its name quoted, because that is
// how SQLite writes a renamed table back into the schema. It still matches the
// declaration it was built from.
func TestPlanIsEmptyForARebuiltTable(t *testing.T) {
	db := setup(t, `CREATE TABLE "users" (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;`)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL) STRICT;`

	if got := plan(t, db, schema); len(got) != 0 {
		t.Errorf("plan = %q, want nothing to do", got)
	}
}

func TestPlanRebuildsForANewColumn(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL);`,
		`INSERT INTO users (email) VALUES ('a@example.com');`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL, tier TEXT NOT NULL DEFAULT 'free');`

	got := plan(t, db, schema)
	want := []string{
		`CREATE TABLE "sqlitex_new_users" (id INTEGER PRIMARY KEY, email TEXT NOT NULL, tier TEXT NOT NULL DEFAULT 'free');`,
		`INSERT INTO "sqlitex_new_users" ("id", "email") SELECT "id", "email" FROM "users";`,
		`DROP TABLE "users";`,
		`ALTER TABLE "sqlitex_new_users" RENAME TO "users";`,
	}

	if !slices.Equal(got, want) {
		t.Errorf("plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPlanRefusesAnUnfillableColumn(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY);`,
		`INSERT INTO users (id) VALUES (1);`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL);`

	_, err := sqlitex.Plan(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected a refusal for a NOT NULL column without a default")
	}
	if !strings.Contains(err.Error(), "email") || !strings.Contains(err.Error(), "NOT NULL") {
		t.Errorf("refusal does not explain itself: %v", err)
	}
}

// The same column on an empty table is fine: there are no rows to fill.
func TestPlanAllowsAnUnfillableColumnOnAnEmptyTable(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY);`)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL);`

	if got := plan(t, db, schema); len(got) == 0 {
		t.Error("expected a rebuild, got nothing to do")
	}
}

func TestPlanRefusesToDropWithoutPermission(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE sessions (id INTEGER PRIMARY KEY);`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY);`

	_, err := sqlitex.Plan(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected a refusal for an undeclared table")
	}
	if !strings.Contains(err.Error(), "sessions") {
		t.Errorf("refusal does not name the table: %v", err)
	}

	got, err := sqlitex.Plan(t.Context(), db, schema, sqlitex.WithAllowDrop())
	if err != nil {
		t.Fatalf("Plan with WithAllowDrop failed: %v", err)
	}
	if want := []string{`DROP TABLE "sessions";`}; !slices.Equal(got, want) {
		t.Errorf("plan = %q, want %q", got, want)
	}
}

func TestPlanRefusesToDropAColumnWithoutPermission(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT);`)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY);`

	_, err := sqlitex.Plan(t.Context(), db, schema)
	if err == nil {
		t.Fatal("expected a refusal for an undeclared column")
	}
	if !strings.Contains(err.Error(), "nickname") {
		t.Errorf("refusal does not name the column: %v", err)
	}

	got, err := sqlitex.Plan(t.Context(), db, schema, sqlitex.WithAllowDrop())
	if err != nil {
		t.Fatalf("Plan with WithAllowDrop failed: %v", err)
	}
	if len(got) == 0 {
		t.Error("expected a rebuild that leaves the column behind")
	}
}

func TestPlanReplacesAChangedIndex(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`,
		`CREATE INDEX idx_users_email ON users(email);`,
	)

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE UNIQUE INDEX idx_users_email ON users(email);
`

	got := plan(t, db, schema)
	want := []string{
		`DROP INDEX "idx_users_email";`,
		"CREATE UNIQUE INDEX idx_users_email ON users(email);",
	}

	if !slices.Equal(got, want) {
		t.Errorf("plan =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A rebuild reparses every view and trigger in the schema, so they all step
// aside first and come back from the declaration.
func TestPlanCyclesViewsAndTriggersAroundARebuild(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY);`,
		`CREATE TABLE audit (id INTEGER PRIMARY KEY);`,
		`CREATE VIEW everyone AS SELECT id FROM users;`,
		`CREATE TRIGGER audited AFTER INSERT ON audit BEGIN SELECT 1; END;`,
	)

	const schema = `
CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);
CREATE TABLE audit (id INTEGER PRIMARY KEY);
CREATE VIEW everyone AS SELECT id FROM users;
CREATE TRIGGER audited AFTER INSERT ON audit BEGIN SELECT 1; END;
`

	got := plan(t, db, schema)

	dropView := slices.Index(got, `DROP VIEW "everyone";`)
	dropTrigger := slices.Index(got, `DROP TRIGGER "audited";`)
	rename := slices.IndexFunc(got, func(s string) bool { return strings.HasPrefix(s, "ALTER TABLE") })

	if dropView < 0 || dropTrigger < 0 {
		t.Fatalf("plan does not step the view and trigger aside:\n%s", strings.Join(got, "\n"))
	}
	if dropView > rename || dropTrigger > rename {
		t.Errorf("view and trigger must go before the rename:\n%s", strings.Join(got, "\n"))
	}

	createView := slices.IndexFunc(got, func(s string) bool { return strings.Contains(s, "CREATE VIEW") })
	if createView < rename {
		t.Errorf("view must come back after the rename:\n%s", strings.Join(got, "\n"))
	}
}

func TestPlanPreservesAnAutoincrementSequence(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT);`,
		`INSERT INTO users (email) VALUES ('a'),('b'),('c');`,
		`DELETE FROM users WHERE id > 1;`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, email TEXT, tier TEXT);`

	got := plan(t, db, schema)

	if !slices.Contains(got, `INSERT INTO sqlite_sequence (name, seq) VALUES ('users', 3);`) {
		t.Errorf("plan does not restore the sequence:\n%s", strings.Join(got, "\n"))
	}
}

func TestPlanRejectsAnEmptySchema(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY);`)

	if _, err := sqlitex.Plan(t.Context(), db, "   \n"); err == nil {
		t.Error("expected an empty declaration to be rejected")
	}
}

func TestPlanReportsInvalidSchema(t *testing.T) {
	db := setup(t)

	if _, err := sqlitex.Plan(t.Context(), db, "CREATE TABLE ((("); err == nil {
		t.Error("expected an invalid declaration to be reported")
	}
}
