package sqlitex

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type maintenanceConfig struct {
	optimizePeriod   time.Duration
	checkpointPeriod time.Duration
	logger           *slog.Logger
	database         string
}

// maintenanceOption configures the maintenance loop.
type maintenanceOption func(*maintenanceConfig) error

// WithOptimizePeriod sets the interval for PRAGMA optimize.
// A zero period disables periodic optimization.
func WithOptimizePeriod(period time.Duration) maintenanceOption {
	return func(cfg *maintenanceConfig) error {
		if period < 0 {
			return errors.New("optimize period must be at least zero")
		}

		cfg.optimizePeriod = period

		return nil
	}
}

// WithCheckpointPeriod sets the interval for PRAGMA wal_checkpoint(PASSIVE).
// A zero period disables periodic checkpoints; the final checkpoint on shutdown
// still runs.
func WithCheckpointPeriod(period time.Duration) maintenanceOption {
	return func(cfg *maintenanceConfig) error {
		if period < 0 {
			return errors.New("checkpoint period must be at least zero")
		}

		cfg.checkpointPeriod = period

		return nil
	}
}

// WithLogger sets the structured logger for maintenance events.
func WithLogger(logger *slog.Logger) maintenanceOption {
	return func(cfg *maintenanceConfig) error {
		if logger == nil {
			return errors.New("logger must not be nil")
		}

		cfg.logger = logger

		return nil
	}
}

// WithDatabaseName sets the name reported in the "database" attribute of every
// maintenance log record. When unset, Maintain queries the database for the
// file path of its main schema, which needs a pooled connection at startup;
// pass the name explicitly to skip that query.
func WithDatabaseName(name string) maintenanceOption {
	return func(cfg *maintenanceConfig) error {
		cfg.database = name
		return nil
	}
}

// Maintain starts a background maintenance loop for the database.
// It performs periodic WAL checkpoints and optimizations.
// It returns only when ctx is canceled or a fatal error occurs during final checkpoint.
//
// While the loop runs it takes over WAL checkpointing: it disables SQLite's
// automatic checkpoints ("wal_autocheckpoint=0") so that only this loop
// checkpoints, and restores the previous setting before returning. Since
// wal_autocheckpoint is a per-connection setting, this covers the connections
// the loop itself uses; pass WithWALAutoCheckpoint(0) to OpenReadWrite to
// disable automatic checkpoints on every connection in the pool.
func Maintain(ctx context.Context, db *sql.DB, options ...maintenanceOption) error {
	cfg := &maintenanceConfig{
		optimizePeriod:   4 * time.Hour,
		checkpointPeriod: 1 * time.Minute,
		logger:           slog.Default(),
	}

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return fmt.Errorf("apply option: %w", err)
		}
	}

	database := cfg.database
	if database == "" {
		database = mainDatabaseName(ctx, db)
	}

	logger := cfg.logger.With("database", database)

	logger.Info("database maintenance started",
		"optimize_period", cfg.optimizePeriod,
		"checkpoint_period", cfg.checkpointPeriod)

	// A nil channel never fires, so a zero period disables that task.
	var optC, cpC <-chan time.Time

	if cfg.optimizePeriod > 0 {
		optTicker := time.NewTicker(cfg.optimizePeriod)
		defer optTicker.Stop()
		optC = optTicker.C
	}

	if cfg.checkpointPeriod > 0 {
		cpTicker := time.NewTicker(cfg.checkpointPeriod)
		defer cpTicker.Stop()
		cpC = cpTicker.C
	}

	// Take over checkpointing for the lifetime of the loop, and hand it back on
	// the way out so that a database outliving its maintenance keeps checkpointing.
	autoCheckpoint := walAutoCheckpoint(ctx, db)
	setWALAutoCheckpoint(ctx, db, 0, logger)
	defer setWALAutoCheckpoint(context.WithoutCancel(ctx), db, autoCheckpoint, logger)

	doCheckpoint := func(mode string) error {
		// Use WithoutCancel to ensure the checkpoint can run even if the parent context is canceled,
		// while preserving context values (e.g. for tracing).
		baseCtx := context.WithoutCancel(ctx)
		tCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
		defer cancel()

		// Reassert ownership: the pool may have replaced the connection that was
		// configured at startup, and a fresh one checkpoints automatically again.
		setWALAutoCheckpoint(tCtx, db, 0, logger)

		query := fmt.Sprintf("PRAGMA wal_checkpoint(%s);", mode)
		_, err := db.ExecContext(tCtx, query)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			logger.Info("database maintenance stopping", "mode", "TRUNCATE")
			if err := doCheckpoint("TRUNCATE"); err != nil {
				logger.Error("final checkpoint failed", "mode", "TRUNCATE", "error", err)
				return fmt.Errorf("final checkpoint on %s: %w", database, err)
			}
			logger.Info("database maintenance stopped")
			return nil

		case <-cpC:
			if err := doCheckpoint("PASSIVE"); err != nil {
				logger.Warn("checkpoint failed", "mode", "PASSIVE", "error", err)
			} else {
				logger.Debug("checkpoint completed", "mode", "PASSIVE")
			}

		case <-optC:
			if _, err := db.ExecContext(ctx, "PRAGMA optimize;"); err != nil {
				logger.Warn("optimize failed", "error", err)
			} else {
				logger.Debug("optimize completed")
			}
		}
	}
}

// defaultWALAutoCheckpoint is SQLite's built-in wal_autocheckpoint page count,
// used when the current setting cannot be read.
const defaultWALAutoCheckpoint = 1000

// walAutoCheckpoint reads the current wal_autocheckpoint page count, falling
// back to SQLite's default if it cannot be read.
func walAutoCheckpoint(ctx context.Context, db *sql.DB) int {
	tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	var pages int
	if err := db.QueryRowContext(tCtx, "PRAGMA wal_autocheckpoint;").Scan(&pages); err != nil {
		return defaultWALAutoCheckpoint
	}

	return pages
}

// setWALAutoCheckpoint sets the wal_autocheckpoint page count. Failure is logged
// and otherwise ignored: it only means SQLite keeps checkpointing on its own
// schedule alongside the maintenance loop, which is safe.
func setWALAutoCheckpoint(ctx context.Context, db *sql.DB, pages int, logger *slog.Logger) {
	tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	query := fmt.Sprintf("PRAGMA wal_autocheckpoint = %d;", pages)
	if _, err := db.ExecContext(tCtx, query); err != nil {
		logger.Debug("setting wal_autocheckpoint failed", "pages", pages, "error", err)
	}
}

// mainDatabaseName reports the file path backing the main schema of db, for use
// as a log attribute. In-memory and temporary databases have no file path, so
// they are reported as ":memory:"; a database that cannot be queried is
// reported as "unknown" rather than failing maintenance.
//
// The timeout is deliberately short: the query may have to wait for a pooled
// connection, and a read-write pool holds only one. Delaying the caller is
// worse than logging an unresolved name.
func mainDatabaseName(ctx context.Context, q querier) string {
	const unknown = "unknown"

	tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	rows, err := q.QueryContext(tCtx, "PRAGMA database_list;")
	if err != nil {
		return unknown
	}
	defer rows.Close()

	database := unknown
	for rows.Next() {
		var (
			seq    int
			schema string
			file   sql.NullString
		)
		if err := rows.Scan(&seq, &schema, &file); err != nil {
			return unknown
		}
		if schema != "main" {
			continue
		}

		database = cmp.Or(file.String, ":memory:")

		break
	}

	if rows.Err() != nil {
		return unknown
	}

	return database
}
