//go:build !mattn

package sqlitex_test

import _ "modernc.org/sqlite" // Register sqlite driver

// testDriver is the driver the suite runs against. Build with -tags mattn to
// run the same tests against github.com/mattn/go-sqlite3 instead.
const testDriver = "sqlite"
