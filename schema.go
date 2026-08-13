package sqlitex

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
)

// querier is the read-only part of *sql.DB, *sql.Conn and *sql.Tx, so schema
// inspection works both against a pool and inside the migration transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// object is one entry of sqlite_schema — a table, index, view or trigger —
// together with the CREATE statement SQLite stored for it.
type object struct {
	typ     string // "table", "index", "view" or "trigger"
	name    string
	table   string // the table the object belongs to; its own name for tables and views
	sql     string
	virtual bool // the table is a virtual one, which cannot be rebuilt
}

// readSchema returns the objects of the main schema in creation order, leaving
// out the ones whose name, or whose table's name, matches an ignore pattern.
//
// Entries with a NULL sql are the indexes SQLite creates on its own for UNIQUE
// and PRIMARY KEY constraints; they arrive with the table that declares them
// and disappear with it, so they are never migrated separately. The "sqlite_"
// prefix is reserved for internal objects, which are never declared either.
//
// The patterns are matched by SQLite rather than by this package, so they mean
// what they would mean in a LIKE anywhere else.
//
// A virtual table keeps its data in shadow tables, which are ordinary tables
// as far as sqlite_schema is concerned — an FTS5 index called "docs" brings
// "docs_data", "docs_idx" and several more. They belong to the virtual table
// that created them and are dropped with it, so pragma_table_list, which knows
// which those are, keeps them out of the comparison. It needs SQLite 3.37 or
// newer.
func readSchema(ctx context.Context, q querier, ignore []string) ([]object, error) {
	filters := []string{
		`s.sql IS NOT NULL`,
		`s.name NOT LIKE 'sqlite\_%' ESCAPE '\'`,
		`coalesce(t.type, '') <> 'shadow'`,
	}

	args := make([]any, 0, 2*len(ignore))
	for _, pattern := range ignore {
		filters = append(filters, `s.name NOT LIKE ? ESCAPE '\'`, `s.tbl_name NOT LIKE ? ESCAPE '\'`)
		args = append(args, pattern, pattern)
	}

	query := `SELECT s.type, s.name, s.tbl_name, s.sql, coalesce(t.type = 'virtual', 0)
		FROM sqlite_schema AS s
		LEFT JOIN pragma_table_list AS t ON t.schema = 'main' AND t.name = s.tbl_name
		WHERE ` + strings.Join(filters, " AND ") + " ORDER BY s.rowid;"

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	defer rows.Close() //nolint:errcheck // whatever it would report arrives through rows.Err below

	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql, &o.virtual); err != nil {
			return nil, fmt.Errorf("read schema: %w", err)
		}

		objects = append(objects, o)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}

	return objects, nil
}

// column is one column of a table, as far as copying data between two versions
// of that table is concerned.
type column struct {
	name       string
	notNull    bool
	hasDefault bool
}

// columns returns the columns of a table in declaration order.
//
// Generated columns are deliberately absent: pragma_table_info omits them, and
// they are computed rather than copied, so the list is exactly the set of
// columns a rebuild can carry over.
func columns(ctx context.Context, q querier, table string) ([]column, error) {
	const query = `SELECT name, "notnull", dflt_value IS NOT NULL FROM pragma_table_info(?);`

	rows, err := q.QueryContext(ctx, query, table)
	if err != nil {
		return nil, fmt.Errorf("read columns of %q: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // whatever it would report arrives through rows.Err below

	var cols []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.notNull, &c.hasDefault); err != nil {
			return nil, fmt.Errorf("read columns of %q: %w", table, err)
		}

		cols = append(cols, c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read columns of %q: %w", table, err)
	}

	return cols, nil
}

// openScratch opens a throwaway in-memory database on the driver of the
// database being migrated, used to hold the declared schema and to let SQLite
// rewrite CREATE statements.
func openScratch(drv driver.Driver) *sql.DB {
	db := openDriver(drv, dsn(rand.Text(), map[string]string{
		"cache": "shared",
		"mode":  "memory",
	}), "PRAGMA foreign_keys = off;", "PRAGMA journal_mode = MEMORY;")

	// A shared-cache memory database exists only while a connection to it is
	// open, so the pool must keep exactly one and never retire it.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	return db
}

// rename returns createSQL rewritten to declare the table under name, and
// reports whether that table uses AUTOINCREMENT.
//
// The rewriting is SQLite's own work: the statement is executed in a throwaway
// database and the table renamed, which is precisely the operation SQLite
// already knows how to reflect back into the stored CREATE text. That keeps
// this package free of an SQL parser and correct for quoted identifiers,
// comments, and every constraint syntax it would otherwise have to understand.
//
// The throwaway holds this one table and nothing else. Renaming inside a
// database that holds the rest of the schema would also rewrite every
// reference to the table in other tables' REFERENCES clauses, in views and in
// triggers — which is exactly the corruption the documented rebuild procedure
// exists to avoid.
func rename(ctx context.Context, drv driver.Driver, createSQL string, name string) (string, bool, error) {
	db := openScratch(drv)
	defer db.Close() //nolint:errcheck // an in-memory database being discarded has nothing to report

	fail := func(err error) (string, bool, error) {
		return "", false, fmt.Errorf("rewrite declaration under name %q: %w", name, err)
	}

	if _, err := db.ExecContext(ctx, createSQL+";"); err != nil {
		return fail(err)
	}

	// AUTOINCREMENT brings sqlite_sequence with it, so it is a table too.
	const declared = `SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\';`

	var old string
	if err := db.QueryRowContext(ctx, declared).Scan(&old); err != nil {
		return fail(err)
	}

	alter := fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(old), quoteIdent(name))
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fail(err)
	}

	var rewritten string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE name = ?;`, name).Scan(&rewritten); err != nil {
		return fail(err)
	}

	// This database holds one declared table, so sqlite_sequence is present
	// only if that table declares AUTOINCREMENT.
	var autoincrement bool
	const sequence = `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name = 'sqlite_sequence');`
	if err := db.QueryRowContext(ctx, sequence).Scan(&autoincrement); err != nil {
		return fail(err)
	}

	return rewritten, autoincrement, nil
}

// sequenceOf returns the AUTOINCREMENT high-water mark recorded for a table,
// which a rebuild would otherwise reset and start handing out again.
func sequenceOf(ctx context.Context, q querier, table string) (int64, bool, error) {
	const present = `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name = 'sqlite_sequence');`

	var exists bool
	if err := q.QueryRowContext(ctx, present).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("read sequence of %q: %w", table, err)
	}
	if !exists {
		return 0, false, nil
	}

	var seq int64
	err := q.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?;`, table).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read sequence of %q: %w", table, err)
	}

	return seq, true, nil
}

// hasRows reports whether a table holds anything, which decides whether a
// column that cannot be filled is a problem.
func hasRows(ctx context.Context, q querier, table string) (bool, error) {
	query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s);", quoteIdent(table))

	var any bool
	if err := q.QueryRowContext(ctx, query).Scan(&any); err != nil {
		return false, fmt.Errorf("check whether %q is empty: %w", table, err)
	}

	return any, nil
}

// fold normalizes an identifier for comparison. SQLite matches identifiers
// case-insensitively, so "Users" and "users" are one table and "Name" and
// "name" are one column; a map keyed by the exact spelling would take each
// pair for two objects and offer to drop one of them. The folding is ASCII
// only, which is the range SQLite itself folds without ICU.
func fold(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}

		return r
	}, name)
}

// quoteIdent quotes an identifier for interpolation into a statement. Names
// come from the schema itself rather than from callers, but they can still
// contain characters that need quoting.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteString quotes a string literal for interpolation into a statement.
func quoteString(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
