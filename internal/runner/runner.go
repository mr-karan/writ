// Package runner runs writ serve's OpenTofu runs as jobs. A job is plain
// data: the commit's files, the environment and credentials the run gets,
// its egress, and the saved plan or state it reads. An Executor runs a job
// and returns its raw result with the record of its network. Local is the
// executor that runs a job in writ serve's own process.
//
// Nothing of writ serve's own crosses into a job: no key, no database, no
// forge token, no handler, and no uid. writ serve masks what comes back,
// seals the saved plan, computes the plan id, and evaluates policy. This
// package does none of that, and imports no package that does.
package runner

import (
	"context"
	"strconv"

	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/tofu"
)

// Kind is which OpenTofu run a job is.
type Kind string

const (
	// Plan runs init, plan, and show of the saved plan.
	Plan Kind = "plan"
	// Apply runs init, show of the job's saved plan, and its apply.
	Apply Kind = "apply"
	// Show runs init without the backend, and show of the job's state.
	Show Kind = "show"
	// Pull runs init, state pull from the stack's backend, and show.
	Pull Kind = "pull"
)

// Job is one OpenTofu run of writ serve.
type Job struct {
	// ID names the run: its work directory, and the limit its requests
	// to the state backend count against.
	ID   string
	Kind Kind
	// Files are the commit's files that repo.TofuSources gathered, by
	// path, and Dir is the stack's directory among them. writ serve
	// checked each path.
	Files map[string][]byte
	Dir   string
	// Lock is the stack's lock file, whose providers the job installs
	// into the plugin cache before tofu runs.
	Lock []byte
	// Env is tofu's environment for every step, and Creds the
	// credentials that plan and apply add to it, as tofu.Stack takes
	// them. They hold the env's provider_env values and the run's
	// tokens: the state backend's and the credential endpoint's.
	Env, Creds []string
	// StateCreds marks a stack whose state backend, such as S3, tofu
	// reaches with Creds.
	StateCreds bool
	Egress     Egress
	// SavedPlan is the saved plan an Apply applies, which writ serve
	// opened and checked against the plan's id.
	SavedPlan []byte
	// State is the state file a Show reads, which writ serve opened.
	State []byte
}

// Egress is how a job reaches the network.
type Egress struct {
	// Mode is the config's egress_mode. Under report or enforce, each
	// step runs in a network namespace of its own, behind an egress
	// proxy of the job's own.
	Mode egress.Mode
	// List is the env's egress list, which the proxy holds the job to.
	List egress.List
	// Registries are the config's provider_registries: under report or
	// enforce, the provider install asks no other registry.
	Registries []string
}

// Result is a job's raw result, which only writ serve's mask may read.
type Result struct {
	// Plan is the result of a Plan, Applied of an Apply, and State of a
	// Show or a Pull.
	Plan    tofu.Plan
	Applied tofu.Applied
	State   tofu.State
	// Network is the record of the job's egress proxies, nil under
	// egress_mode off.
	Network *Network
}

// Network is every destination a job asked its egress proxies for, raw:
// a job chooses its hosts, and can spell a secret in one.
type Network struct {
	Mode egress.Mode
	// Install are the destinations of the provider install, and Run
	// those of tofu's steps.
	Install, Run []egress.Destination
	// Unrecorded counts the requests past the proxies' limit of named
	// destinations.
	Unrecorded int
}

// Executor runs jobs.
type Executor interface {
	// Reserve waits for room to run one job until ctx ends. A run of writ
	// serve reserves its slot before it issues the run's tokens.
	Reserve(ctx context.Context) (Slot, error)
}

// Slot is room for one job.
type Slot interface {
	// Run runs job: it installs the job's providers under start, and runs
	// tofu under ctx. A job that failed before tofu ran is a *StartError.
	// The result holds the network record even when Run fails, and what
	// tofu wrote when tofu fails.
	Run(start, ctx context.Context, job Job) (Result, error)
	// Release ends what still runs in the slot and gives it back.
	Release()
}

// Tofu runs OpenTofu for the stacks of .tf files. *tofu.Runner is one.
type Tofu interface {
	// Fill installs the providers a lock file pins into the plugin cache
	// that every run reads, and returns the lock file a run's init reads,
	// which a tofu.Stack's Lock takes. With net, tofu init runs in a
	// network namespace of its own whose ports net serves, with env; a
	// nil net keeps writ serve's own network.
	Fill(ctx context.Context, lock []byte, net *tofu.Net, env []string) ([]byte, error)
	// Plan runs init, plan, and show of one stack and returns the raw
	// result, which only the mask may read.
	Plan(ctx context.Context, s tofu.Stack) (tofu.Plan, error)
	// Apply runs init, show of a saved plan, and apply, and returns the
	// raw show and `tofu apply -json` event stream, which only the mask
	// may read.
	Apply(ctx context.Context, s tofu.Stack, plan []byte) (tofu.Applied, error)
	// Show runs init without the backend and show of a state, and returns
	// the raw `tofu show -json`, which only the mask may read.
	Show(ctx context.Context, s tofu.Stack, state []byte) (tofu.State, error)
	// Pull runs init, state pull from the stack's backend, and show of
	// the pulled state, and returns the raw `tofu show -json`, which only
	// the mask may read, with the state's serial.
	Pull(ctx context.Context, s tofu.Stack) (tofu.State, error)
}

// StartError is a job that failed before tofu ran: its files were not
// written, or its provider install failed. Err says which.
type StartError struct{ Err error }

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Refusal is a job that an executor does not run, in writ's own words:
// what the config or the job must change, or why no slot came before the
// job's context ended.
type Refusal struct{ Err error }

func (e *Refusal) Error() string { return e.Err.Error() }
func (e *Refusal) Unwrap() error { return e.Err }

// The ports of 127.0.0.1 that a job's own network serves under
// egress_mode report or enforce. Each step has a namespace of its own, so
// the ports are the same for every job.
const (
	// EgressPort is the egress proxy, which HTTP_PROXY and HTTPS_PROXY
	// point to.
	EgressPort = 3128
	// CredsPort is the credential endpoint of awsrole.Broker.
	CredsPort = 3129
	// StatePort is the http state backend, /v1/tfstate/ and nothing else.
	StatePort = 3130
)

// Loopback is where a job with its own network reaches its ports.
const Loopback = "http://127.0.0.1:"

// EgressEnv points every HTTP client of a job at the egress proxy. Both
// spellings are set: curl, which git uses, reads only the lowercase
// http_proxy, and Go reads either. Loopback goes direct, to the
// credential endpoint and the state backend.
var EgressEnv = func() []string {
	proxy := Loopback + strconv.Itoa(EgressPort)
	const direct = "127.0.0.1,localhost,::1"
	return []string{
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=" + direct, "no_proxy=" + direct,
	}
}()
