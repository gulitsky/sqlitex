package sqlitex

import (
	"fmt"
	"strconv"
	"time"
)

type config struct {
	params  map[string]string
	pragmas map[string]string
}

// Option configures the database connection.
type Option func(*config) error

// WithBusyTimeout sets the busy_timeout pragma.
func WithBusyTimeout(timeout time.Duration) Option {
	return func(cfg *config) error {
		if timeout < 0 {
			return fmt.Errorf("busy timeout must be at least zero")
		}

		cfg.pragmas["busy_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)

		return nil
	}
}

// WithCacheSizeKiB sets the cache_size pragma (in KiB).
func WithCacheSizeKiB(size uint64) Option {
	return func(cfg *config) error {
		cfg.pragmas["cache_size"] = "-" + strconv.FormatUint(size, 10)

		return nil
	}
}

// WithMemoryMapSize sets the mmap_size pragma.
func WithMemoryMapSize(size uint64) Option {
	return func(cfg *config) error {
		cfg.pragmas["mmap_size"] = strconv.FormatUint(size, 10)

		return nil
	}
}

// WithWALAutoCheckpoint sets the wal_autocheckpoint pragma (number of pages).
func WithWALAutoCheckpoint(pages uint64) Option {
	return func(cfg *config) error {
		cfg.pragmas["wal_autocheckpoint"] = strconv.FormatUint(pages, 10)

		return nil
	}
}

// WithPragma adds or overrides a specific SQLite pragma.
func WithPragma(name, value string) Option {
	return func(cfg *config) error {
		cfg.pragmas[name] = value
		return nil
	}
}
