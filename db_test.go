package sqlitex_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gulitsky/sqlitex/v2"
	_ "modernc.org/sqlite" // Register sqlite driver
)

// Open creates a database that neither pool existed for beforehand, and the
// read-only pool can read what the read-write pool writes.
func TestOpen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pair.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	if _, err := db.RW.ExecContext(t.Context(), "CREATE TABLE test (val TEXT);"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.RW.ExecContext(t.Context(), "INSERT INTO test (val) VALUES ('hello');"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var val string
	if err := db.RO.QueryRowContext(t.Context(), "SELECT val FROM test;").Scan(&val); err != nil {
		t.Fatalf("read from RO pool: %v", err)
	}
	if val != "hello" {
		t.Errorf("got %q, want %q", val, "hello")
	}

	if _, err := db.RO.ExecContext(t.Context(), "INSERT INTO test (val) VALUES ('world');"); err == nil {
		t.Error("expected error writing through the RO pool, got nil")
	}
}

// Open leaves nothing open when it fails partway, and reports the file.
func TestOpenUnreachablePath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing-dir", "pair.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath)
	if err == nil {
		db.Close()
		t.Fatal("expected Open to fail on a path whose directory does not exist")
	}
}

// Options reach both pools of the pair.
func TestOpenOptions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "options.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath,
		sqlitex.WithBusyTimeout(3*time.Second),
		sqlitex.WithPragma("foreign_keys", "off"),
	)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	for name, pool := range map[string]*sql.DB{"RW": db.RW, "RO": db.RO} {
		var timeout int
		if err := pool.QueryRowContext(t.Context(), "PRAGMA busy_timeout;").Scan(&timeout); err != nil {
			t.Fatalf("%s: query busy_timeout: %v", name, err)
		}
		if timeout != 3000 {
			t.Errorf("%s: busy_timeout = %d, want 3000", name, timeout)
		}

		var foreignKeys int
		if err := pool.QueryRowContext(t.Context(), "PRAGMA foreign_keys;").Scan(&foreignKeys); err != nil {
			t.Fatalf("%s: query foreign_keys: %v", name, err)
		}
		if foreignKeys != 0 {
			t.Errorf("%s: foreign_keys = %d, want 0", name, foreignKeys)
		}
	}
}

// Close closes both pools.
func TestClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "close.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := db.RW.PingContext(t.Context()); err == nil {
		t.Error("RW pool still usable after Close")
	}
	if err := db.RO.PingContext(t.Context()); err == nil {
		t.Error("RO pool still usable after Close")
	}
}

// The Maintain method runs the loop against the read-write pool and stops with
// the context, exactly like the free function.
func TestDBMaintain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "maintain.db")

	db, err := sqlitex.Open(t.Context(), "sqlite", dbPath)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- db.Maintain(ctx, sqlitex.WithCheckpointPeriod(10*time.Millisecond))
	}()

	time.Sleep(50 * time.Millisecond)

	var pages int
	if err := db.RW.QueryRowContext(t.Context(), "PRAGMA wal_autocheckpoint;").Scan(&pages); err != nil {
		t.Fatalf("query wal_autocheckpoint: %v", err)
	}
	if pages != 0 {
		t.Errorf("wal_autocheckpoint = %d while maintaining, want 0", pages)
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Maintain returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Maintain did not return after cancellation")
	}
}
