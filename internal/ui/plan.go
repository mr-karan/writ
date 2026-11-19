package ui

import (
	"cmp"
	"encoding/json"

	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/server"
)

// changesPerPage bounds the changes one plan page lists. The summary
// covers the whole plan.
const changesPerPage = 200

type planView struct {
	Plan *server.PlanInfo
	Text string
	// Shown counts the changes that pass the filters; the page lists at
	// most changesPerPage of them.
	Shown, Page, Pages int
	Filter             struct{ Action, Risk string }
	Actions, Risks     []string
	// ActionPick and RiskPick narrow the changes to one action or one
	// risk class.
	ActionPick, RiskPick picker
	Origin               map[string]any
	// Tofu is the plan of an OpenTofu stack.
	Tofu *tofuView
	// Diagnostics are the refusal of a plan that could not be made.
	Diagnostics []docDiag
	// Older is set for a plan an older Writ made of a stack of the Writ
	// language, whose document this version does not read.
	Older bool
	// DocError says why the plan's document could not be read. The page
	// then offers no approval.
	DocError string
	// Policy is the decision of the plan point, nil when no policy judged
	// the plan.
	Policy *policyView
	// Adopt is the command that adopts the state the plan read, when the
	// plan failed because that state has no secret set.
	Adopt string
	// Steps are where the plan is on its line: plan, approve, apply.
	Steps []step
	// RulesAsked is set once writ serve asked the plan's approval rules.
	RulesAsked bool
	// RunLine says what the plan's run did, "" when no run began.
	RunLine string
}

// newPlanView is the plan page of p before its document is read.
func newPlanView(p *server.PlanInfo, text string, page int) *planView {
	v := &planView{Plan: p, Text: text, Page: page, Steps: planSteps(p), RulesAsked: rulesAsked(p)}
	if p.Run != "" {
		v.RunLine = runLine(v.Steps)
	}
	return v
}

// policyView is the part of writ.policy-decision/v1 the plan page shows.
type policyView struct {
	Outcome string
	Results []struct {
		Set, Level, Rule, Outcome, Message string
	}
}

// Status words the outcome as the status its stamp shows.
func (p *policyView) Status() string {
	switch p.Outcome {
	case "pass":
		return "passed"
	case "warn":
		return "warned"
	}
	return "denied"
}

// newPicker is a picker of label over values, "all" first. Each item is
// the link href builds from its value, "" for all.
func newPicker(label, current string, values []string, href func(string) string) picker {
	p := picker{Label: label, Value: cmp.Or(current, "all")}
	for _, v := range append([]string{""}, values...) {
		p.Items = append(p.Items, pickItem{Name: cmp.Or(v, "all"), Href: href(v), Current: v == current})
	}
	return p
}

// readPlanDoc reads a plan's document: the masked `tofu show -json` of an
// OpenTofu plan, which has a format_version, or the refusal of a plan that
// could not be made, which holds only diagnostics. A document with a
// summary or changes is a plan an older Writ made of a stack of the Writ
// language, and older is set.
func readPlanDoc(raw json.RawMessage) (tofu *mask.Plan, diags []docDiag, older bool, err error) {
	if isTofuPlan(raw) {
		p, err := readTofuPlan(raw)
		return p, nil, false, err
	}
	var doc struct {
		Summary     json.RawMessage `json:"summary"`
		Changes     json.RawMessage `json:"changes"`
		Diagnostics []docDiag       `json:"diagnostics"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, nil, false, err
	}
	if doc.Summary != nil || doc.Changes != nil {
		return nil, nil, true, nil
	}
	return nil, doc.Diagnostics, false, nil
}
