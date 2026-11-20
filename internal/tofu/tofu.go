// Package tofu runs OpenTofu for one stack: init, a saved plan, its JSON,
// and the apply of a saved plan, each run in a scratch directory of its
// own, with bounded time and output, and as the run's own uid when it has
// one. On Linux every tofu process sees a private view of the filesystem
// that holds the run's directory and none of the host's other files, and
// a run with Stack.Net has a network namespace of its own that reaches
// only the ports writ serves in it. It is the only package that starts
// tofu. Fill installs a lock file's providers into the plugin cache that
// every run shares, on writ's own network.
//
// What a run returns is raw: the saved plan, the plan's JSON, and tofu's
// output hold every secret the stack uses. Only the mask may turn them into
// a document that leaves the run.
package tofu

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// Version is the one OpenTofu version Writ runs. Its flags, its JSON, and
// its refusal of a stale plan are tested on this version and no other.
const Version = "1.13.9"

var (
	// ErrVersion means the binary is not OpenTofu Version.
	ErrVersion = errors.New("not OpenTofu " + Version)
	// ErrOutputLimit means a step wrote more than Options.MaxOutput to its
	// stdout or stderr, or saved a larger plan. The step is killed.
	ErrOutputLimit = errors.New("tofu wrote more output than the limit")
	// ErrStale means tofu refused a saved plan because the state changed
	// after the plan was made.
	ErrStale = errors.New("the saved plan is stale: the state changed after the plan was made")
)

// Options configure a Runner. Every field but Git is required.
type Options struct {
	// Binary is the tofu executable.
	Binary string
	// Git, when set, is the git executable that each run's view holds, so
	// tofu init can fetch a module whose source needs git over https.
	// Without it such a source fails.
	Git string
	// PluginCache is the provider plugin cache that every run shares. A
	// run as another User reads it and cannot write it, so a run as writ's
	// own user must fill it first.
	PluginCache string
	// ScratchRoot holds each run's scratch directory. It must belong to
	// writ's user, and every directory above it to root or writ's user.
	// It and every directory above it must be writable only by their
	// owner, or have the sticky bit. A run as another User makes its
	// directory there, so give a root for such runs the mode
	// os.ModeSticky|0o777, as /tmp has. New opens it once, and every run
	// reaches it through that, wherever it is moved afterwards.
	ScratchRoot string
	// Timeout bounds one Plan, every step together.
	Timeout time.Duration
	// ApplyTimeout bounds one Apply, its init and its apply together.
	ApplyTimeout time.Duration
	// MaxOutput bounds each step's stdout and its stderr, and the saved
	// plan, in bytes.
	MaxOutput int
	// Limits bound each step of a run. Memory, Pids, and CPUs hold only
	// where New can give each step a cgroup of its own (Enforcement says
	// whether it could); Scratch holds everywhere.
	Limits Limits
}

// Runner starts tofu. It holds no state between runs but the open
// scratch root, which Close releases, and the hashes Fill recorded.
type Runner struct {
	opts Options
	root *os.Root
	git  *gitTools
	// cgroups is what writ made of the cgroups, with base the cgroup
	// under which each step gets one of its own, "" when Options.Limits
	// sets none or this process cannot make them, and cgroupOff is why
	// it cannot.
	cgroups   cgroupSetup
	cgroupOff string
	// cache is held by Fill to write the plugin cache, and by each run's
	// init to read it.
	cache sync.RWMutex
	// hashes are the hashes Fill recorded of a provider version that a
	// lock pinned with none, by source and version. Fill holds cache to
	// read or write them.
	hashes map[string][]string
}

// New checks opts, that the scratch root is safe from other users, that
// opts.Binary is OpenTofu Version, and that opts.Git, when set, has what a
// clone over https runs. When opts.Limits sets Memory, Pids, or CPUs, the
// first New of the process moves it into a leaf of its own cgroup and
// gives that cgroup's children the controllers, if it may; a Runner that
// cannot runs each step without them, and Enforcement says why.
func New(ctx context.Context, opts Options) (*Runner, error) {
	switch {
	case opts.Binary == "":
		return nil, errors.New("tofu: no binary")
	case opts.PluginCache == "":
		return nil, errors.New("tofu: no plugin cache")
	case opts.ScratchRoot == "":
		return nil, errors.New("tofu: no scratch root")
	case opts.Timeout <= 0:
		return nil, errors.New("tofu: no timeout")
	case opts.ApplyTimeout <= 0:
		return nil, errors.New("tofu: no apply timeout")
	case opts.MaxOutput <= 0:
		return nil, errors.New("tofu: no output limit")
	}
	// A run's view holds the cache at this path, as TF_PLUGIN_CACHE_DIR
	// names it.
	cache, err := filepath.Abs(opts.PluginCache)
	if err != nil {
		return nil, fmt.Errorf("tofu: the plugin cache: %w", err)
	}
	opts.PluginCache = cache
	root, err := openScratchRoot(opts.ScratchRoot)
	if err != nil {
		return nil, err
	}
	if err := CheckVersion(ctx, opts.Binary); err != nil {
		root.Close()
		return nil, err
	}
	r := &Runner{opts: opts, root: root}
	if opts.Limits.cgroup() {
		if r.cgroups, err = setUpCgroups(); err != nil {
			r.cgroupOff = err.Error()
		}
	}
	if opts.Git != "" {
		if r.git, err = findGit(ctx, opts.Git); err != nil {
			root.Close()
			return nil, err
		}
	}
	return r, nil
}

// Close releases the scratch root. r must run nothing after it.
func (r *Runner) Close() error { return r.root.Close() }

// CheckVersion reads the first line of `tofu version`, which names the
// product, and refuses a binary that is not OpenTofu Version. `tofu
// version -json` does not name the product, and Terraform has a 1.13 too.
// The error leaves out what the binary printed, which writ does not trust.
func CheckVersion(ctx context.Context, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, _, err := command{
		binary: binary,
		args:   []string{"version"},
		env:    []string{"CHECKPOINT_DISABLE=1", "TF_CLI_CONFIG_FILE=" + os.DevNull},
		max:    64 << 10,
	}.run(ctx)
	if err != nil {
		return fmt.Errorf("run %s version: %w", binary, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	if strings.TrimSpace(first) != "OpenTofu v"+Version {
		return fmt.Errorf("%s: %w", binary, ErrVersion)
	}
	return nil
}

// User is an OS user and group with no name, as writ serve gives each run.
type User = runas.User

// Stack is one stack's files and how its run starts.
type Stack struct {
	// Files are the commit's files: the stack, the local modules it calls,
	// and its .terraform.lock.hcl. They are copied into the scratch
	// directory. A symbolic link among them is refused.
	Files fs.FS
	// Dir is the stack's directory within Files, "." for its root.
	Dir string
	// Lock, when set, is the lock file that Fill returned, which the run's
	// init reads in place of the one in Files.
	Lock []byte
	// Env is tofu's environment for every step, such as PATH and the
	// state backend's address. Nothing else comes from writ's own
	// environment. Each step adds tofu's and git's settings, and puts
	// Options.Git's directory first on PATH.
	Env []string
	// Creds are the run's cloud credentials, which plan and apply add to
	// Env. Init fetches the modules and providers without them.
	Creds []string
	// StateCreds marks a stack whose state backend, such as S3, needs
	// Creds to be reached. After the init that fetches without them, a
	// second init configures the backend with them, and Apply and Pull
	// read the state with them. Without it, only plan and apply get Creds.
	StateCreds bool
	// User, when set, is the uid every process of the run runs as. It
	// may not be uid 0 or one of writ's own uids.
	User *User
	// Net, when set, gives every step of the run a network namespace of
	// its own. Without it, every step shares writ's network.
	Net *Net
}

// Plan is a plan's raw result.
type Plan struct {
	// File is the saved plan, which Apply takes.
	File []byte
	// JSON is `tofu show -json` of the saved plan.
	JSON []byte
	// Log is the `tofu plan -json` event stream.
	Log []byte
}

// StepError is a step that exited with an error. Its Error text names the
// step, the exit code, and Reason, which is writ's own words, because
// tofu's output may hold a secret.
type StepError struct {
	Step           string
	Code           int
	Stdout, Stderr []byte
	// Reason, when set, is what writ found the failure to need.
	Reason string
	// Limit, when set, is the limit of Options.Limits the step reached,
	// which ended it.
	Limit string
	// Shared reports that Limit is one that every step shares, which may
	// have refused another step, not this one: the error then keeps the
	// exit code and Reason, which may say why this step failed.
	Shared bool
}

func (e *StepError) Error() string {
	if e.Limit != "" && !e.Shared {
		return fmt.Sprintf("tofu %s stopped at a limit: %s", e.Step, e.Limit)
	}
	msg := fmt.Sprintf("tofu %s exited with code %d", e.Step, e.Code)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	if e.Limit != "" {
		msg += "; " + e.Limit
	}
	return msg
}

const planFile = "tfplan"

// Plan runs init, a saved plan without the state lock, and show -json, in
// a scratch directory that is removed when Plan returns. It reads the
// saved plan once plan has ended, when no process of the run is left, and
// show reads those bytes as a sealed file (sealPlan): a provider that show
// starts may write what the run may, and the JSON must be of the File that
// Plan returns.
func (r *Runner) Plan(ctx context.Context, s Stack) (Plan, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	sc, err := r.newScratch(s)
	if err != nil {
		return Plan{}, err
	}
	defer sc.remove()
	if err := r.init(ctx, sc); err != nil {
		return Plan{}, err
	}
	log, err := r.step(ctx, sc, true, "plan", "plan", "-input=false", "-lock=false", "-out="+planFile, "-json")
	if err != nil {
		return Plan{}, err
	}
	file, err := sc.read(planFile, r.opts.MaxOutput)
	if err != nil {
		return Plan{}, err
	}
	saved, err := sc.sealPlan(file)
	if err != nil {
		return Plan{}, err
	}
	js, err := r.step(ctx, sc, true, "show", "show", "-json", saved)
	if err != nil {
		return Plan{}, err
	}
	return Plan{File: file, JSON: js, Log: log}, nil
}

// Applied is an apply's raw result, which only the mask may read.
type Applied struct {
	// Plan is `tofu show -json` of the saved plan, from which the mask
	// learns the plan's secrets before it reads Log.
	Plan []byte
	// Log is the `tofu apply -json` event stream.
	Log []byte
	// State is the state the apply left, read after an apply that
	// succeeded, or after any apply of a stack with StateCreds. ShowErr
	// is why that read failed, which fails no apply.
	State   State
	ShowErr error
}

// State is `tofu show -json` of a state. It holds every value the state
// holds, and only the mask may read Unmasked.
type State struct {
	Unmasked []byte
	// Serial names the state, when it was read from a stack whose
	// StateCreds is set.
	Serial Serial
}

// Serial is a state's lineage and serial number, which OpenTofu raises at
// each write of the state. Serial 0 is no state.
type Serial struct {
	Lineage string
	Serial  int64
}

// ApplyStop is how long an apply has to stop once Options.ApplyTimeout
// passes. tofu, sent SIGINT, starts no more changes, waits for the ones
// running, and writes the state they left; a kill would lose that state
// and leave the objects in doubt. It is killed after ApplyStop.
const ApplyStop = 2 * time.Hour

// Apply runs init, show -json of plan, a Plan's File, and then applies
// plan, and after an apply that succeeded, show -json of the state it
// left, in a scratch directory that is removed when Apply returns. Both
// show and apply read plan as a sealed file (sealPlan), so a provider that
// show starts cannot change what apply applies. s must hold the files the
// plan was made from. Init and both shows get no
// credentials. At Options.ApplyTimeout, init and the shows are killed,
// and apply is interrupted and has ApplyStop to end. On an error from
// apply, it returns what apply wrote too. For a stack with StateCreds,
// the backend's init gets the credentials, and after apply ends, however
// it ends, Apply pulls the state with them and shows the pulled file, in
// Options.Timeout of its own.
func (r *Runner) Apply(ctx context.Context, s Stack, plan []byte) (Applied, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.ApplyTimeout)
	defer cancel()
	sc, err := r.newScratch(s)
	if err != nil {
		return Applied{}, err
	}
	defer sc.remove()
	saved, err := sc.sealPlan(plan)
	if err != nil {
		return Applied{}, err
	}
	if err := r.init(ctx, sc); err != nil {
		return Applied{}, err
	}
	js, err := r.step(ctx, sc, false, "show", "show", "-json", saved)
	if err != nil {
		return Applied{}, err
	}
	sc.stop = ApplyStop
	log, err := r.step(ctx, sc, true, "apply", "apply", "-input=false", "-json", saved)
	var se *StepError
	if errors.As(err, &se) && hasError(se.Stdout, "Saved plan is stale") {
		err = fmt.Errorf("%w: %w", ErrStale, err)
	}
	sc.stop = 0
	applied := Applied{Plan: js, Log: log}
	if s.StateCreds {
		// A failed apply may have written state too, and writ serve must
		// learn the serial it reached, so the read has a time of its own
		// past the apply's.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.opts.Timeout)
		defer cancel()
		applied.State, applied.ShowErr = r.pullShow(readCtx, sc)
		return applied, err
	}
	if err != nil {
		return applied, err
	}
	if st, err := r.step(ctx, sc, false, "show", "show", "-json"); err != nil {
		applied.ShowErr = err
	} else {
		applied.State.Unmasked = st
	}
	return applied, nil
}

// Pull runs init, `tofu state pull` of the stack's state from its backend,
// and show -json of what it pulled, in a scratch directory that is removed
// when Pull returns. A stack with no state yet returns a State with
// serial 0 and no JSON. Only init and the pull get s.Creds, when
// s.StateCreds is set; show reads the pulled file and reaches no backend.
func (r *Runner) Pull(ctx context.Context, s Stack) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	sc, err := r.newScratch(s)
	if err != nil {
		return State{}, err
	}
	defer sc.remove()
	if err := r.init(ctx, sc); err != nil {
		return State{}, err
	}
	return r.pullShow(ctx, sc)
}

// pullShow pulls the state of an initialized stack and shows it.
func (r *Runner) pullShow(ctx context.Context, sc *scratch) (State, error) {
	raw, err := r.step(ctx, sc, sc.stateCreds, "state pull", "state", "pull")
	if err != nil {
		return State{}, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return State{}, nil
	}
	serial, err := stateSerial(raw)
	if err != nil {
		return State{}, fmt.Errorf("tofu state pull: %w", err)
	}
	if serial.Serial == 0 {
		return State{}, nil
	}
	if err := r.showState(ctx, sc, raw); err != nil {
		return State{}, err
	}
	js, err := r.step(ctx, sc, false, "show", "show", "-json", stateFile)
	if err != nil {
		return State{}, err
	}
	return State{Unmasked: js, Serial: serial}, nil
}

// showState writes state for show to read, with its providers named as
// the stack's lock file pins them (StateForLock).
func (r *Runner) showState(ctx context.Context, sc *scratch, state []byte) error {
	lock, err := sc.read(LockFile, r.opts.MaxOutput)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if state, err = StateForLock(state, lock); err != nil {
			return fmt.Errorf("%s: %w", LockFile, err)
		}
	}
	return sc.write(stateFile, state)
}

// stateSerial reads the lineage and serial of a state file and nothing
// else of it. OpenTofu writes an empty state, of a stack that has none,
// with serial 0 and no lineage.
func stateSerial(raw []byte) (Serial, error) {
	var st struct {
		Lineage string `json:"lineage"`
		Serial  *int64 `json:"serial"`
	}
	err := json.Unmarshal(raw, &st)
	switch {
	case err != nil || st.Serial == nil || *st.Serial < 0 || *st.Serial > 0 && st.Lineage == "":
		return Serial{}, errors.New("the state has no lineage and serial that writ can read; an encrypted state is not supported on this backend")
	case *st.Serial == 0:
		return Serial{}, nil
	}
	return Serial{Lineage: st.Lineage, Serial: *st.Serial}, nil
}

// PlanSerial returns the lineage and serial of the state a saved plan was
// made from, which OpenTofu checks against the backend's state before it
// applies the plan. A plan of a stack with no state returns serial 0.
func PlanSerial(plan []byte) (Serial, error) {
	zr, err := zip.NewReader(bytes.NewReader(plan), int64(len(plan)))
	if err != nil {
		return Serial{}, fmt.Errorf("read the saved plan: %w; an encrypted saved plan is not supported on this backend", err)
	}
	for _, f := range zr.File {
		if f.Name != "tfstate" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return Serial{}, fmt.Errorf("read the saved plan's state: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(rc, int64(len(plan))*maxPlanRatio))
		rc.Close()
		if err != nil {
			return Serial{}, fmt.Errorf("read the saved plan's state: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return Serial{}, nil
		}
		s, err := stateSerial(raw)
		if err != nil {
			return Serial{}, fmt.Errorf("the saved plan's state: %w", err)
		}
		return s, nil
	}
	return Serial{}, errors.New("the saved plan holds no state")
}

// maxPlanRatio bounds how far the saved plan's state may inflate.
const maxPlanRatio = 64

const stateFile = "writ.tfstate"

// Show runs init without the state backend and show -json of state, a
// state file as the backend stores it, in a scratch directory that is
// removed when Show returns. s must hold files whose lock file pins the
// state's providers, whose schemas show reads. Neither step gets
// credentials or reaches a state backend.
func (r *Runner) Show(ctx context.Context, s Stack, state []byte) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	sc, err := r.newScratch(s)
	if err != nil {
		return State{}, err
	}
	defer sc.remove()
	if err := r.init(ctx, sc, "-backend=false"); err != nil {
		return State{}, err
	}
	if err := r.showState(ctx, sc, state); err != nil {
		return State{}, err
	}
	js, err := r.step(ctx, sc, false, "show", "show", "-json", stateFile)
	if err != nil {
		return State{}, err
	}
	return State{Unmasked: js}, nil
}

// init runs `tofu init`, which reads the plugin cache, so no Fill writes
// it meanwhile. For a stack whose backend needs the run's credentials, a
// first init without the backend fetches the modules and providers with
// none, and a second, which fetches nothing, configures the backend with
// them, so no module host or getter runs where they are.
func (r *Runner) init(ctx context.Context, sc *scratch, flags ...string) error {
	r.cache.RLock()
	defer r.cache.RUnlock()
	args := []string{"init", "-input=false", "-lockfile=readonly", "-no-color"}
	backend := sc.stateCreds && !slices.Contains(flags, "-backend=false")
	first := flags
	if backend {
		first = append(slices.Clone(flags), "-backend=false")
	}
	_, err := r.step(ctx, sc, false, "init", append(slices.Clone(args), first...)...)
	if se, ok := errors.AsType[*StepError](err); ok && r.git == nil && needsGit(se.Stderr) {
		se.Reason = "a module source needs git, which a run has only when writ serve's tofu block sets git"
	}
	if err != nil || !backend {
		return err
	}
	_, err = r.step(ctx, sc, true, "init", append(append(args, flags...), "-get=false")...)
	return err
}

// hasError reports whether a -json event stream holds an error diagnostic
// with this summary.
func hasError(log []byte, summary string) bool {
	for line := range bytes.Lines(log) {
		var ev struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
			} `json:"diagnostic"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "diagnostic" && ev.Diagnostic.Severity == "error" && ev.Diagnostic.Summary == summary {
			return true
		}
	}
	return false
}
