package sqlitex

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"runtime"
	"sort"
	"time"
)

// OpenReadOnly opens a database in read-only mode.
// It sets "query_only=yes" and optimizes cache for reading.
//
// The connection pool is sized based on GOMAXPROCS.
func OpenReadOnly(driverName string, filePath string, options ...Option) (*sql.DB, error) {
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

	if db != nil {
		parallelism := min(8, max(2, runtime.GOMAXPROCS(0)))
		db.SetMaxOpenConns(parallelism)
		db.SetMaxIdleConns(parallelism)

		db.SetConnMaxIdleTime(15 * time.Minute)
		db.SetConnMaxLifetime(4 * time.Hour)
	}

	return db, err
}

// OpenReadWrite opens a database in read-write mode.
// It sets "mode=rwc", "txlock=immediate", and strictly limits concurrency to 1 connection
// to avoid SQLITE_BUSY errors during transaction upgrades.
func OpenReadWrite(driverName string, filePath string, options ...Option) (*sql.DB, error) {
	cfg := &config{
		params: map[string]string{
			"_loc":    "auto",
			"_txlock": "immediate",
			"cache":   "private",
			"mode":    "rwc",
		},
		pragmas: commonPragmas(),
	}
	cfg.pragmas["cache_size"] = "-64000"
	cfg.pragmas["optimize"] = "0x10002"

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	db, err := Open(driverName, dsn(filePath, cfg.params), pragmas(cfg.pragmas)...)

	if err != nil {
		return nil, err
	}

	if db != nil {
		db.SetMaxIdleConns(1)
		db.SetMaxOpenConns(1)

		db.SetConnMaxIdleTime(1 * time.Hour)
		db.SetConnMaxLifetime(4 * time.Hour)
	}

	return db, err
}

// OpenMemory opens a new shared memory database.
// Each call creates a distinct database unless a specific name is provided via custom options (not yet supported).
func OpenMemory(driverName string, options ...Option) (*sql.DB, error) {
	db, err := OpenReadWrite(driverName, rand.Text(), append(options, func(cfg *config) error {
		cfg.params["mode"] = "memory"
		cfg.params["cache"] = "shared"
		cfg.pragmas["journal_mode"] = "MEMORY"
		return nil
	})...)

	if err != nil {
		return nil, err
	}

	if db != nil {
		db.SetConnMaxIdleTime(0)
		db.SetConnMaxLifetime(0)

		db.SetMaxIdleConns(1)
		db.SetMaxOpenConns(1)
	}

	return db, err
}

func commonPragmas() map[string]string {
	return map[string]string{
		"analysis_limit": "1000",
		"busy_timeout":   "5000",
		"foreign_keys":   "on",
		"journal_mode":   "WAL",
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
		if len(filePath) > 0 && filePath[0] == '/' {
			u.Path = filePath
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
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	res := make([]string, 0, len(m))
	for _, k := range keys {
		res = append(res, fmt.Sprintf("PRAGMA %s = %s;", k, m[k]))
	}

	return res
}
