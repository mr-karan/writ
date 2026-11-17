package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fake answers every tool with the result or error it holds.
type fake struct {
	result Result
	err    error
}

func (f *fake) Plan(context.Context, PlanInput) (Result, error)       { return f.result, f.err }
func (f *fake) GetPlan(context.Context, GetPlanInput) (Result, error) { return f.result, f.err }
func (f *fake) Runs(context.Context, RunsInput) (Result, error)       { return f.result, f.err }
func (f *fake) Drift(context.Context, DriftInput) (Result, error)     { return f.result, f.err }
func (f *fake) StateIndex(context.Context, StateIndexInput) (Result, error) {
	return f.result, f.err
}
func (f *fake) Schema(context.Context, SchemaInput) (Result, error)         { return f.result, f.err }
func (f *fake) ImportBulk(context.Context, ImportBulkInput) (Result, error) { return f.result, f.err }
func (f *fake) ImportAccept(context.Context, ImportAcceptInput) (Result, error) {
	return f.result, f.err
}
func (f *fake) Policy(context.Context, PolicyInput) (Result, error) { return f.result, f.err }

// connect starts the server on tools, with import_bulk, and returns a
// client session to it.
func connect(t *testing.T, tools Tools) *sdk.ClientSession {
	t.Helper()
	return connectWith(t, tools, Options{Version: "test", AllowProviders: true})
}

func connectWith(t *testing.T, tools Tools, o Options) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	ss, err := NewServer(tools, o).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("call %s: %d content items", name, len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("call %s: content %T", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

// Every tool says whether it changes anything. Only plan and import_bulk
// do, neither destroys, and import_bulk is listed only with
// AllowProviders.
func TestToolsSayWhatTheyChange(t *testing.T) {
	annotations := func(o Options) map[string]*sdk.ToolAnnotations {
		res, err := connectWith(t, &fake{}, o).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]*sdk.ToolAnnotations{}
		for _, tool := range res.Tools {
			got[tool.Name] = tool.Annotations
		}
		return got
	}
	got := annotations(Options{AllowProviders: true})
	readOnly := []string{"get_plan", "runs", "drift", "state_index", "schema", "import_accept", "policy"}
	if len(got) != len(readOnly)+2 {
		t.Fatalf("tools %v, want %v, plan, and import_bulk", slices.Sorted(maps.Keys(got)), readOnly)
	}
	for _, name := range readOnly {
		if a := got[name]; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s is not marked read-only: %+v", name, a)
		}
	}
	for _, name := range []string{"plan", "import_bulk"} {
		if a := got[name]; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s is not marked as a change that destroys nothing: %+v", name, a)
		}
	}
	if a := got["schema"]; a == nil || a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("schema is not marked as reaching nothing outside: %+v", a)
	}
	without := annotations(Options{})
	if _, ok := without["import_bulk"]; ok || len(without) != len(got)-1 {
		t.Errorf("without AllowProviders the tools are %v", slices.Sorted(maps.Keys(without)))
	}
}

func TestResults(t *testing.T) {
	diags := make([]string, MaxDiagnostics+7)
	for i := range diags {
		diags[i] = fmt.Sprintf(`{"severity":"error","class":"type","message":"error %d"}`, i)
	}
	manyDiags := `{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[` + strings.Join(diags, ",") + `]}`
	long := strings.Repeat("a line of a long schema listing\n", MaxOutput/16)
	tests := []struct {
		name    string
		tool    string
		args    map[string]any
		fake    fake
		check   func(t *testing.T, text string)
		isError bool
	}{
		{
			name: "a document passes through compacted",
			tool: "drift", fake: fake{result: Result{Text: "{\n  \"format\": \"writ.drift/v0\",\n  \"ok\": true,\n  \"diagnostics\": []\n}\n"}},
			check: func(t *testing.T, text string) {
				if text != `{"format":"writ.drift/v0","ok":true,"diagnostics":[]}` {
					t.Errorf("text %s", text)
				}
			},
		},
		{
			name: "a refused import is an error result that carries its document",
			tool: "import_accept", args: map[string]any{"plan": "p"}, fake: fake{result: Result{Text: `{"format":"writ.import-accept/v0","ok":false}`, Failed: true}},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, `{"format":"writ.import-accept/v0"`) },
			isError: true,
		},
		{
			name: "diagnostics past the bound are dropped and counted",
			tool: "get_plan", args: map[string]any{"id": "p"}, fake: fake{result: Result{Text: manyDiags, Failed: true}},
			check: func(t *testing.T, text string) {
				var doc struct {
					Format      string            `json:"format"`
					Diagnostics []json.RawMessage `json:"diagnostics"`
					Omitted     int               `json:"omitted_diagnostics"`
				}
				if err := json.Unmarshal([]byte(text), &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Format != "writ.remote-plan/v0" || len(doc.Diagnostics) != MaxDiagnostics || doc.Omitted != 7 || !strings.Contains(string(doc.Diagnostics[0]), "error 0") {
					t.Errorf("format %s, %d diagnostics, %d omitted, first %s", doc.Format, len(doc.Diagnostics), doc.Omitted, doc.Diagnostics[0])
				}
			},
			isError: true,
		},
		{
			name: "text past the bound is cut at a line and says so",
			tool: "schema", args: map[string]any{"types": []string{"aws"}}, fake: fake{result: Result{Text: long}},
			check: func(t *testing.T, text string) {
				body, note, ok := strings.Cut(text, "\n[writ mcp cut this result at ")
				if !ok || len(text) > MaxOutput || !strings.HasSuffix(body, "listing\n") || !strings.HasPrefix(note, fmt.Sprint(len(body))) {
					t.Errorf("%d bytes, note %q", len(text), note)
				}
			},
		},
		{
			name: "a long error is bounded too",
			tool: "plan", fake: fake{err: errors.New(strings.Repeat("no such type x\n", MaxOutput/8))},
			check: func(t *testing.T, text string) {
				if len(text) > MaxOutput || !strings.Contains(text, "\n[writ mcp cut this result at ") {
					t.Errorf("%d bytes, ends %q", len(text), text[len(text)-100:])
				}
			},
			isError: true,
		},
		{
			name: "arguments that do not validate get a bounded answer",
			tool: "plan", args: map[string]any{strings.Repeat("x", MaxOutput+100): true},
			check: func(t *testing.T, text string) {
				if len(text) > MaxOutput || !strings.Contains(text, "validating") {
					t.Errorf("%d bytes, starts %.100q", len(text), text)
				}
			},
			isError: true,
		},
		{
			name: "a tool that cannot run says why",
			tool: "plan", fake: fake{err: errors.New("no control plane: start one with writ serve --dev")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "no control plane") },
			isError: true,
		},
		{
			name: "a plan document is bounded too, and keeps its key order",
			tool: "plan", args: map[string]any{"json": true}, fake: fake{result: Result{Text: manyDiags}},
			check: func(t *testing.T, text string) {
				wantPrefix(t, text, `{"format":"writ.remote-plan/v0","ok":false,"diagnostics":[`)
				if !strings.HasSuffix(text, `],"omitted_diagnostics":7}`) {
					t.Errorf("text ends %s", text[len(text)-60:])
				}
			},
			isError: false,
		},
		{
			name:    "schema needs a type",
			tool:    "schema",
			args:    map[string]any{"types": []string{}},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "types takes 1 to 20 types, got 0") },
			isError: true,
		},
		{
			name:    "schema takes a bounded number of types",
			tool:    "schema",
			args:    map[string]any{"types": strings.Split(strings.Repeat("random_pet,", MaxSchemaTypes)+"random", ",")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "types takes 1 to 20 types, got 21") },
			isError: true,
		},
		{
			name:    "schema takes a bounded number of field patterns",
			tool:    "schema",
			args:    map[string]any{"types": []string{"random_pet"}, "fields": strings.Split(strings.Repeat("a,", MaxSchemaFields)+"b", ",")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "fields takes at most 50 patterns, got 51") },
			isError: true,
		},
		{
			name:    "get_plan needs an id",
			tool:    "get_plan",
			args:    map[string]any{"id": ""},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "id is required") },
			isError: true,
		},
		{
			name:    "get_plan keeps a bounded number of addresses",
			tool:    "get_plan",
			args:    map[string]any{"id": "p", "addresses": strings.Split(strings.Repeat("a.b,", MaxAddresses)+"c.d", ",")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "addresses takes at most 50 addresses, got 51") },
			isError: true,
		},
		{
			name:    "runs lists a bounded number",
			tool:    "runs",
			args:    map[string]any{"limit": MaxListed + 1},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "limit takes 1 to 200, got 201") },
			isError: true,
		},
		{
			name:    "drift lists a bounded number",
			tool:    "drift",
			args:    map[string]any{"limit": -1},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "limit takes 1 to 200, got -1") },
			isError: true,
		},
		{
			name:    "state_index needs a stack",
			tool:    "state_index",
			args:    map[string]any{"stack": ""},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "stack is required") },
			isError: true,
		},
		{
			name:    "state_index keeps a bounded number of types",
			tool:    "state_index",
			args:    map[string]any{"stack": "app", "types": strings.Split(strings.Repeat("t,", MaxIndexTypes)+"u", ",")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "types takes at most 20 types, got 21") },
			isError: true,
		},
		{
			name:    "import_bulk needs a type",
			tool:    "import_bulk",
			args:    map[string]any{"type": "", "ids": []string{"a"}},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "type is required") },
			isError: true,
		},
		{
			name:    "import_bulk reads a bounded number of ids",
			tool:    "import_bulk",
			args:    map[string]any{"type": "random_uuid", "ids": strings.Split(strings.Repeat("x,", MaxImportIDs)+"y", ",")},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "ids takes 1 to 100 import ids, got 101") },
			isError: true,
		},
		{
			name:    "import_accept needs a plan",
			tool:    "import_accept",
			args:    map[string]any{"plan": ""},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "plan is required") },
			isError: true,
		},
		{
			name:    "policy needs a set to read one file",
			tool:    "policy",
			args:    map[string]any{"file": "tls.rego"},
			check:   func(t *testing.T, text string) { wantPrefix(t, text, "file needs set") },
			isError: true,
		},
		{
			name: "a refused plan too long for a result keeps its policy decision",
			tool: "get_plan", args: map[string]any{"id": "p", "json": true},
			fake: fake{result: Result{Failed: true, Text: `{"format":"writ.remote-plan/v0","ok":false,"plan":{"id":"p","status":"refused"},"document":{"resource_changes":[` +
				strings.TrimSuffix(strings.Repeat(`{"address":"random_pet.p","change":{"actions":["create"]}},`, MaxOutput/40), ",") +
				`]},"policy":{"format":"writ.policy-decision/v1","point":"plan","outcome":"deny","results":[{"set":"base","level":"mandatory","rule":"deny_rsa","outcome":"deny","message":"tls_private_key.k: an RSA key needs 4096 bits"}],"at":"2026-11-16T00:00:00Z","input":"ab"},"diagnostics":[]}`}},
			check: func(t *testing.T, text string) {
				wantPrefix(t, text, `{"format":"writ.remote-plan/v0","ok":false,"policy":{"format":"writ.policy-decision/v1","point":"plan","outcome":"deny","results":[{"set":"base","level":"mandatory","rule":"deny_rsa","outcome":"deny","message":"tls_private_key.k: an RSA key needs 4096 bits"}],"omitted_results":0},"truncated":true,`)
			},
			isError: true,
		},
		{
			name: "an import document passes through",
			tool: "import_accept", args: map[string]any{"plan": "p"}, fake: fake{result: Result{Text: `{"format":"writ.import-accept/v0","ok":true}`}},
			check: func(t *testing.T, text string) { wantPrefix(t, text, `{"format":"writ.import-accept/v0","ok":true}`) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, isError := call(t, connect(t, &tt.fake), tt.tool, tt.args)
			if isError != tt.isError {
				t.Errorf("isError %v, want %v: %.200s", isError, tt.isError, text)
			}
			tt.check(t, text)
		})
	}
}

func wantPrefix(t *testing.T, text, prefix string) {
	t.Helper()
	if !strings.HasPrefix(text, prefix) {
		t.Errorf("text %.300s\nwant it to start with %s", text, prefix)
	}
}
