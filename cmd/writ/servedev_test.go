package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writ serve --dev checks its flags before it makes the data directory, so a
// wrong flag exits 2 and leaves nothing behind.
func TestServeDevBadFlagMakesNoDataDirectory(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"database not postgres", "--database: the database URL must be postgres://", []string{"--database", "not-a-url"}},
		{"config missing", "read the config", []string{"--config", filepath.Join(t.TempDir(), "missing.hcl")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "data")
			var stdout, stderr bytes.Buffer
			code := run(append([]string{"serve", "--dev", "--data", data, "--listen", "127.0.0.1:0"}, tc.args...), &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q, want exit 2 and %q", code, stderr.String(), tc.want)
			}
			if _, err := os.Stat(data); !os.IsNotExist(err) {
				t.Fatalf("the data directory exists after a bad flag: %v", err)
			}
		})
	}
}
