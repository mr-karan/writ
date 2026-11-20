package runner_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/egress"
	"github.com/mr-karan/writ/internal/runner"
	"github.com/mr-karan/writ/internal/tofu"
)

// fakeTofu records what each call of runner.Tofu gets.
type fakeTofu struct {
	mu        sync.Mutex
	fills     []fillCall
	stacks    []tofu.Stack
	files     [][]string
	savedPlan []byte
	state     []byte
	fillErr   error
}

type fillCall struct {
	lock []byte
	net  *tofu.Net
	env  []string
}

func (f *fakeTofu) Fill(_ context.Context, lock []byte, n *tofu.Net, env []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fills = append(f.fills, fillCall{lock, n, env})
	if f.fillErr != nil {
		return nil, f.fillErr
	}
	return append(slices.Clone(lock), "# filled\n"...), nil
}

func (f *fakeTofu) ran(s tofu.Stack) {
	var names []string
	_ = fs.WalkDir(s.Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(s.Files, p)
			names = append(names, p+"="+string(b))
		}
		return err
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stacks, f.files = append(f.stacks, s), append(f.files, names)
}

func (f *fakeTofu) Plan(_ context.Context, s tofu.Stack) (tofu.Plan, error) {
	f.ran(s)
	return tofu.Plan{File: []byte("saved"), JSON: []byte(`{}`), Log: []byte("log")}, nil
}

func (f *fakeTofu) Apply(_ context.Context, s tofu.Stack, plan []byte) (tofu.Applied, error) {
	f.ran(s)
	f.mu.Lock()
	f.savedPlan = plan
	f.mu.Unlock()
	return tofu.Applied{Plan: []byte(`{}`), Log: []byte("events")}, &tofu.StepError{Step: "apply", Code: 1}
}

func (f *fakeTofu) Show(_ context.Context, s tofu.Stack, state []byte) (tofu.State, error) {
	f.ran(s)
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	return tofu.State{Unmasked: []byte("shown")}, nil
}

func (f *fakeTofu) Pull(_ context.Context, s tofu.Stack) (tofu.State, error) {
	f.ran(s)
	return tofu.State{Unmasked: []byte("pulled"), Serial: tofu.Serial{Lineage: "l", Serial: 3}}, nil
}

// handler is a handler that a test tells apart by its name.
type handler string

func (handler) ServeHTTP(http.ResponseWriter, *http.Request) {}

const lockFile = `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.7.2"
}
`

func job(kind runner.Kind) runner.Job {
	return runner.Job{ID: "run-1", Kind: kind, Dir: "app", Lock: []byte(lockFile),
		Files: map[string][]byte{"app/main.tf": []byte("# main\n"), "modules/m/main.tf": []byte("# module\n"), "app/.terraform.lock.hcl": []byte(lockFile)},
		Env:   []string{"PATH=/bin", "TF_HTTP_PASSWORD=state-token"}, Creds: []string{"NOMAD_TOKEN=n"}}
}

func local(t *testing.T, f *fakeTofu) *runner.Local {
	return &runner.Local{Tofu: f, Work: filepath.Join(t.TempDir(), "work"), Relay: func(string) (http.Handler, http.Handler) {
		t.Error("a job with no network of its own asked for the relay")
		return nil, nil
	}}
}

// A job runs in writ serve's process: its files in a work directory that
// is gone after the run, its lock file filled first and the filled lock
// file given to init, and the run of its kind with the job's environment
// and credentials, on writ's own network.
func TestLocalRunsEachKind(t *testing.T) {
	for _, kind := range []runner.Kind{runner.Plan, runner.Apply, runner.Show, runner.Pull} {
		t.Run(string(kind), func(t *testing.T) {
			f := &fakeTofu{}
			l := local(t, f)
			slot, err := l.Reserve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer slot.Release()
			j := job(kind)
			j.SavedPlan, j.State = []byte("the saved plan"), []byte("the state")
			res, err := slot.Run(context.Background(), context.Background(), j)
			switch kind {
			case runner.Plan:
				if err != nil || string(res.Plan.File) != "saved" {
					t.Fatalf("plan: %+v %v", res, err)
				}
			case runner.Apply:
				if _, ok := errors.AsType[*tofu.StepError](err); !ok || string(res.Applied.Log) != "events" || string(f.savedPlan) != "the saved plan" {
					t.Fatalf("apply: %+v %v, applied %q", res, err, f.savedPlan)
				}
			case runner.Show:
				if err != nil || string(res.State.Unmasked) != "shown" || string(f.state) != "the state" {
					t.Fatalf("show: %+v %v, shown %q", res, err, f.state)
				}
			case runner.Pull:
				if err != nil || res.State.Serial.Serial != 3 {
					t.Fatalf("pull: %+v %v", res, err)
				}
			}
			if res.Network != nil {
				t.Errorf("a job under egress_mode off has a network record: %+v", res.Network)
			}
			if len(f.fills) != 1 || string(f.fills[0].lock) != lockFile || f.fills[0].net != nil || f.fills[0].env != nil {
				t.Fatalf("fills %+v, want one of the job's lock file on writ's network", f.fills)
			}
			s := f.stacks[0]
			if s.Dir != "app" || string(s.Lock) != lockFile+"# filled\n" || !slices.Equal(s.Env, j.Env) || !slices.Equal(s.Creds, j.Creds) || s.User != nil || s.Net != nil || s.StateCreds {
				t.Fatalf("the stack: %+v", s)
			}
			want := []string{"app/.terraform.lock.hcl=" + lockFile, "app/main.tf=# main\n", "modules/m/main.tf=# module\n"}
			if !slices.Equal(f.files[0], want) {
				t.Errorf("tofu read the files %q, want %q", f.files[0], want)
			}
			if _, err := os.Stat(filepath.Join(l.Work, "run-1")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the work directory after the run: %v, want none", err)
			}
		})
	}
}

// A failed install comes back as a FillError before tofu runs.
func TestLocalFillFails(t *testing.T) {
	f := &fakeTofu{fillErr: &tofu.StepError{Step: "init", Code: 1}}
	slot, err := local(t, f).Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	_, err = slot.Run(context.Background(), context.Background(), job(runner.Plan))
	fe, ok := errors.AsType[*runner.FillError](err)
	if _, step := errors.AsType[*tofu.StepError](err); !ok || !step || err.Error() != "install the providers of .terraform.lock.hcl into the plugin cache: tofu init exited with code 1" {
		t.Fatalf("err %v (%T %v), want a FillError of the step", err, fe, ok)
	}
	if len(f.stacks) != 0 {
		t.Errorf("tofu ran after a failed install")
	}
}

// Under egress_mode report, each step gets a network of its own: the
// job's proxy, and the state backend and the credential endpoint through
// the relay of the job's id. The install runs behind a proxy of its own,
// and the result records both proxies.
func TestLocalEgress(t *testing.T) {
	f := &fakeTofu{}
	state, creds := handler("state"), handler("creds")
	var relayed []string
	l := local(t, f)
	l.Relay = func(id string) (http.Handler, http.Handler) {
		relayed = append(relayed, id)
		return state, creds
	}
	slot, err := l.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	j := job(runner.Plan)
	j.Egress = runner.Egress{Mode: egress.Report, Registries: []string{"registry.opentofu.org"}}
	res, err := slot.Run(context.Background(), context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if fill := f.fills[0]; fill.net == nil || len(fill.net.Ports) != 1 || fill.net.Ports[3128] == nil || !slices.Equal(fill.env, runner.EgressEnv) {
		t.Fatalf("the install ran with %+v, want its own proxy on 3128 and the proxy's environment", fill)
	}
	n := f.stacks[0].Net
	if n == nil || len(n.Ports) != 3 || n.Ports[3128] == nil || n.Ports[3128] == f.fills[0].net.Ports[3128] || n.Ports[3129] != creds || n.Ports[3130] != state {
		t.Fatalf("the run's network: %+v", n)
	}
	if !slices.Equal(relayed, []string{"run-1"}) {
		t.Errorf("the relay was asked for %q, want the job's id once", relayed)
	}
	if res.Network == nil || res.Network.Mode != egress.Report || len(res.Network.Install) != 0 || len(res.Network.Run) != 0 {
		t.Errorf("the network record: %+v", res.Network)
	}

	// Without a credential endpoint, the run's network has no port 3129.
	l.Relay = func(string) (http.Handler, http.Handler) { return state, nil }
	slot2, err := l.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slot2.Release()
	if _, err := slot2.Run(context.Background(), context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if n := f.stacks[1].Net; len(n.Ports) != 2 || n.Ports[3129] != nil {
		t.Errorf("the run's network without a credential endpoint: %+v", n)
	}
}

// Under egress_mode report or enforce, a provider from a registry that
// provider_registries does not list is refused before any install.
func TestLocalRefusesUnlistedRegistry(t *testing.T) {
	f := &fakeTofu{}
	slot, err := local(t, f).Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer slot.Release()
	j := job(runner.Plan)
	j.Egress = runner.Egress{Mode: egress.Enforce, Registries: []string{"registry.example.com", "mirror.example.com"}}
	res, err := slot.Run(context.Background(), context.Background(), j)
	_, refused := errors.AsType[*runner.Refusal](err)
	want := "install the providers of .terraform.lock.hcl into the plugin cache: provider registry.opentofu.org/hashicorp/random is from registry registry.opentofu.org, which provider_registries does not list: " +
		"under egress_mode enforce, writ serve installs providers only from registry.example.com, mirror.example.com; add registry.opentofu.org to provider_registries to install from it"
	if _, fill := errors.AsType[*runner.FillError](err); !refused || !fill || err.Error() != want {
		t.Fatalf("err %v, want a refused install:\n%s", err, want)
	}
	if len(f.fills) != 0 || len(f.stacks) != 0 {
		t.Errorf("tofu ran: %d fills, %d runs", len(f.fills), len(f.stacks))
	}
	if res.Network == nil || res.Network.Mode != egress.Enforce || res.Network.Install != nil {
		t.Errorf("the network record of a refused install: %+v", res.Network)
	}
}

// The pool ends what earlier runs left as each uid at start, gives each
// job a uid no other job has, and when every uid is taken, waits for one
// until the job's context ends and says why none came. A uid whose
// processes do not end on release stays out of the pool, and is counted.
func TestLocalUIDs(t *testing.T) {
	var mu sync.Mutex
	var reaped []uint32
	fail := false
	reap := func(uid uint32) error {
		mu.Lock()
		defer mu.Unlock()
		reaped = append(reaped, uid)
		if fail {
			return errors.New("still running")
		}
		return nil
	}
	uids, err := runner.NewUIDs(61000, 61001, reap, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reaped, []uint32{61000, 61001}) {
		t.Fatalf("at start the pool reaped %v, want both uids", reaped)
	}
	f := &fakeTofu{}
	l := local(t, f)
	l.UIDs = uids
	a, err := l.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []runner.Slot{a, b} {
		if _, err := s.Run(context.Background(), context.Background(), job(runner.Plan)); err != nil {
			t.Fatal(err)
		}
	}
	if u, v := f.stacks[0].User, f.stacks[1].User; u == nil || v == nil || u.UID == v.UID || u.UID != u.GID || v.UID != v.GID {
		t.Fatalf("the jobs ran as %+v and %+v, want two uids of their own", u, v)
	}

	waiting := errors.New("the plan ran past limits.plan_timeout, 50ms")
	ctx, cancel := context.WithTimeoutCause(context.Background(), 50*time.Millisecond, waiting)
	defer cancel()
	_, err = l.Reserve(ctx)
	_, refused := errors.AsType[*runner.Refusal](err)
	want := "the plan ran past limits.plan_timeout, 50ms, while it waited for a provider uid: 0 of the 2 provider_uids are withheld"
	if !refused || !errors.Is(err, waiting) || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("a third job: %v, want %q", err, want)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	a.Release()
	mu.Lock()
	fail = false
	mu.Unlock()
	b.Release()
	if !slices.Equal(reaped[2:], []uint32{f.stacks[0].User.UID, f.stacks[1].User.UID}) {
		t.Fatalf("release reaped %v, want each job's uid", reaped[2:])
	}
	c, err := l.Reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	ctx, cancel = context.WithTimeoutCause(context.Background(), 50*time.Millisecond, waiting)
	defer cancel()
	_, err = l.Reserve(ctx)
	if want := fmt.Sprintf("%s, while it waited for a provider uid: 1 of the 2 provider_uids are withheld", waiting); err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("with a uid withheld: %v, want %q", err, want)
	}
}

// NewUIDs fails when the processes of a uid do not end.
func TestNewUIDsFails(t *testing.T) {
	_, err := runner.NewUIDs(61000, 61003, func(uid uint32) error {
		if uid == 61002 {
			return errors.New("still running")
		}
		return nil
	}, slog.New(slog.DiscardHandler))
	if err == nil || err.Error() != "end the processes of provider uid 61002: still running" {
		t.Fatalf("err %v", err)
	}
}
