package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/mcp"
	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/tofu"
)

// serveMCP runs App.MCP on o over pipes, as writ mcp runs it on stdio, and
// returns a client session to it.
func serveMCP(t *testing.T, o Options) *sdk.ClientSession {
	t.Helper()
	return serveMCPWith(t, o, MCPOptions{})
}

// serveMCPWith is serveMCP with writ mcp's flags.
func serveMCPWith(t *testing.T, o Options, mo MCPOptions) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	o.Stdin, o.Stdout = toServer, fromServer
	done := make(chan error, 1)
	go func() {
		done <- New(o).MCP(ctx, mo)
		fromServer.Close()
	}()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: toClient, Writer: fromClient}, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		fromClient.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("writ mcp did not stop after its client closed")
		}
	})
	return cs
}

// callTool calls a tool and returns its one text and whether it is an
// error result.
func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("call %s: %d content items", name, len(res.Content))
	}
	return res.Content[0].(*sdk.TextContent).Text, res.IsError
}

func noEnv(string) (string, bool) { return "", false }

// tofuLockText is the lock of internal/tofu's gate: random 3.9.1, local
// 2.9.1, and tls 4.4.2.
func tofuLockText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "tofu", "testdata", "lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tofuStackAt writes an OpenTofu stack with the gate's lock into dir.
func tofuStackAt(t *testing.T, dir, lock string) {
	t.Helper()
	write(t, filepath.Join(dir, "main.tf"), "resource \"random_pet\" \"name\" {}\n")
	write(t, filepath.Join(dir, tofu.LockFile), lock)
}

// The schema tool returns the types of the providers the stack's lock
// pins, in the shape tofu providers schema -json gives, from Writ's cache.
func TestMCPSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: installs the real random, local, and tls providers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	lock := tofuLockText(t)
	root := t.TempDir()
	tofuStackAt(t, filepath.Join(root, "app"), lock)
	locked, err := tofu.LockedProviders([]byte(lock))
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := importer.TofuRequirements(filepath.Join(root, "app"), locked)
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := provider.DefaultDirs()
	if err != nil {
		t.Fatal(err)
	}
	// The lock's zh: hashes pin the archives, so a warm cache needs no
	// registry, as in OpenTofuStack.
	opts := provider.InstallOptions{Dirs: dirs, Offline: true}
	if _, cached := provider.Install(ctx, opts, reqs); cached != nil {
		opts.Offline = false
		if _, err := provider.Install(ctx, opts, reqs); err != nil {
			t.Fatal(errors.Join(cached, err))
		}
	}
	cs := serveMCP(t, Options{Dir: root, CacheDir: dirs.Cache, LookupEnv: noEnv})
	schemaDoc := func(t *testing.T, args map[string]any) (providerSchemaReport, string, bool) {
		t.Helper()
		args["dir"] = "app"
		text, isError := callTool(t, cs, "schema", args)
		var rep providerSchemaReport
		if err := json.Unmarshal([]byte(text), &rep); err != nil || rep.Format != "writ.provider-schema/v0" {
			t.Fatalf("not a provider schema document: %v\n%s", err, text)
		}
		return rep, text, isError
	}
	names := func(ts schemaTypeInfo) string {
		var n []string
		for name := range ts.Schema.Block.Attributes {
			n = append(n, name)
		}
		for name := range ts.Schema.Block.BlockTypes {
			n = append(n, name+"{}")
		}
		slices.Sort(n)
		return strings.Join(n, " ")
	}

	t.Run("two types, with cty types and marks", func(t *testing.T) {
		rep, text, isError := schemaDoc(t, map[string]any{"types": []string{"random_pet", "tls_private_key"}})
		if isError || !rep.OK || len(rep.Types) != 2 || rep.Types[0].Provider != "registry.opentofu.org/hashicorp/random" || rep.Types[0].Kind != "resource" {
			t.Fatalf("isError %v: %s", isError, text)
		}
		for _, want := range []string{`"length":{"type":"number","optional":true,"computed":true}`, `"keepers":{"type":["map","string"],"optional":true}`, `"private_key_pem":{"type":"string","computed":true,"sensitive":true}`} {
			if !strings.Contains(text, want) {
				t.Errorf("schema lacks %s:\n%s", want, text)
			}
		}
		if len(rep.Providers) != 3 {
			t.Errorf("providers %+v, want the three the lock pins", rep.Providers)
		}
	})
	t.Run("fields by name and glob", func(t *testing.T) {
		rep, text, _ := schemaDoc(t, map[string]any{"types": []string{"random_pet"}, "fields": []string{"length", "pre*"}})
		if got := names(rep.Types[0]); got != "length prefix" || len(text) > 2<<10 {
			t.Fatalf("fields %s, %d bytes:\n%s", got, len(text), text)
		}
	})
	t.Run("required only", func(t *testing.T) {
		rep, text, _ := schemaDoc(t, map[string]any{"types": []string{"local_file"}, "required_only": true})
		if got := names(rep.Types[0]); got != "filename" {
			t.Fatalf("required fields %s:\n%s", got, text)
		}
	})
	t.Run("a data source", func(t *testing.T) {
		rep, text, isError := schemaDoc(t, map[string]any{"types": []string{"data.local_file"}, "fields": []string{"filename", "content"}})
		if isError || len(rep.Types) != 1 || rep.Types[0].Kind != "data_source" || rep.Types[0].Type != "data.local_file" || names(rep.Types[0]) != "content filename" {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("a provider's name lists its types", func(t *testing.T) {
		rep, text, isError := schemaDoc(t, map[string]any{"types": []string{"local"}})
		if isError || len(rep.Lists) != 1 || !slices.Contains(rep.Lists[0].Resources, "local_sensitive_file") || !slices.Contains(rep.Lists[0].DataSources, "local_file") {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("a type no locked provider has", func(t *testing.T) {
		rep, text, isError := schemaDoc(t, map[string]any{"types": []string{"random_pet", "aws_vpc"}})
		if !isError || rep.OK || len(rep.Types) != 1 || len(rep.Diagnostics) != 1 || !strings.Contains(rep.Diagnostics[0].Message, "no provider that .terraform.lock.hcl pins has the resource type aws_vpc; the providers are local (") {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("a bad pattern", func(t *testing.T) {
		text, isError := callTool(t, cs, "schema", map[string]any{"dir": "app", "types": []string{"random_pet"}, "fields": []string{"[x"}})
		if !isError || !strings.Contains(text, `field pattern "[x"`) {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
}

// The schema tool reads only Writ's cache: a provider it lacks is an
// error that names the fix, and nothing is downloaded.
func TestMCPSchemaReadsOnlyTheCache(t *testing.T) {
	root := t.TempDir()
	tofuStackAt(t, root, tofuLockText(t))
	cache := t.TempDir()
	cs := serveMCP(t, Options{Dir: root, CacheDir: cache, LookupEnv: noEnv})
	text, isError := callTool(t, cs, "schema", map[string]any{"types": []string{"random_pet"}})
	if !isError || !strings.Contains(text, "is not in the provider cache") || !strings.Contains(text, "--allow-providers") {
		t.Fatalf("isError %v: %s", isError, text)
	}
	if ents, _ := os.ReadDir(filepath.Join(cache, "providers")); len(ents) != 0 {
		t.Fatalf("the schema tool installed into the cache: %v", ents)
	}
}

// stubPlane serves documents as writ serve does: a masked plan, a state
// index, and refusals, and records each request's path and query.
type stubPlane struct {
	t    *testing.T
	docs map[string]string
	seen []string
}

func (s *stubPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.seen = append(s.seen, r.URL.RequestURI())
	w.Header().Set("Content-Type", "application/json")
	if doc, ok := s.docs[r.URL.Path]; ok {
		w.Write([]byte(doc))
		return
	}
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `{"format":"writ.error/v0","ok":false,"diagnostics":[{"severity":"error","class":"usage","message":"no %s"}]}`, r.URL.Path)
}

// stubOptions points writ mcp at the stub with a CI token, as WRIT_SERVER
// and WRIT_ID_TOKEN do.
func stubOptions(root, url string) Options {
	return Options{Dir: root, LookupEnv: func(name string) (string, bool) {
		switch name {
		case "WRIT_SERVER":
			return url, true
		case "WRIT_ID_TOKEN":
			return "stub-token", true
		}
		return "", false
	}}
}

// The remote tools read the control plane's routes and pass on what it
// serves, a refusal as a failed result, and narrow a masked plan or an
// index to what the caller asked for. import_accept judges the masked
// plan the control plane serves.
func TestMCPReadsTheControlPlane(t *testing.T) {
	masked, err := mask.New(nil).Plan([]byte(acceptRaw))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: &server.PlanInfo{ID: "P1", Status: "pending", Env: "prod"}, Document: doc, Text: "OpenTofu 1.13.9 plan of app@prod\n"})
	if err != nil {
		t.Fatal(err)
	}
	index, err := json.Marshal(server.StateIndexDoc{Format: server.FormatStateIndex, OK: true, Stack: "app", Env: "prod", Diagnostics: []server.Diagnostic{}, Resources: []server.IndexResource{
		{Address: "random_pet.name", Type: "random_pet", ID: "calm-cat"}, {Address: "aws_vpc.main", Type: "aws_vpc", ID: "vpc-1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubPlane{t: t, docs: map[string]string{
		"/v1/plans/P1": string(plan),
		"/v1/index":    string(index),
		"/v1/drift":    `{"format":"writ.drift/v0","ok":true,"stacks":[],"more":false,"diagnostics":[]}`,
		"/v1/applies":  `{"format":"writ.remote-runs/v0","ok":true,"runs":[],"more":false,"diagnostics":[]}`,
	}}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	root := t.TempDir()
	stack := filepath.Join(root, "app")
	write(t, filepath.Join(stack, "imported.tf"), "")
	imports := func(name string, ids ...string) {
		var imps []importer.Import
		for _, id := range ids {
			imps = append(imps, importer.Import{Type: "random_uuid", Name: "u", Key: &id, ID: id})
		}
		text, err := importer.EncodeImports(imps)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(stack, name), string(text))
	}
	imports("imported.imports.json", "a", "b")
	imports("more.imports.json", "a", "b", "c")
	cs := serveMCP(t, stubOptions(root, srv.URL))

	t.Run("get_plan keeps the addresses asked for", func(t *testing.T) {
		text, isError := callTool(t, cs, "get_plan", map[string]any{"id": "P1", "addresses": []string{`random_uuid.u["b"]`}})
		var got server.RemotePlan
		if isError || json.Unmarshal([]byte(text), &got) != nil || got.Plan.ID != "P1" || got.Text != "" {
			t.Fatalf("isError %v: %s", isError, text)
		}
		var m mask.Plan
		if err := json.Unmarshal(got.Document, &m); err != nil || len(m.ResourceChanges) != 1 || m.ResourceChanges[0].Address != `random_uuid.u["b"]` || m.FormatVersion != "1.2" {
			t.Fatalf("document %v: %s", err, got.Document)
		}
	})
	t.Run("get_plan as text", func(t *testing.T) {
		text, isError := callTool(t, cs, "get_plan", map[string]any{"id": "P1"})
		if isError || !strings.HasPrefix(text, "OpenTofu 1.13.9 plan of app@prod\nPlan P1 for prod") {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("state_index keeps the types asked for", func(t *testing.T) {
		text, isError := callTool(t, cs, "state_index", map[string]any{"stack": "app", "env": "prod", "types": []string{"aws_vpc"}})
		var got server.StateIndexDoc
		if isError || json.Unmarshal([]byte(text), &got) != nil || len(got.Resources) != 1 || got.Resources[0].ID != "vpc-1" {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("drift and runs pass through", func(t *testing.T) {
		if text, isError := callTool(t, cs, "drift", map[string]any{"env": "prod", "stack": "app", "limit": 5}); isError || !strings.HasPrefix(text, `{"format":"writ.drift/v0"`) {
			t.Fatalf("drift: isError %v: %s", isError, text)
		}
		if text, isError := callTool(t, cs, "runs", map[string]any{"env": "prod"}); isError || !strings.HasPrefix(text, `{"format":"writ.remote-runs/v0"`) {
			t.Fatalf("runs: isError %v: %s", isError, text)
		}
	})
	t.Run("a refusal carries the control plane's document", func(t *testing.T) {
		text, isError := callTool(t, cs, "runs", map[string]any{"id": "AAAAAAAAAAAAAAAAAAAAAAAAAA"})
		if !isError || !strings.Contains(text, `"message":"no /v1/runs/AAAAAAAAAAAAAAAAAAAAAAAAAA"`) {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("a team's control plane needs env", func(t *testing.T) {
		text, isError := callTool(t, cs, "state_index", map[string]any{"stack": "app"})
		if !isError || !strings.Contains(text, "env is required") {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("import_accept passes a plan that only imports the objects", func(t *testing.T) {
		text, isError := callTool(t, cs, "import_accept", map[string]any{"dir": "app", "plan": "P1"})
		var rep importAcceptReport
		if isError || json.Unmarshal([]byte(text), &rep) != nil || !rep.OK || rep.Imports != 2 || rep.Objects != 2 {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("import_accept refuses a plan that misses an object", func(t *testing.T) {
		text, isError := callTool(t, cs, "import_accept", map[string]any{"dir": "app", "imports": "more.imports.json", "plan": "P1"})
		var rep importAcceptReport
		if !isError || json.Unmarshal([]byte(text), &rep) != nil || rep.OK || len(rep.Refusals) != 1 {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	want := []string{
		"/v1/plans/P1", "/v1/plans/P1", "/v1/index?env=prod&stack=app", "/v1/drift?env=prod&limit=5&stack=app",
		"/v1/applies?env=prod&limit=20", "/v1/runs/AAAAAAAAAAAAAAAAAAAAAAAAAA", "/v1/plans/P1", "/v1/plans/P1",
	}
	if !slices.Equal(stub.seen, want) {
		t.Errorf("requests\n%q\nwant\n%q", stub.seen, want)
	}
}

// The control plane's plan hides an import id that is a secret, and the
// imports file still holds it. import_accept's refusals name the address
// and what differs, and quote no id of either.
func TestMCPImportAcceptQuotesNoID(t *testing.T) {
	const secret, missing = "SYNTHETIC_SECRET_IMPORT_ID", "SYNTHETIC_MISSING_IMPORT_ID"
	raw := `{"format_version":"1.2","resource_changes":[{"address":"random_uuid.p","mode":"managed","type":"random_uuid","name":"p","provider_name":"registry.opentofu.org/hashicorp/random","change":{"actions":["no-op"],"before":null,"after":{},"after_unknown":{},"before_sensitive":false,"after_sensitive":{},"importing":{"id":"` + secret + `"}}}]}`
	masked, err := mask.New([]string{secret}).Plan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: &server.PlanInfo{ID: "P1", Status: "pending", Env: "prod"}, Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan), secret) {
		t.Fatal("the control plane's plan holds the secret")
	}
	srv := httptest.NewServer(&stubPlane{t: t, docs: map[string]string{"/v1/plans/P1": string(plan)}})
	t.Cleanup(srv.Close)
	root := t.TempDir()
	imports, err := importer.EncodeImports([]importer.Import{{Type: "random_uuid", Name: "p", ID: secret}, {Type: "random_uuid", Name: "q", ID: missing}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "imported.imports.json"), string(imports))
	text, isError := callTool(t, serveMCP(t, stubOptions(root, srv.URL)), "import_accept", map[string]any{"plan": "P1"})
	var rep importAcceptReport
	if !isError || json.Unmarshal([]byte(text), &rep) != nil || rep.OK {
		t.Fatalf("isError %v: %s", isError, text)
	}
	if strings.Contains(text, secret) || strings.Contains(text, missing) {
		t.Fatalf("the result quotes an import id: %s", text)
	}
	want := []jsonAcceptRefusal{
		{Address: "random_uuid.p", Reason: "its import id differs from the one the imports file selected"},
		{Address: "random_uuid.q", Reason: "the imports file selected an object at this address, and the plan does not import it"},
	}
	if !slices.Equal(rep.Refusals, want) {
		t.Fatalf("refusals %+v, want %+v", rep.Refusals, want)
	}
}

// The plan tool plans an OpenTofu stack only: a directory of .writ files,
// which needed writ init, is refused before any control plane is asked.
func TestMCPPlanRefusesAWritStack(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "old", "main.writ"), "let x = 1\n")
	cs := serveMCP(t, Options{Dir: root, LookupEnv: noEnv})
	text, isError := callTool(t, cs, "plan", map[string]any{"dir": "old"})
	if !isError || !strings.Contains(text, "holds no OpenTofu stack") {
		t.Fatalf("isError %v: %s", isError, text)
	}
}

// A file a tool reads under --dir, through a link or .., stays inside it:
// the lock the schema tool reads, the imports file import_accept reads,
// and the stack directory. Read, the sentinel would show in the error.
func TestMCPReadsNothingOutsideDir(t *testing.T) {
	const sentinel = "OUTSIDE_SENTINEL"
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	write(t, filepath.Join(outside, "lock.hcl"), sentinel+"\n")
	write(t, filepath.Join(outside, "x.imports.json"), sentinel+"\n")
	write(t, filepath.Join(outside, "main.tf"), sentinel+"\n")

	write(t, filepath.Join(root, "linked", "main.tf"), "")
	symlink(t, filepath.Join(outside, "lock.hcl"), filepath.Join(root, "linked", tofu.LockFile))
	symlink(t, filepath.Join(outside, "x.imports.json"), filepath.Join(root, "linked", "x.imports.json"))
	write(t, filepath.Join(root, "relinked", "main.tf"), "")
	symlink(t, filepath.Join("..", "..", "outside", "lock.hcl"), filepath.Join(root, "relinked", tofu.LockFile))
	symlink(t, filepath.Join("..", "..", "outside", "x.imports.json"), filepath.Join(root, "relinked", "x.imports.json"))
	write(t, filepath.Join(root, "plain", "main.tf"), "")

	cs := serveMCP(t, Options{Dir: root, LookupEnv: noEnv})
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{tool: "schema", args: map[string]any{"dir": "linked", "types": []string{"random"}}, want: "outside the directory writ may reach"},
		{tool: "schema", args: map[string]any{"dir": "relinked", "types": []string{"random"}}, want: "outside the directory writ may reach"},
		{tool: "schema", args: map[string]any{"dir": "../outside", "types": []string{"random"}}, want: "outside the directory writ mcp serves"},
		{tool: "import_accept", args: map[string]any{"dir": "linked", "imports": "x.imports.json", "plan": "P1"}, want: "outside the directory writ mcp serves"},
		{tool: "import_accept", args: map[string]any{"dir": "relinked", "imports": "x.imports.json", "plan": "P1"}, want: "outside the directory writ mcp serves"},
		{tool: "import_accept", args: map[string]any{"dir": "plain", "imports": "../../outside/x.imports.json", "plan": "P1"}, want: "want the name of the file in the stack directory"},
		{tool: "plan", args: map[string]any{"dir": "../outside"}, want: "outside the directory writ mcp serves"},
		{tool: "plan", args: map[string]any{"dir": "plain", "env": "x/../y"}, want: "is not an environment name"},
		{tool: "state_index", args: map[string]any{"stack": "app", "env": "x/../../outside"}, want: "is not an environment name"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.tool, tt.args), func(t *testing.T) {
			text, isError := callTool(t, cs, tt.tool, tt.args)
			if !isError || !strings.Contains(text, tt.want) || strings.Contains(text, sentinel) {
				t.Fatalf("isError %v, want an error with %q and no %s:\n%s", isError, tt.want, sentinel, text)
			}
		})
	}
}

// A writ.estate above --dir is not read: plan does not look there for a
// control plane. Read, this file fails the tool with an error that names
// it.
func TestMCPReadsNoEstateAboveDir(t *testing.T) {
	base := t.TempDir()
	write(t, filepath.Join(base, "writ.estate"), "let OUTSIDE_SENTINEL = 1\n")
	root := filepath.Join(base, "root")
	write(t, filepath.Join(root, "app", "main.tf"), "")
	home := t.TempDir()
	cs := serveMCP(t, Options{Dir: root, LookupEnv: func(name string) (string, bool) {
		if name == "XDG_DATA_HOME" || name == "XDG_CONFIG_HOME" {
			return home, true
		}
		return "", false
	}})
	for tool, args := range map[string]map[string]any{"plan": {"dir": "app"}, "drift": {}} {
		text, isError := callTool(t, cs, tool, args)
		if !isError || !strings.HasPrefix(text, "no control plane") || strings.Contains(text, "writ.estate") {
			t.Fatalf("%s: isError %v: %s", tool, isError, text)
		}
	}
}

// A named pipe where a tool reads a file fails the tool at once, and the
// server answers the next call.
func TestMCPRefusesANamedPipe(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "app", "main.tf"), "")
	for _, name := range []string{tofu.LockFile, "imported.imports.json"} {
		if err := syscall.Mkfifo(filepath.Join(root, "app", name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cs := serveMCP(t, Options{Dir: root, LookupEnv: noEnv})
	for tool, args := range map[string]map[string]any{
		"schema":        {"dir": "app", "types": []string{"random"}},
		"import_accept": {"dir": "app", "plan": "P1"},
	} {
		done := make(chan string, 1)
		go func() {
			text, _ := callTool(t, cs, tool, args)
			done <- text
		}()
		select {
		case text := <-done:
			if !strings.Contains(text, "is a named pipe: not a regular file") {
				t.Errorf("%s: %s", tool, text)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s blocked on the named pipe", tool)
		}
	}
}

// A named pipe renamed into a stack directory's place between the check
// that dir is a directory and the read of its lock fails the call at
// once, and the server answers the next call.
func TestMCPSurvivesADirectorySwappedForAPipe(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "ok", "main.tf"), "")
	flip, aside, pipe := filepath.Join(root, "ok"), filepath.Join(root, "aside"), filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	cs := serveMCP(t, Options{Dir: root, LookupEnv: noEnv})
	stop, swapped := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				swapped <- nil
				return
			default:
			}
			for _, step := range [][2]string{{flip, aside}, {pipe, flip}, {flip, pipe}, {aside, flip}} {
				if err := os.Rename(step[0], step[1]); err != nil {
					swapped <- err
					return
				}
			}
		}
	}()
	args := map[string]any{"dir": "ok", "types": []string{"random"}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 300 {
			callTool(t, cs, "schema", args)
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("schema blocked on a named pipe swapped in for its directory")
	}
	close(stop)
	if err := <-swapped; err != nil {
		t.Fatal(err)
	}
	if text, _ := callTool(t, cs, "schema", args); !strings.Contains(text, "has no "+tofu.LockFile) {
		t.Fatalf("after the swaps: %s", text)
	}
}

// Every result is within the bounds, an error result too.
func TestMCPBoundsEveryResult(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "app", "main.tf"), "")
	cs := serveMCPWith(t, Options{Dir: root, LookupEnv: noEnv}, MCPOptions{AllowProviders: true})
	long := strings.Repeat("x", 70000)
	for _, tt := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{name: "a plan with a long env name", tool: "plan", args: map[string]any{"dir": "app", "env": long}},
		{name: "a plan with a long dir", tool: "plan", args: map[string]any{"dir": long}},
		{name: "a schema with a long dir", tool: "schema", args: map[string]any{"dir": long, "types": []string{"random"}}},
		{name: "an import with a long out", tool: "import_bulk", args: map[string]any{"dir": "app", "type": "random_uuid", "ids": []string{"a"}, "out": long + "/x.tf"}},
		{name: "an accept with a long imports file", tool: "import_accept", args: map[string]any{"dir": "app", "imports": long, "plan": "P1"}},
		{name: "a run with a long id", tool: "runs", args: map[string]any{"id": long}},
		{name: "arguments that do not validate", tool: "get_plan", args: map[string]any{long: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text, isError := callTool(t, cs, tt.tool, tt.args)
			if !isError || len(text) > mcp.MaxOutput {
				t.Fatalf("isError %v, %d bytes", isError, len(text))
			}
		})
	}
}

// A stack directory whose path follows a link and then .. is the one the
// kernel resolves: alias/../stack is nested/stack, not the top-level stack.
func TestMCPDirAfterLinkThenDotDot(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "stack", "main.tf"), "")
	write(t, filepath.Join(root, "nested", "stack", "main.tf"), "")
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("nested", "deep"), filepath.Join(root, "alias"))
	symlink(t, "alias", filepath.Join(root, "chain"))
	write(t, filepath.Join(root, "file"), "")
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cs := serveMCP(t, Options{Dir: root, LookupEnv: noEnv})
	for dir, want := range map[string]string{
		"alias/../stack":          filepath.Join(real, "nested", "stack") + " has no",
		"./alias//../stack":       filepath.Join(real, "nested", "stack") + " has no",
		"chain/./..//stack":       filepath.Join(real, "nested", "stack") + " has no",
		"stack":                   filepath.Join(real, "stack") + " has no",
		"nested/deep/../../stack": filepath.Join(real, "stack") + " has no",
		"alias/../../stack":       filepath.Join(real, "stack") + " has no",
		"alias/../../..":          "is outside the directory writ mcp serves",
		"file/../stack":           "not a directory",
	} {
		t.Run(dir, func(t *testing.T) {
			if text, _ := callTool(t, cs, "schema", map[string]any{"dir": dir, "types": []string{"random"}}); !strings.Contains(text, want) {
				t.Fatalf("want %q in %s", want, text)
			}
		})
	}
}

// --dir is the directory the kernel walks to: root/alias/.. is root/nested
// when alias links to nested/deep, given absolute or relative.
func TestMCPServesTheDirTheKernelFinds(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "main.tf"), "")
	write(t, filepath.Join(root, "nested", "main.tf"), "")
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("nested", "deep"), filepath.Join(root, "alias"))
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ cwd, dir string }{
		{dir: root + "/alias/.."},
		{cwd: root, dir: "alias/.."},
		{cwd: root, dir: "./alias//./.."},
	} {
		t.Run(strings.TrimPrefix(tt.dir, root), func(t *testing.T) {
			if tt.cwd != "" {
				t.Chdir(tt.cwd)
			}
			cs := serveMCP(t, Options{Dir: tt.dir, LookupEnv: noEnv})
			if text, _ := callTool(t, cs, "schema", map[string]any{"types": []string{"random"}}); !strings.Contains(text, filepath.Join(real, "nested")+" has no") {
				t.Fatalf("served %s, not nested: %s", root, text)
			}
		})
	}
}

// A writ mcp asks for one plan at a time: a second plan while the first
// waits on the control plane is refused at once, and the next one after
// it runs.
func TestMCPPlansOneAtATime(t *testing.T) {
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"format":"writ.remote-plan/v0","ok":false,"plan":null,"document":null,"text":"","diagnostics":[{"severity":"error","class":"usage","message":"refused by the stub"}]}`))
	}))
	t.Cleanup(srv.Close)
	cs := serveMCP(t, stubOptions(t.TempDir(), srv.URL))
	args := map[string]any{"stack": "app", "commit": strings.Repeat("a", 40), "env": "prod"}
	first := make(chan string, 1)
	go func() {
		text, _ := callTool(t, cs, "plan", args)
		first <- text
	}()
	<-arrived
	if text, isError := callTool(t, cs, "plan", args); !isError || !strings.Contains(text, "a plan from this writ mcp is still running") {
		t.Fatalf("a second plan: isError %v: %s", isError, text)
	}
	close(release)
	if text := <-first; !strings.Contains(text, "refused by the stub") {
		t.Fatalf("the first plan: %s", text)
	}
	if text, _ := callTool(t, cs, "plan", args); !strings.Contains(text, "refused by the stub") {
		t.Fatalf("a plan after the first ended: %s", text)
	}
}

// A plan's text starts with its policy decision, denies first, each
// result with its set, level, and rule, and the lines the control plane
// ended the text with are not repeated. The policy tool returns the
// control plane's sets, one set, or one file of a set.
func TestMCPReadsThePolicy(t *testing.T) {
	refused, err := json.Marshal(server.RemotePlan{Format: server.FormatRemotePlan, Plan: &server.PlanInfo{ID: "P2", Status: "refused", Env: "dev"},
		Text: "OpenTofu 1.13.9 plan of net@dev\n\n  + tls_private_key.key (create)\n\nPlan: 1 to add, 0 to change, 0 to destroy.\n\n" +
			"Policy: denied, so this plan cannot be approved or applied.\n" +
			"  warn: [advice] local_file.note: file_permission is 0777\n" +
			"  deny: [baseline] tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048\n",
		Policy: json.RawMessage(`{"format":"writ.policy-decision/v1","point":"plan","outcome":"deny","at":"2026-11-16T00:00:00Z","input":"ab","results":[` +
			`{"set":"advice","level":"advisory","rule":"warn_file_permission","outcome":"warn","message":"local_file.note: file_permission is 0777"},` +
			`{"set":"baseline","level":"mandatory","rule":"deny_rsa","outcome":"deny","message":"tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048"}]}`),
		Diagnostics: []server.Diagnostic{}})
	if err != nil {
		t.Fatal(err)
	}
	var many strings.Builder
	for i := range mcp.MaxPolicyResults + 3 {
		fmt.Fprintf(&many, `{"set":"advice","level":"advisory","rule":"warn","outcome":"warn","message":"note %d"},`, i)
	}
	warned := `{"format":"writ.remote-plan/v0","ok":true,"plan":{"id":"P3","status":"pending","env":"dev"},"text":"OpenTofu 1.13.9 plan of net@dev\n",` +
		`"policy":{"format":"writ.policy-decision/v1","point":"plan","outcome":"warn","results":[` + strings.TrimSuffix(many.String(), ",") + `]},"diagnostics":[]}`
	policyDoc := `{"format":"writ.policy/v0","ok":true,"sets":[` +
		`{"name":"baseline","level":"mandatory","files":[{"name":"files.rego","text":"package main\n"},{"name":"tls.rego","text":"package main\n\ndeny_rsa contains msg if {\n}\n"}]},` +
		`{"name":"advice","level":"advisory","files":[{"name":"advice.rego","text":"package main\n"}]}],"diagnostics":[]}`
	srv := httptest.NewServer(&stubPlane{t: t, docs: map[string]string{"/v1/plans/P2": string(refused), "/v1/plans/P3": warned, "/v1/policy": policyDoc}})
	t.Cleanup(srv.Close)
	cs := serveMCP(t, stubOptions(t.TempDir(), srv.URL))

	t.Run("a refused plan's text starts with its decision", func(t *testing.T) {
		text, isError := callTool(t, cs, "get_plan", map[string]any{"id": "P2"})
		want := "Policy: denied, so this plan cannot be approved or applied, and nothing overrides a deny.\n" +
			"  deny: [baseline, mandatory] deny_rsa: tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048\n" +
			"  warn: [advice, advisory] warn_file_permission: local_file.note: file_permission is 0777\n" +
			"\nOpenTofu 1.13.9 plan of net@dev\n\n  + tls_private_key.key (create)\n\nPlan: 1 to add, 0 to change, 0 to destroy.\nPlan P2 for dev"
		if !isError || !strings.HasPrefix(text, want) || strings.Count(text, "tls_private_key.key: an RSA key") != 1 {
			t.Fatalf("isError %v:\n%s\nwant it to start\n%s", isError, text, want)
		}
	})
	t.Run("a plan's text lists the first results and counts the rest", func(t *testing.T) {
		text, isError := callTool(t, cs, "get_plan", map[string]any{"id": "P3"})
		if isError || !strings.HasPrefix(text, "Policy: passed with warnings.\n  warn: [advice, advisory] warn: note 0\n") ||
			strings.Count(text, "] warn: note ") != mcp.MaxPolicyResults || !strings.Contains(text, "  and 3 more; get_plan with json true returns every result\n\nOpenTofu") {
			t.Fatalf("isError %v:\n%s", isError, text)
		}
	})
	t.Run("the policy tool returns every set", func(t *testing.T) {
		text, isError := callTool(t, cs, "policy", nil)
		var doc server.PolicyDoc
		if isError || json.Unmarshal([]byte(text), &doc) != nil || len(doc.Sets) != 2 || doc.Sets[0].Files[1].Text != "package main\n\ndeny_rsa contains msg if {\n}\n" {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("the policy tool returns one file of a set", func(t *testing.T) {
		text, isError := callTool(t, cs, "policy", map[string]any{"set": "baseline", "file": "tls.rego"})
		var doc server.PolicyDoc
		if isError || json.Unmarshal([]byte(text), &doc) != nil || len(doc.Sets) != 1 || doc.Sets[0].Level != "mandatory" || len(doc.Sets[0].Files) != 1 || doc.Sets[0].Files[0].Name != "tls.rego" {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
	t.Run("the policy tool names the sets when one is not there", func(t *testing.T) {
		if text, isError := callTool(t, cs, "policy", map[string]any{"set": "base"}); !isError || !strings.Contains(text, `no policy set "base"; its sets are [baseline, advice]`) {
			t.Fatalf("isError %v: %s", isError, text)
		}
		if text, isError := callTool(t, cs, "policy", map[string]any{"set": "advice", "file": "tls.rego"}); !isError || !strings.Contains(text, `policy set advice has no file "tls.rego"; its files are [advice.rego]`) {
			t.Fatalf("isError %v: %s", isError, text)
		}
	})
}
