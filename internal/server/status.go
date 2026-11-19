package server

import "github.com/mr-karan/writ/internal/control"

// The statuses of a plan, a run, and a drift check as the API's documents
// name them. The control UI branches on them and may not import control.
const (
	PlanPlanning   = string(control.Planning)
	PlanPending    = string(control.Pending)
	PlanApproved   = string(control.Approved)
	PlanApplying   = string(control.Applying)
	PlanApplied    = string(control.Applied)
	PlanRefused    = string(control.RefusedPlan)
	PlanFailed     = string(control.Failed)
	PlanExpired    = string(control.Expired)
	PlanSuperseded = string(control.Superseded)

	RunApplying = string(control.RunApplying)
	RunApplied  = string(control.RunApplied)

	DriftClean   = string(control.DriftClean)
	DriftDrifted = string(control.DriftDrifted)
	DriftFailed  = string(control.DriftFailed)
)

// PlanStatuses are the statuses of a plan in the order of its lifecycle.
var PlanStatuses = []string{PlanPlanning, PlanPending, PlanApproved, PlanApplying, PlanApplied, PlanRefused, PlanFailed, PlanExpired, PlanSuperseded}

// PlanLive reports whether a plan of status may still be approved and
// applied.
func PlanLive(status string) bool { return control.PlanStatus(status).Live() }
