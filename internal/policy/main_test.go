package policy_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/policy"
)

// fdsArg makes the test binary a child that lists its inherited
// descriptors, after closing them when its next argument is close.
const fdsArg = "__policy-fds"

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == fdsArg {
		if os.Args[2] == "close" {
			if err := policy.CloseInherited(); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
		}
		fmt.Println(inherited())
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// inherited lists the descriptors above stderr without FD_CLOEXEC, which
// are the ones this process inherited.
func inherited() string {
	var out []string
	for fd := 3; fd < 256; fd++ {
		flags, _, errno := syscall.RawSyscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			out = append(out, strconv.Itoa(fd))
		}
	}
	return strings.Join(out, ",")
}

// patient is a timeout no evaluation of a test reaches on a loaded
// machine under the race detector, whose child evaluates many times
// slower.
const patient = time.Minute

func compile(t testing.TB, sets ...policy.Set) *policy.Engine {
	t.Helper()
	e, err := policy.Compile(sets, policy.Options{Timeout: patient})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return e
}

func set(name string, level policy.Level, files ...string) policy.Set {
	s := policy.Set{Name: name, Level: level, Files: map[string]string{}}
	for i := 0; i+1 < len(files); i += 2 {
		s.Files[files[i]] = files[i+1]
	}
	return s
}

// rules is a mandatory set of one file in package main.
func rules(name, body string) policy.Set {
	return set(name, policy.Mandatory, "p.rego", "package main\n\n"+body)
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var wednesday = time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC)

// planInput is the plan point's input over a masked fixture, of stack
// network in env.
func planInput(t testing.TB, doc, env string) policy.Input {
	t.Helper()
	in, err := policy.NewInput(fixture(t, doc), policy.Writ{
		Decision: policy.PlanPoint, Now: wednesday, Environment: env, Stack: "network",
		Actor: policy.Actor{Person: "gitlab/user/1001", Display: "Person A", Kind: "person", Teams: []string{"platform"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// applyInput is the apply point's input over a masked fixture at now,
// with one approval.
func applyInput(t testing.TB, doc, env string, now time.Time) policy.Input {
	t.Helper()
	in, err := policy.NewInput(fixture(t, doc), policy.Writ{
		Decision: policy.ApplyPoint, Now: now, Environment: env, Stack: "network",
		Actor:     policy.Actor{Person: "gitlab/user/1001", Display: "Person A", Kind: "person", Teams: []string{"platform"}},
		Approvals: []policy.Approval{{Person: "gitlab/user/2002", Display: "Person B", Via: "browser session"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func decide(t testing.TB, e *policy.Engine, in policy.Input) policy.Decision {
	t.Helper()
	return e.Decide(context.Background(), in)
}

// messages are the messages of rule, or of every rule but error for "".
func messages(d policy.Decision, rule string) []string {
	var out []string
	for _, r := range d.Results {
		if r.Rule == rule || (rule == "" && r.Rule != "error") {
			out = append(out, r.Message)
		}
	}
	return out
}
