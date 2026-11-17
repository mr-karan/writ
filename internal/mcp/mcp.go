// Package mcp serves Writ's control plane to coding agents as tools of the
// Model Context Protocol, over stdio. It owns the protocol and its SDK:
// other packages see Tools, plain Go methods over Writ's own inputs and
// outputs.
//
// No tool applies, approves, or destroys anything. Every result is
// bounded in size. A tool returns a document the control plane serves, or
// one Writ builds from such a document, so it shows a run's output only as
// the mask left it.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Bounds of a tool's input and result.
const (
	// MaxOutput is the most bytes of text one tool call returns, about 16k
	// tokens, notice included. Past it text is cut at a line, and a JSON
	// document is replaced by a short one, and either says so.
	MaxOutput = 64 << 10
	// MaxDiagnostics is the most diagnostics a result keeps, in a JSON
	// document or in text. The first ones are kept, since a fix to one
	// often clears those after it.
	MaxDiagnostics = 50
	// MaxSchemaTypes is the most types one schema call looks up.
	MaxSchemaTypes = 20
	// MaxSchemaFields is the most field patterns one schema call takes.
	MaxSchemaFields = 50
	// MaxAddresses is the most addresses one get_plan call keeps.
	MaxAddresses = 50
	// MaxIndexTypes is the most resource types one state_index call keeps.
	MaxIndexTypes = 20
	// MaxImportIDs is the most import ids one import_bulk call reads.
	MaxImportIDs = 100
	// MaxListed is the most plans, runs, or stacks one call lists, as the
	// control plane's own limit.
	MaxListed = 200
	// MaxPolicyResults is the most policy results a plan's text lists
	// before the plan, denies first.
	MaxPolicyResults = 20
)

// Tools run the calls. Each returns a document or text. An error means
// the call could not run, such as no control plane to reach or a stack
// directory outside the served one.
type Tools interface {
	Plan(context.Context, PlanInput) (Result, error)
	GetPlan(context.Context, GetPlanInput) (Result, error)
	Runs(context.Context, RunsInput) (Result, error)
	Drift(context.Context, DriftInput) (Result, error)
	StateIndex(context.Context, StateIndexInput) (Result, error)
	Schema(context.Context, SchemaInput) (Result, error)
	ImportBulk(context.Context, ImportBulkInput) (Result, error)
	ImportAccept(context.Context, ImportAcceptInput) (Result, error)
	Policy(context.Context, PolicyInput) (Result, error)
}

// Result is what a call returned.
type Result struct {
	// Text is a JSON document or text.
	Text string
	// Failed is true when the call ran and its answer is a failure, such
	// as a refused plan or a plan that does not only import. That is the
	// tool's answer, not an error of the tool.
	Failed bool
}

// PlanInput is the input of the plan tool.
type PlanInput struct {
	Dir    string `json:"dir,omitempty" jsonschema:"the OpenTofu stack directory, relative to the directory writ mcp serves; the plan is of the commit checked out there, so commit first"`
	Stack  string `json:"stack,omitempty" jsonschema:"the stack's name on the control plane, such as app; required with commit, default the stack directory's path in its repository"`
	Commit string `json:"commit,omitempty" jsonschema:"the full 40-character id of a commit to plan; then no file is read and stack is required"`
	Repo   string `json:"repo,omitempty" jsonschema:"with commit: the repository's name on the control plane"`
	Env    string `json:"env,omitempty" jsonschema:"the environment to plan in; required by a team's control plane, default dev on writ serve --dev"`
	JSON   bool   `json:"json,omitempty" jsonschema:"return the writ.remote-plan/v0 document, whose document field is the masked plan; default the plan as text, which is shorter"`
}

// GetPlanInput is the input of the get_plan tool.
type GetPlanInput struct {
	ID        string   `json:"id" jsonschema:"the plan's id, or the ref the plan tool printed while it planned"`
	JSON      bool     `json:"json,omitempty" jsonschema:"return the writ.remote-plan/v0 document, whose document field is the masked plan; default the plan as text"`
	Addresses []string `json:"addresses,omitempty" jsonschema:"return the document with only the resource changes at these addresses, such as random_pet.name; implies json"`
}

// PolicyInput is the input of the policy tool.
type PolicyInput struct {
	Set  string `json:"set,omitempty" jsonschema:"return only this policy set, as a plan's policy result names it, such as baseline; default every set"`
	File string `json:"file,omitempty" jsonschema:"with set: return only this .rego file of the set, as the set's files name it, such as tls.rego"`
}

// RunsInput is the input of the runs tool.
type RunsInput struct {
	ID    string `json:"id,omitempty" jsonschema:"one run's id: return that run with its masked apply document"`
	Env   string `json:"env,omitempty" jsonschema:"without id: the environment whose runs to list; default dev on writ serve --dev"`
	Stack string `json:"stack,omitempty" jsonschema:"without id: list only this stack's runs"`
	Limit int    `json:"limit,omitempty" jsonschema:"without id: list the newest this many runs, 1 to 200; default 20"`
}

// DriftInput is the input of the drift tool.
type DriftInput struct {
	Env   string `json:"env,omitempty" jsonschema:"list only this environment's stacks; default every environment"`
	Stack string `json:"stack,omitempty" jsonschema:"list only this stack"`
	Limit int    `json:"limit,omitempty" jsonschema:"list at most this many stacks, 1 to 200; default 50"`
}

// StateIndexInput is the input of the state_index tool.
type StateIndexInput struct {
	Stack string   `json:"stack" jsonschema:"the stack's name on the control plane, such as app"`
	Env   string   `json:"env,omitempty" jsonschema:"the environment of its state; default dev on writ serve --dev"`
	Types []string `json:"types,omitempty" jsonschema:"keep only resources of these types, such as aws_security_group; default every resource"`
}

// SchemaInput is the input of the schema tool.
type SchemaInput struct {
	Dir          string   `json:"dir,omitempty" jsonschema:"the OpenTofu stack directory, relative to the directory writ mcp serves, whose .terraform.lock.hcl pins the providers; default that directory"`
	Types        []string `json:"types" jsonschema:"resource types such as random_pet, data sources such as data.aws_vpc, or a provider's name such as random to list its types"`
	Fields       []string `json:"fields,omitempty" jsonschema:"return only the attributes and nested blocks whose names match one of these names or glob patterns, such as length or tag*; default all of them"`
	RequiredOnly bool     `json:"required_only,omitempty" jsonschema:"return only the required attributes and the nested blocks that need at least one item; default false"`
}

// ImportBulkInput is the input of the import_bulk tool.
type ImportBulkInput struct {
	Dir        string   `json:"dir,omitempty" jsonschema:"the OpenTofu stack directory, relative to the directory writ mcp serves; it needs .terraform.lock.hcl"`
	Type       string   `json:"type" jsonschema:"the resource type of the objects, such as aws_security_group"`
	IDs        []string `json:"ids" jsonschema:"the import ids of the objects to read, 1 to 100"`
	Provider   string   `json:"provider,omitempty" jsonschema:"the provider configuration to read through, such as aws.west; default the type's default one"`
	Out        string   `json:"out,omitempty" jsonschema:"the name of the new .tf file to write in the stack directory; default imported.tf"`
	MaxObjects int      `json:"max_objects,omitempty" jsonschema:"refuse to read more objects than this, references included; default the writ import bulk default"`
}

// ImportAcceptInput is the input of the import_accept tool.
type ImportAcceptInput struct {
	Dir     string `json:"dir,omitempty" jsonschema:"the OpenTofu stack directory, relative to the directory writ mcp serves; default that directory"`
	Imports string `json:"imports,omitempty" jsonschema:"the name of the file in the stack directory in which import_bulk recorded the objects; default imported.imports.json"`
	Plan    string `json:"plan" jsonschema:"the id of the control plane's plan of the commit that holds the imported code"`
}

const instructions = `Tools for the OpenTofu stacks under this directory, on Writ's control plane.
A plan is of a commit: commit your change, then call plan. Read a plan with get_plan; pass addresses to see the before and after of a few resources.
Every plan, run, and index you read went through Writ's mask, which shows a secret as "(sensitive)".
No tool applies or approves. A person approves a plan, and the plan's author cannot approve their own.
Look up a resource type's attributes with schema before you write it; it reads the providers that .terraform.lock.hcl pins.
To bring existing objects under code, a person runs writ import bulk or writ mcp --allow-providers serves import_bulk; then commit both files it writes, plan the commit, and call import_accept with the plan's id.`

const importInstructions = `
import_bulk reads objects through the stack's providers with the credentials of writ mcp, and writes two new files in the stack directory.`

// Options configures Serve.
type Options struct {
	// Version is writ's own version, which the server reports.
	Version string
	// AllowProviders serves the import_bulk tool, which starts the stack's
	// providers with the server's environment, credentials, and file
	// access. Without it the tool is not listed.
	AllowProviders bool
	Log            *slog.Logger
}

// Serve answers MCP requests on in and out until the client closes in or
// ctx ends.
func Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser, tools Tools, o Options) error {
	s := NewServer(tools, o)
	return s.Run(ctx, &sdk.IOTransport{Reader: in, Writer: out})
}

// NewServer returns the MCP server with its tools on it, and import_bulk
// when o.AllowProviders is set.
func NewServer(tools Tools, o Options) *sdk.Server {
	text := instructions
	if o.AllowProviders {
		text += importInstructions
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "writ", Title: "Writ", Version: o.Version}, &sdk.ServerOptions{
		Instructions: text,
		Logger:       o.Log,
		Capabilities: &sdk.ServerCapabilities{},
	})
	closed, open := new(false), new(true)
	read := &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: open}
	sdk.AddTool(s, &sdk.Tool{
		Name:  "plan",
		Title: "Plan a commit on the control plane",
		Description: "Ask the control plane, the one --server or WRIT_SERVER names, else the one writ serve --dev runs, to plan one OpenTofu stack at a commit, and wait for the plan. " +
			"With dir, it plans the commit checked out there, which must be clean and in a repository the control plane reads; with stack and commit, that commit. " +
			"The control plane keeps the plan for people to approve and apply; this tool applies nothing, and one plan runs at a time. " +
			"Returns the masked plan as text, its id, and the approvals it needs, or with json true the writ.remote-plan/v0 document. " +
			"The text starts with the control plane's policy decision, when a policy judged the plan: each result's outcome, set, level, rule, and message. A plan that a policy denies is refused, and nothing overrides that.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: closed, OpenWorldHint: open},
	}, handler(tools.Plan))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "get_plan",
		Title: "Read a plan",
		Description: "Read a plan the control plane keeps, by its id or ref: its status, approvals, and the masked plan as text, or with json true the writ.remote-plan/v0 document. " +
			"The document's document field is the masked plan, OpenTofu's plan JSON with secrets shown as (sensitive): each resource change has its address, actions, before, after, and replace_paths. " +
			"The text starts with the policy decision, and the document's policy field is it: each result's set, level, rule, outcome, and message. " +
			"Pass addresses to keep only those resource changes.",
		Annotations: read,
	}, handler(func(ctx context.Context, in GetPlanInput) (Result, error) {
		if in.ID == "" {
			return Result{}, errors.New("id is required: give the plan's id or ref, which the plan tool printed")
		}
		if len(in.Addresses) > MaxAddresses {
			return Result{}, fmt.Errorf("addresses takes at most %d addresses, got %d", MaxAddresses, len(in.Addresses))
		}
		return tools.GetPlan(ctx, in)
	}))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "runs",
		Title: "List applies",
		Description: "List the applies the control plane ran in an environment, newest first, as the writ.remote-runs/v0 document: each run's id, plan, status, stack, actor, approvals, and times. " +
			"With id, return one run as the writ.remote-run/v0 document, whose document field is the masked writ.tofu-apply/v0: OpenTofu's counts and each resource's apply events.",
		Annotations: read,
	}, handler(func(ctx context.Context, in RunsInput) (Result, error) {
		if in.Limit < 0 || in.Limit > MaxListed {
			return Result{}, fmt.Errorf("limit takes 1 to %d, got %d", MaxListed, in.Limit)
		}
		return tools.Runs(ctx, in)
	}))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "drift",
		Title: "List stacks and their drift",
		Description: "List the OpenTofu stacks the control plane keeps state for, as the writ.drift/v0 document: each stack@env's backend, how many resources its state index holds, the state version it was read from, " +
			"a write to its state that no apply of the control plane made, and its newest drift check with each change found outside code.",
		Annotations: read,
	}, handler(func(ctx context.Context, in DriftInput) (Result, error) {
		if in.Limit < 0 || in.Limit > MaxListed {
			return Result{}, fmt.Errorf("limit takes 1 to %d, got %d", MaxListed, in.Limit)
		}
		return tools.Drift(ctx, in)
	}))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "state_index",
		Title: "Read a stack's state index",
		Description: "Read what one stack@env's state manages, from the control plane's state index, as the writ.state-index/v0 document: each resource's address, type, provider, and its id, arn, identifier, region, namespace, and modify_index when the state holds them. " +
			"The index holds no other attribute and no secret. Pass types to keep only some resource types.",
		Annotations: read,
	}, handler(func(ctx context.Context, in StateIndexInput) (Result, error) {
		if in.Stack == "" {
			return Result{}, errors.New("stack is required: give the stack's name on the control plane, as the drift tool lists it")
		}
		if len(in.Types) > MaxIndexTypes {
			return Result{}, fmt.Errorf("types takes at most %d types, got %d", MaxIndexTypes, len(in.Types))
		}
		return tools.StateIndex(ctx, in)
	}))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "schema",
		Title: "Look up provider schemas",
		Description: "Return resource types and data sources of the providers a stack's .terraform.lock.hcl pins, at the locked versions, as the writ.provider-schema/v0 document. " +
			"Each type's schema has the shape tofu providers schema -json gives: attributes with their type, required, optional, computed, sensitive, and write_only, and block_types with their nesting_mode. " +
			"A provider's name, such as random, lists its types. Pass fields, names or glob patterns, or required_only, to get only what you need. " +
			"Reads Writ's provider cache and needs no network and no credentials.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: closed},
	}, handler(func(ctx context.Context, in SchemaInput) (Result, error) {
		if len(in.Types) == 0 || len(in.Types) > MaxSchemaTypes {
			return Result{}, fmt.Errorf("types takes 1 to %d types, got %d", MaxSchemaTypes, len(in.Types))
		}
		if len(in.Fields) > MaxSchemaFields {
			return Result{}, fmt.Errorf("fields takes at most %d patterns, got %d", MaxSchemaFields, len(in.Fields))
		}
		return tools.Schema(ctx, in)
	}))
	if o.AllowProviders {
		addImportBulk(s, tools)
	}
	sdk.AddTool(s, &sdk.Tool{
		Name:  "import_accept",
		Title: "Accept an import's plan",
		Description: "Judge the control plane's plan of the commit that holds the code import_bulk or writ import bulk wrote: it passes when the plan imports exactly the objects the imports file records, each once at its address by its import id, and changes nothing. " +
			"Returns the writ.import-accept/v0 document with each refusal. A plan that also updates an imported object is refused here; a person checks that one with writ import accept and the saved plan.",
		Annotations: read,
	}, handler(func(ctx context.Context, in ImportAcceptInput) (Result, error) {
		if in.Plan == "" {
			return Result{}, errors.New("plan is required: plan the commit that holds the imported code with the plan tool, and give its id")
		}
		return tools.ImportAccept(ctx, in)
	}))
	sdk.AddTool(s, &sdk.Tool{
		Name:  "policy",
		Title: "Read the policy",
		Description: "Return the policy sets the control plane judges every OpenTofu plan with, as the writ.policy/v0 document: each set's name, its level, and the text of its .rego files. " +
			"A mandatory set's deny refuses the plan, and an advisory set's deny only warns. The rules read the masked plan document with OpenTofu's key names, such as input.resource_changes and each change's address, type, change.actions, and change.after. " +
			"Read a set to learn what a rule that denied your plan asks of the code.",
		Annotations: read,
	}, handler(func(ctx context.Context, in PolicyInput) (Result, error) {
		if in.File != "" && in.Set == "" {
			return Result{}, errors.New("file needs set: give the set that holds the file, as a plan's policy result names it")
		}
		return tools.Policy(ctx, in)
	}))
	s.AddReceivingMiddleware(boundResults)
	return s
}

// addImportBulk adds the import_bulk tool, which writ mcp serves only with
// --allow-providers, because it starts the stack's providers.
func addImportBulk(s *sdk.Server, tools Tools) {
	sdk.AddTool(s, &sdk.Tool{
		Name:  "import_bulk",
		Title: "Write code for existing objects",
		Description: "Read existing objects through the providers the stack's .terraform.lock.hcl pins, with the environment and credentials of writ mcp, and write two new files in the stack directory: resource blocks with import blocks, and the imports file import_accept reads. " +
			"Writes no state and plans nothing, and never replaces a file. Returns the writ.import-bulk/v0 document: each object's address, type, and import id, and each problem.",
		Annotations: &sdk.ToolAnnotations{DestructiveHint: new(false), OpenWorldHint: new(true)},
	}, handler(func(ctx context.Context, in ImportBulkInput) (Result, error) {
		switch {
		case in.Type == "":
			return Result{}, errors.New("type is required: give the resource type of the objects, such as aws_security_group")
		case len(in.IDs) == 0 || len(in.IDs) > MaxImportIDs:
			return Result{}, fmt.Errorf("ids takes 1 to %d import ids, got %d", MaxImportIDs, len(in.IDs))
		case in.MaxObjects < 0:
			return Result{}, fmt.Errorf("max_objects takes a positive number, got %d", in.MaxObjects)
		}
		return tools.ImportBulk(ctx, in)
	}))
}

// boundResults bounds every tool result as it leaves the server: what a
// tool returned, the error a tool returned, and the SDK's own answer to
// arguments that do not validate.
func boundResults(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		res, err := next(ctx, method, req)
		if method != "tools/call" {
			return res, err
		}
		if je, ok := errors.AsType[*jsonrpc.Error](err); ok {
			bounded := *je
			bounded.Message = Bound(je.Message)
			return res, &bounded
		}
		if r, ok := res.(*sdk.CallToolResult); ok && r != nil {
			for _, c := range r.Content {
				if tc, ok := c.(*sdk.TextContent); ok {
					tc.Text = Bound(tc.Text)
				}
			}
		}
		return res, err
	}
}

// handler adapts a tool to the SDK: a call's output is the result's text,
// a failure the call reported marks the result an error the agent reads,
// and an error of the tool is the result's text.
func handler[In any](run func(context.Context, In) (Result, error)) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		r, err := run(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: r.Text}},
			IsError: r.Failed,
		}, nil, nil
	}
}
