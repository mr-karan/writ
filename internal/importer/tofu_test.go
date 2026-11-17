package importer_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
)

const tofuProviders = `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.9.1" }
    local  = { source = "hashicorp/local", version = "2.9.1" }
    tls    = { source = "hashicorp/tls", version = "4.4.2" }
  }
}
`

// tofuStack writes an OpenTofu stack: the lock of internal/tofu's tests,
// edited by edit, and main.tf.
func tofuStack(t *testing.T, main string, edit func(string) string) string {
	t.Helper()
	lock, err := os.ReadFile("../tofu/testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".terraform.lock.hcl"), []byte(edit(string(lock))), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenTofuStackReadsThroughTheLock(t *testing.T) {
	dirs := installed(t)
	ctx := context.Background()
	dir := tofuStack(t, tofuProviders+"\nresource \"random_uuid\" \"random_uuid\" {}\n", func(s string) string { return s })
	if !importer.IsTofuStack(dir) {
		t.Fatal("IsTofuStack = false for a stack with a lock and a .tf file")
	}
	st, err := importer.OpenTofuStack(ctx, dir, importer.TofuOptions{Dirs: dirs, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(ctx)
	typ, err := st.Type("random_uuid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Type("random_nothing"); err == nil {
		t.Error("Type accepted a type the provider lacks")
	}
	const id = "6f1c2e3a-1b2c-4d5e-8f90-123456789abc"
	sels := []importer.Selection{{Type: typ, IDs: []string{id}}}
	if err := st.Prepare(sels); err != nil {
		t.Fatal(err)
	}
	res, err := importer.Bulk(ctx, importer.BulkOptions{Selections: sels, Reader: st, Lister: st, Taken: st.Taken(), File: "imported.tf"})
	if err != nil {
		t.Fatal(err)
	}
	code := hclCode(t, res)
	// The stack declares random_uuid.random_uuid, so the import takes
	// another name.
	name := "random_uuid_" + strings.ReplaceAll(id, "-", "_")
	for _, want := range []string{`resource "random_uuid" "` + name + `" {`, "to = random_uuid." + name + "\n", `id = "` + id + `"`} {
		if !strings.Contains(code, want) {
			t.Errorf("the code lacks %s:\n%s", want, code)
		}
	}
	if len(res.Objects) != 1 || res.Objects[0].Addr.String() != "random_uuid."+name {
		t.Errorf("objects = %+v", res.Objects)
	}
	if len(res.Imports) != 1 || res.Imports[0] != (importer.Import{Type: "random_uuid", Name: name, ID: id}) {
		t.Errorf("imports = %+v, want random_uuid.%s by %s", res.Imports, name, id)
	}
}

func TestOpenTofuStackRefusesAnArchiveTheLockLacks(t *testing.T) {
	dirs := installed(t)
	zh := regexp.MustCompile(`"zh:[0-9a-f]{64}"`)
	dir := tofuStack(t, tofuProviders, func(s string) string {
		return zh.ReplaceAllString(s, `"zh:`+strings.Repeat("0", 64)+`"`)
	})
	_, err := importer.OpenTofuStack(context.Background(), dir, importer.TofuOptions{Dirs: dirs, Offline: true})
	if err == nil || !strings.Contains(err.Error(), "the lock lists no hash for the archive") {
		t.Fatalf("OpenTofuStack = %v, want a refusal of the archive", err)
	}
}

// A provider in Writ's cache whose archive hash the stack's lock does not
// list is refused before it starts, even with no cached schema, which
// would start it to read one.
func TestOpenTofuStackStartsNoArchiveTheLockLacks(t *testing.T) {
	cache := t.TempDir()
	dirs := provider.Dirs{Cache: cache}
	src, err := schema.ParseSource("registry.opentofu.org/hashicorp/random")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "started")
	binary := []byte(fmt.Sprintf("#!/bin/sh\necho started > %q\nexit 1\n", marker))
	bin := dirs.BinaryPath(src, "3.9.1", provider.CurrentPlatform())
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	archive, sum := sha256.Sum256([]byte("an archive the lock lacks")), sha256.Sum256(binary)
	if err := os.WriteFile(bin+".sum", fmt.Appendf(nil, "archive=sha256:%x binary=sha256:%x\n", archive, sum), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lock := `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.9.1"
  hashes  = ["zh:` + strings.Repeat("0", 64) + `"]
}
`
	main := "terraform {\n  required_providers {\n    random = { source = \"hashicorp/random\" }\n  }\n}\n"
	for name, text := range map[string]string{".terraform.lock.hcl": lock, "main.tf": main} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err = importer.OpenTofuStack(context.Background(), dir, importer.TofuOptions{Dirs: dirs, Offline: true})
	if err == nil || !strings.Contains(err.Error(), "the lock lists no hash for the archive") {
		t.Errorf("OpenTofuStack = %v, want a refusal of the archive", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the provider started before its archive was checked: stat %s = %v", marker, err)
	}
}

func TestIsTofuStack(t *testing.T) {
	for name, files := range map[string][]string{
		"no lock":     {"main.tf"},
		"no code":     {".terraform.lock.hcl"},
		"Writ code":   {".terraform.lock.hcl", "main.tf", "main.writ"},
		"a directory": {".terraform.lock.hcl", "main.tf/"},
	} {
		dir := t.TempDir()
		for _, f := range files {
			if d, ok := strings.CutSuffix(f, "/"); ok {
				if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if importer.IsTofuStack(dir) {
			t.Errorf("%s: IsTofuStack = true", name)
		}
	}
}

func TestOpenTofuStackRefusesCodeItCannotRead(t *testing.T) {
	lock := `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.9.1"
  hashes  = ["zh:` + strings.Repeat("0", 64) + `"]
}
`
	declared := "resource \"random_uuid\" \"random_uuid\" {}\n"
	for _, tc := range []struct {
		name string
		make func(dir string) error
		want string
	}{
		{"a .tf.json file", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "existing.tf.json"), []byte(`{"resource":{"random_uuid":{"random_uuid":{}}}}`), 0o644)
		}, "existing.tf.json: writ import reads only .tf and .tofu files"},
		{"a .tofu.json file", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "existing.tofu.json"), []byte(`{}`), 0o644)
		}, "existing.tofu.json: writ import reads only .tf and .tofu files"},
		{"a .tf symlink", func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "resources.txt"), []byte(declared), 0o644); err != nil {
				return err
			}
			return os.Symlink("resources.txt", filepath.Join(dir, "existing.tf"))
		}, "existing.tf is not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, text := range map[string]string{".terraform.lock.hcl": lock, "main.tf": "terraform {}\n"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := tc.make(dir); err != nil {
				t.Fatal(err)
			}
			_, err := importer.OpenTofuStack(context.Background(), dir, importer.TofuOptions{Dirs: provider.Dirs{Cache: t.TempDir()}, Offline: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("OpenTofuStack = %v, want an error that says %q", err, tc.want)
			}
		})
	}
}
