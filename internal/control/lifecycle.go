package control

import (
	"fmt"
	"slices"
)

// PlanStatus is where a kept plan is in its lifecycle.
type PlanStatus string

const (
	// Planning: the control plane is making the plan.
	Planning PlanStatus = "planning"
	// Pending: the plan is saved and waits for its approvals.
	Pending PlanStatus = "pending"
	// Approved: the plan has the approvals its rules need, or needs none.
	Approved PlanStatus = "approved"
	// Applying: a run applies the plan.
	Applying PlanStatus = "applying"
	// Applied: the run finished with every change applied.
	Applied PlanStatus = "applied"
	// NoChanges: the plan changes no resource, no output, and no state, so
	// there is nothing to approve or apply.
	NoChanges PlanStatus = "no-changes"
	// RefusedPlan: the plan ended with errors, so it was never saved.
	RefusedPlan PlanStatus = "refused"
	// Failed: the plan could not run, or its apply failed.
	Failed PlanStatus = "failed"
	// Expired: plan_ttl passed before the plan was applied.
	Expired PlanStatus = "expired"
	// Superseded: state, or the ref the plan was made from, moved after
	// the plan, so what it would change is no longer what it showed.
	Superseded PlanStatus = "superseded"
)

// transitions are the legal moves of a plan's status, and the only ones:
// every write of a status goes through move, which refuses any other. A
// status with no entry is final.
//
//   - planning ends pending, approved when its rules need no approval,
//     no-changes when its apply would change nothing, refused, failed, or
//     superseded when an apply on its stack@env ran while it planned.
//   - pending becomes approved when its last needed approval comes, or
//     when a run begins after a change of config made approvals count
//     again, and applying only with an override.
//   - approved becomes pending again when an approval is withdrawn below
//     what the rules need, or when an approval, withdrawal, or run finds
//     that a change of config stopped one counting.
//   - a live plan, pending or approved, expires or is superseded.
//   - applying ends applied or failed.
var transitions = map[PlanStatus][]PlanStatus{
	Planning: {Pending, Approved, NoChanges, RefusedPlan, Failed, Superseded},
	Pending:  {Approved, Applying, Expired, Superseded},
	Approved: {Pending, Applying, Expired, Superseded},
	Applying: {Applied, Failed},
}

// Live reports a plan that may still be approved and applied.
func (s PlanStatus) Live() bool { return s == Pending || s == Approved }

// TransitionError is a status change the lifecycle refuses, or one that
// lost a race: the plan is no longer in a status it may move from.
type TransitionError struct {
	Plan     string
	From, To PlanStatus
}

func (e *TransitionError) Error() string {
	if e.From == "" {
		return fmt.Sprintf("plan %s cannot become %s from where it is", e.Plan, e.To)
	}
	return fmt.Sprintf("plan %s is %s, and a %s plan cannot become %s", e.Plan, e.From, e.From, e.To)
}

// mustMoves checks that every move from a status in from to to is legal.
// A caller that asks for another move is a bug, so it is an error before
// any row is touched.
func mustMoves(from []PlanStatus, to PlanStatus) error {
	for _, f := range from {
		if !slices.Contains(transitions[f], to) {
			return fmt.Errorf("the lifecycle has no move from %s to %s", f, to)
		}
	}
	return nil
}
