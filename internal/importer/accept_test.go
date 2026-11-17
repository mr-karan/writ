package importer_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mask"
)

// change is a change of random_uuid.NAME, at the for_each key key unless
// it is "", that imports the object with id importID unless it is "-".
func change(name, key, importID string, actions ...string) mask.ResourceChange {
	address := "random_uuid." + name
	var index any
	if key != "" {
		address += `["` + key + `"]`
		index = key
	}
	c := mask.ResourceChange{Address: address, Mode: "managed", Type: "random_uuid", Name: name, Index: index, Change: mask.Change{Actions: actions}}
	if importID != "-" {
		c.Change.Importing = &mask.Importing{ID: importID}
	}
	return c
}

// at is c with another address, as a plan whose address and fields
// disagree holds it.
func at(address string, c mask.ResourceChange) mask.ResourceChange {
	c.Address = address
	return c
}

func selected(name, key, id string) importer.Import {
	imp := importer.Import{Type: "random_uuid", Name: name, ID: id}
	if key != "" {
		imp.Key = &key
	}
	return imp
}

func TestAcceptPlan(t *testing.T) {
	ab := []importer.Import{selected("u", "a", "id-a"), selected("u", "b", "id-b")}
	for _, tc := range []struct {
		name    string
		plan    mask.Plan
		want    []importer.Import
		imports int
		refused []string
	}{
		{name: "only imports", want: ab, imports: 2, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "a", "id-a", "no-op"), change("u", "b", "id-b", "no-op"),
		}}},
		{name: "a resource the state has is no import", want: []importer.Import{selected("new", "", "n")}, imports: 1, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("kept", "", "-", "no-op"), change("new", "", "n", "no-op"),
		}}},
		{name: "a selected object missing", want: append(ab, selected("u", "c", "id-c")), imports: 2, refused: []string{`random_uuid.u["c"]: the import selected the object with id "id-c", and the plan does not import it`}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "a", "id-a", "no-op"), change("u", "b", "id-b", "no-op"),
		}}},
		{name: "one address imported twice", want: ab, imports: 1, refused: []string{
			`random_uuid.u["a"]: imports an object at an address that another change of the plan imports too`,
			`random_uuid.u["b"]: the import selected the object with id "id-b", and the plan does not import it`,
		}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "a", "id-a", "no-op"), change("u", "a", "id-a", "no-op"),
		}}},
		{name: "an import with no id", want: []importer.Import{selected("u", "", "id-u")}, refused: []string{`random_uuid.u: imports the object with id "", and the import selected id "id-u"`}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "", "", "no-op"),
		}}},
		{name: "another id", want: ab, imports: 1, refused: []string{`random_uuid.u["b"]: imports the object with id "id-x", and the import selected id "id-b"`}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "a", "id-a", "no-op"), change("u", "b", "id-x", "no-op"),
		}}},
		{name: "as many imports of other addresses", want: ab, refused: []string{
			`random_uuid.u["c"]: imports an object, and the import did not select this address`,
			`random_uuid.v["b"]: imports an object, and the import did not select this address`,
			`random_uuid.u["a"]: the import selected`, `random_uuid.u["b"]: the import selected`,
		}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "c", "id-a", "no-op"), change("v", "b", "id-b", "no-op"),
		}}},
		{name: "an extra import", want: ab, imports: 2, refused: []string{`random_uuid.u["c"]: imports an object, and the import did not select this address`}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "a", "id-a", "no-op"), change("u", "b", "id-b", "no-op"), change("u", "c", "id-c", "no-op"),
		}}},
		{name: "a selected import in a module", want: []importer.Import{selected("u", "", "id-u")}, refused: []string{
			"module.m.random_uuid.u: imports an object, and the import did not select this address",
			"random_uuid.u: the import selected",
		}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			{Address: "module.m.random_uuid.u", ModuleAddress: "module.m", Mode: "managed", Type: "random_uuid", Name: "u", Change: mask.Change{Actions: []string{"no-op"}, Importing: &mask.Importing{ID: "id-u"}}},
		}}},
		{name: "an import that updates", want: []importer.Import{selected("s", "", "id-s")}, refused: []string{"random_uuid.s: imports the object and then plans update, and a plan given as JSON does not prove what the update changes"}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("s", "", "id-s", "update"),
		}}},
		{name: "a create", want: []importer.Import{selected("u", "", "id-u")}, imports: 1, refused: []string{"random_uuid.p: plans create, and a plan after an import may only import"}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("u", "", "id-u", "no-op"), change("p", "", "-", "create"),
		}}},
		{name: "a replace", want: []importer.Import{selected("p", "", "id-p")}, refused: []string{"random_uuid.p: imports the object and then plans delete and create"}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			change("p", "", "id-p", "delete", "create"),
		}}},
		{name: "an address that is not its fields", want: []importer.Import{selected("u", "", "id-a")}, refused: []string{
			"module.m.random_uuid.other[0]: its address is not random_uuid.u, which its module, type, name, and index make",
			"random_uuid.u: the import selected",
		}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			at("module.m.random_uuid.other[0]", change("u", "", "id-a", "no-op")),
		}}},
		{name: "keys as tofu quotes them", want: []importer.Import{
			selected("u", `a"b\c`, "1"), selected("u", "${x} %{y} $ %", "2"), selected("u", "a\nb\tc\rd", "3"), selected("u", "bell\a", "4"), selected("u", "é", "5"),
		}, imports: 5, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			at(`random_uuid.u["a\"b\\c"]`, change("u", `a"b\c`, "1", "no-op")),
			at(`random_uuid.u["$${x} %%{y} $ %"]`, change("u", "${x} %{y} $ %", "2", "no-op")),
			at(`random_uuid.u["a\nb\tc\rd"]`, change("u", "a\nb\tc\rd", "3", "no-op")),
			at(`random_uuid.u["bell\u0007"]`, change("u", "bell\a", "4", "no-op")),
			at("random_uuid.u[\"é\"]", change("u", "é", "5", "no-op")),
		}}},
		{name: "a key quoted another way", want: []importer.Import{selected("u", "${x}", "1")}, refused: []string{
			`random_uuid.u["${x}"]: its address is not random_uuid.u["$${x}"]`, `random_uuid.u["$${x}"]: the import selected`,
		}, plan: mask.Plan{ResourceChanges: []mask.ResourceChange{
			at(`random_uuid.u["${x}"]`, change("u", "${x}", "1", "no-op")),
		}}},
		{name: "an errored plan", want: []importer.Import{selected("u", "", "id-u")}, imports: 1, refused: []string{"the plan errored"}, plan: mask.Plan{Errored: true, ResourceChanges: []mask.ResourceChange{
			change("u", "", "id-u", "no-op"),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc, err := importer.AcceptPlan(&tc.plan, tc.want, importer.QuoteIDs)
			if err != nil {
				t.Fatal(err)
			}
			imports := acc.Imports
			var got []string
			for _, r := range acc.Refusals {
				text := r.Reason
				if r.Address != "" {
					text = r.Address + ": " + text
				}
				got = append(got, text)
			}
			if imports != tc.imports || len(got) != len(tc.refused) {
				t.Fatalf("imports %d, refusals %q; want %d imports and refusals like %q", imports, got, tc.imports, tc.refused)
			}
			for i, want := range tc.refused {
				if !strings.Contains(got[i], want) {
					t.Errorf("refusal %q, want it to say %q", got[i], want)
				}
			}
		})
	}
}

const nomad = "registry.opentofu.org/hashicorp/nomad"

// jobPlan is a plan after the import of nomad_job.web@default as `tofu show
// -json` writes it. before and after replace or add attributes to a job
// whose provider-only settings are null; drop removes attributes from
// before or after.
type jobPlan struct {
	provider                                      string
	actions                                       []string
	before, after                                 map[string]any
	dropBefore, dropAfter                         []string
	afterUnknown, beforeSensitive, afterSensitive any
	replacePaths                                  [][]any
	// sensitive are variables declared sensitive, which the mask sweeps
	// out of every value of the plan.
	sensitive map[string]string
}

func (j jobPlan) raw(t *testing.T) []byte {
	t.Helper()
	values := func(over map[string]any, drop []string) map[string]any {
		v := map[string]any{"jobspec": "job {}", "detach": nil, "rerun_if_dead": nil, "modify_index": "11"}
		for k, x := range over {
			v[k] = x
		}
		for _, k := range drop {
			delete(v, k)
		}
		return v
	}
	provider, actions := j.provider, j.actions
	if provider == "" {
		provider = nomad
	}
	if actions == nil {
		actions = []string{"update"}
	}
	vars, decls := map[string]any{}, map[string]any{}
	for name, v := range j.sensitive {
		vars[name] = map[string]any{"value": v}
		decls[name] = map[string]any{"sensitive": true}
	}
	change := map[string]any{
		"actions": actions, "before": values(j.before, j.dropBefore), "after": values(j.after, j.dropAfter),
		"after_unknown": orEmpty(j.afterUnknown), "before_sensitive": orEmpty(j.beforeSensitive), "after_sensitive": orEmpty(j.afterSensitive),
		"importing": map[string]any{"id": "web@default"},
	}
	if j.replacePaths != nil {
		change["replace_paths"] = j.replacePaths
	}
	raw, err := json.Marshal(map[string]any{
		"format_version": "1.2", "terraform_version": "1.13.9", "planned_values": map[string]any{}, "variables": vars,
		"configuration": map[string]any{"root_module": map[string]any{"variables": decls}},
		"resource_changes": []any{map[string]any{
			"address": "nomad_job.web", "mode": "managed", "type": "nomad_job", "name": "web", "provider_name": provider, "change": change,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// jobspec is a Nomad job of priority p.
func jobspec(p int) string {
	return fmt.Sprintf("job \"web\" {\n type = \"batch\"\n priority = %d\n group \"g\" {\n task \"t\" {\n driver = \"raw_exec\"\n }\n }\n}\n", p)
}

// TestAcceptTofuPlanProviderOnlySettings requires that an import may
// update in place only the settings on the named list for its provider
// and type, that every other change is refused by name, and that which
// attributes change is read from the plan's values before the mask.
func TestAcceptTofuPlanProviderOnlySettings(t *testing.T) {
	web := []importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}}
	settings := map[string]any{"detach": true, "rerun_if_dead": false}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{"detach": true, "rerun_if_dead": false}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		plan     jobPlan
		settings []string
		refused  string
	}{
		{name: "listed settings", plan: jobPlan{after: settings}, settings: []string{"detach=true", "rerun_if_dead=false"}},
		{name: "listed settings beside a job that sensitive variables hold", plan: jobPlan{
			before: map[string]any{"jobspec": jobspec(50)}, after: with(map[string]any{"jobspec": jobspec(50)}),
			sensitive: map[string]string{"a": jobspec(50)},
		}, settings: []string{"detach=true", "rerun_if_dead=false"}},
		{name: "an attribute not listed", plan: jobPlan{after: map[string]any{"jobspec": "job { x }"}},
			refused: `plans update of "jobspec", which the named list of provider-only settings does not hold for nomad_job`},
		{name: "listed settings beside another attribute", plan: jobPlan{after: map[string]any{"detach": true, "priority": 60}},
			refused: `plans update of "priority", which`},
		// The mask sweeps both jobs to one placeholder, so only the values
		// before the mask show the change of priority.
		{name: "a job change that the mask hides", plan: jobPlan{
			before: map[string]any{"jobspec": jobspec(50)}, after: with(map[string]any{"jobspec": jobspec(60)}),
			sensitive: map[string]string{"a": jobspec(50), "b": jobspec(60)},
		}, refused: `plans update of "jobspec", which`},
		{name: "an attribute only after, null", plan: jobPlan{after: with(map[string]any{"unexpected": nil})},
			refused: `plans update of "unexpected", which`},
		{name: "an attribute only before, null", plan: jobPlan{before: map[string]any{"unexpected": nil}, after: settings},
			refused: `plans update of "unexpected", which`},
		{name: "an attribute that the update drops", plan: jobPlan{before: map[string]any{"meta": "x"}, after: settings},
			refused: `plans update of "meta", which`},
		{name: "a listed setting beside an unknown attribute", plan: jobPlan{after: settings, afterUnknown: map[string]any{"modify_index": true}},
			refused: `plans update of "modify_index", which`},
		{name: "a nested unknown value", plan: jobPlan{after: settings, afterUnknown: map[string]any{"task_groups": []any{map[string]any{"count": true}}}},
			refused: `plans update of "task_groups", which`},
		{name: "a whole value unknown", plan: jobPlan{after: settings, afterUnknown: true},
			refused: "the plan does not hold its values before and after as objects"},
		{name: "a sensitive attribute that changed", plan: jobPlan{
			before: map[string]any{"token": "old-token-4b1e"}, after: with(map[string]any{"token": "new-token-9c2f"}),
			beforeSensitive: map[string]any{"token": true}, afterSensitive: map[string]any{"token": true},
		}, refused: `plans update of "token", which`},
		{name: "an attribute that becomes sensitive", plan: jobPlan{after: settings, afterSensitive: map[string]any{"jobspec": true}},
			refused: `plans update of "jobspec", which`},
		{name: "a sensitive mark only before", plan: jobPlan{after: settings, beforeSensitive: map[string]any{"jobspec": true}},
			refused: `plans update of "jobspec", which`},
		{name: "a replace", plan: jobPlan{actions: []string{"delete", "create"}, after: settings}, refused: "imports the object and then plans delete and create"},
		{name: "an update with replace paths", plan: jobPlan{after: settings, replacePaths: [][]any{{"detach"}}},
			refused: "imports the object and then plans update, so the code does not describe it yet"},
		{name: "the type of a provider not measured", plan: jobPlan{provider: "registry.opentofu.org/example/nomad", after: settings},
			refused: `plans update of "detach", "rerun_if_dead", which`},
		{name: "an update that changes nothing", plan: jobPlan{}, refused: "plans update, and the plan shows no attribute it changes"},
		// A listed setting must show the value it sets: a null after
		// whose after_unknown is true is not the value apply sets.
		{name: "a listed setting unknown after", plan: jobPlan{after: map[string]any{"detach": nil}, afterUnknown: map[string]any{"detach": true}},
			refused: `plans update of "detach", a provider-only setting whose value after the update is unknown until apply or sensitive`},
		{name: "a listed setting unknown inside", plan: jobPlan{after: map[string]any{"detach": map[string]any{"x": []any{nil}}}, afterUnknown: map[string]any{"detach": map[string]any{"x": []any{true}}}},
			refused: `plans update of "detach", a provider-only setting whose value after the update is unknown until apply or sensitive`},
		{name: "a listed setting sensitive after", plan: jobPlan{after: map[string]any{"detach": "synthetic-secret-71ce"}, afterSensitive: map[string]any{"detach": true}},
			refused: `plans update of "detach", a provider-only setting whose value after the update is unknown until apply or sensitive`},
		{name: "a listed setting sensitive inside", plan: jobPlan{after: map[string]any{"detach": map[string]any{"x": "synthetic-secret-71ce"}}, afterSensitive: map[string]any{"detach": map[string]any{"x": true}}},
			refused: `plans update of "detach", a provider-only setting whose value after the update is unknown until apply or sensitive`},
		{name: "a listed setting unknown beside a known one", plan: jobPlan{after: map[string]any{"detach": true, "rerun_if_dead": nil}, afterUnknown: map[string]any{"rerun_if_dead": true}},
			refused: `plans update of "rerun_if_dead", a provider-only setting whose value after`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc, err := importer.AcceptTofuPlan(tc.plan.raw(t), web)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range acc.Settings {
				if s.Address != "nomad_job.web" || s.Provider != nomad || s.Version != "2.6.1" {
					t.Errorf("setting %+v, want one of nomad_job.web measured on %s 2.6.1", s, nomad)
				}
				got = append(got, fmt.Sprintf("%s=%v", s.Attribute, s.Value))
			}
			if tc.refused == "" {
				if len(acc.Refusals) != 0 || acc.Imports != 1 || !slices.Equal(got, tc.settings) {
					t.Fatalf("AcceptTofuPlan = %+v, want 1 import and settings %v", acc, tc.settings)
				}
				return
			}
			if len(acc.Refusals) != 1 || acc.Imports != 0 || len(acc.Settings) != 0 || !strings.Contains(acc.Refusals[0].Reason, tc.refused) {
				t.Fatalf("AcceptTofuPlan = %+v, want only a refusal like %q", acc, tc.refused)
			}
		})
	}
}

// TestAcceptPlanRefusesUpdateOfJSON requires that a plan given as JSON,
// a masked plan document or tofu show -json's output, has no import update
// accepted, even one of listed settings alone, since nothing proves what
// the plan that will be applied changes.
func TestAcceptPlanRefusesUpdateOfJSON(t *testing.T) {
	web := []importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}}
	raw := jobPlan{after: map[string]any{"detach": true}}.raw(t)
	p, err := mask.New(nil).Plan(raw)
	if err != nil {
		t.Fatal(err)
	}
	masked, err := importer.AcceptPlan(p, web, importer.QuoteIDs)
	if err != nil {
		t.Fatal(err)
	}
	tofuJSON, err := importer.AcceptTofuJSON(raw, web, importer.QuoteIDs)
	if err != nil {
		t.Fatal(err)
	}
	for name, acc := range map[string]importer.Acceptance{"a masked document": masked, "tofu show -json's output": tofuJSON} {
		if len(acc.Refusals) != 1 || acc.Imports != 0 || len(acc.Settings) != 0 || !strings.Contains(acc.Refusals[0].Reason, "pass writ import accept the plan file that tofu plan -out saved") {
			t.Errorf("%s: %+v, want only the refusal of a plan given as JSON", name, acc)
		}
	}
}

// TestAcceptTofuPlanMasksItsText requires that no secret of the plan
// reaches an acceptance's text through an attribute name the plan changes,
// or through an import id that the imports file holds.
func TestAcceptTofuPlanMasksItsText(t *testing.T) {
	const secret = "synthetic-secret-71ce"
	for _, tc := range []struct {
		name string
		plan jobPlan
		want []importer.Import
	}{
		{name: "an attribute whose name holds a secret", plan: jobPlan{after: map[string]any{"detach": true, "attribute_" + secret: nil}, sensitive: map[string]string{"s": secret}},
			want: []importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}}},
		{name: "a quote and a secret in a name", plan: jobPlan{after: map[string]any{"detach": true, `a"` + secret + `\b`: nil}, sensitive: map[string]string{"s": secret}},
			want: []importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}}},
		{name: "an import id the plan does not import", plan: jobPlan{after: map[string]any{"detach": true}, sensitive: map[string]string{"s": secret}},
			want: []importer.Import{{Type: "nomad_job", Name: "web", ID: "web@default"}, {Type: "nomad_job", Name: "other", ID: secret}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc, err := importer.AcceptTofuPlan(tc.plan.raw(t), tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if len(acc.Refusals) == 0 {
				t.Fatalf("AcceptTofuPlan = %+v, want a refusal", acc)
			}
			if text := fmt.Sprintf("%+v", acc); strings.Contains(text, secret) {
				t.Errorf("the acceptance holds the secret: %s", text)
			}
		})
	}
}

func TestImportsFileReadsBack(t *testing.T) {
	want := []importer.Import{selected("u", `a "b" ${c}`, "id-a"), selected("v", "", "id-v")}
	raw, err := importer.EncodeImports(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := importer.ParseImports(raw)
	if err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	if len(got) != 2 || got[0].Key == nil || *got[0].Key != *want[0].Key || got[1].Key != nil || got[0].ID != "id-a" || got[1].Name != "v" {
		t.Errorf("ParseImports = %+v, want %+v", got, want)
	}
}

func TestParseImportsRefuses(t *testing.T) {
	for name, text := range map[string]string{
		"another format":               `{"format":"writ.import-bulk/v0","imports":[{"type":"random_uuid","name":"u","key":null,"id":"x"}]}`,
		"no import":                    `{"format":"writ.import-selected/v0","imports":[]}`,
		"an import with no id":         `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":null,"id":""}]}`,
		"a name that is no identifier": `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u.v","key":null,"id":"x"}]}`,
		"one instance twice":           `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":"a","id":"x"},{"type":"random_uuid","name":"u","key":"a","id":"y"}]}`,
		"an unknown field":             `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":null,"id":"x","index":0}]}`,
		"two documents":                `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":null,"id":"x"}]} {}`,
		"a secret format":              `{"format":"synthetic-secret-71ce","imports":[{"type":"random_uuid","name":"u","key":null,"id":"x"}]}`,
		"a secret name":                `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"synthetic-secret-71ce","key":null,"id":"x"}]}`,
		"a secret key twice":           `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":"synthetic-secret-71ce","id":"x"},{"type":"random_uuid","name":"u","key":"synthetic-secret-71ce","id":"y"}]}`,
		"a secret unknown field":       `{"format":"writ.import-selected/v0","imports":[{"type":"random_uuid","name":"u","key":null,"id":"x","synthetic-secret-71ce":0}]}`,
	} {
		imps, err := importer.ParseImports([]byte(text))
		if err == nil {
			t.Errorf("%s: ParseImports = %+v", name, imps)
			continue
		}
		// The error is printed before a plan's mask has read any secret,
		// so it quotes nothing of the file.
		if strings.Contains(err.Error(), "synthetic-secret-71ce") {
			t.Errorf("%s: the error quotes the file: %v", name, err)
		}
	}
}
