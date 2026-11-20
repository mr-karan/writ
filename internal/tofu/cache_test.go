//go:build linux

package tofu

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLockedProviders(t *testing.T) {
	lock, err := os.ReadFile("testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := LockedProviders(lock)
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{
		{Source: "registry.opentofu.org/hashicorp/local", Version: "2.9.1"},
		{Source: "registry.opentofu.org/hashicorp/random", Version: "3.9.1"},
		{Source: "registry.opentofu.org/hashicorp/tls", Version: "4.4.2"},
	}
	for i := range got {
		if !slices.ContainsFunc(got[i].Hashes, func(h string) bool { return strings.HasPrefix(h, "zh:") && len(h) == 67 }) {
			t.Errorf("%s: hashes %v, want the zh: hashes of its archives", got[i].Source, got[i].Hashes)
		}
		got[i].Hashes = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LockedProviders = %v, want %v", got, want)
	}
	for name, bad := range map[string]string{
		"a source that would break out of a string": `provider "registry.opentofu.org/x/y\" }\nresource \"z" { version = "1.0.0" }`,
		"a version that is an expression":           `provider "registry.opentofu.org/x/y" { version = "1.0.0\"} p = {" }`,
		"no version":                                `provider "registry.opentofu.org/x/y" {}`,
		"two labels":                                `provider "registry.opentofu.org/x/y" "z" { version = "1.0.0" }`,
		"a hash that is an expression":              "provider \"registry.opentofu.org/x/y\" {\n  version = \"1.0.0\"\n  hashes = [var.h]\n}",
	} {
		if _, err := LockedProviders([]byte(bad)); err == nil {
			t.Errorf("%s: LockedProviders accepted %q", name, bad)
		}
	}
}

func TestFill(t *testing.T) {
	cache := t.TempDir()
	bin := fakeTofu(t, "fill")
	r := newRunner(t, bin, cache, t.TempDir(), time.Minute, 1<<20)
	old := filepath.Join(cache, "registry.opentofu.org")
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(old, 0o777); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(old, "terraform-provider-x")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile("testdata/lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Fill(context.Background(), lock, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(lock) {
		t.Fatalf("Fill returned a lock with hashes for every provider changed:\n%s", got)
	}
	b, err := os.ReadFile(filepath.Join(cache, "seen"))
	if err != nil {
		t.Fatal(err)
	}
	want := `terraform {
  required_providers {
    p0 = { source = "registry.opentofu.org/hashicorp/local", version = "2.9.1" }
    p1 = { source = "registry.opentofu.org/hashicorp/random", version = "3.9.1" }
    p2 = { source = "registry.opentofu.org/hashicorp/tls", version = "4.4.2" }
  }
}
init -input=false -lockfile=readonly -backend=false -no-color
`
	if string(b) != want {
		t.Fatalf("tofu was asked to init:\n%s\nwant:\n%s", b, want)
	}
	for p, mode := range map[string]os.FileMode{
		cache:                        0o755,
		old:                          0o755,
		exe:                          0o755,
		filepath.Join(cache, "seen"): 0o644,
	} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s has mode %v after Fill, want %v", strings.TrimPrefix(p, cache), st.Mode().Perm(), mode)
		}
	}
}

func TestFillRecordsHashes(t *testing.T) {
	tfLock, err := os.ReadFile("testdata/terraform.lock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	lock, _, err := FromTerraform(tfLock, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	recorded := []string{"zh:" + strings.Repeat("a", 64)}

	t.Run("the hashes init records are returned, and the next Fill reads them from the Runner", func(t *testing.T) {
		cache := t.TempDir()
		r := newRunner(t, fakeTofu(t, "fill-record"), cache, t.TempDir(), time.Minute, 1<<20)
		first, err := r.Fill(ctx, lock, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		ps, err := LockedProviders(first)
		if err != nil {
			t.Fatalf("Fill returned a lock that does not read: %v\n%s", err, first)
		}
		want := []Provider{
			{Source: "registry.opentofu.org/hashicorp/local", Version: "2.9.1", Hashes: recorded},
			{Source: "registry.opentofu.org/hashicorp/random", Version: "3.9.1", Hashes: recorded},
			{Source: "registry.opentofu.org/hashicorp/tls", Version: "4.4.2", Hashes: recorded},
		}
		if !reflect.DeepEqual(ps, want) {
			t.Fatalf("Fill returned a lock that pins %+v, want %+v", ps, want)
		}
		second, err := r.Fill(ctx, lock, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(second) != string(first) {
			t.Fatalf("the second Fill returned\n%s\nwant the first's\n%s", second, first)
		}
		seen, err := os.ReadFile(filepath.Join(cache, "seen"))
		if err != nil {
			t.Fatal(err)
		}
		wantSeen := "init -input=false -backend=false -no-color\ninit -input=false -lockfile=readonly -backend=false -no-color\n"
		if string(seen) != wantSeen {
			t.Fatalf("tofu was asked to init with\n%s\nwant\n%s", seen, wantSeen)
		}
	})

	t.Run("an init that changes a version fails Fill and records nothing", func(t *testing.T) {
		cache := t.TempDir()
		r := newRunner(t, fakeTofu(t, "fill-bump"), cache, t.TempDir(), time.Minute, 1<<20)
		if _, err := r.Fill(ctx, lock, nil, nil); err == nil || !strings.Contains(err.Error(), "other providers or versions") {
			t.Fatalf("Fill: %v, want a refusal of the changed version", err)
		}
		if len(r.hashes) != 0 {
			t.Fatalf("Fill kept hashes %v after it failed", r.hashes)
		}
	})
}
