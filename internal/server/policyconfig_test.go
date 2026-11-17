package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/policy"
)

func TestParseConfigPolicy(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base/main.rego", "package main\n\ndeny contains \"no\" if false\n")
	write("base/lib/helpers.rego", "package lib\n\nx := 1\n")
	write("base/main_test.rego", "package main\n\ntest_x if true\n")
	write("advice/warn.rego", "package main\n\nwarn contains \"look\" if true\n")
	write("broken/bad.rego", "package main\n\ndeny contains \"x\" if input.planned_values.x == 1\n")
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	src := edited(t, "", `policy {
  timeout = "20s"
  memory = "512MiB"
  set "base" {
    dir = "base"
  }
  set "advice" {
    dir = "advice"
    level = "advisory"
  }
}`)
	cfg, diags := ParseConfig(filepath.Join(dir, "serve"), []byte(src))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	p := cfg.Policies
	if p == nil || p.Timeout != 20*time.Second || p.Memory != 512<<20 || len(p.Sets) != 2 {
		t.Fatalf("policies %+v", p)
	}
	if s := p.Sets[0]; s.Name != "base" || s.Level != policy.Mandatory || s.Dir != filepath.Join(dir, "base") || len(s.Files) != 2 || s.Files["lib/helpers.rego"] == "" {
		t.Fatalf("set base %+v: want main.rego and lib/helpers.rego, and no test file", s)
	}
	if s := p.Sets[1]; s.Level != policy.Advisory {
		t.Fatalf("set advice %+v", s)
	}
	if e, err := CompilePolicies(cfg); err != nil || e.Sets() != 2 {
		t.Fatalf("compile: %v", err)
	}
	if def, _ := ParseConfig(filepath.Join(dir, "serve"), []byte(edited(t, "", "policy {\n  set \"base\" {\n    dir = \"base\"\n  }\n}"))); def == nil || def.Policies.Timeout != policy.DefaultTimeout || def.Policies.Memory != policy.DefaultMemory {
		t.Fatalf("the defaults: %+v", def)
	}

	broken, diags := ParseConfig(filepath.Join(dir, "serve"), []byte(edited(t, "", "policy {\n  set \"broken\" {\n    dir = \"broken\"\n  }\n}")))
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if _, err := CompilePolicies(broken); err == nil || !strings.Contains(err.Error(), "policy set broken: bad.rego:3:22: rego_type_error: undefined ref: input.planned_values") {
		t.Fatalf("a set that reads a key the masked document lacks: %v", err)
	}

	for _, c := range []struct{ name, block, want string }{
		{"no set", "policy {\n  timeout = \"5s\"\n}", "policy needs a set \"NAME\" { dir = \"DIR\" }"},
		{"no dir", "policy {\n  set \"a\" {\n    level = \"advisory\"\n  }\n}", "policy set a needs dir"},
		{"a missing dir", "policy {\n  set \"a\" {\n    dir = \"missing\"\n  }\n}", "policy set a: lstat " + filepath.Join(dir, "missing")},
		{"a dir with no .rego file", "policy {\n  set \"a\" {\n    dir = \"empty\"\n  }\n}", "holds no .rego file"},
		{"a level Writ does not know", "policy {\n  set \"a\" {\n    dir = \"base\"\n    level = \"soft-mandatory\"\n  }\n}", "policy set a has level \"soft-mandatory\""},
		{"a name with capitals", "policy {\n  set \"Base\" {\n    dir = \"base\"\n  }\n}", "a set's name is lower case"},
		{"too little memory", "policy {\n  memory = \"16MiB\"\n  set \"a\" {\n    dir = \"base\"\n  }\n}", "policy.memory must be at least 64MiB"},
		{"two sets of one name", "policy {\n  set \"a\" {\n    dir = \"base\"\n  }\n  set \"a\" {\n    dir = \"advice\"\n  }\n}", "policy names set a twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, diags := ParseConfig(filepath.Join(dir, "serve"), []byte(edited(t, "", c.block)))
			if !diags.HasErrors() || !strings.Contains(diagText(diags), c.want) {
				t.Fatalf("got %v, want an error with %q", diags, c.want)
			}
		})
	}

	dev, diags := ParseDevConfig(filepath.Join(dir, "dev.hcl"), []byte("policy {\n  set \"base\" {\n    dir = \"base\"\n  }\n}\n"))
	if diags.HasErrors() || dev.Policies == nil || len(dev.Policies.Sets) != 1 {
		t.Fatalf("the dev config's policy block: %+v %v", dev, diags)
	}
	if got := DevConfig(DevOptions{URL: "http://127.0.0.1:1", Data: dir, Extra: dev}); got.Policies != dev.Policies {
		t.Fatal("writ serve --dev dropped the policy block of its config")
	}
}
