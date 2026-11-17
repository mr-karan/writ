package repo_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/forge/forgetest"
	"github.com/mr-karan/writ/internal/repo"
	"github.com/mr-karan/writ/internal/tofu"
)

const tofuLock = `provider "registry.opentofu.org/hashicorp/random" {
  version     = "3.9.1"
  constraints = "3.9.1"
  hashes = ["h1:abc="]
}

provider "registry.opentofu.org/hashicorp/tls" {
  version = "4.4.2"
  hashes  = ["h1:def="]
}
`

// tofuRepo is a repository whose path infra holds two OpenTofu stacks, one
// that calls a module under infra and one that calls a module of its own.
func tofuRepo() map[string]forgetest.File {
	return map[string]forgetest.File{
		"README.md":                      {Content: "not read\n"},
		"infra/app/main.tf":              {Content: "terraform {\n  backend \"http\" {}\n}\n\nmodule \"cert\" {\n  source = \"../modules/cert\"\n}\n\nmodule \"remote\" {\n  source = \"hashicorp/consul/aws\"\n}\n"},
		"infra/app/.terraform.lock.hcl":  {Content: tofuLock},
		"infra/app/prod.tfvars":          {Content: "name = \"app\"\n"},
		"infra/app/files/motd.tftpl":     {Content: "hello ${name}\n"},
		"infra/modules/cert/main.tf":     {Content: "module \"key\" {\n  source = \"./key\"\n}\n"},
		"infra/modules/cert/key/main.tf": {Content: "resource \"tls_private_key\" \"k\" {\n  algorithm = \"ED25519\"\n}\n"},
		"infra/modules/unused/main.tf":   {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/modules/unused/data.txt":  {Content: "not read\n"},
		"infra/web/main.tofu":            {Content: "terraform {\n  backend \"s3\" {\n    bucket       = \"b\"\n    use_lockfile = true\n    region       = var.region\n  }\n}\n\nmodule \"own\" {\n  source = \"./own\"\n}\n"},
		"infra/web/.terraform.lock.hcl":  {Content: tofuLock},
		"infra/web/own/main.tf":          {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/notastack/main.tf":        {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/notastack/notes.txt":      {Content: "not read\n"},
	}
}

// tofuCommit is one commit read, and the forge and the store it came
// from, which TofuSources reads a stack's other files from.
type tofuCommit struct {
	*repo.Snapshot
	g   *forgetest.GitLab
	c   *forge.GitLab
	st  *control.Store
	lim repo.Limits
}

func readTofu(t *testing.T, files map[string]forgetest.File) *tofuCommit {
	t.Helper()
	return readTofuWithin(t, files, repo.DefaultLimits)
}

func readTofuWithin(t *testing.T, files map[string]forgetest.File, lim repo.Limits) *tofuCommit {
	t.Helper()
	g, c, st := setup(t)
	sha := g.Commit(4242, "main", files)
	snap, err := repo.Read(context.Background(), c, "4242", sha, "infra", nil, st, lim)
	if err != nil {
		t.Fatal(err)
	}
	return &tofuCommit{Snapshot: snap, g: g, c: c, st: st, lim: lim}
}

func (tc *tofuCommit) sources(dir string) (repo.TofuStack, error) {
	return repo.TofuSources(context.Background(), tc.c, "4242", tc.Snapshot, dir, tc.st, tc.lim)
}

func TestDiscoverTofuStacks(t *testing.T) {
	snap := readTofu(t, tofuRepo()).Snapshot
	var read []string
	for n := range snap.Files {
		read = append(read, n)
	}
	slices.Sort(read)
	// Discovery needs the lock files and the tree: a stack's code is read
	// when the stack is gathered, under the limits on one stack.
	want := []string{"app/.terraform.lock.hcl", "web/.terraform.lock.hcl"}
	if !slices.Equal(read, want) {
		t.Fatalf("files read:\n got %v\nwant %v", read, want)
	}
	stacks, diags, err := repo.Discover(snap)
	if err != nil || diags.HasErrors() {
		t.Fatalf("discover: %v %v", err, diags)
	}
	var dirs []string
	for _, s := range stacks {
		dirs = append(dirs, s.Dir)
	}
	if !slices.Equal(dirs, []string{"app", "web"}) {
		t.Fatalf("stacks %v, want app and web: a directory with no lock file is not an OpenTofu stack", dirs)
	}
	wantProviders := []repo.Provider{{"registry.opentofu.org/hashicorp/random", "3.9.1"}, {"registry.opentofu.org/hashicorp/tls", "4.4.2"}}
	if !slices.Equal(stacks[0].Providers, wantProviders) {
		t.Fatalf("app's providers %v, want %v", stacks[0].Providers, wantProviders)
	}
}

func TestDiscoverNeedsAStack(t *testing.T) {
	snap := readTofu(t, map[string]forgetest.File{"infra/notastack/main.tf": {Content: "\n"}})
	_, _, err := repo.Discover(snap.Snapshot)
	if err == nil || !strings.Contains(err.Error(), "tofu providers lock") || !strings.Contains(err.Error(), "holds no OpenTofu stack") {
		t.Fatalf("a commit with no lock file: %v", err)
	}
}

func TestTofuSources(t *testing.T) {
	snap := readTofu(t, tofuRepo())
	app, err := snap.sources("app")
	if err != nil {
		t.Fatal(err)
	}
	names := slices.Sorted(maps.Keys(app.Files))
	want := []string{"app/.terraform.lock.hcl", "app/files/motd.tftpl", "app/main.tf", "app/prod.tfvars", "modules/cert/key/main.tf", "modules/cert/main.tf"}
	if !slices.Equal(names, want) {
		t.Fatalf("app's files:\n got %v\nwant %v", names, want)
	}
	if app.Backend == nil || app.Backend.Type != "http" || len(app.Backend.Set) != 0 || app.Cloud || string(app.Lock) != tofuLock {
		t.Fatalf("app's backend %+v, cloud %v", app.Backend, app.Cloud)
	}
	web, err := snap.sources("web")
	if err != nil {
		t.Fatal(err)
	}
	// A backend's attribute set by a variable has no literal.
	if web.Backend == nil || web.Backend.Type != "s3" || !slices.Equal(web.Backend.Set, []string{"bucket", "region", "use_lockfile"}) ||
		!maps.Equal(web.Backend.Literals, map[string]string{"bucket": "b", "use_lockfile": "true"}) {
		t.Fatalf("web's backend %+v", web.Backend)
	}

	t.Run("the digest covers the modules", func(t *testing.T) {
		files := tofuRepo()
		files["infra/modules/cert/key/main.tf"] = forgetest.File{Content: "resource \"tls_private_key\" \"k\" {\n  algorithm = \"RSA\"\n}\n"}
		other, err := readTofu(t, files).sources("app")
		if err != nil {
			t.Fatal(err)
		}
		files["infra/modules/unused/main.tf"] = forgetest.File{Content: "\n"}
		same, err := readTofu(t, files).sources("app")
		if err != nil {
			t.Fatal(err)
		}
		if other.Digest == app.Digest || same.Digest != other.Digest {
			t.Fatalf("digests: %s, after a module edit %s, after an edit of an uncalled module %s", app.Digest, other.Digest, same.Digest)
		}
	})

	t.Run("a lock file that Terraform wrote", func(t *testing.T) {
		tfLock := strings.ReplaceAll(tofuLock, "registry.opentofu.org", "registry.terraform.io") +
			"\nprovider \"registry.terraform.io/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes  = [\"h1:ghi=\"]\n}\n"
		for _, tc := range []struct {
			name, code, err string
			// module is m/main.tf, when not one that names sops by
			// Terraform's registry.
			module string
			lock   string
			moved  []tofu.Moved
		}{
			{
				name: "moves the providers the code names with no host",
				code: "terraform {\n  required_providers {\n    sops = { source = \"carlpett/sops\" }\n  }\n}\n",
				lock: "provider \"registry.opentofu.org/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes = [\n    \"h1:ghi=\",\n  ]\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/tls\" {\n  version = \"4.4.2\"\n}\n\n",
				moved: []tofu.Moved{
					{From: "registry.terraform.io/carlpett/sops", To: "registry.opentofu.org/carlpett/sops"},
					{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random", Rehashed: true},
					{From: "registry.terraform.io/hashicorp/tls", To: "registry.opentofu.org/hashicorp/tls", Rehashed: true},
				},
			},
			{
				name: "keeps a provider a module names by Terraform's registry, in any case",
				code: "module \"m\" {\n  source = \"./m\"\n}\n",
				lock: "provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/tls\" {\n  version = \"4.4.2\"\n}\n\n" +
					"provider \"registry.terraform.io/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes = [\n    \"h1:ghi=\",\n  ]\n}\n\n",
				moved: []tofu.Moved{
					{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random", Rehashed: true},
					{From: "registry.terraform.io/hashicorp/tls", To: "registry.opentofu.org/hashicorp/tls", Rehashed: true},
				},
			},
			{
				name: "keeps a provider named by Terraform's registry under a quoted key",
				code: "terraform {\n  required_providers {\n    sops = { \"source\" = \"registry.terraform.io/carlpett/sops\" }\n  }\n}\n",
				lock: "provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/tls\" {\n  version = \"4.4.2\"\n}\n\n" +
					"provider \"registry.terraform.io/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes = [\n    \"h1:ghi=\",\n  ]\n}\n\n",
				moved: []tofu.Moved{
					{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random", Rehashed: true},
					{From: "registry.terraform.io/hashicorp/tls", To: "registry.opentofu.org/hashicorp/tls", Rehashed: true},
				},
			},
			{
				name:   "pins a provider at both registries when a module names it with no host",
				code:   "terraform {\n  required_providers {\n    sops = { source = \"registry.terraform.io/carlpett/sops\" }\n  }\n}\n\nmodule \"m\" {\n  source = \"./m\"\n}\n",
				module: "terraform {\n  required_providers {\n    sops = { \"source\" = \"carlpett/sops\" }\n  }\n}\n",
				lock: "provider \"registry.opentofu.org/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes = [\n    \"h1:ghi=\",\n  ]\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.9.1\"\n}\n\n" +
					"provider \"registry.opentofu.org/hashicorp/tls\" {\n  version = \"4.4.2\"\n}\n\n" +
					"provider \"registry.terraform.io/carlpett/sops\" {\n  version = \"1.1.1\"\n  hashes = [\n    \"h1:ghi=\",\n  ]\n}\n\n",
				moved: []tofu.Moved{
					{From: "registry.terraform.io/carlpett/sops", To: "registry.opentofu.org/carlpett/sops", Kept: true},
					{From: "registry.terraform.io/hashicorp/random", To: "registry.opentofu.org/hashicorp/random", Rehashed: true},
					{From: "registry.terraform.io/hashicorp/tls", To: "registry.opentofu.org/hashicorp/tls", Rehashed: true},
				},
			},
			{
				name: "refuses a hashicorp/ provider named by Terraform's registry",
				code: "terraform {\n  required_providers {\n    random = { source = \"registry.terraform.io/hashicorp/random\", version = \"3.9.1\" }\n  }\n}\n",
				err:  "app/.terraform.lock.hcl: the code names provider registry.terraform.io/hashicorp/random by Terraform's registry",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				files := tofuRepo()
				files["infra/app/.terraform.lock.hcl"] = forgetest.File{Content: tfLock}
				files["infra/app/versions.tf"] = forgetest.File{Content: tc.code}
				// Only the cases that call m count its code.
				module := tc.module
				if module == "" {
					module = "terraform {\n  required_providers {\n    sops = { source = \"Registry.Terraform.io/CarlPett/sops\" }\n  }\n}\n"
				}
				files["infra/app/m/main.tf"] = forgetest.File{Content: module}
				ts, err := readTofu(t, files).sources("app")
				if tc.err != "" {
					if err == nil || !strings.Contains(err.Error(), tc.err) {
						t.Fatalf("TofuSources: %v, want an error that says %q", err, tc.err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if string(ts.Lock) != tc.lock || !slices.Equal(ts.Moved, tc.moved) {
					t.Fatalf("the lock:\n%s\nwant:\n%s\nmoved %+v, want %+v", ts.Lock, tc.lock, ts.Moved, tc.moved)
				}
				if string(ts.Files["app/.terraform.lock.hcl"]) != tfLock {
					t.Fatalf("the stack's files hold a lock file other than the committed one:\n%s", ts.Files["app/.terraform.lock.hcl"])
				}
			})
		}
	})

	for _, tc := range []struct {
		name string
		edit func(map[string]forgetest.File)
		want string
	}{
		{"no lock file", func(f map[string]forgetest.File) { delete(f, "infra/app/.terraform.lock.hcl") }, "run tofu providers lock"},
		{"a committed .terraform directory", func(f map[string]forgetest.File) {
			f["infra/app/.terraform/providers/x"] = forgetest.File{Content: "x"}
		}, "must not be committed"},
		{"code in JSON", func(f map[string]forgetest.File) { f["infra/app/extra.tf.json"] = forgetest.File{Content: "{}"} }, ".tf and .tofu files only"},
		{"a module outside the repository's path", func(f map[string]forgetest.File) {
			f["infra/app/main.tf"] = forgetest.File{Content: "module \"x\" {\n  source = \"../../outside\"\n}\n"}
		}, "leaves the repository's path"},
		{"a module outside the repository's path, from a module", func(f map[string]forgetest.File) {
			f["infra/modules/cert/main.tf"] = forgetest.File{Content: "module \"x\" {\n  source = \"../../../outside\"\n}\n"}
		}, `modules/cert/main.tf:2: module "x" calls ../../../outside, which leaves the repository's path`},
		{"an absolute module path", func(f map[string]forgetest.File) {
			f["infra/app/main.tf"] = forgetest.File{Content: "module \"x\" {\n  source = \"/etc/modules/x\"\n}\n"}
		}, `app/main.tf:2: module "x" has source "/etc/modules/x", which leaves the repository's path`},
		{"a file: module address", func(f map[string]forgetest.File) {
			f["infra/app/main.tf"] = forgetest.File{Content: "module \"x\" {\n  source = \"FILE::/etc/modules/x\"\n}\n"}
		}, `module "x" has source "FILE::/etc/modules/x", which leaves the repository's path`},
		{"a module path with backslashes", func(f map[string]forgetest.File) {
			f["infra/app/main.tf"] = forgetest.File{Content: "module \"x\" {\n  source = \"..\\\\modules\\\\cert\"\n}\n"}
		}, "write a local module's path with /"},
		{"code that does not parse", func(f map[string]forgetest.File) { f["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n"} }, "app/main.tf"},
		{"two backends", func(f map[string]forgetest.File) {
			f["infra/app/backend.tf"] = forgetest.File{Content: "terraform {\n  backend \"local\" {}\n}\n"}
		}, "second backend"},
		{"a directory with no code", func(map[string]forgetest.File) {}, "no OpenTofu stack in modules"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tofuRepo()
			tc.edit(files)
			dir := "app"
			if tc.name == "a directory with no code" {
				dir = "modules"
			}
			_, err := readTofu(t, files).sources(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("TofuSources: %v, want an error that says %q", err, tc.want)
			}
		})
	}
}

// TestTofuModuleSources accepts a module source only when tofu init cannot
// take it from the server's files: a local path inside the repository's
// path, a registry address, or a remote address that names a network host.
func TestTofuModuleSources(t *testing.T) {
	for _, tc := range []struct {
		source string
		ok     bool
	}{
		{"../modules/cert", true},
		{"hashicorp/consul/aws", true},
		{"hashicorp/consul/aws//modules/agent", true},
		{"registry.example.com/team/vpc/aws", true},
		{"registry.example.com:8443/team/vpc/aws//modules/subnet", true},
		{"github.com/example/infra", true},
		{"bitbucket.org/example/infra", true},
		{"git@github.com:example/infra.git", true},
		{"git::git@git.example.com:team/infra.git", true},
		{"git::https://git.example.com/infra.git?ref=v1.2.0", true},
		{"git::ssh://git@git.example.com/infra.git//modules/vpc", true},
		{"git://git.example.com/infra.git", true},
		{"https://example.com/module.zip", true},
		{"hg::http://hg.example.com/infra", true},
		{"s3::https://s3.amazonaws.com/bucket/module.zip", true},
		{"gcs::https://www.googleapis.com/storage/v1/bucket/module.zip", true},
		{"github.com/example/infra?ref=v1.2.0", true},
		{"git::https://git.example.com/infra.git?ref=v1&depth=1", true},
		{"git::https://git.example.com/infra.git//modules/vpc?ref=v1.2.0", true},

		{"/srv/modules/x", false},
		{"file:///srv/modules/x", false},
		{"git::file:///srv/modules/x", false},
		{"git::FiLe:///srv/modules/x", false},
		{"git::/srv/modules/x", false},
		{"git::./modules/cert", false},
		{"git::../modules/cert", false},
		{"git::~/modules/x", false},
		{"hg::/srv/modules/x", false},
		{"s3::/srv/modules/x", false},
		{"~/modules/x", false},
		{"modules/x", false},
		{"C:/modules/x", false},
		{`C:\modules\x`, false},
		{`\\server\share\x`, false},
		{"//server/share/x", false},
		{`./own\..\..\..\..\srv`, false},
		{"https:///srv/modules/x", false},
		{"git.example.com/team/infra", false},
		{"oci://registry.example.com/infra", false},
		{"foo::https://example.com/x", false},
		{"GIT::https://example.com/x", false},

		// go-getter reads query parameters other than the git getter's ref
		// and depth as orders: checksum=file:URL fetches a checksum file
		// from any URL, a path on the server among them, before a download.
		{"https://example.invalid/module.zip?checksum=%66ile%3A%66ile%3A%2F%2F%2Fvar%2Ftmp%2Fx%2Fsums", false},
		{"https://example.invalid/module.zip?checksum=file:file:///var/tmp/x/sums", false},
		{"https://example.invalid/module.zip?checksum=%2566ile%253A%2566ile%253A%252F%252F%252Fvar%252Ftmp%252Fx%252Fsums", false},
		{"https://example.invalid/module.zip?CheckSum=sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"https://example.invalid/module.zip?checksum=sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"https://example.invalid/module.zip?unknown=1", false},
		{"https://example.invalid/module.zip?archive=zip", false},
		{"https://example.invalid/module?filename=module.zip", false},
		{"git::https://git.example.com/infra.git?sshkey=a2V5", false},
		{"git::https://git.example.com/infra.git?ref=v1&Ref=v2", false},
		{"git::https://git.example.com/infra.git?%2572ef=v1", false},
		{"git::https://git.example.com/infra.git//modules/vpc?depth=1&sshkey=a2V5", false},
		{"git@github.com:example/infra.git?sshkey=a2V5", false},
		{"hg::http://hg.example.com/infra?rev=default", false},
		{"s3::https://s3.amazonaws.com/bucket/module.zip?aws_profile=default", false},
		{"s3::https://s3.amazonaws.com/bucket/module.zip?aws_access_key_id=x&aws_access_key_secret=y", false},
		{"git::https://git.example.com/infra.git?ref=v1;depth=1", false},
		{"https://example.invalid/module.zip#%66ile:///var/tmp/x", false},
		{"https://example.invalid/module.zip?ref=%zz", false},
		{"git::https://git.example.com/infra.git?ref=%5C%5Cserver%5Cshare", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			files := tofuRepo()
			files["infra/app/main.tf"] = forgetest.File{Content: fmt.Sprintf("terraform {\n  backend \"http\" {}\n}\n\nmodule \"x\" {\n  source = %q\n}\n", tc.source)}
			_, err := readTofu(t, files).sources("app")
			want := fmt.Sprintf("app/main.tf:6: module \"x\" has source %q", tc.source)
			switch {
			case tc.ok && err != nil:
				t.Fatalf("TofuSources: %v, want no error", err)
			case !tc.ok && (err == nil || !strings.Contains(err.Error(), want)):
				t.Fatalf("TofuSources: %v, want an error that says %q", err, want)
			}
		})
	}
}

// TestTofuModuleSourceParams names the query parameter it refuses,
// decoded, with the module and the file and line of its source.
func TestTofuModuleSourceParams(t *testing.T) {
	for _, tc := range []struct{ query, param string }{
		{"?checksum=%2573ha256%253Aab", "checksum"},
		{"?ref=v1&%2543hecksum=x", "Checksum"},
		{"?%2572ef=v1", "%72ef"},
		{"?depth=1&aws_profile=default", "aws_profile"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			src := "git::https://git.example.com/infra.git" + tc.query
			files := tofuRepo()
			files["infra/app/main.tf"] = forgetest.File{Content: fmt.Sprintf("terraform {\n  backend \"http\" {}\n}\n\nmodule \"x\" {\n  source = %q\n}\n", src)}
			_, err := readTofu(t, files).sources("app")
			want := fmt.Sprintf("app/main.tf:6: module \"x\" has source %q, which takes query parameter %q: writ serve allows only ref and depth", src, tc.param)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("TofuSources: %v, want an error that says %q", err, want)
			}
		})
	}
}

// TestTofuModuleSourceExpression refuses a module call whose source is an
// expression, which tofu init evaluates, before OpenTofu runs: Writ could
// not know which local directory to gather. Only that stack is refused.
func TestTofuModuleSourceExpression(t *testing.T) {
	files := tofuRepo()
	files["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"http\" {}\n}\n\nvariable \"module_path\" {\n  default = \"../modules/cert\"\n}\n\nmodule \"cert\" {\n  source = var.module_path\n}\n"}
	snap := readTofu(t, files)
	stacks, _, err := repo.Discover(snap.Snapshot)
	if err != nil || len(stacks) != 2 {
		t.Fatalf("discover: %v %+v", err, stacks)
	}
	_, err = snap.sources("app")
	if want := `app/main.tf:10: module "cert" takes its source from an expression`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("TofuSources: %v, want an error that says %q", err, want)
	}
	if _, err := snap.sources("web"); err != nil {
		t.Fatalf("web: %v", err)
	}
}

// TestTofuModuleThroughASymlink: a module path cannot leave the
// repository's path through a symbolic link, because the forge refuses a
// tree that holds one.
func TestTofuModuleThroughASymlink(t *testing.T) {
	files := tofuRepo()
	files["infra/app/main.tf"] = forgetest.File{Content: "terraform {\n  backend \"http\" {}\n}\n\nmodule \"x\" {\n  source = \"./outside/x\"\n}\n"}
	files["infra/app/outside"] = forgetest.File{Content: "/etc", Mode: "120000"}
	g, c, st := setup(t)
	sha := g.Commit(4242, "main", files)
	_, err := repo.Read(context.Background(), c, "4242", sha, "infra", nil, st, repo.DefaultLimits)
	if err == nil || !strings.Contains(err.Error(), "app/outside is a symbolic link") {
		t.Fatalf("Read: %v, want a refusal of the symbolic link", err)
	}
}

// TestTofuLargeFiles is a stack that holds a file over a limit: it refuses
// that stack alone, naming the file, while discovery and the other stack
// go on.
func TestTofuLargeFiles(t *testing.T) {
	t.Run("an artifact over the limit on one source file", func(t *testing.T) {
		files := tofuRepo()
		zip := strings.Repeat("z", 1<<20+1)
		files["infra/app/dist/function.zip"] = forgetest.File{Content: zip}
		snap := readTofu(t, files)
		if stacks, _, err := repo.Discover(snap.Snapshot); err != nil || len(stacks) != 2 {
			t.Fatalf("discover: %v %+v", err, stacks)
		}
		app, err := snap.sources("app")
		if err != nil || string(app.Files["app/dist/function.zip"]) != zip {
			t.Fatalf("app: %v, holds the archive: %v", err, app.Files["app/dist/function.zip"] != nil)
		}
		if _, err := snap.sources("web"); err != nil {
			t.Fatalf("web: %v", err)
		}
	})

	lim := repo.Limits{Files: 2000, Bytes: 16 << 20, File: 300, StackFile: 100, StackBytes: 2000}
	pad := func(n int) string { return "#" + strings.Repeat("x", n-2) + "\n" }
	for _, tc := range []struct {
		name string
		path string
		text string
		want string
	}{
		{"an artifact", "infra/app/dist/function.zip", strings.Repeat("z", 101), "app/dist/function.zip of commit %s is over 100 bytes, the limit on a stack's other files"},
		{"variables", "infra/app/prod.tfvars", pad(301), "app/prod.tfvars of commit %s is over 300 bytes, the limit on a stack's .tf, .tofu, .tfvars, and lock files"},
		{"a module's code", "infra/modules/cert/main.tf", pad(301), "modules/cert/main.tf of commit %s is over 300 bytes, the limit on a stack's .tf"},
		{"the lock file", "infra/app/.terraform.lock.hcl", pad(301), "app/.terraform.lock.hcl of commit %s is over 300 bytes"},
		{"all the files", "", "", "the files of stack app and its local modules in commit %s are over 2000 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tofuRepo()
			if tc.path != "" {
				files[tc.path] = forgetest.File{Content: tc.text}
			} else {
				for i := range 25 {
					files[fmt.Sprintf("infra/app/files/%02d.txt", i)] = forgetest.File{Content: strings.Repeat(string(rune('a'+i)), 90)}
				}
			}
			snap := readTofuWithin(t, files, lim)
			stacks, diags, err := repo.Discover(snap.Snapshot)
			if err != nil || len(stacks) != 2 || stacks[0].Dir != "app" || stacks[1].Dir != "web" {
				t.Fatalf("discover: %v %+v", err, stacks)
			}
			if tc.name == "the lock file" && (!diags.HasErrors() || !strings.Contains(fmt.Sprint(diags), "the lock file is over 300 bytes")) {
				t.Fatalf("discovery's diagnostics: %v", diags)
			}
			want := fmt.Sprintf(tc.want, snap.Commit[:8])
			if _, err := snap.sources("app"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("app: %v, want an error that says %q", err, want)
			}
			if _, err := snap.sources("web"); err != nil {
				t.Fatalf("web: %v", err)
			}
		})
	}

	t.Run("an artifact the store holds", func(t *testing.T) {
		files := tofuRepo()
		files["infra/app/dist/function.zip"] = forgetest.File{Content: strings.Repeat("z", 101)}
		first := readTofu(t, files)
		if _, err := first.sources("app"); err != nil {
			t.Fatal(err)
		}
		if err := first.Keep(context.Background(), first.st, now); err != nil {
			t.Fatal(err)
		}
		first.g.Calls()
		snap, err := repo.Read(context.Background(), first.c, "4242", first.Commit, "infra", first.Tree, first.st, lim)
		if err != nil {
			t.Fatal(err)
		}
		again := &tofuCommit{Snapshot: snap, c: first.c, st: first.st, lim: lim}
		if _, err := again.sources("app"); err == nil || !strings.Contains(err.Error(), "app/dist/function.zip of commit") {
			t.Fatalf("app: %v, want a refusal of the archive by its stored size", err)
		}
		if calls := first.g.Calls(); blobCalls(calls) != 0 {
			t.Fatalf("the stored blobs were fetched again: %v", calls)
		}
	})
}

// TestTofuModuleCalls refuses a stack whose local module declares a module
// call that Writ cannot check, wherever OpenTofu would read it: code in
// JSON in a module's directory is refused as in the stack's own, and the
// other code files OpenTofu reads there, .tofu and override files, have
// their module calls checked. Only that stack is refused.
func TestTofuModuleCalls(t *testing.T) {
	escape := "module \"escape\" {\n  source = \"/abs/host/path\"\n}\n"
	for _, tc := range []struct {
		name, path, text, want string
	}{
		{"JSON in a module", "infra/modules/cert/escape.tf.json", `{"module":{"escape":{"source":"/abs/host/path"}}}`, "holds modules/cert/escape.tf.json: writ serve reads"},
		{"JSON for OpenTofu in a module", "infra/modules/cert/key/escape.tofu.json", `{"module":{"escape":{"source":"/abs/host/path"}}}`, "holds modules/cert/key/escape.tofu.json: writ serve reads"},
		{"a .tofu file in a module", "infra/modules/cert/escape.tofu", escape, `modules/cert/escape.tofu:2: module "escape" has source "/abs/host/path"`},
		{"an override file in a module", "infra/modules/cert/main_override.tf", escape, `modules/cert/main_override.tf:2: module "escape" has source "/abs/host/path"`},
		{"an override file for OpenTofu in a module", "infra/modules/cert/key/x_override.tofu", escape, `modules/cert/key/x_override.tofu:2: module "escape" has source "/abs/host/path"`},
		{"an override file in the stack", "infra/app/override.tf", escape, `app/override.tf:2: module "escape" has source "/abs/host/path"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := tofuRepo()
			files[tc.path] = forgetest.File{Content: tc.text}
			snap := readTofu(t, files)
			if _, err := snap.sources("app"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("app: %v, want an error that says %q", err, tc.want)
			}
			if _, err := snap.sources("web"); err != nil {
				t.Fatalf("web: %v", err)
			}
		})
	}
	t.Run("JSON in an uncalled directory", func(t *testing.T) {
		files := tofuRepo()
		files["infra/modules/unused/escape.tf.json"] = forgetest.File{Content: `{"module":{"escape":{"source":"/abs/host/path"}}}`}
		if _, err := readTofu(t, files).sources("app"); err != nil {
			t.Fatalf("app, which calls no module in that directory: %v", err)
		}
	})
}

// TestTofuCodeElsewhere is a repository with a valid stack and more
// OpenTofu code in an unrelated directory than the estate's limit on all
// its files: the code of a stack counts against that stack's limits only,
// so the commit is read and the stack gathered.
func TestTofuCodeElsewhere(t *testing.T) {
	files := tofuRepo()
	for i := range 17 {
		files[fmt.Sprintf("infra/unrelated/f%02d.tf", i)] = forgetest.File{Content: "#" + strings.Repeat("x", 1<<20-2) + "\n"}
	}
	snap := readTofu(t, files)
	stacks, diags, err := repo.Discover(snap.Snapshot)
	if err != nil || diags.HasErrors() || len(stacks) != 2 {
		t.Fatalf("discover: %v %v %+v", err, diags, stacks)
	}
	if _, err := snap.sources("app"); err != nil {
		t.Fatalf("app: %v", err)
	}
}

// TestTofuSourcesCountsHeldFiles gathers three stacks on one snapshot: c
// calls the modules of a and b, which their plans already read. The files
// held count against c's limits as the files it reads do, so c is refused
// as it is on a fresh snapshot.
func TestTofuSourcesCountsHeldFiles(t *testing.T) {
	data := strings.Repeat("d", 1100)
	stackTF := func(mods ...string) string {
		s := "terraform {\n  backend \"http\" {}\n}\n"
		for _, m := range mods {
			s += fmt.Sprintf("module %q {\n  source = \"../modules/%s\"\n}\n", m, m)
		}
		return s
	}
	files := map[string]forgetest.File{
		"infra/modules/m1/main.tf":    {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/modules/m1/data.txt":   {Content: data},
		"infra/modules/m2/main.tf":    {Content: "resource \"random_pet\" \"p\" {}\n"},
		"infra/modules/m2/data.txt":   {Content: data},
		"infra/a/main.tf":             {Content: stackTF("m1")},
		"infra/a/.terraform.lock.hcl": {Content: "\n"},
		"infra/b/main.tf":             {Content: stackTF("m2")},
		"infra/b/.terraform.lock.hcl": {Content: "\n"},
		"infra/c/main.tf":             {Content: stackTF("m1", "m2")},
		"infra/c/.terraform.lock.hcl": {Content: "\n"},
	}
	lim := repo.Limits{Files: 2000, Bytes: 16 << 20, File: 1 << 20, StackFile: 16 << 20, StackBytes: 2200}
	want := "the files of stack c and its local modules in commit"
	if _, err := readTofuWithin(t, files, lim).sources("c"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("c on a fresh snapshot: %v, want an error that says %q", err, want)
	}
	snap := readTofuWithin(t, files, lim)
	for _, dir := range []string{"a", "b"} {
		if _, err := snap.sources(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	if _, err := snap.sources("c"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("c after a and b: %v, want an error that says %q", err, want)
	}

	t.Run("files", func(t *testing.T) {
		lim := lim
		lim.StackBytes, lim.Files = 1<<20, 5
		snap := readTofuWithin(t, files, lim)
		for _, dir := range []string{"a", "b"} {
			if _, err := snap.sources(dir); err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
		}
		if _, err := snap.sources("c"); err == nil || !strings.Contains(err.Error(), "stack c and its local modules hold over 5 files") {
			t.Fatalf("c after a and b: %v, want a refusal of its 6 files", err)
		}
	})
}
