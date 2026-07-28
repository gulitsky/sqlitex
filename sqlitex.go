// Package sqlitex is a robust wrapper around database/sql for the
// modernc.org/sqlite driver. It configures WAL mode, sane pragmas, and
// connection pooling for read-only and read-write use, and provides a
// background maintenance loop for periodic checkpoints and optimization.
package sqlitex

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// OpenReadOnly opens a database in read-only mode.
// It sets "query_only=yes" and optimizes cache for reading.
//
// The connection pool is sized based on GOMAXPROCS.
func OpenReadOnly(driverName string, filePath string, options ...option) (*sql.DB, error) {
	cfg := &config{
		params: map[string]string{
			"_loc":    "auto",
			"_txlock": "deferred",
			"cache":   "private",
			"mode":    "ro",
		},
		pragmas: commonPragmas(),
	}
	cfg.pragmas["cache_size"] = "-16000"
	cfg.pragmas["query_only"] = "yes"

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	db, err := Open(driverName, dsn(filePath, cfg.params), pragmas(cfg.pragmas)...)
	if err != nil {
		return nil, err
	}

	parallelism := min(8, max(2, runtime.GOMAXPROCS(0)))
	db.SetMaxOpenConns(parallelism)
	db.SetMaxIdleConns(parallelism)

	db.SetConnMaxIdleTime(15 * time.Minute)
	db.SetConnMaxLifetime(4 * time.Hour)

	return db, nil
}

// OpenReadWrite opens a database in read-write mode.
// It sets "mode=rwc", "txlock=immediate", and strictly limits concurrency to 1 connection
// to avoid SQLITE_BUSY errors during transaction upgrades.
//
// Automatic WAL checkpointing is disabled ("wal_autocheckpoint=0"); use Maintain
// to run checkpoints on a schedule instead.
func OpenReadWrite(driverName string, filePath string, options ...option) (*sql.DB, error) {
	cfg := &config{
		params: map[string]string{
			"_loc":    "auto",
			"_txlock": "immediate",
			"cache":   "private",
			"mode":    "rwc",
		},
		pragmas: commonPragmas(),
	}
	cfg.pragmas["analysis_limit"] = "1000"
	cfg.pragmas["cache_size"] = "-64000"
	cfg.pragmas["optimize"] = "0x10002"
	cfg.pragmas["wal_autocheckpoint"] = "0"

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	db, err := Open(driverName, dsn(filePath, cfg.params), pragmas(cfg.pragmas)...)
	if err != nil {
		return nil, err
	}

	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(1)

	db.SetConnMaxIdleTime(1 * time.Hour)
	db.SetConnMaxLifetime(4 * time.Hour)

	return db, nil
}

// OpenMemory opens a new shared memory database.
// Each call creates a distinct database unless a specific name is provided via custom options (not yet supported).
func OpenMemory(driverName string, options ...option) (*sql.DB, error) {
	opts := append(slices.Clone(options), func(cfg *config) error {
		cfg.params["mode"] = "memory"
		cfg.params["cache"] = "shared"
		cfg.pragmas["journal_mode"] = "MEMORY"
		return nil
	})

	db, err := OpenReadWrite(driverName, rand.Text(), opts...)
	if err != nil {
		return nil, err
	}

	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	db.SetMaxIdleConns(1)
	db.SetMaxOpenConns(1)

	return db, nil
}

func commonPragmas() map[string]string {
	return map[string]string{
		"busy_timeout": "5000",
		"foreign_keys": "on",
		"journal_mode": "WAL",
		// mmap_size is set to 32GB. On 32-bit systems, this will be clamped
		// to the maximum process address space, which is safe.
		"mmap_size":   "34359738368",
		"synchronous": "NORMAL",
		"temp_store":  "MEMORY",
	}
}

func dsn(filePath string, params map[string]string) string {
	u, err := url.Parse(filePath)
	if err != nil || u.Scheme == "" {
		u = &url.URL{Scheme: "file"}
		if filepath.IsAbs(filePath) {
			// SQLite's file: URI filenames always use forward slashes, and
			// Windows drive-letter paths (C:/...) need a leading slash.
			p := filepath.ToSlash(filePath)
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			u.Path = p
		} else {
			u.Opaque = filePath
		}
	}

	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	return u.String()
}

func pragmas(m map[string]string) []string {
	keys := slices.Sorted(maps.Keys(m))

	res := make([]string, 0, len(m))
	for _, k := range keys {
		res = append(res, fmt.Sprintf("PRAGMA %s = %s;", k, m[k]))
	}

	return res
}
