package importer

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/provider"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// planConfig commits omissions only after a successful plan preserves the
// imported values. Only structured paths or exact quoted provider paths
// attribute a diagnostic to a field.
func planConfig(ctx context.Context, ask Validator, n *node) ([]Problem, error) {
	if n.pruningStopped || n.plan == nil {
		return nil, nil
	}
	unproven := func(why string) ([]Problem, error) {
		return []Problem{{Kind: Gap, From: n.tf, Reason: "the provider could not prove a no-change plan for the generated code, because " + why + "; the code keeps these values and needs a hand fix"}}, nil
	}
	planned, err := n.plan(ctx, n.config)
	if err == nil {
		if !planned.Value.IsWhollyKnown() {
			return unproven("the plan contains unknown values")
		}
		if !value.Equal(planned.Value, n.state) || len(planned.RequiresReplace) != 0 {
			return []Problem{{Kind: Gap, From: n.tf, Reason: "the provider could not prove a no-change plan for the generated code; the code needs a hand fix"}}, nil
		}
		return nil, nil
	}
	e, ok := errors.AsType[*provider.Error](err)
	if !ok {
		return unproven("the plan request failed; provider error text is withheld")
	}
	var shown []string
	for _, d := range e.Diags {
		if !d.Error {
			continue
		}
		path := n.pathText(d.Path)
		if path == "" {
			path = "the whole object (no structured attribute path)"
		}
		shown = append(shown, path+": "+n.subject().Show(d).Text)
	}
	trial := n.config
	var pending []planCandidate
	for _, c := range planCandidates(n.schema.Block, n.config, nil) {
		if c.disabled || !slices.ContainsFunc(e.Diags, func(d provider.Diag) bool { return diagnosticNames(n.schema.Block, d, c.path) }) {
			continue
		}
		// Protocols do not expose SDK defaults. A legacy zero is only a
		// candidate; the successful plan below must prove the default.
		if !c.computed && !(n.schema.LegacySDK && isZero(n.config.At(c.path))) {
			continue
		}
		trial = setPath(trial, c.path, c.empty)
		pending = append(pending, c)
	}
	if len(pending) > 0 {
		p, planErr := n.plan(ctx, trial)
		if planErr == nil && p.Value.IsWhollyKnown() && len(p.RequiresReplace) == 0 && value.Equal(p.Value, n.state) {
			diags, validationErr := ask(ctx, n.schema.Wire, trial)
			if validationErr != nil || slices.ContainsFunc(diags, func(d provider.Diag) bool { return d.Error }) {
				var notes []Problem
				for _, c := range pending {
					notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: n.redactor().Text("the code keeps " + n.pathText(c.path) + ", because validation of its omission failed despite a no-change plan; the code needs a hand fix")})
				}
				return notes, nil
			}
			n.config = trial
			var notes []Problem
			for _, c := range pending {
				notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: "the code leaves out " + n.pathText(c.path) + ", because a provider diagnostic names its complete path and the provider proves its omission plans no change"})
			}
			return notes, nil
		}
	}
	return []Problem{{Kind: Gap, From: n.tf, Reason: n.redactor().Text("the provider rejects the generated code at " + strings.Join(shown, "; ") + "; the code keeps these values and needs a hand fix")}}, nil
}

func diagnosticNames(b schema.Block, d provider.Diag, path value.Path) bool {
	if !d.Error {
		return false
	}
	wirePath := providerPath(b, path)
	if len(d.Path) > 0 {
		return slices.Equal(d.Path, path) || slices.Equal(d.Path, wirePath)
	}
	// A top-level name alone is too weak to attribute a text diagnostic.
	if len(path) < 2 {
		return false
	}
	quoted := strconv.Quote(wirePath.String())
	return strings.Contains(d.Summary, quoted) || strings.Contains(d.Detail, quoted)
}

func providerPath(b schema.Block, path value.Path) value.Path {
	var out value.Path
	for i := 0; i < len(path); i++ {
		step := path[i]
		out = append(out, step)
		name, ok := step.Name()
		if !ok {
			continue
		}
		for _, nb := range b.Blocks {
			if nb.Name != name {
				continue
			}
			b = nb.Block
			if nb.IsRecord() && nb.Nesting != schema.NestSingle {
				out = out.Index(0)
			} else if !nb.IsRecord() && i+1 < len(path) {
				i++
				out = append(out, path[i])
			}
			break
		}
	}
	return out
}

type planCandidate struct {
	path     value.Path
	empty    value.Value
	disabled bool
	computed bool
}

func planCandidates(b schema.Block, v value.Value, p value.Path) []planCandidate {
	var out []planCandidate
	for _, a := range b.Attributes {
		if a.Optional && !a.Required && !isNull(v.Get(a.Name)) {
			out = append(out, planCandidate{path: p.Attr(a.Name), empty: value.NullVal(), computed: a.Computed})
		}
	}
	for _, nb := range b.Blocks {
		bv, bp := v.Get(nb.Name), p.Attr(nb.Name)
		if isNull(bv) {
			continue
		}
		element := func(ev value.Value, ep value.Path) {
			out = append(out, planCandidates(nb.Block, ev, ep)...)
		}
		disabled := false
		switch {
		case nb.IsRecord():
			disabled = disabledBlock(nb, bv)
			element(bv, bp)
		case nb.Nesting == schema.NestMap:
			for _, k := range bv.Keys() {
				element(bv.Get(k), bp.Key(k))
			}
		default:
			disabled = bv.Len() > 0
			for i := range bv.Len() {
				disabled = disabled && disabledBlock(nb, bv.Index(i))
				element(bv.Index(i), bp.Index(int64(i)))
			}
		}
		if disabled {
			empty := value.NullVal()
			if !nb.IsRecord() {
				empty = value.ListVal(nil)
			}
			out = append([]planCandidate{{path: bp, empty: empty, disabled: true}}, out...)
		}
	}
	return out
}

func omitDisabled(ctx context.Context, n *node) ([]Problem, error) {
	if n.pruningStopped || n.plan == nil {
		return nil, nil
	}
	var notes []Problem
	calls := 0
	for _, c := range planCandidates(n.schema.Block, n.config, nil) {
		if !c.disabled {
			continue
		}
		if calls >= 128 {
			n.pruningStopped = true
			notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: n.tf + ": stopped checking disabled blocks after 128 provider calls"})
			break
		}
		calls++
		trial := setPath(n.config, c.path, c.empty)
		p, err := n.plan(ctx, trial)
		if err == nil && p.Value.IsWhollyKnown() && value.Equal(p.Value, n.state) && len(p.RequiresReplace) == 0 {
			n.config = trial
			notes = append(notes, Problem{Kind: Gap, From: n.tf, Reason: "the code leaves out " + n.pathText(c.path) + ", because the provider proves its omission plans no change"})
		}
	}
	return notes, nil
}
