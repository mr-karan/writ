package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mask"
)

// acceptRaw is tofu show -json of a plan after an import of two uuids, the
// first with a sensitive keeper.
const acceptRaw = `{"format_version":"1.2","terraform_version":"1.13.9","planned_values":{},"variables":{},
"resource_changes":[
 {"address":"random_uuid.u[\"a\"]","mode":"managed","type":"random_uuid","name":"u","index":"a","provider_name":"registry.opentofu.org/hashicorp/random",
  "change":{"actions":["no-op"],"before":{"id":"a","keepers":{"k":"accept-canary-7f3e"}},"after":{"id":"a","keepers":{"k":"accept-canary-7f3e"}},"after_unknown":{},"before_sensitive":{"keepers":{"k":true}},"after_sensitive":{"keepers":{"k":true}},"importing":{"id":"a"}}},
 {"address":"random_uuid.u[\"b\"]","mode":"managed","type":"random_uuid","name":"u","index":"b","provider_name":"registry.opentofu.org/hashicorp/random",
  "change":{"actions":["no-op"],"before":{"id":"b"},"after":{"id":"b"},"after_unknown":{},"before_sensitive":{},"after_sensitive":{},"importing":{"id":"b"}}}
],"configuration":{"root_module":{}}}`

func TestImportAcceptPlanReadsEveryForm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	masked, err := mask.New(nil).Plan([]byte(acceptRaw))
	if err != nil {
		t.Fatal(err)
	}
	maskedText, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(maskedText, []byte("accept-canary-7f3e")) {
		t.Fatal("the masked document holds the sensitive keeper")
	}
	remote, err := json.Marshal(map[string]any{"format": "writ.remote-plan/v0", "ok": true, "document": json.RawMessage(maskedText)})
	if err != nil {
		t.Fatal(err)
	}
	imports := func(ids ...string) string {
		var imps []importer.Import
		for _, id := range ids {
			imps = append(imps, importer.Import{Type: "random_uuid", Name: "u", Key: &id, ID: id})
		}
		text, err := importer.EncodeImports(imps)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "imported.imports.json")
		if err := os.WriteFile(path, text, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ab, abc := imports("a", "b"), imports("a", "b", "c")
	for name, text := range map[string]string{"tofu.json": acceptRaw, "masked.json": string(maskedText), "remote.json": string(remote)} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		app := bulkApp(dir, &out)
		if err := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: path, Imports: ab}); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, &out)
		}
		if !strings.Contains(out.String(), "Accepted: the plan imports 2 objects") {
			t.Errorf("%s: %s", name, &out)
		}
		out.Reset()
		err := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: path, Imports: abc, JSON: true})
		var rep importAcceptReport
		if !errors.Is(err, ErrFailed) || json.Unmarshal(out.Bytes(), &rep) != nil || rep.OK || rep.Objects != 3 || rep.Imports != 2 || len(rep.Refusals) != 1 {
			t.Errorf("%s with 3 imports: %v, %s; want a refusal of the missing one", name, err, &out)
		}
		if strings.Contains(out.String(), "accept-canary-7f3e") {
			t.Errorf("%s: the report holds the sensitive keeper", name)
		}
	}
}

func TestImportAcceptPlanRefusesOtherDocuments(t *testing.T) {
	for name, text := range map[string]string{
		"a Writ plan":           `{"format":"writ.plan.json/v2","changes":[]}`,
		"a remote plan running": `{"format":"writ.remote-plan/v0","ok":true,"document":null}`,
		"no format version":     `{"resource_changes":[]}`,
		"not JSON":              `resource "x" "y" {}`,
	} {
		if _, err := acceptJSON([]byte(text), nil, importer.QuoteIDs); err == nil {
			t.Errorf("%s: acceptJSON accepted it", name)
		}
	}
}

// TestImportAcceptPlanPrintsSettings requires that writ import accept names
// each provider-only setting an accepted plan sets, and none of a plan it
// refuses.
func TestImportAcceptPlanPrintsSettings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	job := func(after string) string {
		return `{"format_version":"1.2","terraform_version":"1.13.9","planned_values":{},"variables":{},
"resource_changes":[{"address":"nomad_job.web","mode":"managed","type":"nomad_job","name":"web","provider_name":"registry.opentofu.org/hashicorp/nomad",
 "change":{"actions":["update"],"before":{"jobspec":"job {}","detach":null,"preserve_counts":null},"after":` + after + `,"after_unknown":{},"before_sensitive":{},"after_sensitive":{},"importing":{"id":"web@default"}}}],
"configuration":{"root_module":{}}}`
	}
	imports, err := importer.EncodeImports([]importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}})
	if err != nil {
		t.Fatal(err)
	}
	importsPath := filepath.Join(dir, "imported.imports.json")
	if err := os.WriteFile(importsPath, imports, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPlan := stubTofu(t, dir, job(`{"jobspec":"job {}","detach":true,"preserve_counts":false}`))
	var out bytes.Buffer
	app := bulkApp(dir, &out)
	if err := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: settingsPlan, Imports: importsPath}); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	for _, want := range []string{
		"~ nomad_job.web: sets detach = true, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return\n",
		"~ nomad_job.web: sets preserve_counts = false, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return\n",
		"Accepted: the plan imports 1 object, sets 2 provider-only settings that the cloud does not return, and changes nothing else.\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, &out)
		}
	}
	out.Reset()
	if err := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: settingsPlan, Imports: importsPath, JSON: true}); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	var rep importAcceptReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	want := []jsonAcceptSetting{
		{Address: "nomad_job.web", Attribute: "detach", Value: true, Provider: "registry.opentofu.org/hashicorp/nomad", Version: "2.6.1"},
		{Address: "nomad_job.web", Attribute: "preserve_counts", Value: false, Provider: "registry.opentofu.org/hashicorp/nomad", Version: "2.6.1"},
	}
	if !rep.OK || rep.Imports != 1 || len(rep.Settings) != 2 || rep.Settings[0] != want[0] || rep.Settings[1] != want[1] {
		t.Errorf("report %+v, want 1 import that sets %+v", rep, want)
	}
	out.Reset()
	jobspecPlan := stubTofu(t, dir, job(`{"jobspec":"job { x }","detach":true,"preserve_counts":false}`))
	err = app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: jobspecPlan, Imports: importsPath, JSON: true})
	rep = importAcceptReport{}
	if !errors.Is(err, ErrFailed) || json.Unmarshal(out.Bytes(), &rep) != nil || rep.OK || len(rep.Settings) != 0 || len(rep.Refusals) != 1 || !strings.Contains(rep.Refusals[0].Reason, `"jobspec"`) {
		t.Errorf("a jobspec change: %v, %s; want a refusal that names jobspec and no setting", err, &out)
	}
}

// nomadImportPlan is tofu show -json of a plan after the import of
// nomad_job.web, whose job goes from jobspec before to jobspec after while
// detach goes from null to true. vars are variables declared sensitive,
// and extra is more of after.
func nomadImportPlan(before, after, vars, extra string) string {
	return `{"format_version":"1.2","terraform_version":"1.13.9","planned_values":{},"variables":{` + vars + `},
"configuration":{"root_module":{"variables":{"a":{"sensitive":true},"b":{"sensitive":true}}}},
"resource_changes":[{"address":"nomad_job.web","mode":"managed","type":"nomad_job","name":"web","provider_name":"registry.opentofu.org/hashicorp/nomad",
 "change":{"actions":["update"],"before":{"jobspec":` + before + `,"detach":null},"after":{"jobspec":` + after + `,"detach":true` + extra + `},
 "after_unknown":{},"before_sensitive":{},"after_sensitive":{},"importing":{"id":"web@default"}}}]}`
}

// TestImportAcceptPlanJudgesValuesBeforeTheMask requires that writ import
// accept reads what an import update changes from the values of tofu show
// -json of the saved plan, not from the masked values it reports, that it
// prints no value the mask hides, and that it accepts no import update of
// a plan given as JSON.
func TestImportAcceptPlanJudgesValuesBeforeTheMask(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const job50 = `"job \"web\" {\n type = \"batch\"\n priority = 50\n}\n"`
	const job60 = `"job \"web\" {\n type = \"batch\"\n priority = 60\n}\n"`
	vars := `"a":{"value":` + job50 + `},"b":{"value":` + job60 + `}`
	imports, err := importer.EncodeImports([]importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}})
	if err != nil {
		t.Fatal(err)
	}
	importsPath := filepath.Join(dir, "imported.imports.json")
	if err := os.WriteFile(importsPath, imports, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsOnly := nomadImportPlan(job50, job50, vars, "")
	masked, err := mask.New(nil).Plan([]byte(settingsOnly))
	if err != nil {
		t.Fatal(err)
	}
	maskedText, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	const asJSON = "a plan given as JSON does not prove what the update changes; pass writ import accept the plan file that tofu plan -out saved"
	for _, tc := range []struct {
		name, plan string
		// json passes the plan as a JSON file, not as a saved plan.
		json bool
		// refused is "" when the plan is accepted with detach = true.
		refused string
	}{
		{name: "a job that sensitive variables hold, unchanged", plan: settingsOnly},
		// The mask sweeps both jobs to one placeholder.
		{name: "a job change that the mask hides", plan: nomadImportPlan(job50, job60, vars, ""), refused: `plans update of "jobspec", which`},
		{name: "an attribute only after, null", plan: nomadImportPlan(`"old job"`, `"old job"`, "", `,"unexpected":null`), refused: `plans update of "unexpected", which`},
		{name: "tofu show -json's output", plan: settingsOnly, json: true, refused: asJSON},
		{name: "a masked document", plan: string(maskedText), json: true, refused: asJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(path, []byte(tc.plan), 0o600); err != nil {
				t.Fatal(err)
			}
			if !tc.json {
				path = stubTofu(t, dir, tc.plan)
			}
			var human, out bytes.Buffer
			app := bulkApp(dir, &human)
			humanErr := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: path, Imports: importsPath})
			app = bulkApp(dir, &out)
			err := app.ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: path, Imports: importsPath, JSON: true})
			for _, text := range []string{human.String(), out.String()} {
				if strings.Contains(text, "priority") {
					t.Errorf("the report holds a job that a sensitive variable holds:\n%s", text)
				}
			}
			var rep importAcceptReport
			if jerr := json.Unmarshal(out.Bytes(), &rep); jerr != nil {
				t.Fatalf("%v: %s", jerr, &out)
			}
			if tc.refused == "" {
				want := jsonAcceptSetting{Address: "nomad_job.web", Attribute: "detach", Value: true, Provider: "registry.opentofu.org/hashicorp/nomad", Version: "2.6.1"}
				if humanErr != nil || err != nil || !rep.OK || len(rep.Settings) != 1 || rep.Settings[0] != want || !strings.Contains(human.String(), "~ nomad_job.web: sets detach = true,") {
					t.Fatalf("%v, %v: %s%s; want the settings update accepted", humanErr, err, &human, &out)
				}
				return
			}
			if !errors.Is(humanErr, ErrFailed) || !errors.Is(err, ErrFailed) || rep.OK || len(rep.Settings) != 0 || len(rep.Refusals) != 1 || !strings.Contains(rep.Refusals[0].Reason, tc.refused) || !strings.Contains(human.String(), tc.refused) {
				t.Fatalf("%v, %v: %s%s; want only a refusal like %q", humanErr, err, &human, &out, tc.refused)
			}
		})
	}
}

// stubTofu puts first on PATH a tofu that prints show as tofu show -json
// PLAN prints a saved plan when it runs in dir, and fails otherwise. It
// returns a new saved plan file in dir, which starts as tofu's zip
// archives do.
func stubTofu(t *testing.T, dir, show string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	showPath := filepath.Join(bin, "show.json")
	plan, err := os.CreateTemp(dir, "*.plan")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if _, err := plan.WriteString("PK\x03\x04 a saved plan"); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		`[ "$#" = 3 ] && [ "$1" = show ] && [ "$2" = -json ] && [ -f "$3" ] && [ "$(pwd -P)" = ` + addr.ShellWord(real) + " ] || exit 9\n" +
		"exec cat " + addr.ShellWord(showPath) + "\n"
	for path, text := range map[string]string{showPath: show, filepath.Join(bin, "tofu"): script} {
		if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return plan.Name()
}

// acceptReports runs writ import accept of plan, with --imports a file
// that selects nomad_job.web by web@default, in dir and with --json, and
// returns each run's error and output.
func acceptReports(t *testing.T, dir, plan string) (human, report string, humanErr, jsonErr error) {
	t.Helper()
	imports, err := importer.EncodeImports([]importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}})
	if err != nil {
		t.Fatal(err)
	}
	importsPath := filepath.Join(dir, "imported.imports.json")
	if err := os.WriteFile(importsPath, imports, 0o600); err != nil {
		t.Fatal(err)
	}
	var h, j bytes.Buffer
	humanErr = bulkApp(dir, &h).ImportAcceptPlan(context.Background(), ImportAcceptPlanOptions{Plan: plan, Imports: importsPath})
	jsonErr = bulkApp(dir, &j).ImportAcceptPlan(context.Background(), ImportAcceptPlanOptions{Plan: plan, Imports: importsPath, JSON: true})
	return h.String(), j.String(), humanErr, jsonErr
}

// TestImportAcceptPlanMasksAttributeNames requires that a refusal masks an
// attribute name that holds a sensitive variable's value, in the human
// output and in the report, whether the plan comes saved or as JSON.
func TestImportAcceptPlanMasksAttributeNames(t *testing.T) {
	const secret = "synthetic-secret-71ce"
	repro := readFile(t, "testdata/import-accept", "secret-attribute.json")
	for _, tc := range []struct {
		name string
		plan func(dir string) string
		want string
	}{
		{name: "a saved plan", plan: func(dir string) string { return stubTofu(t, dir, repro) }, want: `plans update of "attribute_(sensitive)", which the named list`},
		{name: "a JSON plan", plan: func(string) string { return "testdata/import-accept/secret-attribute.json" }, want: "pass writ import accept the plan file that tofu plan -out saved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			human, report, humanErr, jsonErr := acceptReports(t, dir, tc.plan(dir))
			var rep importAcceptReport
			if !errors.Is(humanErr, ErrFailed) || !errors.Is(jsonErr, ErrFailed) || json.Unmarshal([]byte(report), &rep) != nil || rep.OK || len(rep.Refusals) != 1 {
				t.Fatalf("%v, %v: %s%s; want one refusal", humanErr, jsonErr, human, report)
			}
			for _, text := range []string{human, report} {
				if strings.Contains(text, secret) {
					t.Errorf("the output holds the secret:\n%s", text)
				}
			}
			if !strings.Contains(human, tc.want) || !strings.Contains(rep.Refusals[0].Reason, tc.want) {
				t.Errorf("the refusal is not like %q:\n%s%s", tc.want, human, report)
			}
		})
	}
}

// TestImportAcceptPlanRefusesUpdateOfAnyJSON requires that a JSON plan
// whose import updates settings is refused however it looks, a masked
// document with planned_values null or one inside a writ.remote-plan
// document, and that an import update of a listed setting whose value
// after is unknown is refused from a saved plan too.
func TestImportAcceptPlanRefusesUpdateOfAnyJSON(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		saved      bool
		want       string
	}{
		{name: "a masked document with planned_values null", file: "actual-masked-marker.json", want: "a plan given as JSON does not prove what the update changes"},
		{name: "a masked document in a remote plan", file: "remote-masked-marker.json", want: "a plan given as JSON does not prove what the update changes"},
		{name: "a listed setting unknown after, saved", file: "unknown-listed.json", saved: true, want: `plans update of "detach", a provider-only setting whose value after the update is unknown until apply or sensitive`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			plan := filepath.Join("testdata/import-accept", tc.file)
			if tc.saved {
				plan = stubTofu(t, dir, readFile(t, "testdata/import-accept", tc.file))
			}
			human, report, humanErr, jsonErr := acceptReports(t, dir, plan)
			var rep importAcceptReport
			if !errors.Is(humanErr, ErrFailed) || !errors.Is(jsonErr, ErrFailed) || json.Unmarshal([]byte(report), &rep) != nil || rep.OK || len(rep.Settings) != 0 || len(rep.Refusals) != 1 || !strings.Contains(rep.Refusals[0].Reason, tc.want) || !strings.Contains(human, tc.want) {
				t.Fatalf("%v, %v: %s%s; want only a refusal like %q", humanErr, jsonErr, human, report, tc.want)
			}
		})
	}
}

// TestImportAcceptPlanRunsTofuShowInTheStack requires that writ import
// accept runs tofu show -json of a saved plan in the directory of the
// imports file, or in --dir, and fails, naming the command, when tofu
// show fails.
func TestImportAcceptPlanRunsTofuShowInTheStack(t *testing.T) {
	ctx := context.Background()
	stack := t.TempDir()
	a, b := "a", "b"
	imports, err := importer.EncodeImports([]importer.Import{{Type: "random_uuid", Name: "u", Key: &a, ID: "a"}, {Type: "random_uuid", Name: "u", Key: &b, ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	importsPath := filepath.Join(stack, "imported.imports.json")
	if err := os.WriteFile(importsPath, imports, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := stubTofu(t, stack, acceptRaw)
	var out bytes.Buffer
	if err := New(Options{Stdout: &out, Stderr: &out}).ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: plan, Imports: importsPath}); err != nil || !strings.Contains(out.String(), "Accepted: the plan imports 2 objects") {
		t.Fatalf("in the imports file's directory: %v\n%s", err, &out)
	}
	out.Reset()
	if err := bulkApp(stack, &out).ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: plan, Imports: importsPath}); err != nil {
		t.Fatalf("with --dir: %v\n%s", err, &out)
	}
	out.Reset()
	err = bulkApp(t.TempDir(), &out).ImportAcceptPlan(ctx, ImportAcceptPlanOptions{Plan: plan, Imports: importsPath})
	if err == nil || !strings.Contains(err.Error(), "tofu show -json "+plan) || !strings.Contains(err.Error(), "run it there to see why") {
		t.Fatalf("with --dir another directory: %v, want tofu show to fail there", err)
	}
}

// TestImportAcceptPlanErrorsQuoteNoPlan requires that an error found while
// reading a JSON plan, before its mask has learned the plan's secrets,
// quotes nothing of the plan, in the human output and in the report.
func TestImportAcceptPlanErrorsQuoteNoPlan(t *testing.T) {
	const secret = "synthetic-secret-71ce"
	base := map[string]any{
		"format_version": "1.2",
		"planned_values": map[string]any{},
		"variables":      map[string]any{"s": map[string]any{"value": secret}},
		"configuration":  map[string]any{"root_module": map[string]any{"variables": map[string]any{"s": map[string]any{"sensitive": true}}}},
	}
	with := func(k string, v any) map[string]any {
		m := maps.Clone(base)
		m[k] = v
		return m
	}
	for name, plan := range map[string]map[string]any{
		"a format":         with("format", secret),
		"a format_version": with("format_version", secret),
		"a malformed output change keyed by the secret": with("output_changes", map[string]any{secret: map[string]any{"actions": 42}}),
		"a masked document with a malformed output change": {
			"format_version": "1.2",
			"output_changes": map[string]any{secret: map[string]any{"actions": 42}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			text, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "plan.json")
			if err := os.WriteFile(path, text, 0o600); err != nil {
				t.Fatal(err)
			}
			human, report, humanErr, jsonErr := acceptReports(t, dir, path)
			if humanErr == nil || jsonErr == nil {
				t.Fatalf("accepted: %s%s", human, report)
			}
			for _, out := range []string{human, report, humanErr.Error(), jsonErr.Error()} {
				if strings.Contains(out, secret) {
					t.Errorf("the output quotes the plan:\n%s", out)
				}
			}
		})
	}
}
