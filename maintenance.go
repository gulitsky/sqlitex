package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
// A zero period disables periodic checkpoints and leaves SQLite's automatic
// ones in place, since they are then all the database has; the final
// checkpoint on shutdown still runs.
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
//
// It returns when ctx is canceled, after a final wal_checkpoint(TRUNCATE). A
// final checkpoint that fails, or that a reader or a writer keeps from
// finishing, is reported to the logger and nothing more: the WAL it leaves
// behind is applied by the next connection to open the database, so a shutdown
// is not a failure of the program shutting down. The only error Maintain
// returns is one an option gave it.
//
// While it checkpoints on a timer the loop takes over WAL checkpointing: it
// disables SQLite's automatic checkpoints ("wal_autocheckpoint=0") so that
// only this loop checkpoints, and restores the previous setting before
// returning. Since wal_autocheckpoint is a per-connection setting, this covers
// the connections the loop itself uses; pass WithWALAutoCheckpoint(0) to
// OpenReadWrite to disable automatic checkpoints on every connection in the
// pool. WithCheckpointPeriod(0) hands checkpointing back to SQLite entirely,
// all but the final one.
//
// Taking over that way counts on the read-write pool being the single
// connection OpenReadWrite gives it. A pool of several connections hands each
// pragma whichever one is free, so which connections end up with automatic
// checkpoints off, and which get the old value back, is up to the pool.
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
		slog.Duration("optimize_period", cfg.optimizePeriod),
		slog.Duration("checkpoint_period", cfg.checkpointPeriod))

	// A nil channel never fires, so a zero period disables that task.
	var optC, cpC <-chan time.Time

	if cfg.optimizePeriod > 0 {
		optTicker := time.NewTicker(cfg.optimizePeriod)
		defer optTicker.Stop()
		optC = optTicker.C
	}

	// Checkpointing on a timer is what makes this loop the one that
	// checkpoints, so it is also what decides whether SQLite's own automatic
	// checkpoints are in the way. With a zero period they are all the database
	// has until the final checkpoint, and switching them off would leave the
	// WAL to grow unchecked for the whole run.
	owned := cfg.checkpointPeriod > 0

	if owned {
		cpTicker := time.NewTicker(cfg.checkpointPeriod)
		defer cpTicker.Stop()
		cpC = cpTicker.C

		// Take over checkpointing for the lifetime of the loop, and hand it back on
		// the way out so that a database outliving its maintenance keeps checkpointing.
		autoCheckpoint := walAutoCheckpoint(ctx, pool, logger)
		setWALAutoCheckpoint(ctx, pool, 0, logger)
		defer setWALAutoCheckpoint(context.WithoutCancel(ctx), pool, autoCheckpoint, logger)
	}

	// Checkpoints on a database that keeps no WAL do nothing at all, which is
	// worth saying once rather than on every tick.
	if mode := journalMode(ctx, pool, logger); mode != "" && !strings.EqualFold(mode, "wal") {
		logger.Warn("checkpoints have no effect", slog.String("journal_mode", mode))
	}

	doCheckpoint := func(mode string) (checkpointResult, error) {
		// Use WithoutCancel to ensure the checkpoint can run even if the parent context is canceled,
		// while preserving context values (e.g. for tracing).
		baseCtx := context.WithoutCancel(ctx)
		tCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
		defer cancel()

		if owned {
			// Reassert ownership: the pool may have replaced the connection that was
			// configured at startup, and a fresh one checkpoints automatically again.
			setWALAutoCheckpoint(tCtx, pool, 0, logger)
		}

		// A checkpoint that cannot finish is not an error: SQLite reports a
		// reader or writer in the way as an ordinary result row with busy set,
		// so the row is what has to be read to tell the two apart.
		var busy, walFrames, checkpointed int

		query := fmt.Sprintf("PRAGMA wal_checkpoint(%s);", mode)
		if err := pool.QueryRowContext(tCtx, query).Scan(&busy, &walFrames, &checkpointed); err != nil {
			return checkpointResult{}, err
		}

		return checkpointResult{busy: busy == 1, walFrames: walFrames, checkpointed: checkpointed}, nil
	}

	// Consecutive PASSIVE checkpoints that left frames behind. One is ordinary
	// — a transaction was open as it ran — where a run of them is a reader or
	// a writer that never lets go, and a WAL that only grows.
	var lagging int

	for {
		select {
		case <-ctx.Done():
			logger.Debug("database maintenance stopping", slog.String("mode", "TRUNCATE"))

			start := time.Now()

			res, err := doCheckpoint("TRUNCATE")
			switch {
			case err != nil:
				logger.Warn("final checkpoint failed",
					slog.String("mode", "TRUNCATE"), slog.Any("error", err))
			case res.busy:
				logger.Warn("final checkpoint incomplete",
					slog.String("mode", "TRUNCATE"),
					slog.Int("wal_frames", res.walFrames),
					slog.Int("checkpointed", res.checkpointed))
			}

			logger.Info("database maintenance stopped", slog.Duration("duration", time.Since(start)))

			return nil

		case <-cpC:
			res, err := doCheckpoint("PASSIVE")
			switch {
			case err != nil:
				logger.Warn("checkpoint failed",
					slog.String("mode", "PASSIVE"), slog.Any("error", err))
			case res.walFrames < 0:
				// No WAL to checkpoint, which the loop reported at startup.
			case res.checkpointed < res.walFrames:
				lagging++

				level := slog.LevelDebug
				if lagging >= laggingCheckpoints {
					level = slog.LevelWarn
				}

				logger.Log(ctx, level, "checkpoint lagging",
					slog.String("mode", "PASSIVE"),
					slog.Int("wal_frames", res.walFrames),
					slog.Int("checkpointed", res.checkpointed),
					slog.Int("consecutive", lagging))
			default:
				lagging = 0
				logger.Debug("checkpoint completed",
					slog.String("mode", "PASSIVE"), slog.Int("wal_frames", res.walFrames))
			}

		case <-optC:
			tCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := pool.ExecContext(tCtx, "PRAGMA optimize;")
			cancel()

			switch {
			case err != nil && ctx.Err() != nil:
				// Shutdown landed on an optimize. Nothing broke, and the
				// final checkpoint is what the loop still owes the database.
			case err != nil:
				logger.Warn("optimize failed", slog.Any("error", err))
			default:
				logger.Debug("optimize completed")
			}
		}
	}
}

// laggingCheckpoints is how many PASSIVE checkpoints in a row may leave frames
// behind before the loop says so out loud.
const laggingCheckpoints = 5

// checkpointResult is the row PRAGMA wal_checkpoint returns: whether the
// checkpoint gave up on a lock, how many frames the WAL held, and how many of
// them made it into the database. The two counts are -1 when the database
// keeps no WAL.
type checkpointResult struct {
	busy         bool
	walFrames    int
	checkpointed int
}

// journalMode reports the journal mode of the database, or "" if it cannot be read.
func journalMode(ctx context.Context, db *sql.DB, logger *slog.Logger) string {
	tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	var mode string
	if err := db.QueryRowContext(tCtx, "PRAGMA journal_mode;").Scan(&mode); err != nil {
		logger.Debug("reading journal_mode failed", slog.Any("error", err))

		return ""
	}

	return mode
}

// defaultWALAutoCheckpoint is SQLite's built-in wal_autocheckpoint page count,
// used when the current setting cannot be read.
const defaultWALAutoCheckpoint = 1000

// walAutoCheckpoint reads the current wal_autocheckpoint page count, falling
// back to SQLite's default if it cannot be read. The fallback is logged,
// because restoring it on the way out turns automatic checkpoints back on for
// a database that was opened with WithWALAutoCheckpoint(0) to keep them off.
func walAutoCheckpoint(ctx context.Context, db *sql.DB, logger *slog.Logger) int {
	tCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	var pages int
	if err := db.QueryRowContext(tCtx, "PRAGMA wal_autocheckpoint;").Scan(&pages); err != nil {
		logger.Debug("reading wal_autocheckpoint failed",
			slog.Int("fallback_pages", defaultWALAutoCheckpoint), slog.Any("error", err))

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
		logger.Debug("setting wal_autocheckpoint failed",
			slog.Int("pages", pages), slog.Any("error", err))
	}
}
