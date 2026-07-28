package sqlitex

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type config struct {
	params  map[string]string
	pragmas map[string]string
}

// option configures the database connection.
type option func(*config) error

// WithBusyTimeout sets the busy_timeout pragma.
func WithBusyTimeout(timeout time.Duration) option {
	return func(cfg *config) error {
		if timeout < 0 {
			return errors.New("busy timeout must be at least zero")
		}

		cfg.pragmas["busy_timeout"] = strconv.FormatInt(timeout.Milliseconds(), 10)

		return nil
	}
}

// WithCacheSizeKiB sets the cache_size pragma (in KiB).
func WithCacheSizeKiB(size uint64) option {
	return func(cfg *config) error {
		cfg.pragmas["cache_size"] = "-" + strconv.FormatUint(size, 10)

		return nil
	}
}

// WithMemoryMapSize sets the mmap_size pragma.
func WithMemoryMapSize(size uint64) option {
	return func(cfg *config) error {
		cfg.pragmas["mmap_size"] = strconv.FormatUint(size, 10)

		return nil
	}
}

// WithWALAutoCheckpoint sets the wal_autocheckpoint pragma (number of pages).
func WithWALAutoCheckpoint(pages uint64) option {
	return func(cfg *config) error {
		cfg.pragmas["wal_autocheckpoint"] = strconv.FormatUint(pages, 10)

		return nil
	}
}

// WithPragma adds or overrides a specific SQLite pragma.
//
// name and value are interpolated directly into a "PRAGMA name = value;"
// statement, since SQLite has no way to bind PRAGMA parameters. Only pass
// trusted, compile-time values — never forward untrusted user input.
func WithPragma(name, value string) option {
	return func(cfg *config) error {
		const disallowed = ";\x00\r\n"
		if strings.ContainsAny(name, disallowed) || strings.ContainsAny(value, disallowed) {
			return fmt.Errorf("pragma name %q or value %q contains disallowed characters", name, value)
		}

		cfg.pragmas[name] = value

		return nil
	}
}
