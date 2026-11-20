package ui

import (
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/server"
)

// tones sorts every status a page shows into the five looks a stamp has:
// run while something works, wait while it waits for a person, ok when it
// ended well, bad when it failed, and void when it no longer applies.
var tones = map[string]string{
	server.PlanPlanning: "run", server.PlanApplying: "run",
	server.PlanPending: "wait", server.DriftDrifted: "wait", "warned": "wait",
	server.PlanApproved: "ok", server.PlanApplied: "ok", server.DriftClean: "ok", "ok": "ok", "passed": "ok", "allowed": "ok",
	server.PlanRefused: "bad", server.PlanFailed: "bad", "failing": "bad", "denied": "bad", "auth-failed": "bad", "error": "bad",
	server.PlanSuperseded: "void", server.PlanExpired: "void",
}

// tone is the look of a status, "" for one the table does not know.
func tone(status string) string { return tones[status] }

// valueClass marks a value the mask hid and a value OpenTofu knows only
// after the apply, so neither reads as a literal.
func valueClass(v string) string {
	switch v {
	case mask.Sensitive:
		return "masked"
	case "(known after apply)":
		return "unknown"
	}
	return ""
}

// step is one station of the line every change runs: plan, approve,
// apply. State is done, now (writ serve works on it), next (it waits for
// a person), failed, void (it will not happen), or todo.
type step struct {
	Name, State, Note string
	// Given and Need are the approvals a step counts, before its note.
	Given, Need *figure
}

// planSteps draws where a plan is on its line.
func planSteps(p *server.PlanInfo) []step {
	plan := step{Name: "Plan", State: "done", Note: "made"}
	approve := step{Name: "Approve", State: "todo"}
	apply := step{Name: "Apply", State: "todo"}
	switch {
	case p.Status == server.PlanPlanning:
		approve.Note = "not known yet"
	case !rulesAsked(p):
	case p.Approvals.Need == 0:
		approve.Note = "none needed"
	default:
		approve.Given, approve.Need = &figure{N: min(len(p.Approvals.ApprovedBy), p.Approvals.Need)}, &figure{N: p.Approvals.Need}
	}
	switch p.Status {
	case server.PlanPlanning:
		plan.State, plan.Note = "now", p.Status
	case server.PlanPending:
		approve.State = "next"
	case server.PlanApproved:
		approve.State, apply.State = "done", "next"
	case server.PlanApplying:
		approve.State, apply.State, apply.Note = "done", "now", p.Status
	case server.PlanApplied:
		approve.State, apply.State, apply.Note = "done", "done", p.Status
	case server.PlanRefused, server.PlanFailed:
		if p.Run != "" {
			approve.State, apply.State, apply.Note = "done", "failed", server.PlanFailed
			break
		}
		plan.State, plan.Note = "failed", p.Status
		approve.State, apply.State = "void", "void"
	case server.PlanExpired, server.PlanSuperseded:
		if rulesAsked(p) && len(p.Approvals.ApprovedBy) >= p.Approvals.Need {
			approve.State = "done"
		} else {
			approve.State = "void"
		}
		apply.State, apply.Note = "void", p.Status
	}
	return []step{plan, approve, apply}
}

// offer is what the plan page offers its reader toward approving and
// applying the plan. writ serve decides an approval; the page only says
// what the reader may ask for.
type offer struct {
	// Live is set while the plan may still be approved and applied.
	Live bool
	// Approve is set when the reader may approve the plan, and Own when
	// the reader made it and so cannot.
	Approve, Own bool
	// Speculative is set for a plan of files a client sent, which takes no
	// approval.
	Speculative bool
}

func offerOf(p *server.PlanInfo, s *server.Session) offer {
	o := offer{Live: server.PlanLive(p.Status), Speculative: p.Source.Kind == server.SourceSpeculative}
	if o.Live && p.Source.Kind == server.SourceCommit {
		o.Own = p.Author.Person == s.Person
		o.Approve = !o.Own
	}
	return o
}

// rulesAsked reports whether writ serve asked the approval rules of a
// plan. It asks them when it saves a plan, so not while the plan plans,
// nor of one refused or failed before it was saved, nor of a request that
// turned out the same as an earlier plan: that request has no id of its
// own, and the earlier plan takes the approvals.
func rulesAsked(p *server.PlanInfo) bool {
	switch {
	case p.ID == "", p.Status == server.PlanPlanning, p.Status == server.PlanRefused:
		return false
	case p.Status == server.PlanFailed:
		return p.Run != ""
	}
	return true
}

// runLine says what a plan's run did, in the words of the plan's apply
// station, the last of steps.
func runLine(steps []step) string {
	switch steps[len(steps)-1].State {
	case "now":
		return "is applying this plan"
	case "done":
		return "applied this plan"
	case "failed":
		return "failed to apply this plan"
	}
	return ""
}

// runSteps draws where a run is on its plan's line.
func runSteps(r *server.RunInfo) []step {
	approve := step{Name: "Approve", State: "done", Given: &figure{N: len(r.ApprovedBy)}, Note: "given"}
	if r.Override != "" {
		approve.Given, approve.Note = nil, "override"
	} else if len(r.ApprovedBy) == 0 {
		approve.Given, approve.Note = nil, "none needed"
	}
	apply := step{Name: "Apply", State: "done", Note: r.Status}
	switch r.Status {
	case server.RunApplying:
		apply.State = "now"
	case server.RunApplied:
	default:
		apply.State = "failed"
	}
	return []step{{Name: "Plan", State: "done", Note: "made"}, approve, apply}
}
