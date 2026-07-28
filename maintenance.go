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
	logger           *slog.Logger
}

// maintenanceOption configures the maintenance loop.
type maintenanceOption func(*maintenanceConfig) error

// WithOptimizePeriod sets the interval for PRAGMA optimize.
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
		cfg.logger = logger
		return nil
	}
}

// Maintain starts a background maintenance loop for the database.
// It performs periodic WAL checkpoints and optimizations.
// It returns only when ctx is canceled or a fatal error occurs during final checkpoint.
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

	cfg.logger.Info("starting background maintenance",
		"optimize_period", cfg.optimizePeriod,
		"checkpoint_period", cfg.checkpointPeriod)

	optTicker := time.NewTicker(cfg.optimizePeriod)
	defer optTicker.Stop()

	cpTicker := time.NewTicker(cfg.checkpointPeriod)
	defer cpTicker.Stop()

	doCheckpoint := func(mode string) error {
		// Use WithoutCancel to ensure the checkpoint can run even if the parent context is canceled,
		// while preserving context values (e.g. for tracing).
		baseCtx := context.WithoutCancel(ctx)
		tCtx, cancel := context.WithTimeout(baseCtx, 10*time.Second)
		defer cancel()

		query := fmt.Sprintf("PRAGMA wal_checkpoint(%s);", mode)
		_, err := db.ExecContext(tCtx, query)
		return err
	}

	for {
		select {
		case <-ctx.Done():
			cfg.logger.Info("stopping maintenance, performing final checkpoint")
			if err := doCheckpoint("TRUNCATE"); err != nil {
				cfg.logger.Error("final checkpoint failed", "error", err)
				return fmt.Errorf("final checkpoint: %w", err)
			}
			cfg.logger.Info("maintenance stopped gracefully")
			return nil

		case <-cpTicker.C:
			if err := doCheckpoint("PASSIVE"); err != nil {
				cfg.logger.Warn("passive checkpoint failed", "error", err)
			} else {
				cfg.logger.Debug("passive checkpoint completed")
			}

		case <-optTicker.C:
			if _, err := db.ExecContext(ctx, "PRAGMA optimize;"); err != nil {
				cfg.logger.Warn("optimization failed", "error", err)
			} else {
				cfg.logger.Debug("optimization completed")
			}
		}
	}
}
