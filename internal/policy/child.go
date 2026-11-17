package policy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
)

// maxHeader bounds the request line, which holds the sets.
const maxHeader = 32 << 20

// childName is os.Args[0] of an evaluation process: the writ binary,
// started again from /proc/self/exe, which names writ's own binary even
// after an upgrade replaced the file.
const childName = "writ-policy-eval"

func init() {
	if len(os.Args) == 1 && os.Args[0] == childName {
		os.Exit(runChild(os.Stdin, os.Stdout))
	}
}

// runChild is an evaluation process: it reads a request line and an input
// document from stdin, sets the request's memory limit, and writes a
// result line for each set and then the done line to stdout. It returns
// the process's exit code. The parent kills it at the deadline, so it
// sets none of its own.
func runChild(stdin io.Reader, stdout io.Writer) int {
	// Evaluation depends only on the input: no descriptor but the three
	// standard ones, and "Local" is UTC.
	if err := closeInherited(); err != nil {
		return 2
	}
	time.Local = time.UTC
	in := bufio.NewReader(io.LimitReader(stdin, maxHeader+MaxInput+1))
	line, err := in.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		line, err = readLongLine(in, line)
	}
	var req childRequest
	if err != nil || json.Unmarshal(line, &req) != nil || req.Memory <= 0 {
		return 2
	}
	// The soft limit makes the garbage collector work hard near it. On
	// Linux the address space may grow by Memory past what the process
	// maps at its start, so an allocation past it fails and ends the
	// process.
	debug.SetMemoryLimit(req.Memory / 2)
	if err := limitMemory(uint64(req.Memory)); err != nil {
		return 2
	}
	doc, err := io.ReadAll(in)
	if err != nil || len(doc) > MaxInput {
		return 2
	}
	results := make([]setResult, len(req.Sets))
	if input, err := ast.ValueFromReader(bytes.NewReader(doc)); err != nil {
		for i := range results {
			results[i].err = fmt.Sprintf("read the input: %v", err)
		}
	} else {
		for i, s := range req.Sets {
			results[i] = evaluateSet(s, input)
		}
	}
	enc := json.NewEncoder(stdout)
	for i, r := range results {
		if enc.Encode(resultLine(i, r)) != nil {
			return 2
		}
	}
	done := len(results)
	if enc.Encode(childLine{Done: &done}) != nil {
		return 2
	}
	return 0
}

func readLongLine(r *bufio.Reader, first []byte) ([]byte, error) {
	line := append([]byte(nil), first...)
	for {
		more, err := r.ReadSlice('\n')
		line = append(line, more...)
		if len(line) > maxHeader {
			return nil, errors.New("the request line is too long")
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// evaluateSet compiles and evaluates one set. A panic in OPA becomes the
// set's error, and the next set still runs.
func evaluateSet(s childSet, input ast.Value) (out setResult) {
	defer func() {
		if r := recover(); r != nil {
			out = setResult{err: fmt.Sprintf("the evaluation panicked: %v", r)}
		}
	}()
	p, problems := compileSet(s.Name, s.Files)
	if len(problems) > 0 {
		return setResult{err: "the set does not compile: " + strings.Join(problems, "; ")}
	}
	rs, err := p.query.Eval(context.Background(), rego.EvalParsedInput(input))
	if err != nil {
		return setResult{err: err.Error()}
	}
	msgs, err := readRules(rs, p.rules)
	if err != nil {
		return setResult{err: err.Error()}
	}
	return setResult{messages: msgs}
}

// readRules reads each rule's messages, in conftest's shapes: a set or
// array of strings, or of objects whose msg is a string.
func readRules(rs rego.ResultSet, rules []string) ([]message, error) {
	if len(rs) != 1 {
		return nil, fmt.Errorf("the rules gave %d results", len(rs))
	}
	out := []message{}
	for i, rule := range rules {
		got, _ := rs[0].Bindings[fmt.Sprintf("r%d", i)].([]any)
		if len(got) == 0 {
			continue
		}
		xs, ok := got[0].([]any)
		if !ok {
			return nil, fmt.Errorf("%s is %v; it must be a set of strings", rule, got[0])
		}
		for _, x := range xs {
			if o, ok := x.(map[string]any); ok {
				x = o["msg"]
			}
			text, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("%s holds %v; each message must be a string, or an object whose msg is a string", rule, x)
			}
			out = append(out, message{rule: rule, text: text})
		}
	}
	return out, nil
}
