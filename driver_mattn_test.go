//go:build mattn

package sqlitex_test

import _ "github.com/mattn/go-sqlite3" // Register sqlite3 driver

// testDriver is the driver the suite runs against. The virtual table tests
// additionally need -tags "mattn sqlite_fts5", since this driver leaves FTS5
// out of the build otherwise.
const testDriver = "sqlite3"
