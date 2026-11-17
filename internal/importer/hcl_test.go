package importer_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/types"
	"github.com/mr-karan/writ/internal/value"
)

// These tests run Bulk with OpenTofu output on the objects of bulk_test.go.
// e2e/import_hcl_test.go plans the same kind of code with real tofu.

// hclCode checks that code parses as HCL and that formatting it changes
// nothing, and returns it.
func hclCode(t *testing.T, res *importer.BulkResult) string {
	t.Helper()
	if _, diags := hclsyntax.ParseConfig(res.Code, "imported.tf", hcl.InitialPos); diags.HasErrors() {
		t.Fatalf("the code does not parse: %s\n%s", diags.Error(), res.Code)
	}
	if got := hclwrite.Format(res.Code); string(got) != string(res.Code) {
		t.Fatalf("the code is not formatted:\n%s", res.Code)
	}
	return string(res.Code)
}

func TestBulkHCLGroupsAndReferences(t *testing.T) {
	ss := schemas(t)
	const (
		u1 = "11111111-aaaa-4bbb-8ccc-000000000001"
		u2 = "11111111-aaaa-4bbb-8ccc-000000000002"
	)
	r := &fakeReader{objects: map[string]importer.Found{
		"random.Uuid " + u1: uuidObj(t, ss, u1),
		"random.Uuid " + u2: uuidObj(t, ss, u2),
		"random.Integer 1":  integerObj(t, ss, 1, u1),
		"random.Integer 2":  integerObj(t, ss, 2, u2),
		"random.Integer 5":  unseeded(integerObj(t, ss, 5, "")),
	}}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{
			{Type: integerType, IDs: []string{"1", "2"}},
			{Type: uuidType, IDs: []string{u2, u1}},
			{Type: integerType, IDs: []string{"5"}},
		},
		Reader: r, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	code := hclCode(t, res)
	for _, want := range []string{
		"resource \"random_uuid\" \"random_uuid\" {\n  for_each = toset([\"" + u1 + "\", \"" + u2 + "\"])\n}",
		"import {\n  for_each = toset([\"" + u1 + "\", \"" + u2 + "\"])\n  to       = random_uuid.random_uuid[each.key]\n  id       = each.key\n}",
		"seed = random_uuid.random_uuid[each.value.random_uuid].id",
		"\"1\" = {\n      random_uuid = \"" + u1 + "\"\n    }",
		"\"2\" = {\n      random_uuid = \"" + u2 + "\"\n    }",
		"resource \"random_integer\" \"random_integer_5\" {",
		"import {\n  to = random_integer.random_integer_5\n  id = \"5\"\n}",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the code lacks\n%s", want)
		}
	}
	if res.Linked != 2 || res.Groups != 2 || res.Singles != 1 {
		t.Errorf("linked %d, groups %d, singles %d; want 2, 2, 1", res.Linked, res.Groups, res.Singles)
	}
}

// unseeded is f without a seed, so its code has another shape.
func unseeded(f importer.Found) importer.Found {
	fields := map[string]value.Value{}
	for _, k := range f.State.Keys() {
		fields[k] = f.State.Get(k)
	}
	fields["seed"] = value.NullVal()
	f.State = value.ObjectVal(fields)
	return f
}

func TestBulkHCLSecretsAreVariables(t *testing.T) {
	ss := schemas(t)
	r, src := resourceOf(t, ss, sensitive)
	file := func(name, content string) importer.Found {
		return importer.Found{Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
			"filename": value.StringVal("/srv/" + name), "content": value.StringVal(content).Secret(),
			"file_permission": value.StringVal("0700"), "directory_permission": value.StringVal("0700"),
			"id": value.StringVal(name + "-0123456789abcdef"),
		})}
	}
	reader := &fakeReader{objects: map[string]importer.Found{
		"local.SensitiveFile a": file("a", "first-secret-content"),
		"local.SensitiveFile b": file("b", "second-secret-content"),
	}}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: sensitive, IDs: []string{"a", "b"}}},
		Reader:     reader, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	code := hclCode(t, res)
	if res.Groups != 0 || res.Singles != 2 || len(res.Params) != 2 {
		t.Fatalf("groups %d, singles %d, params %+v; want two resources, each with a variable", res.Groups, res.Singles, res.Params)
	}
	for _, secret := range []string{"first-secret-content", "second-secret-content"} {
		if strings.Contains(code, secret) {
			t.Errorf("the code holds a secret")
		}
	}
	for _, p := range res.Params {
		want := "variable \"" + p.Name + "\" {\n  type      = string\n  sensitive = true\n}"
		if p.Type != "string" || !strings.Contains(code, want) || !strings.Contains(code, "content              = var."+p.Name) {
			t.Errorf("param %+v: the code lacks its variable or its use", p)
		}
	}
}

func TestBulkHCLRefusesASecretImportID(t *testing.T) {
	ss := schemas(t)
	password := types.Name{Provider: "random", Type: "Password"}
	r, src := resourceOf(t, ss, password)
	const pw = "Zq8!vT2#pLm9-secret"
	reader := &fakeReader{objects: map[string]importer.Found{
		"random.Password " + pw: {Schema: r, Source: src, State: value.ObjectVal(map[string]value.Value{
			"id": value.StringVal("none"), "result": value.StringVal(pw).Secret(), "length": value.IntVal(int64(len(pw))),
			"lower": value.BoolVal(true), "upper": value.BoolVal(true), "numeric": value.BoolVal(true), "special": value.BoolVal(true),
			"min_lower": value.IntVal(0), "min_upper": value.IntVal(0), "min_numeric": value.IntVal(0), "min_special": value.IntVal(0),
		})},
	}}
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{
		Selections: []importer.Selection{{Type: password, IDs: []string{pw}}},
		Reader:     reader, File: "imported.tf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed() || len(res.Code) != 0 || len(res.Problems) != 1 || !strings.Contains(res.Problems[0].Reason, "OpenTofu refuses a sensitive import id") {
		t.Fatalf("problems %+v, code %q; want the password refused and nothing written", res.Problems, res.Code)
	}
	if strings.Contains(res.Problems[0].Reason, pw) || strings.Contains(res.Problems[0].From, pw) {
		t.Errorf("the problem names the secret id")
	}
}

// emitHCL runs Bulk with OpenTofu output on the objects r holds.
func emitHCL(t *testing.T, r *fakeReader, sels ...importer.Selection) (*importer.BulkResult, string) {
	t.Helper()
	res, err := importer.Bulk(context.Background(), importer.BulkOptions{Selections: sels, Reader: r, File: "imported.tf"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() {
		t.Fatalf("problems %+v", res.Problems)
	}
	return res, hclCode(t, res)
}

// An ARN in a slot that the annotation layer types as an ARN of an IAM role
// refers to the role the run read, and one no object holds stays a literal.
// The schemas are attributes of hashicorp/aws 6.70.0.
func TestBulkHCLInfersTypedReferences(t *testing.T) {
	attr := func(name string, required, optional, computed bool) schema.Attribute {
		return schema.Attribute{Name: name, Type: types.TString, Required: required, Optional: optional, Computed: computed}
	}
	p, err := schema.NewProvider(schema.Source{Namespace: "hashicorp", Type: "aws"}, "6.70.0", schema.Block{}, []schema.RawResource{
		{Wire: "aws_iam_role", Block: schema.Block{Attributes: []schema.Attribute{
			attr("arn", false, false, true), attr("assume_role_policy", true, false, false), attr("id", false, true, true), attr("name", false, true, true),
		}}},
		{Wire: "aws_ecs_task_definition", Block: schema.Block{Attributes: []schema.Attribute{
			attr("arn", false, false, true), attr("container_definitions", true, false, false), attr("execution_role_arn", false, true, false),
			attr("family", true, false, false), attr("id", false, true, true), attr("task_role_arn", false, true, false),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := func(wire string, fields map[string]string) importer.Found {
		r, err := p.ResourceByWire(wire)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]value.Value{}
		for _, a := range r.Block.Attributes {
			m[a.Name] = value.NullVal()
		}
		for k, v := range fields {
			m[k] = value.StringVal(v)
		}
		return importer.Found{Schema: r, Source: p.Source, State: value.ObjectVal(m)}
	}
	const execArn = "arn:aws:iam::123456789012:role/app-exec"
	role, task := types.Name{Provider: "aws", Type: "IamRole"}, types.Name{Provider: "aws", Type: "EcsTaskDefinition"}
	r := &fakeReader{objects: map[string]importer.Found{
		role.String() + " app-exec": found("aws_iam_role", map[string]string{"arn": execArn, "assume_role_policy": "{}", "id": "app-exec", "name": "app-exec"}),
		task.String() + " app": found("aws_ecs_task_definition", map[string]string{
			"arn": "arn:aws:ecs:us-east-1:123456789012:task-definition/app:1", "container_definitions": "[]", "family": "app", "id": "app",
			"execution_role_arn": execArn, "task_role_arn": "arn:aws:iam::123456789012:role/not-imported",
		}),
	}}
	res, code := emitHCL(t, r, importer.Selection{Type: task, IDs: []string{"app"}}, importer.Selection{Type: role, IDs: []string{"app-exec"}})
	for _, want := range []string{
		"execution_role_arn    = aws_iam_role.aws_iam_role_app_exec.arn\n",
		`task_role_arn         = "arn:aws:iam::123456789012:role/not-imported"`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the code lacks %s:\n%s", want, code)
		}
	}
	if res.Linked != 1 || res.Literals != 1 {
		t.Errorf("linked %d, literals %d; want 1 and 1", res.Linked, res.Literals)
	}
}

// thing is a type of a stand-in provider: a name, a size a runtime owns,
// a sensitive map and list, and a list of rule blocks.
var thing = types.Name{Provider: "x", Type: "Thing"}

func thingObj(name string, size int64, port int64, data map[string]value.Value) importer.Found {
	res := &schema.Resource{Name: thing, Wire: "x_thing", Scaled: []string{"size"}, Block: schema.Block{
		Attributes: []schema.Attribute{
			{Name: "id", Type: types.TString, Computed: true},
			{Name: "name", Type: types.TString, Required: true},
			{Name: "size", Type: types.TInt, Optional: true},
			{Name: "data", Type: types.MapOf(types.TString), Optional: true, Sensitive: true},
			{Name: "tokens", Type: types.ListOf(types.TString), Optional: true, Sensitive: true},
		},
		Blocks: []schema.NestedBlock{{Name: "rule", Nesting: schema.NestList, Block: schema.Block{Attributes: []schema.Attribute{{Name: "port", Type: types.TInt, Required: true}}}}},
	}}
	fields := map[string]value.Value{
		"id": value.StringVal(name), "name": value.StringVal(name), "size": value.IntVal(size), "data": value.NullVal(), "tokens": value.NullVal(),
		"rule": value.ListVal([]value.Value{value.ObjectVal(map[string]value.Value{"port": value.IntVal(port)})}),
	}
	if data != nil {
		fields["data"] = value.ObjectVal(data).Secret()
		fields["tokens"] = value.ListVal([]value.Value{value.StringVal("u20-secret-list-item")}).Secret()
	}
	return importer.Found{Schema: res, Source: schema.Source{Namespace: "x", Type: "x"}, State: value.ObjectVal(fields)}
}

func TestBulkHCLKeysByNameAndIgnoresWhatARuntimeOwns(t *testing.T) {
	r := &fakeReader{objects: map[string]importer.Found{
		"x.Thing id-a": thingObj("alpha", 2, 80, nil),
		"x.Thing id-b": thingObj("beta", 3, 80, nil),
	}}
	res, code := emitHCL(t, r, importer.Selection{Type: thing, IDs: []string{"id-b", "id-a"}})
	for _, want := range []string{
		"  for_each = {\n    \"alpha\" = {\n      size = 2\n    }\n    \"beta\" = {\n      size = 3\n    }\n  }\n",
		"  name = each.key\n  size = each.value.size\n  rule {\n    port = 80\n  }\n  lifecycle {\n    ignore_changes = [size]\n  }\n",
		"for_each = {\n    \"alpha\" = \"id-a\"\n    \"beta\"  = \"id-b\"\n  }\n  to = x_thing.x_thing[each.key]\n  id = each.value\n",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("the code lacks\n%s\nin\n%s", want, code)
		}
	}
	if len(res.Problems) != 2 || !strings.Contains(res.Problems[0].Reason, "lifecycle.ignore_changes") {
		t.Errorf("problems %+v, want a note on size for each object", res.Problems)
	}
}

func TestBulkHCLDifferentBlocksDoNotShareAResource(t *testing.T) {
	r := &fakeReader{objects: map[string]importer.Found{
		"x.Thing a": thingObj("alpha", 2, 80, nil),
		"x.Thing b": thingObj("beta", 2, 443, nil),
	}}
	res, code := emitHCL(t, r, importer.Selection{Type: thing, IDs: []string{"a", "b"}})
	if res.Groups != 0 || res.Singles != 2 || !strings.Contains(code, "port = 80") || !strings.Contains(code, "port = 443") {
		t.Errorf("groups %d, singles %d; want two resources, each with its rule:\n%s", res.Groups, res.Singles, code)
	}
}

func TestBulkHCLSecretContainerIsOneVariable(t *testing.T) {
	const key, elem = "u20-secret-map-key", "u20-secret-map-value"
	r := &fakeReader{objects: map[string]importer.Found{
		"x.Thing a": thingObj("alpha", 2, 80, map[string]value.Value{key: value.StringVal(elem)}),
	}}
	res, code := emitHCL(t, r, importer.Selection{Type: thing, IDs: []string{"a"}})
	if strings.Contains(code+fmt.Sprintf("%+v", res.Params), key) || strings.Contains(code, elem) || strings.Contains(code, "u20-secret-list-item") {
		t.Errorf("the code or a variable holds a secret:\n%s", code)
	}
	want := map[string]string{"data": "map(string)", "tokens": "list(string)"}
	for _, p := range res.Params {
		if p.Type != want[p.Attr] || !strings.Contains(code, "variable \""+p.Name+"\" {\n  type      = "+p.Type+"\n  sensitive = true\n}") || !strings.Contains(code, "= var."+p.Name+"\n") {
			t.Errorf("variable %+v: want type %s, declared and used", p, want[p.Attr])
		}
	}
	if len(res.Params) != 2 {
		t.Errorf("variables %+v, want one for data and one for tokens", res.Params)
	}
}

// OpenTofu keeps every string of its code in Unicode normal form C, so a
// string in another form does not reach the provider as it was read: an
// import id of "e" and a combining acute imports the precomposed "é", and
// tofu plans a replacement of the object. Bulk refuses such an object and
// writes nothing.
func TestBulkHCLRefusesStringsOutsideNFC(t *testing.T) {
	ss := schemas(t)
	const decomposed, composed = "e\u0301", "\u00e9"
	keepers := func(id string, keepers map[string]value.Value) importer.Found {
		f := stringObj(t, ss, id)
		f.State = withAttr(f.State, "keepers", value.ObjectVal(keepers))
		return f
	}
	r := &fakeReader{objects: map[string]importer.Found{
		"random.String " + decomposed: stringObj(t, ss, decomposed),
		"random.String " + composed:   stringObj(t, ss, composed),
		"random.String plain":         stringObj(t, ss, "plain"),
		"random.String value":         keepers("value", map[string]value.Value{"k": value.StringVal(decomposed)}),
		"random.String key":           keepers("key", map[string]value.Value{decomposed: value.StringVal("v")}),
	}}
	for _, tc := range []struct {
		name, id, reason string
	}{
		{name: "an import id", id: decomposed, reason: "the import id is not in Unicode normal form C"},
		{name: "a value", id: "value", reason: "keepers holds a string that is not in Unicode normal form C"},
		{name: "a map key", id: "key", reason: "keepers holds a string that is not in Unicode normal form C"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := importer.Bulk(context.Background(), importer.BulkOptions{
				Selections: []importer.Selection{{Type: stringType, IDs: []string{tc.id, "plain"}}},
				Reader:     r, File: "imported.tf",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Failed() || len(res.Code) != 0 || len(res.Problems) != 1 {
				t.Fatalf("problems %+v, code:\n%s\nwant the object refused and nothing written", res.Problems, res.Code)
			}
			p := res.Problems[0]
			if p.From != "objects[0].ids[0]" || !strings.Contains(p.Reason, tc.reason) || !strings.Contains(p.Reason, "OpenTofu keeps every string of its code in NFC") {
				t.Errorf("problem %+v, want objects[0].ids[0] refused: %s", p, tc.reason)
			}
		})
	}
	t.Run("NFC passes", func(t *testing.T) {
		_, code := emitHCL(t, r, importer.Selection{Type: stringType, IDs: []string{composed, "plain"}})
		if !strings.Contains(code, `toset(["plain", "`+composed+`"])`) {
			t.Errorf("the code does not import %q:\n%s", composed, code)
		}
	})
}
