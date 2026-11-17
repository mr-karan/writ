package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mcp"
	"github.com/mr-karan/writ/internal/policy"
	"github.com/mr-karan/writ/internal/server"
)

// MCP serves the control plane's tools for OpenTofu stacks on stdin and
// stdout until the client closes stdin. The tools read and plan through
// the control plane a remote command finds, with its token, and return
// the documents it serves. The schema and import tools read a stack under
// --dir, through an os.Root on --dir, so no tool reads a file outside it.
// No tool applies or approves.
//
// --dir is opened as given, so the kernel walks it, and every path after
// that is spelled from the directory it found.
func (a *App) MCP(ctx context.Context, o MCPOptions) error {
	files, err := confine.Open(a.opts.Dir)
	if err != nil {
		return usagef("--dir: %v", err)
	}
	defer files.Close()
	root := files.Dir()
	a.opts.Log.Info("writ mcp serving on stdio", "dir", root, "version", a.opts.Version)
	tools := &mcpTools{root: root, base: a.opts, allowProviders: o.AllowProviders}
	tools.base.Files = files
	return mcp.Serve(ctx, io.NopCloser(a.opts.Stdin), nopWriteCloser{a.opts.Stdout}, tools, mcp.Options{Version: a.opts.Version, Log: a.opts.Log, AllowProviders: o.AllowProviders})
}

// MCPOptions are the flags of writ mcp.
type MCPOptions struct {
	// AllowProviders is --allow-providers: serve the import_bulk tool,
	// which starts the stack's providers with writ mcp's environment,
	// credentials, and file access, and let the schema tool download a
	// provider that Writ's cache lacks.
	AllowProviders bool
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Bounds of the MCP tools that writ mcp itself keeps.
const (
	// DefaultMCPRuns is how many runs the runs tool lists by default.
	DefaultMCPRuns = 20
	// maxMCPImports bounds the imports file import_accept reads.
	maxMCPImports = maxAcceptImports
)

// mcpTools runs each tool call on its own App, whose stdout is a buffer and
// whose Files are confined to root.
type mcpTools struct {
	root           string
	base           Options
	allowProviders bool
	// planning is held while a plan runs: a writ mcp asks for one plan at a
	// time, so an agent cannot fill the control plane's plan slots.
	planning sync.Mutex
}

// app returns an App for the stack directory rel under the root, in env
// when it is set, that prints into the returned buffer.
func (t *mcpTools) app(rel, env string) (*App, *bytes.Buffer, error) {
	dir, err := t.dir(rel)
	if err != nil {
		return nil, nil, err
	}
	if err := checkEnv(env); err != nil {
		return nil, nil, err
	}
	buf := &bytes.Buffer{}
	o := t.base
	o.Dir, o.Stdout, o.Color = dir, buf, false
	if env != "" {
		o.Env = env
	}
	return New(o), buf, nil
}

func checkEnv(env string) error {
	if env != "" && !server.ValidEnvName(env) {
		return usagef("env %q is not an environment name: it starts with a lowercase letter and holds up to 63 lowercase letters, digits, _, and -", env)
	}
	return nil
}

// dir resolves rel against the root, and refuses a path that leaves it,
// through .. or a symbolic link, as every later read does. The stack
// directory is where Resolve walks rel to, with no link or .. left, so
// each file path the stack's App joins to it names what the kernel finds.
func (t *mcpTools) dir(rel string) (string, error) {
	if rel == "" {
		return t.root, nil
	}
	if filepath.IsAbs(rel) {
		return "", usagef("dir %q is absolute: give a path relative to the directory writ mcp serves", rel)
	}
	dir, err := t.base.Files.Resolve(t.root + string(filepath.Separator) + rel)
	var info fs.FileInfo
	if err == nil {
		info, err = t.base.Files.Stat(dir)
	}
	switch {
	case errors.Is(err, confine.ErrOutside):
		return "", usagef("dir %q is outside the directory writ mcp serves", rel)
	case errors.Is(err, fs.ErrNotExist):
		return "", usagef("dir %q: no such directory under the directory writ mcp serves", rel)
	case err != nil:
		return "", usagef("dir %q: %v", rel, err)
	case !info.IsDir():
		return "", usagef("dir %q is a file: give a stack directory", rel)
	}
	return dir, nil
}

// document turns a command's outcome into a tool result: the document it
// printed, or, when it stopped before it printed one, the failure document
// it prints with --json.
func document(command string, buf *bytes.Buffer, err error) (mcp.Result, error) {
	switch {
	case err == nil:
		return mcp.Result{Text: buf.String()}, nil
	case errors.Is(err, ErrFailed):
		return mcp.Result{Text: buf.String(), Failed: true}, nil
	}
	// A tool call has no command line, so a usage error's fix, to run writ
	// help, does not apply.
	d := failureDiag(err)
	if d.Class == diag.Usage {
		d.Fix = nil
	}
	var doc bytes.Buffer
	if werr := writeJSON(&doc, failureReport(command, nil, diag.List{d})); werr != nil {
		return mcp.Result{}, werr
	}
	return mcp.Result{Text: doc.String(), Failed: true}, nil
}

// text turns a command's outcome into a tool result in text: what it
// printed, or an error when it stopped before it could run.
func text(buf *bytes.Buffer, err error) (mcp.Result, error) {
	if err != nil && !errors.Is(err, ErrFailed) {
		return mcp.Result{}, err
	}
	return mcp.Result{Text: buf.String(), Failed: err != nil}, nil
}

// remote finds the control plane and the token, as a remote command run
// in the served directory does, and the environment a call names, which
// the dev control plane defaults to dev.
func (t *mcpTools) remote(ctx context.Context, env string, needEnv bool) (*remote, string, error) {
	app, _, err := t.app("", "")
	if err != nil {
		return nil, "", err
	}
	if err := checkEnv(env); err != nil {
		return nil, "", err
	}
	r, err := app.remote(ctx)
	if err != nil {
		return nil, "", err
	}
	if env == "" && needEnv {
		if !r.dev {
			return nil, "", usagef("env is required: a team's control plane keeps each stack in its environments")
		}
		env = server.DevEnv
	}
	return r, env, nil
}

// get fetches the document at path from the control plane. A refusal, such
// as a stack the token may not read, is a failed result that carries the
// control plane's document.
func get(ctx context.Context, r *remote, path string) ([]byte, *mcp.Result, error) {
	doc, err := r.get(ctx, path, remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return nil, &mcp.Result{Text: string(re.doc), Failed: true}, nil
	}
	return doc, nil, err
}

// served is the result of a document the control plane served as is.
func served(ctx context.Context, r *remote, path string) (mcp.Result, error) {
	doc, refused, err := get(ctx, r, path)
	if refused != nil || err != nil {
		return deref(refused), err
	}
	return mcp.Result{Text: string(doc)}, nil
}

func deref(r *mcp.Result) mcp.Result {
	if r == nil {
		return mcp.Result{}
	}
	return *r
}

// reencode writes v as the result's JSON document.
func reencode(v any) (mcp.Result, error) {
	var b bytes.Buffer
	if err := writeJSON(&b, v); err != nil {
		return mcp.Result{}, err
	}
	return mcp.Result{Text: b.String()}, nil
}

func (t *mcpTools) Plan(ctx context.Context, in mcp.PlanInput) (mcp.Result, error) {
	app, buf, err := t.app(in.Dir, in.Env)
	if err != nil {
		return mcp.Result{}, err
	}
	if in.Commit == "" && in.Repo == "" && !app.tofuStack() {
		return mcp.Result{}, usagef("%s holds no OpenTofu stack: give the directory of a stack's .tf files, or stack and commit", app.path("."))
	}
	if !t.planning.TryLock() {
		return mcp.Result{}, usagef("a plan from this writ mcp is still running: wait for it, then plan again")
	}
	defer t.planning.Unlock()
	err = app.RemotePlan(ctx, RemotePlanOptions{
		RemoteOptions: RemoteOptions{Stack: in.Stack},
		JSON:          true,
		Repo:          in.Repo,
		Commit:        in.Commit,
	})
	if in.JSON {
		return document("plan", buf, err)
	}
	return remotePlanText(app, buf, err)
}

func (t *mcpTools) GetPlan(ctx context.Context, in mcp.GetPlanInput) (mcp.Result, error) {
	if len(in.Addresses) == 0 {
		app, buf, err := t.app("", "")
		if err != nil {
			return mcp.Result{}, err
		}
		err = app.Show(ctx, ShowOptions{JSON: true, ID: in.ID})
		if in.JSON {
			return document("plan", buf, err)
		}
		return remotePlanText(app, buf, err)
	}
	r, _, err := t.remote(ctx, "", false)
	if err != nil {
		return mcp.Result{}, err
	}
	doc, refused, err := get(ctx, r, "/v1/plans/"+url.PathEscape(in.ID))
	if refused != nil || err != nil {
		return deref(refused), err
	}
	var p server.RemotePlan
	if err := json.Unmarshal(doc, &p); err != nil || p.Plan == nil {
		return mcp.Result{}, fmt.Errorf("writ serve sent a plan that is not %s", server.FormatRemotePlan)
	}
	if p.Document, err = keepAddresses(p.Document, in.Addresses); err != nil {
		return mcp.Result{}, err
	}
	// The text names every change; the caller asked for a few.
	p.Text = ""
	res, err := reencode(p)
	res.Failed = !p.OK || p.Plan.Status == "refused" || p.Plan.Status == "failed"
	return res, err
}

// remotePlanText prints, as text, the document a plan command printed
// into buf with --json: a plan as writ plan --remote prints it, but with
// its policy decision first, so the cut of a long plan keeps the verdict;
// a run as writ show --remote prints it; any other document's diagnostics.
func remotePlanText(app *App, buf *bytes.Buffer, err error) (mcp.Result, error) {
	if err != nil && !errors.Is(err, ErrFailed) {
		return mcp.Result{}, err
	}
	doc := bytes.Clone(buf.Bytes())
	buf.Reset()
	var head struct {
		Format string `json:"format"`
	}
	_ = json.Unmarshal(doc, &head)
	switch head.Format {
	case server.FormatRemotePlan:
		if doc, err = policyFirst(buf, doc); err == nil {
			err = app.printRemotePlan(false, doc)
		}
	case server.FormatRemoteRun:
		err = app.printRemoteRun(false, doc)
	default:
		err = app.remoteFailed(false, doc)
	}
	return text(buf, err)
}

// policyFirst writes the policy decision of the plan doc to w, at most
// mcp.MaxPolicyResults results with denies first, and returns doc with the
// decision's lines cut from the end of its text. A plan no policy judged
// is returned as it is.
func policyFirst(w io.Writer, doc []byte) ([]byte, error) {
	var p server.RemotePlan
	if err := json.Unmarshal(doc, &p); err != nil {
		return nil, fmt.Errorf("writ serve sent a plan that is not %s", server.FormatRemotePlan)
	}
	if len(p.Policy) == 0 || string(p.Policy) == "null" {
		return doc, nil
	}
	var d policy.Decision
	if err := json.Unmarshal(p.Policy, &d); err != nil || d.Format != policy.DecisionFormat {
		return nil, fmt.Errorf("writ serve sent a policy decision that is not %s", policy.DecisionFormat)
	}
	p.Text = strings.TrimSuffix(p.Text, server.PolicyText(d))
	switch {
	case d.Outcome.Denies():
		fmt.Fprintln(w, "Policy: denied, so this plan cannot be approved or applied, and nothing overrides a deny.")
	case d.Outcome == policy.Warn:
		fmt.Fprintln(w, "Policy: passed with warnings.")
	default:
		fmt.Fprintln(w, "Policy: passed.")
	}
	results := slices.Clone(d.Results)
	slices.SortStableFunc(results, func(a, b policy.Result) int {
		switch {
		case a.Outcome.Denies() == b.Outcome.Denies():
			return 0
		case a.Outcome.Denies():
			return -1
		}
		return 1
	})
	for _, r := range results[:min(len(results), mcp.MaxPolicyResults)] {
		fmt.Fprintf(w, "  %s: [%s, %s] %s: %s\n", escapeHuman(string(r.Outcome), false), escapeHuman(r.Set, false), escapeHuman(string(r.Level), false), escapeHuman(r.Rule, false), escapeHuman(r.Message, false))
	}
	if n := len(results) - mcp.MaxPolicyResults; n > 0 {
		fmt.Fprintf(w, "  and %d more; get_plan with json true returns every result\n", n)
	}
	fmt.Fprintln(w)
	return json.Marshal(p)
}

// Policy returns the control plane's policy sets, or one set, or one
// file of a set.
func (t *mcpTools) Policy(ctx context.Context, in mcp.PolicyInput) (mcp.Result, error) {
	r, _, err := t.remote(ctx, "", false)
	if err != nil {
		return mcp.Result{}, err
	}
	if in.Set == "" {
		return served(ctx, r, "/v1/policy")
	}
	doc, refused, err := get(ctx, r, "/v1/policy")
	if refused != nil || err != nil {
		return deref(refused), err
	}
	var p server.PolicyDoc
	if err := json.Unmarshal(doc, &p); err != nil || p.Format != server.FormatPolicy {
		return mcp.Result{}, fmt.Errorf("writ serve sent a body that is not %s", server.FormatPolicy)
	}
	i := slices.IndexFunc(p.Sets, func(s server.PolicySetInfo) bool { return s.Name == in.Set })
	if i < 0 {
		names := []string{}
		for _, s := range p.Sets {
			names = append(names, s.Name)
		}
		return mcp.Result{}, usagef("the control plane has no policy set %q; its sets are [%s]", in.Set, strings.Join(names, ", "))
	}
	set := p.Sets[i]
	if in.File != "" {
		j := slices.IndexFunc(set.Files, func(f server.PolicyFileInfo) bool { return f.Name == in.File })
		if j < 0 {
			names := []string{}
			for _, f := range set.Files {
				names = append(names, f.Name)
			}
			return mcp.Result{}, usagef("policy set %s has no file %q; its files are [%s]", set.Name, in.File, strings.Join(names, ", "))
		}
		set.Files = set.Files[j : j+1]
	}
	p.Sets = []server.PolicySetInfo{set}
	return reencode(p)
}

// keepAddresses keeps, of a masked plan document, only the resource
// changes and drift at the addresses in keep. A plan that made no document
// keeps its null.
func keepAddresses(doc json.RawMessage, keep []string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil || fields == nil {
		return doc, nil
	}
	for _, key := range []string{"resource_changes", "resource_drift"} {
		var changes []json.RawMessage
		if raw, ok := fields[key]; !ok || json.Unmarshal(raw, &changes) != nil {
			continue
		}
		kept := []json.RawMessage{}
		for _, c := range changes {
			var head struct {
				Address string `json:"address"`
			}
			if json.Unmarshal(c, &head) == nil && slices.Contains(keep, head.Address) {
				kept = append(kept, c)
			}
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b.Bytes()), nil
}

func (t *mcpTools) Runs(ctx context.Context, in mcp.RunsInput) (mcp.Result, error) {
	if in.ID != "" {
		if !runIDRE.MatchString(in.ID) {
			return mcp.Result{}, usagef("id %q is not a run's id", in.ID)
		}
		r, _, err := t.remote(ctx, "", false)
		if err != nil {
			return mcp.Result{}, err
		}
		return served(ctx, r, "/v1/runs/"+url.PathEscape(in.ID))
	}
	r, env, err := t.remote(ctx, in.Env, true)
	if err != nil {
		return mcp.Result{}, err
	}
	q := url.Values{"env": {env}, "limit": {strconv.Itoa(cmpInt(in.Limit, DefaultMCPRuns))}}
	if in.Stack != "" {
		q.Set("stack", in.Stack)
	}
	return served(ctx, r, "/v1/applies?"+q.Encode())
}

func (t *mcpTools) Drift(ctx context.Context, in mcp.DriftInput) (mcp.Result, error) {
	r, env, err := t.remote(ctx, in.Env, false)
	if err != nil {
		return mcp.Result{}, err
	}
	q := url.Values{}
	for k, v := range map[string]string{"env": env, "stack": in.Stack} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	return served(ctx, r, "/v1/drift?"+q.Encode())
}

func (t *mcpTools) StateIndex(ctx context.Context, in mcp.StateIndexInput) (mcp.Result, error) {
	r, env, err := t.remote(ctx, in.Env, true)
	if err != nil {
		return mcp.Result{}, err
	}
	path := "/v1/index?" + url.Values{"env": {env}, "stack": {in.Stack}}.Encode()
	if len(in.Types) == 0 {
		return served(ctx, r, path)
	}
	doc, refused, err := get(ctx, r, path)
	if refused != nil || err != nil {
		return deref(refused), err
	}
	var ix server.StateIndexDoc
	if err := json.Unmarshal(doc, &ix); err != nil || ix.Format != server.FormatStateIndex {
		return mcp.Result{}, fmt.Errorf("writ serve sent a body that is not %s", server.FormatStateIndex)
	}
	ix.Resources = slices.DeleteFunc(ix.Resources, func(r server.IndexResource) bool { return !slices.Contains(in.Types, r.Type) })
	return reencode(ix)
}

func cmpInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

func (t *mcpTools) ImportBulk(ctx context.Context, in mcp.ImportBulkInput) (mcp.Result, error) {
	app, buf, err := t.app(in.Dir, "")
	if err != nil {
		return mcp.Result{}, err
	}
	if in.Out != "" && filepath.Base(in.Out) != in.Out {
		return mcp.Result{}, usagef("out %q: want the name of a new .tf file in the stack directory, such as %s", in.Out, DefaultTofuBulkFile)
	}
	return text(buf, app.ImportBulk(ctx, ImportBulkOptions{
		JSON: true, Type: in.Type, IDs: in.IDs, Provider: in.Provider, Out: in.Out, MaxObjects: in.MaxObjects,
	}))
}

// ImportAccept judges the control plane's plan, the masked plan document
// it serves, against the imports file of the stack, read through the
// os.Root. A refusal quotes no import id, of the plan or of the file,
// since the plan's mask hid any id that is a secret.
func (t *mcpTools) ImportAccept(ctx context.Context, in mcp.ImportAcceptInput) (mcp.Result, error) {
	name := in.Imports
	if name == "" {
		name = ImportsFile(DefaultTofuBulkFile)
	}
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".imports.json") {
		return mcp.Result{}, usagef("imports %q: want the name of the file in the stack directory that writ import bulk wrote, such as %s", name, ImportsFile(DefaultTofuBulkFile))
	}
	app, buf, err := t.app(in.Dir, "")
	if err != nil {
		return mcp.Result{}, err
	}
	importsText, err := readConfined(t.base.Files, app.path(name), maxMCPImports)
	if err != nil {
		return mcp.Result{}, err
	}
	want, err := importer.ParseImports(importsText)
	if err != nil {
		return mcp.Result{}, usagef("%s: %s", name, err)
	}
	r, _, err := t.remote(ctx, "", false)
	if err != nil {
		return mcp.Result{}, err
	}
	doc, refused, err := get(ctx, r, "/v1/plans/"+url.PathEscape(in.Plan))
	if refused != nil || err != nil {
		return deref(refused), err
	}
	acc, err := acceptJSON(doc, want, importer.QuoteNoIDs)
	if err != nil {
		return mcp.Result{}, usagef("plan %s: %s", in.Plan, err)
	}
	return text(buf, app.printAccept(true, len(want), acc))
}

// readConfined reads the regular file at p through files, refusing one
// larger than limit.
func readConfined(files confine.Files, p string, limit int64) ([]byte, error) {
	b, err := files.ReadFileMax(p, limit)
	switch {
	case errors.Is(err, confine.ErrOutside):
		return nil, usagef("%s is outside the directory writ mcp serves", p)
	case errors.Is(err, fs.ErrNotExist):
		return nil, usagef("%s does not exist: import_bulk or writ import bulk writes it beside the code", p)
	case err != nil:
		return nil, usagef("%v", err)
	}
	return b, nil
}
