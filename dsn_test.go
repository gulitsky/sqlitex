package sqlitex

import (
	"runtime"
	"testing"
)

func TestDSN(t *testing.T) {
	tests := map[string]struct {
		filePath string
		want     string
	}{
		"relative": {
			filePath: "test.db",
			want:     "file:test.db?mode=ro",
		},
		"relative with directory": {
			filePath: "data/test.db",
			want:     "file:data/test.db?mode=ro",
		},
		"absolute": {
			filePath: "/var/lib/app/test.db",
			want:     "file:///var/lib/app/test.db?mode=ro",
		},
		// A drive letter parses as a scheme, so without special handling the params
		// would end up in a DSN the driver ignores.
		"windows drive letter": {
			filePath: "C:/data/test.db",
			want:     "file:///C:/data/test.db?mode=ro",
		},
		// Opaque file names are written verbatim by url.URL.String, so anything that
		// could be read as query or fragment has to be escaped.
		"question mark": {
			filePath: "a?b.db",
			want:     "file:a%3Fb.db?mode=ro",
		},
		"hash": {
			filePath: "a#b.db",
			want:     "file:a%23b.db?mode=ro",
		},
		"space": {
			filePath: "a b.db",
			want:     "file:a%20b.db?mode=ro",
		},
		"memory": {
			filePath: ":memory:",
			want:     "file::memory:?mode=ro",
		},
		"uri keeps its own params": {
			filePath: "file:test.db?_txlock=deferred",
			want:     "file:test.db?_txlock=deferred&mode=ro",
		},
	}

	// filepath.ToSlash only rewrites separators on Windows; elsewhere a backslash
	// is an ordinary character in a file name and must be left alone.
	if runtime.GOOS == "windows" {
		tests["windows separators"] = struct {
			filePath string
			want     string
		}{
			filePath: `C:\data\test.db`,
			want:     "file:///C:/data/test.db?mode=ro",
		}
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := dsn(tt.filePath, map[string]string{"mode": "ro"})
			if got != tt.want {
				t.Errorf("dsn(%q) = %q, want %q", tt.filePath, got, tt.want)
			}
		})
	}
}
