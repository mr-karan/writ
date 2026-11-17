// Package policy compiles policy sets, Rego rules about OpenTofu plans,
// and judges one plan at one point with them. It is the only package that
// imports Open Policy Agent. Its input is the masked plan document, with
// OpenTofu's own key names, and one key of Writ's, input.writ, so a
// policy never sees a secret value, and the same .rego file runs under
// conftest on the output of `tofu show -json`.
//
// Compile parses and type checks in the calling process, which runs no
// built-in. Decide evaluates in a child process with a memory limit, UTC
// as its only time zone, and no environment, and kills it at the
// deadline, so no built-in can stall, exhaust, or crash the caller.
package policy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// DecisionFormat names the decision document.
const DecisionFormat = "writ.policy-decision/v1"

// Point is when a decision is made: when a plan ends, or when an apply of
// it is asked for.
type Point string

const (
	PlanPoint  Point = "plan"
	ApplyPoint Point = "apply"
)

// Level is what a set's deny does. A mandatory set's deny refuses; an
// advisory set's deny only warns.
type Level string

const (
	Mandatory Level = "mandatory"
	Advisory  Level = "advisory"
)

// Set is one policy set: a name, a level, and its .rego files by name. An
// empty Level is mandatory.
type Set struct {
	Name  string
	Level Level
	Files map[string]string
}

// Outcome is a decision's verdict, and what one result does.
type Outcome string

const (
	Pass Outcome = "pass"
	Warn Outcome = "warn"
	Deny Outcome = "deny"
)

// Denies reports whether o refuses. Only pass and warn allow, so any other
// value denies.
func (o Outcome) Denies() bool { return o != Pass && o != Warn }

// Decision is the merged result of every set at one point,
// writ.policy-decision/v1.
type Decision struct {
	Format  string    `json:"format"`
	Point   Point     `json:"point"`
	Outcome Outcome   `json:"outcome"`
	Results []Result  `json:"results"`
	At      time.Time `json:"at"`
	// Input is the hex SHA-256 of the input document. The decision keeps
	// no other part of the input.
	Input string `json:"input"`
	// Approvals is the digest of the approvals the apply point judged,
	// which the run's start checks against the approvals it records.
	Approvals string `json:"approvals,omitempty"`
}

// Result is one message of one set. Rule is the Rego rule that gave it,
// such as deny or warn_tags, or "error" when the set failed. Outcome is
// what the result does: a mandatory set's deny and error deny, and
// everything else warns.
type Result struct {
	Set     string  `json:"set"`
	Level   Level   `json:"level"`
	Rule    string  `json:"rule"`
	Outcome Outcome `json:"outcome"`
	Message string  `json:"message"`
}

// Defaults of Options.
const (
	DefaultTimeout = 10 * time.Second
	DefaultMemory  = 1 << 30
)

// Options bound each Decide.
type Options struct {
	// Timeout bounds one whole Decide: the child process and every set's
	// evaluation. Zero is DefaultTimeout.
	Timeout time.Duration
	// Memory is how far the child process's address space may grow past
	// what it maps at its start, in bytes. Its garbage collector works
	// hard from half of it. Zero is DefaultMemory.
	Memory int64
}

// Engine holds compiled sets. It is safe for concurrent use.
type Engine struct {
	// command starts the evaluation process: its executable and its
	// arguments, whose first is childName.
	command []string
	timeout time.Duration
	memory  int64
	sets    []compiled
}

type compiled struct {
	name  string
	level Level
	files map[string]string
}

// maxSetBytes bounds the Rego of one set.
const maxSetBytes = 1 << 20

// Compile parses and type checks every set against the input's schema. A
// set whose files do not compile is an error, and so is one that defines
// none of the rules Writ reads. The error names each problem with its
// file and place.
func Compile(sets []Set, opts Options) (*Engine, error) {
	exe, err := self()
	if err != nil {
		return nil, fmt.Errorf("find the writ binary that evaluates policies: %w", err)
	}
	e := &Engine{command: []string{exe, childName}, timeout: opts.Timeout, memory: opts.Memory}
	if e.timeout <= 0 {
		e.timeout = DefaultTimeout
	}
	if e.memory <= 0 {
		e.memory = DefaultMemory
	}
	var errs []string
	seen := map[string]bool{}
	for _, s := range sets {
		level := s.Level
		if level == "" {
			level = Mandatory
		}
		size := 0
		for _, text := range s.Files {
			size += len(text)
		}
		switch {
		case s.Name == "":
			errs = append(errs, "a policy set has no name")
			continue
		case seen[s.Name]:
			errs = append(errs, fmt.Sprintf("two policy sets are named %s", s.Name))
			continue
		case level != Mandatory && level != Advisory:
			errs = append(errs, fmt.Sprintf("policy set %s has level %q; use mandatory or advisory", s.Name, level))
			continue
		case size > maxSetBytes:
			errs = append(errs, fmt.Sprintf("policy set %s holds %d bytes of Rego, more than the %d MiB limit", s.Name, size, maxSetBytes>>20))
			continue
		}
		seen[s.Name] = true
		if _, problems := compileSet(s.Name, s.Files); len(problems) > 0 {
			errs = append(errs, problems...)
			continue
		}
		e.sets = append(e.sets, compiled{name: s.Name, level: level, files: maps.Clone(s.Files)})
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return e, nil
}

// Sets is the number of sets the engine judges with.
func (e *Engine) Sets() int { return len(e.sets) }

// MaxInput bounds the input document Decide sends to an evaluation
// process: 16 MiB. A larger input is an error at each set's level.
const MaxInput = 16 << 20

// maxOutput bounds what the parent reads from an evaluation process.
const maxOutput = 4 << 20

// Bounds of what a decision keeps of a set's messages.
const (
	maxMessage  = 1000
	maxMessages = 100
)

// CutMessages cuts each message of d to 1,000 bytes, or before a marker
// that the 1,000th byte falls inside, the text a mask writes in place of a
// secret. Decide keeps each message whole, so that a caller masks the
// whole message first: a cut before the mask keeps a prefix of a long
// secret that the mask no longer matches.
func (d *Decision) CutMessages(marker string) {
	for i, r := range d.Results {
		if len(r.Message) <= maxMessage {
			continue
		}
		cut := maxMessage
		// Two markers cannot overlap, so the last one that starts in the
		// window is the only one that can cross the cut.
		if at := strings.LastIndex(r.Message[:min(len(r.Message), maxMessage+len(marker)-1)], marker); at >= 0 && at < maxMessage && at+len(marker) > maxMessage {
			cut = at
		}
		d.Results[i].Message = strings.ToValidUTF8(r.Message[:cut], "") + " (cut)"
	}
}

// Decide evaluates every set on in and merges the results. It never
// fails: a set that fails has the result error, which denies at a
// mandatory set's level and warns at an advisory one's. One deadline, the
// engine's timeout, covers the whole call. It keeps at most 100 messages
// of each set, each whole: see CutMessages.
func (e *Engine) Decide(ctx context.Context, in Input) Decision {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	d := Decision{Format: DecisionFormat, Point: in.point, At: in.now.UTC(), Results: []Result{}}
	results := make([]setResult, len(e.sets))
	switch {
	case in.doc == nil:
		fail(results, "the input was not built")
	case len(in.doc) > MaxInput:
		fail(results, fmt.Sprintf("the input is larger than the %d MiB limit", MaxInput>>20))
	default:
		sum := sha256.Sum256(in.doc)
		d.Input = hex.EncodeToString(sum[:])
		if len(e.sets) > 0 {
			e.evaluate(ctx, e.sets, in.doc, results)
		}
	}
	denied := false
	for i, s := range e.sets {
		add := func(rule string, outcome Outcome, msg string) {
			if s.level == Advisory {
				outcome = Warn
			}
			d.Results = append(d.Results, Result{Set: s.name, Level: s.level, Rule: rule, Outcome: outcome, Message: msg})
			denied = denied || outcome == Deny
		}
		r := results[i]
		if r.err != "" {
			add("error", Deny, r.err)
			continue
		}
		for n, m := range r.messages {
			if n == maxMessages {
				add(m.rule, m.outcome(), fmt.Sprintf("and %d more messages of set %s, cut", len(r.messages)-n, s.name))
				break
			}
			add(m.rule, m.outcome(), m.text)
		}
	}
	switch {
	case denied:
		d.Outcome = Deny
	case len(d.Results) > 0:
		d.Outcome = Warn
	default:
		d.Outcome = Pass
	}
	return d
}

func fail(results []setResult, msg string) {
	for i := range results {
		results[i] = setResult{err: msg}
	}
}

// evaluate runs sets in one child process and fills results. It accepts
// the child's output only when it follows the protocol in full and the
// child exits 0. Otherwise every set gets an error, also a set whose
// result arrived.
func (e *Engine) evaluate(ctx context.Context, sets []compiled, input []byte, results []setResult) {
	req := childRequest{Memory: e.memory}
	for _, s := range sets {
		req.Sets = append(req.Sets, childSet{Name: s.name, Files: s.files})
	}
	head, err := json.Marshal(req)
	if err != nil {
		fail(results, fmt.Sprintf("send the evaluation: %v", err))
		return
	}
	cmd := exec.CommandContext(ctx, e.command[0])
	cmd.Args = slices.Clone(e.command[1:])
	cmd.Env = []string{"TZ=UTC"}
	cmd.Dir = "/"
	cmd.Stdin = io.MultiReader(bytes.NewReader(head), bytes.NewReader([]byte("\n")), bytes.NewReader(input))
	cmd.WaitDelay = 100 * time.Millisecond
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		fail(results, fmt.Sprintf("start the evaluation process: %v", err))
		return
	}
	out, readErr := io.ReadAll(io.LimitReader(stdout, maxOutput+1))
	if len(out) > maxOutput {
		// It would block on a full pipe that nothing reads.
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	var got []setResult
	switch {
	case ctx.Err() != nil:
		err = fmt.Errorf("the evaluation did not end within the %s limit", e.timeout)
	case readErr != nil:
		err = fmt.Errorf("read the evaluation process: %w", readErr)
	case len(out) > maxOutput:
		err = fmt.Errorf("the evaluation process wrote more than %d MiB", maxOutput>>20)
	case waitErr != nil:
		err = fmt.Errorf("the evaluation process failed, as it does past its memory limit: %w", waitErr)
	default:
		got, err = readOutput(out, len(sets))
		if err != nil {
			err = fmt.Errorf("the evaluation process broke its protocol: %w", err)
		}
	}
	if err != nil {
		fail(results, err.Error())
		return
	}
	copy(results, got)
}
