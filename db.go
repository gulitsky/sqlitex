package sqlitex

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// DB is a matched pair of pools over one database file: a single-connection
// writer and a concurrent reader. Both are exported, so a DB is used through
// database/sql directly — pick RW for statements that write and RO for the
// ones that only read.
//
// A DB is not required. Applications that need only one of the two roles can
// call OpenReadWrite or OpenReadOnly and use the resulting pool on its own.
type DB struct {
	RW *sql.DB // read-write, limited to one connection
	RO *sql.DB // read-only, sized by GOMAXPROCS

	// Logger receives what Migrate and Maintain have to say about this pair.
	// Open sets it from WithLogger; a pair assembled by hand sets it itself,
	// which is also how a lone pool is given one:
	//
	//	db := &sqlitex.DB{RW: pool, RO: pool, Logger: logger}
	//
	// A nil Logger means slog.Default as of the moment each record is
	// written. The package tags its records with nothing of its own, so a
	// program that opens more than one database passes each pair a logger
	// already carrying whatever it calls that one.
	Logger *slog.Logger
}

// Open opens a read-write and a read-only pool over filePath.
//
// The read-write pool is opened first and pinged, because the pools are lazy:
// nothing touches the file until a connection is actually needed, and a
// read-only connection can create neither the database nor its WAL. Pinging
// forces that work to happen while the read-write pool is the only one open.
//
// Options apply to both pools. The role-specific defaults still differ — cache
// size, transaction locking, and the optimize settings are chosen per role —
// so an option that overrides one of them overrides it for both.
//
// The returned DB must be closed.
func Open(ctx context.Context, driverName string, filePath string, options ...option) (*DB, error) {
	// Applied here only to reach the logger, which is the one option that
	// outlives the connection string it is passed with. The pools apply them
	// again, over the defaults each role opens with.
	cfg := &config{params: map[string]string{}, pragmas: map[string]string{}}
	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	rw, err := OpenReadWrite(driverName, filePath, options...)
	if err != nil {
		return nil, err
	}

	if err := rw.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("open read-write pool on %s: %w", filePath, err), rw.Close())
	}

	ro, err := OpenReadOnly(driverName, filePath, options...)
	if err != nil {
		return nil, errors.Join(err, rw.Close())
	}

	if err := ro.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("open read-only pool on %s: %w", filePath, err), ro.Close(), rw.Close())
	}

	return &DB{RW: rw, RO: ro, Logger: cfg.logger}, nil
}

// Close closes both pools and returns their joined errors.
//
// The read-only pool is closed first: its connections hold read locks that
// would otherwise keep a concurrent final checkpoint from truncating the WAL.
func (db *DB) Close() error {
	// A pair assembled by hand — over a memory database, say — may use one pool
	// for both roles, and closing it twice would report the second close as an error.
	if db.RO == db.RW {
		return db.RW.Close()
	}

	return errors.Join(db.RO.Close(), db.RW.Close())
}

// Migrate brings the schema in line with the declared one, on the read-write
// pool, reporting to Logger. See the [Migrate] function for what it does and
// what it refuses to do.
func (db *DB) Migrate(ctx context.Context, schema string, options ...migrateOption) error {
	cfg, err := newMigrateConfig(options)
	if err != nil {
		return err
	}

	return migrate(ctx, db, schema, cfg)
}

// Maintain runs the maintenance loop on the read-write pool, reporting to
// Logger. It returns when ctx is canceled.
//
// See Maintain for what the loop does and how it takes over checkpointing.
func (db *DB) Maintain(ctx context.Context, options ...maintenanceOption) error {
	cfg, err := newMaintenanceConfig(options)
	if err != nil {
		return err
	}

	return maintain(ctx, db, cfg)
}

// logger reports where this pair's records go, which is slog.Default until
// something says otherwise.
func (db *DB) logger() *slog.Logger {
	return cmp.Or(db.Logger, slog.Default())
}
