package sqlitex_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gulitsky/sqlitex/v2"
)

func TestOpenReadOnly(t *testing.T) {
	// Setup: Create a real DB first using OpenReadWrite
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	initDB, err := sqlitex.OpenReadWrite(testDriver, dbPath)
	if err != nil {
		t.Fatalf("failed to create init db: %v", err)
	}
	_, err = initDB.Exec("CREATE TABLE test (id INTEGER PRIMARY KEY, val TEXT); INSERT INTO test (val) VALUES ('hello');")
	initDB.Close()
	if err != nil {
		t.Fatalf("failed to init db: %v", err)
	}

	// Test OpenReadOnly
	db, err := sqlitex.OpenReadOnly(testDriver, dbPath)
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

	db, err := sqlitex.OpenReadWrite(testDriver, dbPath)
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
	db1, err := sqlitex.OpenMemory(testDriver)
	if err != nil {
		t.Fatalf("OpenMemory 1 failed: %v", err)
	}
	defer db1.Close()

	db2, err := sqlitex.OpenMemory(testDriver)
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
	db, err := sqlitex.OpenMemory(testDriver, sqlitex.WithPragma("foreign_keys", "off"))
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

// A time.Time has to come back as one whatever driver wrote it, and SQLite's
// own date functions have to be able to read the column either way. The two
// drivers disagree on the format unless each is told which one to use, and
// only one of the two parameters that say so is read by each.
func TestTimeIsStoredInSQLiteFormat(t *testing.T) {
	db, err := sqlitex.OpenMemory(testDriver)
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE events (at DATETIME NOT NULL);`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	when := time.Date(2026, 9, 23, 10, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	if _, err := db.Exec(`INSERT INTO events (at) VALUES (?);`, when); err != nil {
		t.Fatalf("insert a time: %v", err)
	}

	// SQLite parses what was written, rather than returning null for a format
	// only the driver that wrote it understands.
	var normalized sql.NullString
	if err := db.QueryRow(`SELECT datetime(at) FROM events;`).Scan(&normalized); err != nil {
		t.Fatalf("read the time back through datetime(): %v", err)
	}
	if !normalized.Valid {
		var raw string
		_ = db.QueryRow(`SELECT CAST(at AS TEXT) FROM events;`).Scan(&raw)
		t.Fatalf("datetime() cannot parse the stored value %q", raw)
	}
	if normalized.String != "2026-09-23 08:30:00" {
		t.Errorf("datetime(at) = %q, want the value in UTC", normalized.String)
	}

	// And the driver reads its own writing back as the same instant.
	var back time.Time
	if err := db.QueryRow(`SELECT at FROM events;`).Scan(&back); err != nil {
		t.Fatalf("scan into a time.Time: %v", err)
	}
	if !back.Equal(when) {
		t.Errorf("read back %s, want %s", back, when)
	}
}

// WithParam reaches the settings that live in the connection string rather
// than in a pragma, which is the only way to get at some driver behaviour.
func TestWithParam(t *testing.T) {
	db, err := sqlitex.OpenMemory(testDriver, sqlitex.WithParam("mode", "ro"))
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	defer db.Close()

	// OpenMemory applies its own mode last, so the override does not take.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY);`); err != nil {
		t.Fatalf("OpenMemory did not keep mode=memory: %v", err)
	}

	// An empty name would produce a connection string parameter with none.
	if _, err := sqlitex.OpenMemory(testDriver, sqlitex.WithParam("  ", "x")); err == nil {
		t.Error("expected an empty parameter name to be rejected")
	}
}

// A zero period disables one task instead of panicking in time.NewTicker.
func TestMaintainZeroPeriods(t *testing.T) {
	db, err := sqlitex.OpenMemory(testDriver)
	if err != nil {
		t.Fatalf("OpenMemory failed: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- sqlitex.Maintain(ctx, db,
			sqlitex.WithOptimizePeriod(0),
			sqlitex.WithCheckpointPeriod(0),
		)
	}()

	time.Sleep(20 * time.Millisecond)
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

// Maintain owns checkpointing while it runs, and hands it back when it stops.
func TestMaintainWALAutoCheckpoint(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")

	db, err := sqlitex.OpenReadWrite(testDriver, dbPath)
	if err != nil {
		t.Fatalf("OpenReadWrite failed: %v", err)
	}
	defer db.Close()

	autoCheckpoint := func() int {
		t.Helper()

		var pages int
		if err := db.QueryRow("PRAGMA wal_autocheckpoint;").Scan(&pages); err != nil {
			t.Fatalf("query wal_autocheckpoint: %v", err)
		}

		return pages
	}

	before := autoCheckpoint()
	if before <= 0 {
		t.Fatalf("expected automatic checkpoints to be enabled on open, got %d", before)
	}

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- sqlitex.Maintain(ctx, db, sqlitex.WithCheckpointPeriod(10*time.Millisecond))
	}()

	time.Sleep(50 * time.Millisecond)

	if during := autoCheckpoint(); during != 0 {
		t.Errorf("wal_autocheckpoint = %d while maintaining, want 0", during)
	}

	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Maintain returned error: %v", err)
	}

	if after := autoCheckpoint(); after != before {
		t.Errorf("wal_autocheckpoint = %d after maintaining, want %d restored", after, before)
	}
}

func TestMaintain(t *testing.T) {
	db, err := sqlitex.OpenMemory(testDriver)
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
