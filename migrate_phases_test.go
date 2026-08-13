package sqlitex_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

// renameNickname is the premigration this package exists to make possible: it
// says that a column was renamed, which no comparison of two schemas could
// have worked out on its own. It asks the database what it holds rather than
// remembering what it has already done, so running it again does nothing.
func renameNickname(ctx context.Context, conn *sql.Conn) error {
	var legacy int
	err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('users') WHERE name = 'nickname';`).Scan(&legacy)
	if err != nil || legacy == 0 {
		return err
	}

	_, err = conn.ExecContext(ctx, `ALTER TABLE users RENAME COLUMN nickname TO handle;`)

	return err
}

// Without the premigration this is a refused drop; with it, it is a rename
// that keeps the data and leaves nothing for the comparison to do.
func TestMigratePremigrationRenames(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT);`,
		`INSERT INTO users (nickname) VALUES ('nick');`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, handle TEXT);`

	// What it looks like to the comparison alone.
	if _, err := sqlitex.Plan(t.Context(), db, schema); err == nil {
		t.Fatal("expected the rename to look like a drop without a premigration")
	}

	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithPremigration(renameNickname)); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[string](t, db, `SELECT handle FROM users;`); got != "nick" {
		t.Errorf("handle = %q, want the renamed column to keep its data", got)
	}

	// And it is idempotent: the second run finds the column already renamed.
	if err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithPremigration(renameNickname)); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM users;`); got != 1 {
		t.Errorf("rows = %d, want 1", got)
	}
}

// Plan runs the premigration so that what it reports is what Migrate would do,
// and rolls it back so that reporting changes nothing.
func TestPlanRunsAndUndoesThePremigration(t *testing.T) {
	db := setup(t,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT);`,
		`INSERT INTO users (nickname) VALUES ('nick');`,
	)

	const schema = `CREATE TABLE users (id INTEGER PRIMARY KEY, handle TEXT);`

	got, err := sqlitex.Plan(t.Context(), db, schema, sqlitex.WithPremigration(renameNickname))
	if err != nil {
		t.Fatalf("Plan failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("plan =\n%s\nwant nothing left to do after the rename", strings.Join(got, "\n"))
	}

	// The rename itself was rolled back.
	if got := scalar[int](t, db, `SELECT count(*) FROM pragma_table_info('users') WHERE name = 'nickname';`); got != 1 {
		t.Error("Plan kept the premigration it ran")
	}
}

// A premigration that fails takes the migration with it.
func TestMigratePremigrationFailureRollsBack(t *testing.T) {
	db := setup(t, `CREATE TABLE users (id INTEGER PRIMARY KEY);`)

	sentinel := errors.New("premigration says no")

	err := sqlitex.Migrate(t.Context(), db, `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT);`,
		sqlitex.WithPremigration(func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, `CREATE TABLE scratch (x TEXT);`); err != nil {
				return err
			}

			return sentinel
		}))

	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the premigration's own error", err)
	}

	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name = 'scratch';`); got != 0 {
		t.Error("the failed premigration left its work behind")
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM pragma_table_info('users') WHERE name = 'email';`); got != 0 {
		t.Error("the schema was migrated despite the premigration failing")
	}
}

func TestMigrateFixtures(t *testing.T) {
	db := setup(t)

	const schema = `
CREATE TABLE tiers (name TEXT PRIMARY KEY, rank INTEGER NOT NULL) STRICT;
CREATE TABLE users (id INTEGER PRIMARY KEY, tier TEXT NOT NULL REFERENCES tiers(name)) STRICT;
`

	const fixtures = `
INSERT INTO tiers (name, rank) VALUES ('free', 0), ('paid', 1)
	ON CONFLICT (name) DO UPDATE SET rank = excluded.rank;
`

	migrate := func() error {
		return sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithFixtures(fixtures))
	}

	if err := migrate(); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM tiers;`); got != 2 {
		t.Fatalf("tiers = %d, want 2", got)
	}

	// Fixtures run every time, so they have to converge rather than pile up.
	if err := migrate(); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM tiers;`); got != 2 {
		t.Errorf("tiers = %d after running twice, want 2", got)
	}

	// A schema change rebuilds the table underneath them, and they put the
	// rows back.
	const changed = `
CREATE TABLE tiers (name TEXT PRIMARY KEY, rank INTEGER NOT NULL, label TEXT) STRICT;
CREATE TABLE users (id INTEGER PRIMARY KEY, tier TEXT NOT NULL REFERENCES tiers(name)) STRICT;
`

	if err := sqlitex.Migrate(t.Context(), db, changed, sqlitex.WithFixtures(fixtures)); err != nil {
		t.Fatalf("Migrate to the changed schema failed: %v", err)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM tiers;`); got != 2 {
		t.Errorf("tiers = %d after the rebuild, want 2", got)
	}
}

// Seed data that points at nothing fails the migration rather than settling
// into the database, even though enforcement is off while it runs.
func TestMigrateFixturesAreChecked(t *testing.T) {
	db := setup(t)

	const schema = `
CREATE TABLE tiers (name TEXT PRIMARY KEY) STRICT;
CREATE TABLE users (id INTEGER PRIMARY KEY, tier TEXT NOT NULL REFERENCES tiers(name)) STRICT;
`

	const fixtures = `INSERT INTO users (id, tier) VALUES (1, 'nonexistent');`

	err := sqlitex.Migrate(t.Context(), db, schema, sqlitex.WithFixtures(fixtures))
	if err == nil {
		t.Fatal("expected fixtures pointing at a missing row to be rejected")
	}
	if !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("error does not explain itself: %v", err)
	}

	// The whole migration went with them, schema included.
	if got := scalar[int](t, db, `SELECT count(*) FROM sqlite_schema WHERE name IN ('tiers', 'users');`); got != 0 {
		t.Error("the rejected migration left its schema behind")
	}
}

// All three phases, in the order they are documented in.
func TestMigrateAllPhases(t *testing.T) {
	db := setup(t,
		`CREATE TABLE tiers (name TEXT PRIMARY KEY);`,
		`CREATE TABLE users (id INTEGER PRIMARY KEY, nickname TEXT);`,
		`INSERT INTO tiers (name) VALUES ('free');`,
		`INSERT INTO users (nickname) VALUES ('nick');`,
	)

	const schema = `
CREATE TABLE tiers (name TEXT PRIMARY KEY);
CREATE TABLE users (id INTEGER PRIMARY KEY, handle TEXT, tier TEXT NOT NULL DEFAULT 'free' REFERENCES tiers(name));
`

	const fixtures = `INSERT INTO tiers (name) VALUES ('free'), ('paid') ON CONFLICT DO NOTHING;`

	if err := sqlitex.Migrate(t.Context(), db, schema,
		sqlitex.WithPremigration(renameNickname),
		sqlitex.WithFixtures(fixtures),
	); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	if got := scalar[string](t, db, `SELECT handle FROM users;`); got != "nick" {
		t.Errorf("handle = %q, want the premigration to have renamed the column", got)
	}
	if got := scalar[string](t, db, `SELECT tier FROM users;`); got != "free" {
		t.Errorf("tier = %q, want the declared default", got)
	}
	if got := scalar[int](t, db, `SELECT count(*) FROM tiers;`); got != 2 {
		t.Errorf("tiers = %d, want the fixtures to have added one", got)
	}

	if got := plan(t, db, schema); len(got) != 0 {
		t.Errorf("plan after migrating =\n%s\nwant nothing to do", strings.Join(got, "\n"))
	}
}
