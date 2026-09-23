// Package sqlitex is a robust wrapper around database/sql for SQLite. It
// configures WAL mode, sane pragmas, and connection pooling for read-only and
// read-write use, and provides a background maintenance loop for periodic
// checkpoints and optimization.
//
// The driver is a parameter: every constructor takes the name one was
// registered under. Both modernc.org/sqlite (pure Go, registered as "sqlite")
// and github.com/mattn/go-sqlite3 (cgo, registered as "sqlite3") are tested,
// and the package neither imports nor depends on either.
//
// SQLite 3.37 or later is required, since the schema a migration compares
// against may use STRICT tables. Both drivers have shipped a newer one for
// years. Declaring a virtual table additionally needs the module it names to
// be compiled in: mattn/go-sqlite3 leaves FTS5 out unless it is built with
// -tags sqlite_fts5, where modernc.org/sqlite always has it.
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
//
// The database and its WAL have to exist already: a read-only connection can
// create neither, and the pool is lazy, so the failure surfaces at the first
// query rather than here. Open pairs this pool with a read-write one and
// establishes that ordering itself.
func OpenReadOnly(driverName string, filePath string, options ...option) (*sql.DB, error) {
	cfg := &config{
		params:  commonParams("deferred", "ro"),
		pragmas: commonPragmas(),
	}
	cfg.pragmas["cache_size"] = "-16000"
	cfg.pragmas["query_only"] = "yes"

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	db, err := open(driverName, dsn(filePath, cfg.params), pragmas(cfg.pragmas)...)
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
// SQLite's automatic WAL checkpointing is left enabled, so a database opened
// this way is safe without a maintenance loop. Maintain disables it for as long
// as it runs; pass WithWALAutoCheckpoint(0) to disable it on every connection.
//
// Applications that also read concurrently want a read-only pool alongside
// this one, since it is limited to a single connection. Open provides both.
func OpenReadWrite(driverName string, filePath string, options ...option) (*sql.DB, error) {
	cfg := &config{
		params:  commonParams("immediate", "rwc"),
		pragmas: commonPragmas(),
	}
	cfg.pragmas["analysis_limit"] = "1000"
	cfg.pragmas["cache_size"] = "-64000"
	cfg.pragmas["optimize"] = "0x10002"

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("apply option: %w", err)
		}
	}

	db, err := open(driverName, dsn(filePath, cfg.params), pragmas(cfg.pragmas)...)
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

// commonParams are the connection string parameters every pool is opened with,
// given the transaction lock and access mode that distinguish the pools.
//
// Two of them are read by one driver each and ignored by the other, which is
// what makes a database written through either one readable through both.
// mattn/go-sqlite3 reads "_loc" and writes a time.Time in SQLite's own format;
// modernc.org/sqlite reads "_time_format" and would otherwise write Go's
// time.String(), which neither the other driver nor SQLite's date functions
// can parse.
func commonParams(txlock, mode string) map[string]string {
	return map[string]string{
		"_loc":         "auto",
		"_time_format": "sqlite",
		"_txlock":      txlock,
		"cache":        "private",
		"mode":         mode,
	}
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
	// A Windows drive letter parses as a one-letter scheme ("C:/db" -> "c"), so it
	// has to be recognized here rather than treated as an already-formed URI.
	u, err := url.Parse(filePath)
	if err != nil || u.Scheme == "" || isDriveLetter(u.Scheme) {
		u = &url.URL{Scheme: "file"}

		// SQLite's file: URI filenames always use forward slashes.
		p := filepath.ToSlash(filePath)
		if isAbs(p) {
			// Windows drive-letter paths (C:/...) need a leading slash.
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			u.Path = p
		} else {
			// Opaque is written verbatim by URL.String, so escape it the same way
			// EscapedPath would, keeping "?" and "#" out of the query and fragment.
			u.Opaque = (&url.URL{Path: p}).EscapedPath()
		}
	}

	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	return u.String()
}

// isDriveLetter reports whether scheme is a single letter, which url.Parse
// produces for a Windows drive-letter path such as "C:/data/db.sqlite".
func isDriveLetter(scheme string) bool {
	if len(scheme) != 1 {
		return false
	}

	c := scheme[0]

	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isAbs reports whether a slash-separated path is absolute for SQLite's purposes,
// on any host OS: rooted ("/data/db.sqlite") or drive-qualified ("C:/db.sqlite").
func isAbs(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}

	prefix, _, found := strings.Cut(p, "/")

	return found && len(prefix) == 2 && prefix[1] == ':' && isDriveLetter(prefix[:1])
}

func pragmas(m map[string]string) []string {
	keys := slices.Sorted(maps.Keys(m))

	res := make([]string, 0, len(m))
	for _, k := range keys {
		res = append(res, fmt.Sprintf("PRAGMA %s = %s;", k, m[k]))
	}

	return res
}
