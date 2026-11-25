// Command writ is the Writ CLI. It parses flags into cli option structs, calls
// one cli.App method, and maps the result to an exit code. It holds no logic
// of its own.
//
// Exit codes: 0 success, 1 the command reported a failure, 2 usage error.
// With --json, stdout holds exactly one JSON document on every one of them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/mr-karan/writ/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...", or
// taken from the build info (version.go), and logged at startup.
var version = "dev"

const usage = `usage: writ [--dir DIR] [--env ENV] [--verbose] <command> [flags] [args]

commands:
  import   bulk SELECTION.json [--out FILE] [--max-objects N] [--max-bytes N] [--json]
  import   bulk --type TYPE --id ID... [--provider ADDR] [--out FILE] [--max-objects N] [--max-bytes N] [--json]
                                          take over many objects into an OpenTofu stack: write one file
                                          of resource blocks and one of the imports, and plan nothing
  import   accept --imports FILE PLAN [--json]
                                          accept the plan of an OpenTofu stack after writ import
                                          bulk: it imports exactly the objects FILE records;
                                          PLAN is the file tofu plan -out saved, which tofu
                                          show -json reads in FILE's directory or --dir; a
                                          JSON plan passes only an import that plans no update
  agent    init [--json] [--force] [--print]
                                          write the writ skill and a section of AGENTS.md for coding
                                          agents of an OpenTofu repository; --print writes nothing
                                          and prints them
  mcp      [--allow-providers]            serve MCP tools on stdio for the OpenTofu stacks under
                                          --dir: plan a commit on the control plane, read plans, runs,
                                          drift, and state indexes, look up provider schemas, and
                                          accept an import's plan; nothing applies or approves;
                                          --allow-providers adds import_bulk, which runs the stack's
                                          providers with this process's environment and credentials
  version  [--json]

remote commands, against the first control plane of --server, WRIT_SERVER, the one you
hold a session for, and the one writ serve --dev runs, with the CI token in
WRIT_ID_TOKEN, else a GitHub Actions job's ID token, else your session from writ login,
else the dev token:
  login    [URL] [--issuer NAME] [--device | --browser] [--json]
                                          sign in through the team's identity provider: with the
                                          device flow when it offers one, else in a browser on this
                                          computer; the session lasts 12 hours and is kept in
                                          ~/.config/writ/sessions.json, readable by you alone
  logout   [URL] [--json]                 end the session on the control plane and forget it
  plan     --remote --env ENV [--stack NAME] [--hint-merge-request N] [--json]
                                          plan the OpenTofu stack on writ serve, which keeps the plan:
                                          in a clean work tree of a repository writ serve reads, the
                                          commit you are on, which writ serve fetches
  plan     --remote --env ENV --stack NAME --commit SHA [--repo NAME] [--slot-wait 15m]
                                          plan a commit and read no file, as a CI job with no checkout
  repo     add NAME FORGE/PROJECT [--path PATH] [--prefix PREFIX] [--json]
                                          add a repository: writ serve reads its default branch with the
                                          forge's read token and finds its stacks
  repo     add [NAME] DIR [--json]        under writ serve --dev, add the local Git repository at DIR
  repo     list [--json]                  list the repositories and the last commit read of each
  repo     show NAME [--json]             print a repository and the stacks found at its head
  ci       gitlab --repo NAME [--envs LIST] [--image IMAGE] [--component REF]
                                          print the .gitlab-ci.yml lines and the config lines for a repository
  ci       github --repo NAME [--envs LIST] [--writ-version V] [--runs-on LABEL]
                                          print a GitHub Actions workflow that plans a repository's stacks,
                                          and the config lines for it
  ui       [--json]                       print the control UI's address; under --dev, a sign-in link
  show     --remote ID [--json]           print a plan writ serve keeps, by id or ref, or a run, by id
  show     --remote --request KEY [--json]
                                          print the run an apply request started, or that none did;
                                          asking closes a key with no run
  approve  --remote ID [--withdraw] [--json]
                                          approve a plan another person made, or take the approval back
  approve  --remote --pipeline [--json]   in a CI job: approve every pending plan of the job's pipeline
  apply    --remote ID [--override REASON] [--json]
                                          apply a plan writ serve keeps, with its approvals, or with an
                                          override's reason when you hold override
  apply    --remote --merged --env ENV --stack NAME [--repo NAME --commit SHA] [--json]
                                          after a merge: apply the approved plan whose sources equal the
                                          merged commit's, the CI job's own commit on the default branch
  apply    --remote --yes [--env ENV] [--stack NAME] [--json]
                                          plan as plan --remote does, and apply the plan when no approval
                                          rule needs one
  state    adopt STACK --env ENV (--version N | --serial N) --reason TEXT [--json]
                                          seal a secret set with the newest state of an OpenTofu
                                          stack that no apply of writ serve sealed one with, such as
                                          a state on S3 written outside Writ; needs adopt
  plans    --remote --env ENV [--stack NAME] [--status STATUS] [--started-by person|ci]
           [--limit N] [--cursor C] [--json]
                                          list the plans writ serve keeps, newest first, with what
                                          started each: a person or a CI job
  runs     --remote --env ENV [--stack NAME] [--json] [--limit N]
                                          list the runs writ serve started, newest first
  audit    --remote [--env ENV [--stack NAME]] [--plan ID] [--run ID] [--person ID] [--op OP]
           [--changes] [--since TIME] [--until TIME] [--limit N] [--cursor C] [--json]
                                          list the rows of writ serve's audit log that the token may
                                          see, newest first; --plan takes in its approvals, its run,
                                          and the run's state writes; --changes leaves out reads;
                                          TIME is RFC 3339
  whoami   [--remote] [--json]            print the control plane, who the token speaks for, its
                                          teams, and its grants
  serve    --config FILE [--check [--notify-test]]
                                          run the control plane; --check prints what the config
                                          allows, --notify-test sends each notifier a test message
  serve    --dev [--data DIR] [--listen 127.0.0.1:PORT] [--database URL] [--config FILE]
                                          run a control plane for this user alone, on loopback;
                                          the remote commands find it with no flag. --config adds
                                          forge and repo blocks, and poll_every

--dir, --env, and --verbose work before or after the command.
Output is colored on a terminal, and plain when piped or when NO_COLOR is set.
Exit codes: 0 success, 1 failure, 2 usage error. With --json, stdout is one
JSON document on every exit code.
`

func main() {
	// writ serve --check runs writ as a provider's user to see what that
	// user can reach. See cli.RunIsolationProbe.
	if req, ok := os.LookupEnv(cli.IsolationProbeEnv); ok {
		os.Exit(cli.RunIsolationProbe(req, os.Stdout))
	}
	os.Exit(runProfiled())
}

func runProfiled() int {
	if path := os.Getenv("WRIT_CPUPROFILE"); path != "" {
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "writ: create cpu profile: "+err.Error())
			return cli.ExitFailed
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			fmt.Fprintln(os.Stderr, "writ: start cpu profile: "+err.Error())
			return cli.ExitFailed
		}
		defer pprof.StopCPUProfile()
	}
	return run(os.Args[1:], os.Stdout, os.Stderr)
}

// globals are the flags every command accepts.
type globals struct {
	dir     string
	env     string
	verbose bool
	server  string
}

// register adds the global flags to fs, keeping values already parsed.
func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.dir, "dir", g.dir, "stack `directory`")
	fs.StringVar(&g.env, "env", g.env, "the `environment` of the control plane to run in (WRIT_ENV)")
	fs.BoolVar(&g.verbose, "verbose", g.verbose, "log at debug level")
	fs.StringVar(&g.server, "server", g.server, "run on the control plane at `URL`, or dev for the one writ serve --dev runs; implies --remote (WRIT_SERVER)")
}

// output is stdout, remembering whether a command wrote to it, so a command
// that stops without a report gets exactly one JSON document and a command
// that printed its own never gets a second.
type output struct {
	w     io.Writer
	wrote bool
}

func (o *output) Write(p []byte) (int, error) {
	o.wrote = o.wrote || len(p) > 0
	return o.w.Write(p)
}

// wantsJSON reports whether the command line asks for JSON. It reads the raw
// arguments, because a command line that fails to parse must still be
// answered in JSON.
func wantsJSON(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		switch a {
		case "--json", "-json", "--json=true", "-json=true":
			return true
		}
		return false
	})
}

// invocation is one command line: where its output goes and what to print
// when it stops without having reported.
type invocation struct {
	stdout *output
	stderr io.Writer
	asJSON bool
	// command and pos name the document shape: the command, and its
	// positional arguments once the flags parse, or all its arguments
	// until then. Both are empty when the line names no command.
	command string
	pos     []string
}

// stopped reports an error no command reported: the message on stderr, and
// with --json the command's failure document on stdout unless the command
// already wrote there.
func (inv *invocation) stopped(err error) int {
	fmt.Fprintln(inv.stderr, "writ: "+err.Error())
	if cli.ExitCode(err) == cli.ExitUsage {
		fmt.Fprintln(inv.stderr, "run writ help for usage")
	}
	if inv.asJSON && !inv.stdout.wrote {
		if werr := cli.WriteFailure(inv.stdout, inv.command, inv.pos, err); werr != nil {
			fmt.Fprintln(inv.stderr, "writ: write the failure document: "+werr.Error())
		}
	}
	return cli.ExitCode(err)
}

// guarded runs fn and turns a panic into the same report as any other error
// that stops a command, with the stack on stderr, so a bug in writ still
// leaves one JSON document on stdout.
func (inv *invocation) guarded(fn func() int) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(inv.stderr, "%s\n", debug.Stack())
			code = inv.stopped(fmt.Errorf("writ hit a bug and stopped: %v", r))
		}
	}()
	return fn()
}

func run(args []string, stdout, stderr io.Writer) int {
	inv := &invocation{stdout: &output{w: stdout}, stderr: stderr, asJSON: wantsJSON(args)}
	return inv.guarded(func() int { return inv.run(args) })
}

func (inv *invocation) run(args []string) int {
	stdout, stderr := inv.stdout, inv.stderr
	g := &globals{dir: "."}
	top := flag.NewFlagSet("writ", flag.ContinueOnError)
	top.SetOutput(io.Discard)
	g.register(top)
	if err := top.Parse(args); errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, usage)
		return cli.ExitOK
	} else if err != nil {
		return inv.stopped(&cli.UsageError{Msg: err.Error()})
	}
	if top.NArg() == 0 {
		fmt.Fprint(stderr, usage)
		return cli.ExitUsage
	}
	name, rest := top.Arg(0), top.Args()[1:]
	switch name {
	case "help":
		fmt.Fprint(stdout, usage)
		return cli.ExitOK
	case "version":
		if inv.asJSON {
			if err := cli.WriteVersion(stdout, version); err != nil {
				return inv.stopped(err)
			}
			return cli.ExitOK
		}
		fmt.Fprintln(stdout, "writ "+version)
		return cli.ExitOK
	}

	fs := flag.NewFlagSet("writ "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	call, err := command(name, fs)
	if err != nil {
		return inv.stopped(&cli.UsageError{Msg: err.Error()})
	}
	inv.command, inv.pos = name, rest
	pos, err := parseInterleaved(fs, rest)
	if errors.Is(err, flag.ErrHelp) {
		fs.SetOutput(stdout)
		fmt.Fprintf(stdout, "usage of writ %s:\n", name)
		fs.PrintDefaults()
		return cli.ExitOK
	}
	if err != nil {
		return inv.stopped(&cli.UsageError{Msg: err.Error()})
	}
	inv.pos = pos
	if g.server != "" && name != "login" && name != "logout" && name != "repo" && name != "ui" && name != "ci" && name != "mcp" {
		f := fs.Lookup("remote")
		if f == nil {
			return inv.stopped(&cli.UsageError{Msg: "--server goes with the commands that run on a control plane: login, logout, plan, apply, approve, show, runs, audit, whoami, repo, ci, ui, and mcp"})
		}
		_ = f.Value.Set("true")
	}

	level := slog.LevelWarn
	if g.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	log.Debug("writ starting", "version", version, "command", name)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drain := make(chan struct{})
	stopSignals := handleSignals(log, cancel, drain, name == "apply")
	defer stopSignals()

	app := cli.New(cli.Options{Dir: g.dir, Stdout: stdout, Env: g.env, Stderr: stderr, Log: log, Drain: drain, Version: version, Server: g.server,
		Color: cli.ColorFor(stdout.w)})
	err = call(ctx, app, pos)
	if err != nil && !errors.Is(err, cli.ErrFailed) {
		return inv.stopped(err)
	}
	return cli.ExitCode(err)
}

// handleSignals stops a command in two steps. The first interrupt closes
// drain, so writ apply --remote stops following its run, which goes on on
// the control plane; other commands are cancelled at once. The second
// interrupt cancels everything and exits. The returned func stops the handler
// and waits for its goroutine.
func handleSignals(log *slog.Logger, cancel context.CancelFunc, drain chan struct{}, drains bool) func() {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		select {
		case <-sigs:
		case <-done:
			return
		}
		close(drain)
		if drains {
			log.Warn("interrupted: no longer following the run, which goes on; interrupt again to stop now")
		} else {
			cancel()
		}
		select {
		case <-sigs:
		case <-done:
			return
		}
		log.Warn("interrupted again: stopping now")
		cancel()
		os.Exit(130)
	})
	return func() {
		signal.Stop(sigs)
		close(done)
		wg.Wait()
	}
}

// parseInterleaved parses flags that may come before or after positional
// arguments, so `writ show --remote ID --json` works.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// moves is a repeatable flag, such as --id ID. cli
// parses the addresses.
type moves []string

func (m *moves) String() string { return strings.Join(*m, " ") }

func (m *moves) Set(s string) error {
	*m = append(*m, s)
	return nil
}

type call func(ctx context.Context, app *cli.App, pos []string) error

// command registers the flags of one command on fs and returns the call that
// runs it with the parsed values.
func command(name string, fs *flag.FlagSet) (call, error) {
	asJSON := func() *bool { return fs.Bool("json", false, "print the result as JSON") }
	remote := func() (*bool, *string) {
		r := fs.Bool("remote", false, "run against writ serve at WRIT_SERVER, with the token in WRIT_ID_TOKEN")
		st := fs.String("stack", "", "with --remote: the stack's `name` on writ serve; default: its path from the Git work tree")
		return r, st
	}
	// remoteOnly refuses a command line without --remote: the command has
	// no local form.
	remoteOnly := func(rem *bool) error {
		if !*rem {
			return &cli.UsageError{Msg: fmt.Sprintf("%s runs on the control plane: writ %s --remote", name, name)}
		}
		return nil
	}
	switch name {
	case "plan":
		j := asJSON()
		rem, stackName := remote()
		mr := fs.Int("hint-merge-request", 0, "with --remote: the merge request `IID` the plan belongs to, for writ serve's records")
		repo := fs.String("repo", "", "with --remote: the `repository` whose commit to plan, as writ repo list names it; a CI job's token names its own")
		commit := fs.String("commit", "", "with --remote: the commit `SHA` to plan, which writ serve fetches; no file is read")
		slotWait := fs.Duration("slot-wait", 0, "with --remote: how long to keep asking while writ serve's plan slots are busy, such as 15m; at most 1h")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			if err := remoteOnly(rem); err != nil {
				return err
			}
			return app.RemotePlan(ctx, cli.RemotePlanOptions{RemoteOptions: cli.RemoteOptions{Stack: *stackName}, JSON: *j, HintMergeRequest: *mr, Repo: *repo, Commit: *commit, SlotWait: *slotWait})
		}, nil
	case "apply":
		j := asJSON()
		rem, stackName := remote()
		yes := fs.Bool("yes", false, "with --remote: plan on writ serve and apply the plan when it needs no approval")
		override := fs.String("override", "", "with --remote ID: apply without the approvals the rules need, for this `reason` of at least 10 characters; needs override")
		merged := fs.Bool("merged", false, "with --remote: apply the approved plan of --stack whose sources equal a merged commit's, the CI job's own or --commit")
		repo := fs.String("repo", "", "with --remote: the `repository` of --merged or --yes, as writ repo list names it; a CI job's token names its own")
		commit := fs.String("commit", "", "with --remote: the commit `SHA` of --merged, or the one --yes plans; a CI job's token names its own")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) > 1 {
				return &cli.UsageError{Msg: "apply takes at most one plan id"}
			}
			if err := remoteOnly(rem); err != nil {
				return err
			}
			o := cli.RemoteApplyOptions{JSON: *j, Override: *override, Merged: *merged, Stack: *stackName, Repo: *repo, Commit: *commit, Yes: *yes}
			if len(pos) == 1 {
				o.ID = pos[0]
			}
			o.Plan = cli.RemotePlanOptions{RemoteOptions: cli.RemoteOptions{Stack: *stackName}, JSON: *j, Repo: *repo, Commit: *commit}
			return app.RemoteApply(ctx, o)
		}, nil
	case "approve":
		j := asJSON()
		rem := fs.Bool("remote", false, "approve on writ serve; approve has no local form")
		pipeline := fs.Bool("pipeline", false, "in a CI job: approve every pending plan of the job's own pipeline, all or none")
		withdraw := fs.Bool("withdraw", false, "take back your approval of the plan, until it starts applying")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if !*rem {
				return &cli.UsageError{Msg: "approve asks writ serve: writ approve --remote ID"}
			}
			if len(pos) > 1 {
				return &cli.UsageError{Msg: "approve takes one plan id"}
			}
			o := cli.ApproveOptions{JSON: *j, Pipeline: *pipeline, Withdraw: *withdraw}
			if len(pos) == 1 {
				o.ID = pos[0]
			}
			return app.Approve(ctx, o)
		}, nil
	case "state":
		j := asJSON()
		// state adopt always asks a control plane; --remote is kept for the
		// command lines that pass it.
		fs.Bool("remote", true, "ask the control plane; state has no local form")
		version := fs.Int64("version", 0, "with adopt: the state version `N` of a stack on the http backend")
		serial := fs.Int64("serial", 0, "with adopt: the serial `N` of a stack's state on S3")
		reason := fs.String("reason", "", "with adopt: why, such as the ticket and where the state came from, `10 to 500 characters`")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) != 2 || pos[0] != "adopt" {
				return &cli.UsageError{Msg: "state takes adopt: writ state adopt STACK --env ENV --version N --reason TEXT, or --serial N for a stack on S3"}
			}
			return app.StateAdopt(ctx, cli.StateAdoptOptions{JSON: *j, Stack: pos[1], Version: *version, Serial: *serial, Reason: *reason})
		}, nil
	case "import":
		j := asJSON()
		typ := fs.String("type", "", "with bulk: the `TYPE` of the ids, such as aws_security_group")
		prov := fs.String("provider", "", "with bulk and --type: read the objects through the provider configuration at this `ADDR`, such as 'aws.west'")
		out := fs.String("out", "", "with bulk: the new file to write in the stack, default "+cli.DefaultTofuBulkFile)
		ids := &moves{}
		fs.Var(ids, "id", "with bulk and --type: an import `ID` to read; repeatable")
		maxObjects := fs.Int("max-objects", 0, fmt.Sprintf("with bulk: refuse to read more than `N` objects (default %d)", cli.DefaultBulkMaxObjects))
		maxBytes := fs.Int("max-bytes", 0, fmt.Sprintf("with bulk: refuse to write a file larger than `N` bytes (default %d)", cli.DefaultBulkMaxBytes))
		imports := fs.String("imports", "", "with accept: the `file` in which writ import bulk recorded the objects it selected, such as "+cli.ImportsFile(cli.DefaultTofuBulkFile))
		return func(ctx context.Context, app *cli.App, pos []string) error {
			bulkOnly := *maxObjects != 0 || *maxBytes != 0 || len(*ids) > 0 || *typ != "" || *prov != "" || *out != ""
			switch {
			case cli.AcceptsPlan(pos):
				if bulkOnly {
					return &cli.UsageError{Msg: "writ import accept PLAN takes only --imports, --json, and --dir"}
				}
				return app.ImportAcceptPlan(ctx, cli.ImportAcceptPlanOptions{JSON: *j, Plan: pos[1], Imports: *imports})
			case *imports != "":
				return &cli.UsageError{Msg: "--imports goes with writ import accept PLAN"}
			case cli.ImportsBulk(pos):
				o := cli.ImportBulkOptions{JSON: *j, Out: *out, MaxObjects: *maxObjects, MaxBytes: *maxBytes, Type: *typ, IDs: *ids, Provider: *prov}
				if len(pos) == 2 {
					o.Selection = pos[1]
				}
				return app.ImportBulk(ctx, o)
			}
			return &cli.UsageError{Msg: "import takes bulk, writ import bulk SELECTION.json or writ import bulk --type TYPE --id ID, or accept, writ import accept --imports FILE PLAN"}
		}, nil
	case "agent":
		j := asJSON()
		printOnly := fs.Bool("print", false, "write nothing; print the section of AGENTS.md and the skill files")
		force := fs.Bool("force", false, "replace skill files that differ from this writ's")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) != 1 || pos[0] != "init" {
				return &cli.UsageError{Msg: "agent takes init: writ agent init"}
			}
			return app.AgentInit(ctx, cli.AgentInitOptions{JSON: *j, Print: *printOnly, Force: *force})
		}, nil
	case "mcp":
		allow := fs.Bool("allow-providers", false, "also serve import_bulk, which starts the stack's providers with this process's environment, credentials, and file access, and let schema download a provider Writ's cache lacks")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			return app.MCP(ctx, cli.MCPOptions{AllowProviders: *allow})
		}, nil
	case "plans":
		j := asJSON()
		limit := fs.Int("limit", cli.DefaultPlansLimit, "list the newest `N` plans, 1 to 200")
		rem, stackName := remote()
		o := cli.PlansOptions{}
		fs.StringVar(&o.Status, "status", "", "the plans of this `status`, such as pending or applied")
		fs.StringVar(&o.StartedBy, "started-by", "", "the plans that `person` or `ci` started: a person's session or a CI job's token")
		fs.StringVar(&o.Cursor, "cursor", "", "the `cursor` of the next page, as the last page printed it")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			if err := remoteOnly(rem); err != nil {
				return err
			}
			o.Stack, o.Limit, o.JSON = *stackName, *limit, *j
			return app.RemotePlans(ctx, o)
		}, nil
	case "runs":
		j := asJSON()
		limit := fs.Int("limit", cli.DefaultRunsLimit, "list the newest `N` runs, 1 to 200")
		rem, stackName := remote()
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			if err := remoteOnly(rem); err != nil {
				return err
			}
			return app.RemoteRuns(ctx, cli.RemoteOptions{Stack: *stackName}, *limit, *j)
		}, nil
	case "audit":
		j := asJSON()
		limit := fs.Int("limit", cli.DefaultAuditLimit, "list at most `N` rows, 1 to 200")
		rem, stackName := remote()
		o := cli.AuditOptions{}
		fs.StringVar(&o.Plan, "plan", "", "the rows of the plan with this `id` or ref, of its run, and of the run's state writes")
		fs.StringVar(&o.Run, "run", "", "the rows of the run with this `id`")
		fs.StringVar(&o.Person, "person", "", "the rows of the person with this `id`, as writ whoami prints it")
		fs.StringVar(&o.Op, "op", "", "the rows of this `operation`, such as plan, approve, apply, or state-write")
		fs.BoolVar(&o.Changes, "changes", false, "leave out the reads: the rows of op read and state-read, and the GET requests of no op that were allowed or needed no decision")
		fs.StringVar(&o.Since, "since", "", "the rows at or after this RFC 3339 `time`")
		fs.StringVar(&o.Until, "until", "", "the rows before this RFC 3339 `time`")
		fs.StringVar(&o.Cursor, "cursor", "", "the `cursor` of the next page, as the last page printed it")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			if err := remoteOnly(rem); err != nil {
				return err
			}
			o.Stack, o.Limit, o.JSON = *stackName, *limit, *j
			return app.RemoteAudit(ctx, o)
		}, nil
	case "show":
		j := asJSON()
		rem := fs.Bool("remote", false, "show a plan writ serve keeps; show has no local form")
		request := fs.String("request", "", "with --remote: the `key` of an apply request, as writ apply --remote prints it; prints the run it started, or that none did, and closes a key with no run")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if *request != "" {
				if len(pos) != 0 || !*rem {
					return &cli.UsageError{Msg: "show --request takes no plan id: writ show --remote --request KEY"}
				}
				return app.Show(ctx, cli.ShowOptions{JSON: *j, Request: *request})
			}
			id, err := oneArg(name, "plan ID", pos)
			if err != nil {
				return err
			}
			if !*rem {
				return &cli.UsageError{Msg: "show prints a plan writ serve keeps: writ show --remote ID"}
			}
			return app.Show(ctx, cli.ShowOptions{JSON: *j, ID: id})
		}, nil
	case "whoami":
		j := asJSON()
		// whoami always asks a control plane; --remote is kept for the
		// command lines that pass it.
		fs.Bool("remote", true, "ask the control plane; whoami has no local form")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			return app.Whoami(ctx, *j)
		}, nil
	case "login":
		j := asJSON()
		issuer := fs.String("issuer", "", "the people issuer to sign in with, when the control plane has several")
		device := fs.Bool("device", false, "sign in with the device flow: type a code at the identity provider's page, in a browser anywhere")
		browser := fs.Bool("browser", false, "sign in in a browser on this computer, which the identity provider sends back to a loopback port")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) > 1 {
				return &cli.UsageError{Msg: "login takes at most one URL: writ login https://writ.example.com"}
			}
			if *device && *browser {
				return &cli.UsageError{Msg: "login takes --device or --browser, not both"}
			}
			o := cli.LoginOptions{JSON: *j, Issuer: *issuer, Device: *device, Browser: *browser}
			if len(pos) == 1 {
				o.URL = pos[0]
			}
			return app.Login(ctx, o)
		}, nil
	case "logout":
		j := asJSON()
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) > 1 {
				return &cli.UsageError{Msg: "logout takes at most one URL"}
			}
			o := cli.LogoutOptions{JSON: *j}
			if len(pos) == 1 {
				o.URL = pos[0]
			}
			return app.Logout(ctx, o)
		}, nil
	case "serve":
		cfg := fs.String("config", "", "the config `file`, in HCL")
		check := fs.Bool("check", false, "read the config, print what each team may do, and serve nothing")
		notifyTest := fs.Bool("notify-test", false, "with --check: send a test message to each notify block's channel")
		dev := fs.Bool("dev", false, "run a control plane for this user alone, on loopback, with its store in SQLite")
		data := fs.String("data", "", "with --dev: the data `directory`; default $XDG_DATA_HOME/writ/dev")
		listen := fs.String("listen", "", "with --dev: the loopback `address` to listen on; default "+cli.DevListen)
		database := fs.String("database", "", "with --dev: a postgres:// `URL` to keep the control database and state in, in place of SQLite")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			if *dev {
				if *check || *notifyTest {
					return &cli.UsageError{Msg: "--dev takes no --check or --notify-test"}
				}
				return app.ServeDev(ctx, cli.DevServeOptions{Data: *data, Listen: *listen, Database: *database, Config: *cfg, Version: version})
			}
			if *data != "" || *listen != "" || *database != "" {
				return &cli.UsageError{Msg: "--data, --listen, and --database go with --dev; a team's writ serve takes them from its config"}
			}
			return app.Serve(ctx, cli.ServeOptions{Config: *cfg, Check: *check, NotifyTest: *notifyTest, Version: version})
		}, nil
	case "repo":
		j := asJSON()
		p := fs.String("path", "", "with add: the `directory` in the project that holds the stacks; default its root, or for a local directory that directory")
		prefix := fs.String("prefix", "", "with add: put `PREFIX` before each stack's name, such as payments/, when another repository has a stack of the same name")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			switch {
			case len(pos) == 3 && pos[0] == "add":
				return app.RepoAdd(ctx, cli.RepoAddOptions{JSON: *j, Name: pos[1], Location: pos[2], Path: *p, Prefix: *prefix})
			case len(pos) == 2 && pos[0] == "add":
				return app.RepoAdd(ctx, cli.RepoAddOptions{JSON: *j, Location: pos[1], Path: *p, Prefix: *prefix})
			case *p != "" || *prefix != "":
				return &cli.UsageError{Msg: "--path and --prefix go with repo add"}
			case len(pos) == 1 && pos[0] == "list":
				return app.RepoList(ctx, *j)
			case len(pos) == 2 && pos[0] == "show":
				return app.RepoShow(ctx, pos[1], *j)
			}
			return &cli.UsageError{Msg: "repo takes add NAME FORGE/PROJECT, add DIR, list, or show NAME"}
		}, nil
	case "ci":
		repo := fs.String("repo", "", "the `repository` the CI jobs plan, as writ repo list names it")
		envs := fs.String("envs", "", "a comma-separated `list` of environments; default every one writ serve serves")
		image := fs.String("image", "registry.example.com/writ:"+version, "the `image` whose PATH holds the writ binary")
		component := fs.String("component", cli.DefaultComponent, "the GitLab CI/CD component to include, `PATH@VERSION`")
		writVersion := fs.String("writ-version", "", "the Writ `release` a GitHub Actions job installs, v0.1.9 or later; default this writ's version when it is a release")
		runsOn := fs.String("runs-on", "", "the `label` of the runner a GitHub Actions job runs on; default ubuntu-24.04 at github.com, required elsewhere")
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if len(pos) != 1 || pos[0] != "gitlab" && pos[0] != "github" {
				return &cli.UsageError{Msg: "ci takes gitlab or github: writ ci gitlab --repo NAME, or writ ci github --repo NAME"}
			}
			if *repo == "" {
				return &cli.UsageError{Msg: "ci " + pos[0] + " needs --repo, a repository writ repo list names"}
			}
			var list []string
			for e := range strings.SplitSeq(*envs, ",") {
				if e = strings.TrimSpace(e); e != "" {
					list = append(list, e)
				}
			}
			return app.CI(ctx, pos[0], cli.CIOptions{Repo: *repo, Envs: list, Image: *image, Component: *component, WritVersion: *writVersion, RunsOn: *runsOn})
		}, nil
	case "ui":
		j := asJSON()
		return func(ctx context.Context, app *cli.App, pos []string) error {
			if err := noArgs(name, pos); err != nil {
				return err
			}
			return app.UI(ctx, *j)
		}, nil
	}
	return nil, fmt.Errorf("unknown command %q", name)
}

func noArgs(name string, pos []string) error {
	if len(pos) > 0 {
		return &cli.UsageError{Msg: fmt.Sprintf("%s takes no arguments, got %q", name, pos)}
	}
	return nil
}

func oneArg(name, what string, pos []string) (string, error) {
	if len(pos) != 1 {
		return "", &cli.UsageError{Msg: fmt.Sprintf("%s takes one %s", name, what)}
	}
	return pos[0], nil
}
