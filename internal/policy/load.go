package policy

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadDir reads a set's files: every .rego file under dir, at any depth,
// but those whose names end in _test.rego, which hold the set's tests for
// conftest verify and opa test. Each file is named by its path under dir.
func LoadDir(dir string) (map[string]string, error) {
	files := map[string]string{}
	size := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".rego") || strings.HasSuffix(name, "_test.rego") {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if size += len(b); size > maxSetBytes {
			return fmt.Errorf("%s holds more than %d MiB of Rego", dir, maxSetBytes>>20)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s holds no .rego file", dir)
	}
	return files, nil
}
