//go:build e2e && e2e_tofu && linux

package e2e

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/controltest"
)

// maskStacks bounds TestVerifyMask: how many stacks the generator writes.
var maskStacks = flag.Int("verify-mask.stacks", 6, "how many OpenTofu stacks TestVerifyMask generates")

const maskStacksEnv = "WRIT_E2E_VERIFY_MASK_STACKS"

// The two runs' canaries: of one length, with a slash and a plus sign so
// their encoded forms differ from their plain ones.
var maskCanaries = [2]string{"vm-canary/4c1e+aa71-one", "vm-canary/93d0+5be2-two"}

// maskPlanned is the default of a sensitive variable, the same in both
// runs: only the stack's files and its plan hold it, never the
// environment of writ serve, so a mask knows it only from the plan.
const maskPlanned = "vm-planned/6b0e+31c9-src"

// TestVerifyMask is just verify-mask, the differential test of the mask
// in docs/secrets.md. A generator writes OpenTofu
// stacks over random, local, and tls that pass a sensitive variable
// through direct references, string templates, encoding functions,
// nested blocks, for_each, a module, outputs, and nonsensitive(). Each
// stack is planned and applied twice through writ serve --dev, by the
// real path, in two control planes whose runs differ only in the
// variable's value, one canary each. Check 1: every plan, run, and list
// the API serves, and every row of the control database but the sealed
// blobs, is the same in both once ids, hashes, and times are normalized.
// Check 2: neither canary, plain or encoded, is in the control database,
// the data directory, the scratch directory, an output, or the log.
// Check 3: the default of a sensitive variable, which only the stack's
// files and plan hold, is in no row of the state index, no output, and
// no log line. Each control plane indexes its stacks twice: after each
// apply, and again by the refresh at a restart of writ serve with the
// index emptied, as when the show after an apply failed.
//
// The rows of the state index hold the ids a provider chose, such as a
// random_pet's name, which differ between the runs whatever the mask
// does. Check 1 compares their addresses, types, providers, and which of
// their attributes hold a value, and Check 2 their values.
//
// Policy, the UI pages, and notifications are not built yet, so the
// checks do not cover them.
func TestVerifyMask(t *testing.T) {
	if os.Getenv(tofuChildEnv) != "" {
		verifyMaskChild(t)
		return
	}
	inTofuNamespace(t, "TestVerifyMask", tofuNamespace, maskStacksEnv+"="+strconv.Itoa(*maskStacks))
}

// maskFeature is one way a stack uses the canary: main.tf's text, and
// files of a module it calls.
type maskFeature struct {
	name string
	hcl  string
	mod  map[string]string
}

var maskFeatures = []maskFeature{
	{name: "direct", hcl: `
resource "random_pet" "direct" {
  keepers = { canary = var.canary }
}
`},
	{name: "template", hcl: `
resource "terraform_data" "template" {
  input = "before ${var.canary} after"
}
`},
	{name: "encodings", hcl: `
resource "terraform_data" "encodings" {
  input = {
    b64  = base64encode(var.canary)
    url  = urlencode(var.canary)
    json = jsonencode({ v = var.canary })
    sha  = sha256(var.canary)
  }
}
`},
	{name: "nested", hcl: `
resource "tls_private_key" "nested" {
  algorithm = "ED25519"
}

resource "tls_cert_request" "nested" {
  private_key_pem = tls_private_key.nested.private_key_pem

  subject {
    common_name  = "${var.canary}.example.test"
    organization = upper(var.canary)
  }
}
`},
	{name: "for_each", hcl: `
resource "random_id" "each" {
  for_each    = toset(["a", "b"])
  byte_length = 2
  keepers     = { k = "${each.key}:${var.canary}" }
}

resource "terraform_data" "keyed" {
  for_each = toset([nonsensitive(var.canary), "plain"])
  input    = each.key
}
`},
	{name: "planned", hcl: `
variable "planned" {
  type      = string
  sensitive = true
  default   = "` + maskPlanned + `"
}

resource "terraform_data" "planned" {
  for_each = toset([nonsensitive(var.planned)])
}
`},
	{name: "module", hcl: `
module "inner" {
  source = "./inner"
  secret = var.canary
}
`, mod: map[string]string{"inner/main.tf": `variable "secret" {
  type      = string
  sensitive = true
}

resource "random_pet" "inner" {
  keepers = { s = var.secret }
}

output "echo" {
  value     = var.secret
  sensitive = true
}
`}},
	{name: "outputs", hcl: `
output "secret" {
  value     = var.canary
  sensitive = true
}

output "unmarked" {
  value = nonsensitive(base64encode(var.canary))
}
`},
	{name: "nonsensitive", hcl: `
resource "terraform_data" "unmarked" {
  input = nonsensitive(var.canary)
}

resource "terraform_data" "unmarked_url" {
  input = { u = nonsensitive(urlencode(var.canary)) }
}
`},
	{name: "local", hcl: `
resource "local_sensitive_file" "secret" {
  filename = "${path.module}/secret.txt"
  content  = var.canary
}

resource "local_file" "note" {
  filename = "${path.module}/note.txt"
  content  = "note ${base64encode(var.canary)}"
}
`},
}

// maskStackFiles is stack i's files: stack 0 holds every feature, and each
// other stack a subset that its index seeds.
func maskStackFiles(i int, lock []byte) (map[string]string, []string) {
	r := rand.New(rand.NewPCG(uint64(i), 0x6d61736b))
	var main strings.Builder
	main.WriteString("terraform {\n  backend \"http\" {}\n" + tofuProviders + "}\n\nvariable \"canary\" {\n  type      = string\n  sensitive = true\n}\n")
	dir := fmt.Sprintf("s%02d", i)
	files := map[string]string{dir + "/.terraform.lock.hcl": string(lock)}
	var names []string
	for _, f := range maskFeatures {
		if i != 0 && r.IntN(2) == 0 {
			continue
		}
		names = append(names, f.name)
		main.WriteString(f.hcl)
		for name, text := range f.mod {
			files[dir+"/"+name] = text
		}
	}
	if len(names) == 0 {
		f := maskFeatures[r.IntN(len(maskFeatures))]
		names = append(names, f.name)
		main.WriteString(f.hcl)
		for name, text := range f.mod {
			files[dir+"/"+name] = text
		}
	}
	files[dir+"/main.tf"] = main.String()
	return files, names
}

// maskRun is what one control plane stored and served.
type maskRun struct {
	served []string // in request order, for Check 1
	rows   []string // every row of the control database, for Check 1
	home   string
	log    string
}

func verifyMaskChild(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv(maskStacksEnv))
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a count of stacks", maskStacksEnv, os.Getenv(maskStacksEnv))
	}
	lock := tofuLock(t)
	bin, shared := tofuChildRoot(t)

	work := t.TempDir()
	repoDir := filepath.Join(work, "infra")
	var stacks []string
	for i := range n {
		files, names := maskStackFiles(i, lock)
		for name, text := range files {
			p := filepath.Join(repoDir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		stack := fmt.Sprintf("s%02d", i)
		stacks = append(stacks, stack)
		t.Logf("stack %s: %s", stack, strings.Join(names, ", "))
	}
	gitIn(t, repoDir, "init", "-q", "-b", "main")
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-q", "-m", "generated stacks")

	var runs [2]maskRun
	for k, canary := range maskCanaries {
		runs[k] = maskThrough(t, bin, shared, repoDir, stacks, canary)
	}

	// Check 1.
	for _, part := range []struct {
		what string
		a, b []string
	}{{"what the API served", runs[0].served, runs[1].served}, {"the control database", runs[0].rows, runs[1].rows}} {
		a, b := normalizeMask(part.a, work), normalizeMask(part.b, work)
		if len(a) != len(b) {
			t.Errorf("check 1: %s holds %d items in one run and %d in the other", part.what, len(a), len(b))
			continue
		}
		differs := false
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("check 1: %s differs between the runs at item %d:\n%s\n%s", part.what, i, a[i], b[i])
				differs = true
				break
			}
		}
		if !differs {
			t.Logf("check 1: %s, %d items, the same in both runs", part.what, len(a))
		}
	}

	// Check 2.
	for k := range runs {
		for _, canary := range maskCanaries {
			for _, dir := range []string{filepath.Join(runs[k].home, "writ"), filepath.Join(runs[k].home, "runs")} {
				scanDirFor(t, dir, canary)
			}
			outs := append(slices.Clone(runs[k].served), runs[k].rows...)
			for i, o := range append(outs, runs[k].log) {
				for form, v := range canaryForms(canary) {
					if strings.Contains(o, v) {
						t.Errorf("check 2: output or row %d of run %d holds a canary, %s", i, k+1, form)
					}
				}
			}
		}
	}

	// Check 3.
	for k := range runs {
		outs := append(slices.Clone(runs[k].served), runs[k].log)
		masked := 0
		for _, row := range runs[k].rows {
			if strings.Contains(row, `"_table":"state_index"`) {
				outs = append(outs, row)
				if strings.Contains(row, `"address":"terraform_data.planned[\"(sensitive)\"]"`) {
					masked++
				}
			}
		}
		if masked == 0 {
			t.Errorf("check 3: run %d's state index has no row of terraform_data.planned with its key masked", k+1)
		}
		for i, o := range outs {
			for form, v := range canaryForms(maskPlanned) {
				if strings.Contains(o, v) {
					t.Errorf("check 3: output, index row, or log %d of run %d holds the sensitive variable's default, %s", i, k+1, form)
				}
			}
		}
	}
}

// maskThrough starts writ serve --dev with the stacks' variable set to
// canary, plans and applies each stack by commit, reads back what the API
// serves of them, empties the state index and starts the server again so
// its refresh indexes each stack, stops the server, and dumps its control
// database.
func maskThrough(t *testing.T, bin, shared, repoDir string, stacks []string, canary string) maskRun {
	t.Helper()
	home := t.TempDir()
	cache := filepath.Join(home, "plugins")
	if err := os.CopyFS(cache, os.DirFS(shared)); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(home, "dev.hcl")
	if err := os.WriteFile(config, fmt.Appendf(nil, "tofu {\n  binary = %q\n  plugin_cache = %q\n  scratch_dir = %q\n}\n", bin, cache, filepath.Join(home, "runs")), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"XDG_DATA_HOME=" + home}
	srv := startDevAt(t, home, []string{"TF_VAR_canary=" + canary}, "--config", config)
	repo := &stackDir{t: t, dir: repoDir, env: env}
	repo.run(0, "repo", "add", ".")
	data := filepath.Join(home, "writ", "dev")
	var client struct{ URL, Token string }
	readClient := func() {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(data, "client.json")); err != nil || json.Unmarshal(b, &client) != nil {
			t.Fatalf("the dev client file: %v", err)
		}
	}
	readClient()
	var out maskRun
	get := func(path string) {
		t.Helper()
		req, _ := http.NewRequest("GET", client.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+client.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		out.served = append(out.served, string(b))
	}
	for _, stack := range stacks {
		s := &stackDir{t: t, dir: filepath.Join(repoDir, stack), env: env}
		var p tofuPlanDoc
		r := s.runJSON(0, &p, "plan", "--remote", "--json")
		if !p.OK || p.Plan.ID == "" {
			t.Fatalf("plan of %s: %+v\n%s", stack, p.Plan, r)
		}
		out.served = append(out.served, r.stdout)
		var run tofuRun
		r = s.runJSON(0, &run, "apply", "--remote", "--json", p.Plan.ID)
		if !run.OK || run.Run.Status != "applied" || run.Document == nil || !run.Document.Applied {
			t.Fatalf("apply of %s: %+v\n%s", stack, run.Run, r)
		}
		out.served = append(out.served, r.stdout)
		get("/v1/plans/" + p.Plan.ID)
		get("/v1/runs/" + run.Run.ID)
		get("/v1/plans?env=dev&stack=" + stack)
	}
	get("/v1/applies?env=dev")
	srv.stop(t)
	out.log = srv.out.String()

	control := filepath.Join(data, "control.db")
	raw := controltest.Raw(t, control)
	for _, q := range []string{"DELETE FROM state_index", "DELETE FROM state_index_stacks"} {
		if _, err := raw.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	srv = startDevAt(t, home, []string{"TF_VAR_canary=" + canary}, "--config", config)
	readClient()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
		var n int
		if err := raw.QueryRowContext(context.Background(), "SELECT count(*) FROM state_index_stacks").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == len(stacks) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresh at start indexed %d of %d stacks:\n%s", n, len(stacks), srv.out.String())
		}
	}
	get("/v1/applies?env=dev")
	srv.stop(t)
	out.log += srv.out.String()
	out.home = home
	out.rows = dumpSQLite(t, control)
	return out
}

// dumpSQLite is every row of every table of a SQLite control database,
// in table and row order, each a JSON object of its columns, with each
// blob, which holds sealed bytes or a hash, replaced: a sealed state's
// length follows the random values in it.
func dumpSQLite(t *testing.T, path string) []string {
	t.Helper()
	db := controltest.Raw(t, path)
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+table+`" ORDER BY rowid`)
		if err != nil {
			// A table without a rowid keeps its primary key's order.
			if rows, err = db.QueryContext(ctx, `SELECT * FROM "`+table+`"`); err != nil {
				t.Fatal(err)
			}
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"_table": table}
			for i, v := range vals {
				if _, ok := v.([]byte); ok {
					v = "<blob>"
				}
				row[cols[i]] = v
			}
			b, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// maskVolatile are the parts of a stored or served item that differ
// between two runs of the same stacks whatever the mask does: ids, hashes,
// times, ports, process ids, and the run's temporary directories.
var maskVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\b[0-9a-f]{64}\b`), "<sha256>"},
	{regexp.MustCompile(`\b[0-9a-f]{32}\b`), "<id>"},
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`\b[A-Z2-7]{26}\b`), "<run>"},
	{regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`), "<time>"},
	// A time in milliseconds stays a number, so a row stays JSON.
	{regexp.MustCompile(`\b1[0-9]{12}\b`), "1000000000000"},
	{regexp.MustCompile(`\b1[0-9]{9}-`), "<s>-"},
	{regexp.MustCompile(`127\.0\.0\.1:\d+`), "<addr>"},
	{regexp.MustCompile(`(elapsed_ms(=|":))\d+`), "${1}0"},
	{regexp.MustCompile(`\bpid \d+`), "pid <pid>"},
	{regexp.MustCompile(`/tmp/[^"\s]*?/\d{3}`), "<tmp>"},
}

// indexAttrs are the columns of a row of the state index that hold a
// resource's attributes.
var indexAttrs = []string{"id", "arn", "identifier", "region", "namespace", "modify_index"}

func normalizeMask(items []string, work string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		var row map[string]any
		if json.Unmarshal([]byte(s), &row) == nil && row["_table"] == "state_index" {
			for _, a := range indexAttrs {
				if v, ok := row[a].(string); ok && v != "" {
					row[a] = "<value>"
				}
			}
			b, err := json.Marshal(row)
			if err != nil {
				panic(err)
			}
			s = string(b)
		}
		s = strings.ReplaceAll(s, work, "<work>")
		for _, v := range maskVolatile {
			s = v.re.ReplaceAllString(s, v.with)
		}
		out[i] = canonical(s)
	}
	return out
}

// canonical is s with what OpenTofu's parallelism orders put in one
// order: OpenTofu applies resources that do not depend on each other at
// once, so the order of an apply's events, and of the lines that report
// them, differs between runs. In JSON, each array of events is sorted,
// their elapsed_seconds, a time, become 0, and each string is made
// canonical in turn, as a stored document is a string of JSON. Text of
// lines has its lines sorted.
func canonical(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return sortLines(s)
	}
	b, err := json.Marshal(canonicalJSON(v))
	if err != nil {
		panic(err)
	}
	return string(b)
}

func canonicalJSON(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if k == "elapsed_seconds" {
				v[k] = 0
				continue
			}
			v[k] = canonicalJSON(x)
		}
		return v
	case []any:
		for i, x := range v {
			v[i] = canonicalJSON(x)
		}
		if ev, ok := firstOf(v).(map[string]any); ok && ev["address"] != nil && ev["type"] != nil {
			slices.SortFunc(v, func(a, b any) int {
				ja, _ := json.Marshal(a)
				jb, _ := json.Marshal(b)
				return strings.Compare(string(ja), string(jb))
			})
		}
		return v
	case string:
		return canonical(v)
	}
	return v
}

func firstOf(v []any) any {
	if len(v) == 0 {
		return nil
	}
	return v[0]
}

func sortLines(s string) string {
	if !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// scanDirFor fails t for each file under dir that holds canary, plain or
// encoded. A dir that does not exist holds nothing.
func scanDirFor(t *testing.T, dir, canary string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for form, v := range canaryForms(canary) {
			if strings.Contains(string(b), v) {
				t.Errorf("check 2: %s holds a canary, %s", p, form)
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
