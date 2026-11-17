package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Input is a policy's input at one point: the masked plan document, with
// input.writ added.
type Input struct {
	point Point
	now   time.Time
	doc   []byte
}

// Writ is input.writ: who asks for what, where, and when. Under conftest
// the key is absent.
type Writ struct {
	Decision    Point
	Now         time.Time
	Environment string
	Stack       string
	Actor       Actor
	// Approvals are the approvals that count, at the apply point. The plan
	// point has none.
	Approvals []Approval
}

// Actor is the plan's author at the plan point, and the person or CI job
// that asks for the apply at the apply point.
type Actor struct {
	Person  string   `json:"person"`
	Display string   `json:"display"`
	Kind    string   `json:"kind"`
	Teams   []string `json:"teams"`
}

type Approval struct {
	Person  string `json:"person"`
	Display string `json:"display"`
	Via     string `json:"via"`
}

// maskedKeys are the top-level keys of the masked plan document, the
// keys of mask.Plan. A document with any other key, such as the
// planned_values or prior_state of raw `tofu show -json`, is not one.
var maskedKeys = []string{
	"format_version", "terraform_version", "timestamp", "errored", "variables", "configuration",
	"resource_changes", "resource_drift", "output_changes", "checks", "relevant_attributes",
}

// NewInput builds an input from a masked plan document as writ serve
// stores it. It refuses any other document, a plan that errored, and an
// input past MaxInput.
func NewInput(doc []byte, w Writ) (Input, error) {
	if len(doc) > MaxInput {
		return Input{}, fmt.Errorf("the plan document is larger than the %d MiB limit of a policy input", MaxInput>>20)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return Input{}, fmt.Errorf("read the plan document: %w", err)
	}
	for key := range top {
		if !slices.Contains(maskedKeys, key) {
			return Input{}, fmt.Errorf("the plan document has the key %q, so it is not a masked plan document; a policy reads only those", key)
		}
	}
	if _, ok := top["format_version"]; !ok {
		return Input{}, errors.New("the plan document has no format_version, so it is not a masked plan document")
	}
	if _, ok := top["resource_changes"]; !ok {
		return Input{}, errors.New("the plan document has no resource_changes, so it is not a masked plan document")
	}
	var errored bool
	if raw, ok := top["errored"]; ok && (json.Unmarshal(raw, &errored) != nil || errored) {
		return Input{}, errors.New("the plan errored; a policy judges only a plan that OpenTofu made")
	}
	switch {
	case w.Decision != PlanPoint && w.Decision != ApplyPoint:
		return Input{}, fmt.Errorf("the decision point is %q, not plan or apply", w.Decision)
	case w.Now.IsZero():
		return Input{}, errors.New("the input has no time")
	case w.Environment == "" || w.Stack == "":
		return Input{}, errors.New("the input names no environment or no stack")
	case w.Decision == PlanPoint && len(w.Approvals) > 0:
		return Input{}, errors.New("the plan point has no approvals")
	}
	actor := w.Actor
	actor.Teams = nonNil(actor.Teams)
	head, err := json.Marshal(struct {
		Decision    Point      `json:"decision"`
		Now         string     `json:"now"`
		Environment string     `json:"environment"`
		Stack       string     `json:"stack"`
		Actor       Actor      `json:"actor"`
		Approvals   []Approval `json:"approvals"`
	}{w.Decision, w.Now.UTC().Format(time.RFC3339Nano), w.Environment, w.Stack, actor, nonNil(w.Approvals)})
	if err != nil {
		return Input{}, err
	}
	var b bytes.Buffer
	if err := json.Compact(&b, doc); err != nil {
		return Input{}, err
	}
	// The document is an object with at least two keys, so it ends in "}"
	// after one or more members.
	out := append(b.Bytes()[:b.Len()-1], `,"writ":`...)
	out = append(append(out, head...), '}')
	if len(out) > MaxInput {
		return Input{}, fmt.Errorf("the policy input is larger than the %d MiB limit", MaxInput>>20)
	}
	return Input{point: w.Decision, now: w.Now, doc: out}, nil
}

// MarshalJSON writes the input document.
func (in Input) MarshalJSON() ([]byte, error) {
	if in.doc == nil {
		return nil, errors.New("the input was not built")
	}
	return in.doc, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
