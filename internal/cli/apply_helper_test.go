package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func bulkApp(dir string, out *bytes.Buffer) *App {
	return New(Options{Dir: dir, Stdout: out, Stderr: out, LookupEnv: func(string) (string, bool) { return "", false }})
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
