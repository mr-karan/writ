package server

import (
	"encoding/json"
	"time"
)

// Document formats of the API. The CLI's own documents, such as
// writ.plan.json/v2 and writ.state/v1, travel inside or as the body,
// unchanged.
const (
	FormatServer      = "writ.server/v0"
	FormatHealth      = "writ.health/v0"
	FormatWhoami      = "writ.whoami/v0"
	FormatPlanRequest = "writ.plan-request/v1"
	FormatRemotePlan  = "writ.remote-plan/v0"
	FormatRemotePlans = "writ.remote-plans/v0"
	FormatError       = "writ.error/v0"
	FormatRepoRequest = "writ.repo-request/v0"
	FormatRepo        = "writ.repo/v0"
	FormatRepos       = "writ.repos/v0"
	FormatApproveReq  = "writ.approve-request/v0"
	FormatApprove     = "writ.approve/v0"
	FormatRunRequest  = "writ.run-request/v1"
	FormatRemoteRun   = "writ.remote-run/v0"
	FormatRemoteRuns  = "writ.remote-runs/v0"
	FormatOutcome     = "writ.apply-outcome/v0"
	FormatDevice      = "writ.device/v0"
	FormatBrowser     = "writ.browser/v0"
	FormatSession     = "writ.session/v0"
	FormatSessions    = "writ.sessions/v0"
)

// Diagnostic is a diagnostic of the API's own documents, in the fields
// every Writ document's diagnostics have.
type Diagnostic struct {
	Severity string `json:"severity"`
	Class    string `json:"class"`
	Message  string `json:"message"`
}

// ServerInfo is GET /v1/server.
type ServerInfo struct {
	Format  string `json:"format"`
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	URL     string `json:"url"`
	// Dev is true for writ serve --dev, which a CLI may find on its own.
	Dev     bool         `json:"dev"`
	Issuers []IssuerInfo `json:"issuers"`
	// Login is true when writ login is set up: a people issuer exists.
	Login bool `json:"login"`
	// DeviceIssuers are the people issuers whose identity providers
	// offer the device flow, which writ login uses with them by default.
	// writ login signs in with the others in a browser.
	DeviceIssuers []string     `json:"device_issuers"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
}

// IssuerInfo is one trusted issuer.
type IssuerInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
	// Identity names whose user ids the issuer's tokens carry: a person
	// is IDENTITY/user/ID.
	Identity string `json:"identity"`
}

// Health is GET /healthz.
type Health struct {
	Format      string         `json:"format"`
	OK          bool           `json:"ok"`
	Database    bool           `json:"database"`
	Issuers     []IssuerHealth `json:"issuers"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
}

// IssuerHealth is one issuer's key set.
type IssuerHealth struct {
	Name string `json:"name"`
	// KeysAt is the last good fetch of the key set.
	KeysAt string `json:"keys_at"`
	// ClockOffsetMS is the issuer's clock minus this host's, from the Date
	// header of that fetch. Absent when the issuer sent no Date.
	ClockOffsetMS *int64 `json:"clock_offset_ms,omitempty"`
}

// Whoami is GET /v1/whoami.
type Whoami struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Principal   *Principal   `json:"principal"`
	Teams       []string     `json:"teams"`
	Grants      []GrantRow   `json:"grants"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Principal is who a verified token speaks for.
type Principal struct {
	Kind    string `json:"kind"` // ci-job
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Display string `json:"display"`
	// Person is the stable id of the person behind the token, such as
	// gitlab/user/1001. Self-approval is judged by it.
	Person  string `json:"person"`
	Expires string `json:"expires_at"`
	// Session is the id of the session a person signed in with, "" for a
	// CI job or the dev user. It names the session in GET and DELETE
	// /v1/sessions; it is not the token.
	Session string `json:"session,omitempty"`
}

// GrantRow is one grant that applies to the principal.
type GrantRow struct {
	Grant  int      `json:"grant"` // 1-based position in the config's grants
	Envs   []string `json:"envs"`
	Stacks []string `json:"stacks"`
	Can    []string `json:"can"`
	Risks  []string `json:"risks"`
}

// PlanRequest is the body of POST /v1/plans. Source names the commit of a
// repository to plan, which the control plane fetches.
type PlanRequest struct {
	Format string         `json:"format"`
	Env    string         `json:"env"`
	Stacks []string       `json:"stacks"`
	Hints  Hints          `json:"hints"`
	Source *SourceRequest `json:"source,omitempty"`
}

// SourceRequest names a commit to plan. From a CI job, Repo and Commit may
// be left out: the token's project and sha give them, and when given they
// must equal the token's.
type SourceRequest struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// Hints are what a job says about itself. They pick where a note goes and
// never decide anything.
type Hints struct {
	MergeRequest int `json:"merge_request,omitempty"`
}

// RemotePlan is a kept plan, as POST /v1/plans and GET /v1/plans/{id}
// return it.
type RemotePlan struct {
	Format string    `json:"format"`
	OK     bool      `json:"ok"`
	Plan   *PlanInfo `json:"plan"`
	// Document is the writ.plan.json/v2 document of the plan, as writ plan
	// --json prints it, once the plan ends.
	Document json.RawMessage `json:"document"`
	// Text is the plan as writ plan prints it.
	Text string `json:"text"`
	// Policy is the writ.policy-decision/v1 of the plan point, null when
	// no policy judged the plan.
	Policy json.RawMessage `json:"policy"`
	// Network is the writ.network/v1 of the plan's run, null under
	// egress_mode off or when OpenTofu did not run.
	Network     json.RawMessage `json:"network"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
}

// PlanInfo is a plan's resource fields.
type PlanInfo struct {
	// Ref names the plan from the request on. ID is the saved plan's
	// signature, known when the plan ends; "" before.
	Ref               string          `json:"ref"`
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	Env               string          `json:"env"`
	Stacks            []PlanStack     `json:"stacks"`
	Author            Principal       `json:"author"`
	RequestedAt       string          `json:"requested_at"`
	MadeAt            string          `json:"made_at,omitempty"`
	ExpiresAt         string          `json:"expires_at,omitempty"`
	Risks             []string        `json:"risks"`
	TeamsThatMayApply []string        `json:"teams_that_may_apply"`
	Origin            json.RawMessage `json:"origin"`
	// SupersededBy is the id of a newer pending plan of the same stack@env
	// from the same merge request, "" when none.
	SupersededBy string `json:"superseded_by"`
	// Source is what the plan was made from.
	Source PlanSourceInfo `json:"source"`
	// SupersededWhy says why the plan was superseded.
	SupersededWhy string `json:"superseded_why,omitempty"`
	// Approvals are what the rules ask of the plan and who gave them.
	Approvals PlanApprovals `json:"approvals"`
	// History is each change of the plan's status, oldest first. Only GET
	// /v1/plans/{id} fills it.
	History []PlanEvent `json:"history,omitempty"`
	// Run is the run that applied the plan, "" before one began.
	Run string `json:"run,omitempty"`
}

// PlanApprovals are a plan's approval rules and approvals.
type PlanApprovals struct {
	// Need is how many approvals the plan needs, and Rules the 1-based
	// positions of the rules that asked for them.
	Need  int   `json:"need"`
	Rules []int `json:"rules"`
	// ApprovedBy are the approvals that count, oldest first.
	ApprovedBy []ApprovalInfo `json:"approved_by"`
	// TeamsThatMayApprove are the teams with approve on the plan's
	// stack@env.
	TeamsThatMayApprove []string `json:"teams_that_may_approve"`
}

// ApprovalInfo is one approval.
type ApprovalInfo struct {
	Person  string `json:"person"`
	Display string `json:"display"`
	// Via says how the person proved it, such as "gitlab job 9922140".
	Via string `json:"via"`
	At  string `json:"at"`
}

// PlanEvent is one change of a plan's status.
type PlanEvent struct {
	At     string `json:"at"`
	From   string `json:"from"`
	To     string `json:"to"`
	Person string `json:"person,omitempty"`
	By     string `json:"by,omitempty"`
	Run    string `json:"run,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ApproveRequest is the body of POST /v1/approvals, and optionally of
// POST /v1/plans/{id}/approvals.
type ApproveRequest struct {
	Format string `json:"format"`
	// Pipeline approves every live plan of the CI job's own pipeline, all
	// or none. Only a CI job's token may ask for it.
	Pipeline bool `json:"pipeline,omitempty"`
}

// ApproveDoc is writ.approve/v0: the plans an approval or a withdrawal
// named, as they are after it.
type ApproveDoc struct {
	Format      string         `json:"format"`
	OK          bool           `json:"ok"`
	Plans       []ApprovedPlan `json:"plans"`
	Diagnostics []Diagnostic   `json:"diagnostics"`
}

// ApprovedPlan is one plan of writ.approve/v0.
type ApprovedPlan struct {
	ID         string         `json:"id"`
	Env        string         `json:"env"`
	Stack      string         `json:"stack"`
	Status     string         `json:"status"`
	Need       int            `json:"need"`
	ApprovedBy []ApprovalInfo `json:"approved_by"`
}

// RunRequest is the body of POST /v1/runs. It names the plan to apply in
// one of two ways: Plan, its id, or Merged, a commit of the default
// branch whose sources of the one stack in Stacks equal an approved
// plan's.
type RunRequest struct {
	Format string `json:"format"`
	Plan   string `json:"plan,omitempty"`
	// Override applies a plan without the approvals its rules need, with
	// this reason, of at least 10 characters. Only a person may give one.
	Override string         `json:"override,omitempty"`
	Env      string         `json:"env,omitempty"`
	Stacks   []string       `json:"stacks,omitempty"`
	Merged   *MergedRequest `json:"merged,omitempty"`
}

// MergedRequest names a commit of a repository's default branch. From a
// CI job, both may be left out: the token's project and sha give them.
type MergedRequest struct {
	Repo   string `json:"repo,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// RemoteRun is writ.remote-run/v0: an apply the control plane runs, as
// POST /v1/runs and GET /v1/runs/{id} return it.
type RemoteRun struct {
	Format string   `json:"format"`
	OK     bool     `json:"ok"`
	Run    *RunInfo `json:"run"`
	// Document is writ.tofu-apply/v0 once the run ends, or writ.apply/v0
	// for an apply that could not start or was lost, and null before.
	Document json.RawMessage `json:"document"`
	// Text is the apply as writ apply prints it, once the run ends.
	Text string `json:"text"`
	// Policy is the writ.policy-decision/v1 of the apply point that let
	// the run begin, null when no policy judged it.
	Policy json.RawMessage `json:"policy"`
	// Network is the writ.network/v1 of the run, null while it applies,
	// under egress_mode off, or when OpenTofu did not run.
	Network     json.RawMessage `json:"network"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
}

// RemoteRuns is GET /v1/applies: the runs writ serve started, newest
// first, without their documents.
type RemoteRuns struct {
	Format string    `json:"format"`
	OK     bool      `json:"ok"`
	Runs   []RunInfo `json:"runs"`
	// More is true when older runs were left out.
	More        bool         `json:"more"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Outcomes of an apply request. A 503 of POST /v1/runs or of the outcome
// route carries OutcomeNone or OutcomeUnknown in its outcome field.
const (
	// OutcomeStarted is a key whose request began a run.
	OutcomeStarted = "started"
	// OutcomeNone is a key closed with no run, or in a 503 of POST
	// /v1/runs, a request that began no run.
	OutcomeNone = "none"
	// OutcomeUnknown is a 503 after which writ serve does not know
	// whether a run began.
	OutcomeUnknown = "unknown"
)

// ApplyOutcome is writ.apply-outcome/v0, the answer of POST
// /v1/apply-requests/{key}/outcome: started with the run, or none.
type ApplyOutcome struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Key         string       `json:"key"`
	Outcome     string       `json:"outcome"`
	Run         *RunInfo     `json:"run"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// RunInfo is a run's fields.
type RunInfo struct {
	ID     string    `json:"id"`
	Plan   string    `json:"plan"`
	Status string    `json:"status"`
	Env    string    `json:"env"`
	Stack  string    `json:"stack"`
	Actor  Principal `json:"actor"`
	// ApprovedBy are the approvals that counted when the run began.
	ApprovedBy []ApprovalInfo `json:"approved_by"`
	Override   string         `json:"override"`
	// NamedBy is plan when the request named the plan's id, and merged
	// when it named a merged commit, MergedCommit.
	NamedBy      string `json:"named_by"`
	MergedCommit string `json:"merged_commit,omitempty"`
	RoleSession  string `json:"role_session,omitempty"`
	StartedAt    string `json:"started_at"`
	EndedAt      string `json:"ended_at,omitempty"`
	// Sources is the source digest of the run's plan. Only GET
	// /v1/applies sets it.
	Sources string `json:"sources,omitempty"`
}

// PlanSourceInfo is what a plan was made from: a commit of a repository,
// or, for kind speculative, files a client sent, on a plan kept from
// before writ serve planned only commits.
type PlanSourceInfo struct {
	Kind   string `json:"kind"` // SourceCommit or SourceSpeculative
	Repo   string `json:"repo,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
	// RefCheck is how a commit plan's ref was checked: on-ref when the
	// forge showed the commit is on the ref, no-ref when the plan names
	// none.
	RefCheck string `json:"ref_check,omitempty"`
}

// The kinds of a plan's source.
const (
	SourceCommit      = "commit"
	SourceSpeculative = "speculative"
)

// PlanStack is one stack of a plan.
type PlanStack struct {
	Stack        string `json:"stack"`
	Changes      int    `json:"changes"`
	SourceDigest string `json:"source_digest"`
}

// RemotePlans is GET /v1/plans.
type RemotePlans struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Plans       []PlanInfo   `json:"plans"`
	Next        string       `json:"next,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// RepoRequest is the body of POST /v1/repos. It names a forge and a
// project, and never a credential: the forge's token is a file the config
// names.
type RepoRequest struct {
	Format string `json:"format"`
	Name   string `json:"name"`
	// Forge is a forge's name in the config, or the host of its URL. Under
	// writ serve --dev, dir names a local Git repository.
	Forge string `json:"forge"`
	// Project is the project's path, such as acme/infra, or its id; for
	// dir, the absolute path of the repository.
	Project string `json:"project"`
	// Path is the directory in the project that holds the stacks.
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
}

// RepoInfo is a repository's fields.
type RepoInfo struct {
	Name          string `json:"name"`
	Forge         string `json:"forge"`
	Kind          string `json:"kind"`
	ForgeURL      string `json:"forge_url"`
	Project       string `json:"project"`
	ProjectID     string `json:"project_id"`
	Path          string `json:"path"`
	Prefix        string `json:"prefix"`
	DefaultBranch string `json:"default_branch"`
	WebURL        string `json:"web_url"`
	// Origin is api for a repository added through POST /v1/repos, and
	// config for one the config pins.
	Origin  string `json:"origin"`
	AddedBy string `json:"added_by"`
	AddedAt string `json:"added_at"`
	// Head is the newest commit of the default branch read.
	Head       string `json:"head"`
	HeadReadAt string `json:"head_read_at"`
	PolledAt   string `json:"polled_at"`
	// Error says why the last poll failed, "" when it worked.
	Error   string `json:"error"`
	ErrorAt string `json:"error_at,omitempty"`
	// Stacks counts the stacks found at the head, in writ.repos/v0 only.
	Stacks *int `json:"stacks,omitempty"`
}

// RepoDoc is writ.repo/v0: a repository and the stacks found at the head
// of its default branch.
type RepoDoc struct {
	Format      string       `json:"format"`
	OK          bool         `json:"ok"`
	Repo        RepoInfo     `json:"repo"`
	Commit      *CommitInfo  `json:"commit"`
	Stacks      []FoundStack `json:"stacks"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// CommitInfo is a commit the control plane read.
type CommitInfo struct {
	SHA    string `json:"sha"`
	ReadAt string `json:"read_at"`
	// Files and Bytes count the estate's own files.
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// FoundStack is an OpenTofu stack discovery found in a commit.
type FoundStack struct {
	// Stack is its name on the control plane: the prefix and its
	// directory from the repository's path.
	Stack string `json:"stack"`
	// Dir is its directory in the project.
	Dir string `json:"dir"`
	// Served are the environments this control plane plans the stack in,
	// and NotServed the ones it does not.
	Served    []string        `json:"served"`
	NotServed []string        `json:"not_served"`
	Providers []FoundProvider `json:"providers"`
}

// FoundProvider is a provider a stack's .terraform.lock.hcl pins, and
// whether this control plane runs it.
type FoundProvider struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	Runs    bool   `json:"runs"`
}

// ReposDoc is writ.repos/v0.
type ReposDoc struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	// Pinned is set when the config lists the repositories.
	Pinned      bool         `json:"pinned"`
	Repos       []RepoInfo   `json:"repos"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
