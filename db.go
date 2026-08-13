package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	// path is the file the pair was opened from, reported as the database name
	// in maintenance logs. A DB assembled by hand leaves it empty, and Maintain
	// falls back to querying the database for its own path.
	path string
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

	return &DB{RW: rw, RO: ro, path: filePath}, nil
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

// Maintain runs the maintenance loop on the read-write pool, reporting the
// path the pair was opened from as the database name. It returns only when ctx
// is canceled or the final checkpoint fails.
//
// See Maintain for what the loop does and how it takes over checkpointing.
func (db *DB) Maintain(ctx context.Context, options ...maintenanceOption) error {
	// Prepending the name skips the startup query for it, which would occupy
	// the single connection of the read-write pool. A caller-supplied
	// WithDatabaseName comes later in the slice and still wins.
	options = append([]maintenanceOption{WithDatabaseName(db.path)}, options...)

	return Maintain(ctx, db.RW, options...)
}
