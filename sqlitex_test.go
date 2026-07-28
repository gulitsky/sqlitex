package sqlitex_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gulitsky/sqlitex"
	_ "modernc.org/sqlite" // Register sqlite driver
)

func TestOpenReadOnly(t *testing.T) {
	// Setup: Create a real DB first using OpenReadWrite
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	initDB, err := sqlitex.OpenReadWrite("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to create init db: %v", err)
	}
	_, err = initDB.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY, val TEXT); INSERT INTO test (val) VALUES ('hello');")
	initDB.Close()
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}

	// Test OpenReadOnly
	db, err := sqlitex.OpenReadOnly("sqlite", dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly failed: %v", err)
	}
	defer db.Close()

	// Verify we can read
	var val string
	err = db.QueryRow("SELECT val FROM test WHERE id=1").Scan(&val)
	if err != nil {
		t.Errorf("failed to read from RO db: %v", err)
	}
	if val != "hello" {
		t.Errorf("got %q, want 'hello'", val)
	}

	// Verify we cannot write
	_, err = db.Exec("INSERT INTO test (val) VALUES ('world')")
	if err == nil {
		t.Error("expected error writing to RO db, got nil")
	}
}

func TestOpenReadWrite(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_rw.db")

	db, err := sqlitex.OpenReadWrite("sqlite", dbPath)
	if err != nil {
		t.Fatalf("OpenReadWrite failed: %v", err)
	}
	defer db.Close()

	// Verify strict concurrency (can't easily test without causing deadlock or check internal state,
	// but we can check if it works)
	_, err = db.Exec("CREATE TABLE foo (bar TEXT)")
	if err != nil {
		t.Errorf("failed to write: %v", err)
	}
}

func TestOpenMemory(t *testing.T) {
	// Open two memory databases. They should be distinct.
	db1, err := sqlitex.OpenMemory("sqlite")
	if err != nil {
		t.Fatalf("OpenMemory 1 failed: %v", err)
	}
	defer db1.Close()

	db2, err := sqlitex.OpenMemory("sqlite")
	if err != nil {
		t.Fatalf("OpenMemory 2 failed: %v", err)
	}
	defer db2.Close()

	if _, err := db1.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatalf("db1 create: %v", err)
	}
	if _, err := db1.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("db1 insert: %v", err)
	}

	// db2 should not have table t
	_, err = db2.Exec("SELECT * FROM t")
	if err == nil {
		t.Error("db2 should not see table t from db1")
	}
}

func TestWithPragma(t *testing.T) {
	db, err := sqlitex.OpenMemory("sqlite", sqlitex.WithPragma("foreign_keys", "off"))
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	defer db.Close()

	var fk string
	err = db.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if err != nil {
		t.Fatalf("query pragma: %v", err)
	}
	// "0" is off in sqlite result
	if fk != "0" {
		t.Errorf("expected foreign_keys=0, got %q", fk)
	}
}

func TestMaintain(t *testing.T) {
	db, err := sqlitex.OpenMemory("sqlite")
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(t.Context())

	// Start maintenance in background
	errCh := make(chan error)
	go func() {
		// Use short periods for testing
		errCh <- sqlitex.Maintain(ctx, db,
			sqlitex.WithOptimizePeriod(10*time.Millisecond),
			sqlitex.WithCheckpointPeriod(10*time.Millisecond),
		)
	}()

	// Let it run a bit
	time.Sleep(50 * time.Millisecond)
	cancel()

	// Wait for return
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Maintain returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Maintain did not return after cancellation")
	}
}
