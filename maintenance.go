package sqlitex

import (
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
//
// The loop reports what it does to slog.Default, and adds no attributes of its
// own: a program maintaining more than one database runs the loop through a
// DB, whose DB.Logger it can tag with the name it knows that database by.
func Maintain(ctx context.Context, db *sql.DB, options ...maintenanceOption) error {
	cfg, err := newMaintenanceConfig(options)
	if err != nil {
		return err
	}

	// A lone pool is a pair that reads and writes through the same one. Going
	// through a DB is what gives the loop a logger, which is DB.Logger and
	// here, with nothing to take it from, slog.Default.
	return maintain(ctx, &DB{RW: db, RO: db}, cfg)
}

func newMaintenanceConfig(options []maintenanceOption) (*maintenanceConfig, error) {
	cfg := &maintenanceConfig{
		optimizePeriod:   4 * time.Hour,
		checkpointPeriod: 1 * time.Minute,
	}

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	return cfg, nil
}

// maintain is the body of Maintain, taking a config rather than the options it
// was built from, so that DB.Maintain can hand down its own.
func maintain(ctx context.Context, db *DB, cfg *maintenanceConfig) error {
	pool, logger := db.RW, db.logger()

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
	autoCheckpoint := walAutoCheckpoint(ctx, pool)
	setWALAutoCheckpoint(ctx, pool, 0, logger)
	defer setWALAutoCheckpoint(context.WithoutCancel(ctx), pool, autoCheckpoint, logger)

	doCheckpoint := func(mode string) error {
		// Use WithoutCancel to ensure the checkpoint can run even if the parent context is canceled,
		// while preserving context values (e.g. for tracing).
		baseCtx := context.WithoutCancel(ctx)
		tCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
		defer cancel()

		// Reassert ownership: the pool may have replaced the connection that was
		// configured at startup, and a fresh one checkpoints automatically again.
		setWALAutoCheckpoint(tCtx, pool, 0, logger)

		query := fmt.Sprintf("PRAGMA wal_checkpoint(%s);", mode)
		_, err := pool.ExecContext(tCtx, query)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			logger.Debug("database maintenance stopping", "mode", "TRUNCATE")
			if err := doCheckpoint("TRUNCATE"); err != nil {
				logger.Error("final checkpoint failed", "mode", "TRUNCATE", "error", err)
				return fmt.Errorf("final checkpoint: %w", err)
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
			if _, err := pool.ExecContext(ctx, "PRAGMA optimize;"); err != nil {
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
