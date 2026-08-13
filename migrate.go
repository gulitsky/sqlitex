package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// tempPrefix names the table a rebuild builds beside the one it replaces. The
// documented procedure requires the new table to be created under a name of
// its own and renamed into place afterwards; renaming the old one out of the
// way first would corrupt references to it.
const tempPrefix = "sqlitex_new_"

// canonicalName is the name both sides of a table comparison are rewritten
// under, so that a difference in how the table name is written is not mistaken
// for a difference in the table.
const canonicalName = "sqlitex_canonical"

type migrateConfig struct {
	allowDrop    bool
	premigration func(context.Context, *sql.Conn) error
	fixtures     string
	ignore       []string
	logger       *slog.Logger
}

// migrateOption configures a migration.
type migrateOption func(*migrateConfig) error

// WithAllowDrop permits dropping tables and columns the declared schema no
// longer mentions.
//
// Without it, a migration that would drop either is refused, because dropping
// is indistinguishable from renaming: a schema that renames a column declares
// exactly what a schema that drops one and adds another declares. Renames
// belong in a premigration, which runs before the comparison and can say which
// of the two it meant.
//
// Indexes, views and triggers are dropped without this option. They hold no
// data, and the declared schema is the whole truth about them.
func WithAllowDrop() migrateOption {
	return func(cfg *migrateConfig) error {
		cfg.allowDrop = true

		return nil
	}
}

// WithPremigration registers a function to run before the schemas are
// compared, on the connection and inside the transaction the migration uses.
//
// It is where changes live that a comparison cannot infer. Renaming a column
// is the reason it exists: a schema that renames one declares exactly what a
// schema that drops one and adds another declares, so a rename has to be
// performed rather than deduced. Having run, it leaves a database the
// comparison finds nothing to do about.
//
//	sqlitex.WithPremigration(func(ctx context.Context, conn *sql.Conn) error {
//		var legacy int
//		err := conn.QueryRowContext(ctx,
//			`SELECT count(*) FROM pragma_table_info('users') WHERE name = 'name';`).Scan(&legacy)
//		if err != nil || legacy == 0 {
//			return err
//		}
//
//		_, err = conn.ExecContext(ctx, `ALTER TABLE users RENAME COLUMN name TO full_name;`)
//		return err
//	})
//
// It runs on every migration, so it has to decide for itself whether there is
// anything to do, the way the example above asks the database what it holds.
// That is deliberate: this package keeps no record of which premigrations have
// run, so nothing can disagree with the database about what state it is in.
//
// Returning an error rolls the whole migration back. The function must not
// commit or roll back on its own, and it runs with foreign key enforcement
// off, like everything else in the migration.
func WithPremigration(fn func(ctx context.Context, conn *sql.Conn) error) migrateOption {
	return func(cfg *migrateConfig) error {
		if fn == nil {
			return errors.New("premigration must not be nil")
		}

		cfg.premigration = fn

		return nil
	}
}

// WithFixtures registers a script of statements to run once the schema is in
// place — the rows that have to exist for the database to be usable at all,
// such as reference tables the rest of the schema points at.
//
// It runs on every migration, whether or not the schema changed, so it has to
// be written to converge rather than to accumulate: INSERT OR IGNORE, ON
// CONFLICT DO NOTHING, or ON CONFLICT DO UPDATE. What it inserts is covered by
// the foreign key check the migration ends with, so seed data that points at
// nothing fails the migration instead of settling into the database.
//
// The first two forms stop writing once the rows are in place, which lets a
// migration with nothing else to do skip that check. ON CONFLICT DO UPDATE
// rewrites its rows on every run even when the values are identical, and a
// database large enough for the check to be slow will feel it on every
// startup.
func WithFixtures(fixtures string) migrateOption {
	return func(cfg *migrateConfig) error {
		cfg.fixtures = fixtures

		return nil
	}
}

// WithIgnore leaves objects whose name matches any of the given LIKE patterns
// out of the comparison entirely, in both directions: they are neither dropped
// for being undeclared nor compared against a declaration.
//
// It is for the tables other tools keep in the same database — Litestream's
// "_litestream_seq" and "_litestream_lock", for instance — which a declarative
// migration would otherwise offer to drop as soon as WithAllowDrop is given:
//
//	sqlitex.WithIgnore("\\_litestream\\_%")
//
// Objects belonging to an ignored table are ignored with it, so an index on
// one is left alone too. The patterns are SQLite LIKE patterns matched with a
// backslash escape, so "_" stands for any character until it is written as
// "\_", and matching is case-insensitive for ASCII.
//
// Since ignored objects are never touched, they are also not stepped aside
// while a table is rebuilt. An ignored view that reads a declared table will
// therefore fail that rebuild, the same way a view SQLite cannot reparse
// always does.
func WithIgnore(patterns ...string) migrateOption {
	return func(cfg *migrateConfig) error {
		for _, pattern := range patterns {
			if strings.TrimSpace(pattern) == "" {
				return errors.New("ignore pattern must not be empty")
			}
		}

		cfg.ignore = append(cfg.ignore, patterns...)

		return nil
	}
}

// WithMigrationLogger sets the structured logger for migration events, which
// otherwise go to slog.Default.
//
// A migration with nothing to do says so at debug level and is silent
// otherwise; one that changes something reports each statement at debug level
// and what it did overall at info level, once the transaction has committed.
// Failures are returned rather than logged.
//
// The name is not WithLogger only because that one already belongs to
// Maintain, and Go has no way to give both the same one.
func WithMigrationLogger(logger *slog.Logger) migrateOption {
	return func(cfg *migrateConfig) error {
		if logger == nil {
			return errors.New("logger must not be nil")
		}

		cfg.logger = logger

		return nil
	}
}

// Plan reports the statements that would bring the schema of db in line with
// the declared one, in the order they would run. It applies none of them, and
// returns nothing at all when the two already agree.
//
// A premigration is the exception: it is run, inside a transaction that is
// rolled back afterwards, because the plan for a database it has yet to touch
// is not the plan Migrate would apply. Whatever it does outside the database
// is not undone by that rollback.
//
// The declared schema is a script of CREATE statements — the file that would
// build the database from nothing. Plan executes it in a throwaway in-memory
// database on the same driver and compares what SQLite stored there with what
// it stored in db, so the two sides are normalized identically and no SQL is
// parsed on the way.
//
// Plan refuses, rather than reporting statements, when the migration would
// lose data that cannot be recovered from the declaration: a dropped table or
// column without WithAllowDrop, or a new NOT NULL column without a default on
// a table that already has rows. Every refusal is reported at once.
func Plan(ctx context.Context, db *sql.DB, schema string, options ...migrateOption) ([]string, error) {
	cfg, err := newMigrateConfig(options)
	if err != nil {
		return nil, err
	}

	// Nothing to run, so nothing to undo: the comparison is all reads.
	if cfg.premigration == nil {
		return buildPlan(ctx, cfg, db.Driver(), db, schema)
	}

	// A premigration changes what there is to plan. A rename is a rename only
	// because the premigration performs it; a plan made without running it
	// would report the drop and the addition the rename looks like from
	// outside, which is worse than no plan at all. So it runs, against a
	// transaction that is rolled back rather than committed.
	var stmts []string

	err = migrating(ctx, db, false, func(conn *sql.Conn) error {
		if err := cfg.premigration(ctx, conn); err != nil {
			return fmt.Errorf("premigration: %w", err)
		}

		var err error
		stmts, err = buildPlan(ctx, cfg, db.Driver(), conn, schema)

		return err
	})
	if err != nil {
		return nil, err
	}

	return stmts, nil
}

func newMigrateConfig(options []migrateOption) (*migrateConfig, error) {
	cfg := &migrateConfig{logger: slog.Default()}

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	return cfg, nil
}

// buildPlan compares the declared schema with the one actual holds. It takes a
// querier rather than a pool so that a migration can plan inside its own
// transaction, against the state its earlier steps have already produced.
func buildPlan(ctx context.Context, cfg *migrateConfig, drv driver.Driver, actual querier, schema string) ([]string, error) {
	// An empty declaration describes a database with nothing in it. That is
	// far more likely to be a schema that failed to load than a request to
	// drop everything.
	if strings.TrimSpace(schema) == "" {
		return nil, errors.New("declared schema is empty")
	}

	pristine := openScratch(drv)
	defer pristine.Close() //nolint:errcheck // an in-memory database being discarded has nothing to report

	if _, err := pristine.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("execute declared schema: %w", err)
	}

	// Both sides are filtered the same way, so an ignored object is not
	// something the comparison sees and decides to leave alone — it is
	// something the comparison never hears about.
	desired, err := readSchema(ctx, pristine, cfg.ignore)
	if err != nil {
		return nil, err
	}

	existing, err := readSchema(ctx, actual, cfg.ignore)
	if err != nil {
		return nil, err
	}

	p := &planner{
		cfg:      cfg,
		drv:      drv,
		actual:   actual,
		pristine: pristine,
		desired:  desired,
		existing: existing,
	}

	return p.run(ctx)
}

// Migrate brings the schema of db in line with the declared one, applying
// everything Plan reports, or nothing at all.
//
// It has three phases, in order: the premigration says what a comparison could
// not have inferred, the declared schema is compared and the difference
// applied, and the fixtures put back the rows the schema takes for granted.
// Only the middle one is required; see WithPremigration and WithFixtures.
//
// It runs on a single connection taken from db, inside one transaction, with
// foreign key enforcement off for its duration — the rebuild procedure drops
// and recreates tables that other tables point at, which enforcement would
// reject, so the transaction is checked as a whole with PRAGMA
// foreign_key_check before it commits. Enforcement is restored afterwards,
// whether or not the migration succeeded.
//
// The plan is computed inside that transaction, so what Migrate applies is
// what the database held when the transaction began, and concurrent callers
// migrating the same file are serialized rather than racing: the second one to
// arrive finds the work already done and does nothing.
//
// Migrate needs to write, so it is given the read-write pool. See Plan for
// what it will refuse to do.
func Migrate(ctx context.Context, db *sql.DB, schema string, options ...migrateOption) error {
	cfg, err := newMigrateConfig(options)
	if err != nil {
		return err
	}

	var (
		logger  = cfg.logger
		applied int
		written int64
		started = time.Now()
	)

	err = migrating(ctx, db, true, func(conn *sql.Conn) error {
		logger = logger.With("database", mainDatabaseName(ctx, conn))

		// Rows written before anything ran, so that the foreign key check at
		// the end can be skipped when there is nothing for it to find.
		before, err := totalChanges(ctx, conn)
		if err != nil {
			return err
		}

		// First, because it exists to change what the comparison then sees.
		if cfg.premigration != nil {
			if err := cfg.premigration(ctx, conn); err != nil {
				return fmt.Errorf("premigration: %w", err)
			}

			logger.DebugContext(ctx, "premigration applied")
		}

		stmts, err := buildPlan(ctx, cfg, db.Driver(), conn, schema)
		if err != nil {
			return err
		}

		for _, stmt := range stmts {
			logger.DebugContext(ctx, "applying statement", "statement", stmt)

			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s: %w", stmt, err)
			}
		}

		// Last, once the schema they are written against is in place.
		if strings.TrimSpace(cfg.fixtures) != "" {
			if _, err := conn.ExecContext(ctx, cfg.fixtures); err != nil {
				return fmt.Errorf("apply fixtures: %w", err)
			}

			logger.DebugContext(ctx, "fixtures applied")
		}

		after, err := totalChanges(ctx, conn)
		if err != nil {
			return err
		}

		applied, written = len(stmts), after-before

		// Checking foreign keys reads every row of every table that has one,
		// which is not a price to pay on each startup for a migration that
		// turned out to have nothing to do. Fixtures written with INSERT OR
		// IGNORE or ON CONFLICT DO NOTHING stop writing once they have taken,
		// and land here; ON CONFLICT DO UPDATE rewrites its rows every time,
		// so it pays for the check every time.
		if applied == 0 && written == 0 {
			return nil
		}

		return checkForeignKeys(ctx, conn)
	})
	if err != nil {
		return err
	}

	// Reported once the transaction has committed, so that nothing claims a
	// migration that a failed commit took back.
	if applied == 0 && written == 0 {
		logger.DebugContext(ctx, "database schema is up to date")

		return nil
	}

	logger.InfoContext(ctx, "database schema migrated",
		"statements", applied,
		"rows", written,
		"duration", time.Since(started))

	return nil
}

// migrating runs fn on one connection of db, with foreign key enforcement off
// and a transaction open, committing it when commit is set and rolling it back
// otherwise.
//
// The transaction is begun by hand because database/sql cannot ask for an
// immediate one, and a deferred transaction is the wrong shape here: it would
// read the schema first and take the write lock only later, and that upgrade
// can fail outright instead of waiting out busy_timeout.
func migrating(ctx context.Context, db *sql.DB, commit bool, fn func(conn *sql.Conn) error) (err error) {
	// One connection throughout: every pragma below is a property of a
	// connection, not of the database.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("take connection: %w", err)
	}
	// Returning the connection to the pool cannot fail in a way worth
	// reporting, and it may already have been discarded on purpose below.
	defer conn.Close() //nolint:errcheck

	// Foreign keys have to be turned off outside a transaction, since the
	// pragma is a silent no-op inside one.
	var foreignKeys bool
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}

	if foreignKeys {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = off;"); err != nil {
			return fmt.Errorf("disable foreign keys: %w", err)
		}

		// Restoring is not optional and not conditional on success: this
		// connection goes back to a pool whose callers expect enforcement, and
		// a canceled context must not leave it disabled.
		defer func() {
			_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = on;")
			if restoreErr == nil {
				return
			}

			// Otherwise this connection would go back to the pool still not
			// enforcing foreign keys, and everything served by it afterwards
			// would quietly be allowed to break them. Returning ErrBadConn
			// from Raw makes database/sql close the connection instead of
			// handing it out again.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })

			// The work itself may well have committed by now, so this is
			// reported alongside its outcome rather than in place of it.
			// Migrate is idempotent, so a caller that retries loses nothing.
			err = errors.Join(err, fmt.Errorf("restore foreign keys: %w", restoreErr))
		}()
	}

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE;"); err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// Rolling back has to happen even when the context is what went wrong, or
	// the connection returns to the pool mid-transaction.
	open := true

	rollback := func() error {
		open = false

		if _, err := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK;"); err != nil {
			return fmt.Errorf("roll back: %w", err)
		}

		return nil
	}

	// A premigration is arbitrary code, and code panics. Without this the
	// connection would go back to the pool with the transaction still open,
	// and the pragma restored above would be restored inside it, where it is a
	// documented no-op that reports no error — leaving a pool that neither
	// commits nor enforces anything.
	defer func() {
		if open {
			_ = rollback()
		}
	}()

	if err := fn(conn); err != nil {
		return errors.Join(err, rollback())
	}

	if !commit {
		return rollback()
	}

	if _, err := conn.ExecContext(ctx, "COMMIT;"); err != nil {
		return errors.Join(fmt.Errorf("commit: %w", err), rollback())
	}

	open = false

	return nil
}

// totalChanges reports how many rows this connection has written since it was
// opened, which is how a migration tells whether anything it ran had an effect.
func totalChanges(ctx context.Context, q querier) (int64, error) {
	var changes int64
	if err := q.QueryRowContext(ctx, "SELECT total_changes();").Scan(&changes); err != nil {
		return 0, fmt.Errorf("read total_changes: %w", err)
	}

	return changes, nil
}

// checkForeignKeys reports the violations left behind by a migration that ran
// with enforcement off.
func checkForeignKeys(ctx context.Context, q querier) error {
	rows, err := q.QueryContext(ctx, "PRAGMA foreign_key_check;")
	if err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}
	defer rows.Close() //nolint:errcheck // whatever it would report arrives through rows.Err below

	// One broken reference usually means many, and the first few say as much
	// about the cause as all of them.
	const report = 5

	var (
		violations []string
		seen       = map[string]bool{}
	)

	for rows.Next() {
		var (
			table  string
			rowid  sql.NullInt64 // absent for WITHOUT ROWID tables
			parent string
			fkid   int
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("check foreign keys: %w", err)
		}

		violation := fmt.Sprintf("%s -> %s", quoteIdent(table), quoteIdent(parent))
		if seen[violation] {
			continue
		}
		seen[violation] = true

		if len(violations) < report {
			violations = append(violations, violation)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("check foreign keys: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf("migration would leave foreign keys pointing at nothing: %s", strings.Join(violations, ", "))
	}

	return nil
}

// planner turns the difference between two schemas into statements.
type planner struct {
	cfg      *migrateConfig
	drv      driver.Driver
	actual   querier
	pristine querier

	desired  []object // as declared, in declaration order
	existing []object // as found, in creation order

	refusals []string
}

func (p *planner) run(ctx context.Context) ([]string, error) {
	desired := index(p.desired)
	existing := index(p.existing)

	var (
		rebuild  = map[string]bool{} // folded names of tables whose definition changed
		create   = map[string]bool{} // folded names of declared objects to create
		drop     []object            // existing objects to drop explicitly
		gone     = map[string]bool{} // folded names of tables and views that will not survive as they are
		dataFree []object            // declared indexes, views and triggers
	)

	for _, want := range p.desired {
		if want.typ != "table" {
			dataFree = append(dataFree, want)
		}

		have, ok := existing[fold(want.name)]
		switch {
		case !ok:
			create[fold(want.name)] = true

		case have.typ != want.typ:
			// The name is taken by something else entirely, so the old object
			// goes and the declared one takes its place.
			drop = append(drop, have)
			create[fold(want.name)] = true

			if have.typ == "table" || have.typ == "view" {
				gone[fold(have.name)] = true
			}

		case want.typ == "table":
			same, err := p.sameTable(ctx, have, want)
			if err != nil {
				return nil, err
			}
			if same {
				break
			}

			// A virtual table is defined by its module, and the procedure for
			// changing a table definition does not apply: there is no way to
			// build a second one beside it holding the same content.
			if have.virtual || want.virtual {
				p.refuse("virtual table %s is declared differently and cannot be rebuilt; change it in a premigration",
					quoteIdent(want.name))

				break
			}

			rebuild[fold(want.name)] = true
			gone[fold(want.name)] = true

		case have.sql != want.sql:
			// Indexes, views and triggers hold no data, so a change is a drop
			// and a create rather than anything more careful.
			drop = append(drop, have)
			create[fold(want.name)] = true

			// Dropping a view takes its INSTEAD OF triggers with it, declared
			// or not, so the view counts as gone even though it comes back.
			if have.typ == "view" {
				gone[fold(have.name)] = true
			}
		}
	}

	for _, have := range p.existing {
		if _, ok := desired[fold(have.name)]; ok {
			continue
		}

		drop = append(drop, have)
		if have.typ == "table" || have.typ == "view" {
			gone[fold(have.name)] = true
		}
	}

	// Renaming a table into place makes SQLite reparse every view and trigger
	// in the schema to update references to it. A view left behind that reads
	// a table being rebuilt is invalid at that moment and fails the rename, and
	// which tables a view reads is not knowable without parsing it. So when any
	// table is rebuilt or dropped, every view and trigger goes first and comes
	// back from the declaration afterwards.
	if len(rebuild) > 0 || len(gone) > 0 {
		for _, have := range p.existing {
			if have.typ == "view" || have.typ == "trigger" {
				drop = append(drop, have)

				if have.typ == "view" {
					gone[fold(have.name)] = true
				}
			}
		}

		for _, want := range dataFree {
			if want.typ == "view" || want.typ == "trigger" {
				create[fold(want.name)] = true
			}
		}
	}

	// Whatever belongs to a table or view that does not survive goes away with
	// it — indexes with their table, INSTEAD OF triggers with their view — so
	// it is never dropped explicitly, and is always created again.
	for _, want := range dataFree {
		if want.typ != "view" && gone[fold(want.table)] {
			create[fold(want.name)] = true
		}
	}

	drop = compact(drop, gone)

	for _, have := range drop {
		if have.typ == "table" && !p.cfg.allowDrop {
			p.refuse("table %s is not declared", quoteIdent(have.name))
		}
	}

	var stmts []string

	// Triggers before the views they may belong to, and both before the tables
	// they read: dropping something that another object depends on before that
	// object keeps every intermediate state valid.
	for _, kind := range []string{"trigger", "view", "index"} {
		for _, have := range drop {
			if have.typ == kind {
				stmts = append(stmts, fmt.Sprintf("DROP %s %s;", strings.ToUpper(have.typ), quoteIdent(have.name)))
			}
		}
	}

	for _, have := range drop {
		if have.typ == "table" {
			stmts = append(stmts, fmt.Sprintf("DROP TABLE %s;", quoteIdent(have.name)))
		}
	}

	for _, want := range p.desired {
		if want.typ != "table" || !rebuild[fold(want.name)] {
			continue
		}

		rebuilt, err := p.rebuild(ctx, want, existing)
		if err != nil {
			return nil, err
		}

		stmts = append(stmts, rebuilt...)
	}

	// Tables before the objects that read them.
	for _, want := range p.desired {
		if want.typ == "table" && create[fold(want.name)] {
			stmts = append(stmts, want.sql+";")
		}
	}

	for _, want := range dataFree {
		if create[fold(want.name)] {
			stmts = append(stmts, want.sql+";")
		}
	}

	if len(p.refusals) > 0 {
		return nil, fmt.Errorf("migration refused:\n  %s", strings.Join(p.refusals, "\n  "))
	}

	return stmts, nil
}

// sameTable reports whether a declared table is already in place.
func (p *planner) sameTable(ctx context.Context, have, want object) (bool, error) {
	// The common case, and the one a freshly created table lands in: SQLite
	// stored the declaration verbatim on both sides.
	if have.sql == want.sql {
		return true, nil
	}

	// A rebuilt table was renamed into place, and SQLite quotes the name it
	// writes into the stored statement, so a table this package built never
	// matches its own declaration byte for byte. Compare both definitions
	// under one name instead of trying to tell the quoting apart.
	canonicalHave, _, err := rename(ctx, p.drv, have.sql, canonicalName)
	if err != nil {
		return false, err
	}

	canonicalWant, _, err := rename(ctx, p.drv, want.sql, canonicalName)
	if err != nil {
		return false, err
	}

	return canonicalHave == canonicalWant, nil
}

// rebuild returns the statements that replace a table with its declared
// version, following the procedure documented for schema changes SQLite cannot
// make in place: build the new table beside the old one, copy, drop, rename.
func (p *planner) rebuild(ctx context.Context, want object, existing map[string]object) ([]string, error) {
	temp := tempPrefix + want.name
	if _, taken := existing[fold(temp)]; taken {
		return nil, fmt.Errorf("rebuild %s: the name %s it needs is taken", quoteIdent(want.name), quoteIdent(temp))
	}

	create, autoincrement, err := rename(ctx, p.drv, want.sql, temp)
	if err != nil {
		return nil, err
	}

	wantCols, err := columns(ctx, p.pristine, want.name)
	if err != nil {
		return nil, err
	}

	haveCols, err := columns(ctx, p.actual, want.name)
	if err != nil {
		return nil, err
	}

	held := make(map[string]bool, len(haveCols))
	for _, c := range haveCols {
		held[fold(c.name)] = true
	}

	declared := make(map[string]bool, len(wantCols))
	for _, c := range wantCols {
		declared[fold(c.name)] = true
	}

	populated, err := hasRows(ctx, p.actual, want.name)
	if err != nil {
		return nil, err
	}

	var carried []string
	for _, c := range wantCols {
		switch {
		case held[fold(c.name)]:
			carried = append(carried, quoteIdent(c.name))

		case populated && c.notNull && !c.hasDefault:
			// Nothing in the declaration says what the existing rows should
			// hold here, and the copy would fail on the first one.
			p.refuse("column %s.%s is NOT NULL without a default and %s is not empty; give it a default, or fill it in a premigration",
				quoteIdent(want.name), quoteIdent(c.name), quoteIdent(want.name))
		}
	}

	if !p.cfg.allowDrop {
		for _, c := range haveCols {
			if !declared[fold(c.name)] {
				p.refuse("column %s.%s is not declared", quoteIdent(want.name), quoteIdent(c.name))
			}
		}
	}

	stmts := []string{create + ";"}

	// With nothing in common there is nothing to carry over, and an empty
	// column list is not a statement SQLite would accept.
	if len(carried) > 0 {
		list := strings.Join(carried, ", ")
		stmts = append(stmts, fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s;",
			quoteIdent(temp), list, list, quoteIdent(want.name)))
	}

	stmts = append(stmts,
		fmt.Sprintf("DROP TABLE %s;", quoteIdent(want.name)),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(temp), quoteIdent(want.name)),
	)

	if autoincrement {
		seq, ok, err := sequenceOf(ctx, p.actual, want.name)
		if err != nil {
			return nil, err
		}

		// The new table starts counting from the rows it received, which would
		// hand out identifiers the old table has already used and deleted.
		if ok {
			stmts = append(stmts,
				fmt.Sprintf("DELETE FROM sqlite_sequence WHERE name = %s;", quoteString(want.name)),
				fmt.Sprintf("INSERT INTO sqlite_sequence (name, seq) VALUES (%s, %d);", quoteString(want.name), seq),
			)
		}
	}

	return stmts, nil
}

func (p *planner) refuse(format string, args ...any) {
	p.refusals = append(p.refusals, fmt.Sprintf(format, args...))
}

// index keys objects by folded name. Tables, indexes, views and triggers share
// one namespace, so a name identifies at most one object whatever its type.
func index(objects []object) map[string]object {
	byName := make(map[string]object, len(objects))
	for _, o := range objects {
		byName[fold(o.name)] = o
	}

	return byName
}

// compact drops the entries that whatever they belong to takes with it anyway,
// and the duplicates left by collecting the same object from two directions.
//
// Only indexes and triggers are dropped this way. A view belongs to itself, so
// finding its own name among the departing would silently swallow its DROP.
func compact(objects []object, gone map[string]bool) []object {
	seen := make(map[string]bool, len(objects))

	kept := objects[:0]
	for _, o := range objects {
		if seen[fold(o.name)] {
			continue
		}
		seen[fold(o.name)] = true

		if (o.typ == "index" || o.typ == "trigger") && gone[fold(o.table)] {
			continue
		}

		kept = append(kept, o)
	}

	return kept
}
