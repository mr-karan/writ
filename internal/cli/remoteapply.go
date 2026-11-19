package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
)

// ApproveOptions are the flags of `writ approve --remote`.
type ApproveOptions struct {
	JSON bool
	// ID is the plan to approve, or with Withdraw the plan whose approval
	// to take back.
	ID string
	// Pipeline approves every live plan of the CI job's own pipeline.
	Pipeline bool
	Withdraw bool
}

// Approve approves a plan writ serve keeps, every plan of the CI job's
// pipeline, or withdraws an approval.
func (a *App) Approve(ctx context.Context, o ApproveOptions) (err error) {
	defer escapeError(&err, o.JSON)
	switch {
	case o.Pipeline && (o.ID != "" || o.Withdraw):
		return usagef("--pipeline approves the plans of the CI job's pipeline, and takes no plan id or --withdraw")
	case !o.Pipeline && o.ID == "":
		return usagef("approve takes a plan id, or --pipeline in a CI job")
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	method, path, body := http.MethodPost, "/v1/plans/"+url.PathEscape(o.ID)+"/approvals", []byte(nil)
	switch {
	case o.Pipeline:
		path, body = "/v1/approvals", []byte(`{"format":"`+server.FormatApproveReq+`","pipeline":true}`)
	case o.Withdraw:
		method, path = http.MethodDelete, "/v1/plans/"+url.PathEscape(o.ID)+"/approvals/mine"
	}
	status, resp, err := r.call(ctx, method, path, body, remoteTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return a.remoteFailed(o.JSON, resp)
	}
	var doc server.ApproveDoc
	if err := json.Unmarshal(resp, &doc); err != nil || doc.Format != server.FormatApprove {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatApprove)
	}
	if o.JSON {
		_, err := a.opts.Stdout.Write(resp)
		return err
	}
	out := a.humanOutput()
	verb := "Approved"
	if o.Withdraw {
		verb = "Withdrew your approval of"
	}
	for _, p := range doc.Plans {
		var by []string
		for _, ap := range p.ApprovedBy {
			by = append(by, ap.Display)
		}
		fmt.Fprintf(out, "%s plan %s for %s (%s): %d of %d approvals%s, %s.\n", verb, p.ID, p.Env, escapeHuman(p.Stack, false), len(p.ApprovedBy), p.Need,
			withWho(by), p.Status)
	}
	return nil
}

func withWho(by []string) string {
	if len(by) == 0 {
		return ""
	}
	return " (" + escapeHuman(strings.Join(by, ", "), false) + ")"
}

// RemoteApplyOptions are the flags of `writ apply --remote`.
type RemoteApplyOptions struct {
	JSON bool
	// ID is the plan to apply. Empty with Merged applies the approved plan
	// whose sources equal the merged commit's, and empty with Yes plans
	// first.
	ID       string
	Override string
	// Merged applies after a merge: the plan of Stack in the environment
	// whose sources equal Commit's, a commit of Repo's default branch. A
	// CI job's token names its own repository and commit.
	Merged       bool
	Stack        string
	Repo, Commit string
	// Yes plans the stack, as writ plan --remote does, and applies the plan
	// when it needs no approval.
	Yes  bool
	Plan RemotePlanOptions
}

// RemoteApply asks writ serve to apply a plan it keeps, and follows the
// run until it ends. The run belongs to writ serve: the first interrupt
// stops following it, and the run goes on.
func (a *App) RemoteApply(ctx context.Context, o RemoteApplyOptions) (err error) {
	defer escapeError(&err, o.JSON)
	n := 0
	for _, set := range []bool{o.ID != "", o.Merged, o.Yes} {
		if set {
			n++
		}
	}
	if n != 1 {
		return usagef("apply --remote takes a plan id, --merged, or --yes")
	}
	if o.Override != "" && o.ID == "" {
		return usagef("--override goes with a plan id: writ apply --remote ID --override REASON")
	}
	var r *remote
	req := server.RunRequest{Format: server.FormatRunRequest, Plan: o.ID, Override: o.Override}
	switch {
	case o.Merged:
		if o.Commit != "" && !forge.IsSHA(o.Commit) {
			return usagef("--commit %q is not a commit's full id", o.Commit)
		}
		if o.Stack == "" {
			return usagef("--merged needs --stack, the stack to apply")
		}
		if r, err = a.remote(ctx); err != nil {
			return err
		}
		if a.opts.Env == "" && !r.dev {
			return usagef("--merged needs --env")
		}
		req.Env, req.Stacks, req.Merged = a.opts.Env, []string{o.Stack}, &server.MergedRequest{Repo: o.Repo, Commit: o.Commit}
	case o.Yes:
		var doc []byte
		if r, doc, err = a.sendRemotePlan(ctx, o.Plan); err != nil {
			return err
		}
		var p server.RemotePlan
		if err := json.Unmarshal(doc, &p); err != nil || p.Plan == nil {
			return fmt.Errorf("writ serve sent a plan that is not %s", server.FormatRemotePlan)
		}
		// With --json, stdout holds one document: the plan when no run
		// follows, else the run. An approved plan always runs: one with no
		// resource change may still change outputs.
		if p.Plan.Status != "approved" {
			return cmpOrFailed(a.printRemotePlan(o.JSON, doc))
		}
		if !o.JSON {
			io.WriteString(a.humanOutput(), p.Text)
			fmt.Fprintln(a.humanOutput())
		}
		req.Plan = p.Plan.ID
	default:
		if r, err = a.remote(ctx); err != nil {
			return err
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	run, err := a.startRemoteRun(ctx, r, body, server.NewApplyKey(a.opts.Now()), o.JSON)
	if err != nil {
		return err
	}
	if !o.JSON {
		fmt.Fprintf(a.humanOutput(), "Run %s of plan %s for %s (%s), by %s%s%s.\n", run.ID, run.Plan, run.Env, escapeHuman(run.Stack, false),
			escapeHuman(run.Actor.Display, false), approvedWords(run), overrideWords(run))
	}
	doc, err := a.waitRun(ctx, r, run.ID)
	if errors.Is(err, errDetached) {
		if o.JSON {
			return fmt.Errorf("stopped following run %s, which goes on; follow it with writ show --remote %s", run.ID, run.ID)
		}
		fmt.Fprintf(a.humanOutput(), "Stopped following run %s. It goes on.\nFollow it with: writ show --remote %s\n", run.ID, run.ID)
		return ErrFailed
	}
	if err != nil {
		return err
	}
	return a.printRemoteRun(o.JSON, doc)
}

func cmpOrFailed(err error) error {
	if err != nil {
		return err
	}
	return ErrFailed
}

func approvedWords(run *server.RunInfo) string {
	if len(run.ApprovedBy) == 0 {
		return ""
	}
	var by []string
	for _, a := range run.ApprovedBy {
		by = append(by, a.Display)
	}
	return ", approved by " + escapeHuman(strings.Join(by, ", "), false)
}

func overrideWords(run *server.RunInfo) string {
	if run.Override == "" {
		return ""
	}
	return ", override: " + escapeHuman(run.Override, false)
}

// startRemoteRun sends POST /v1/runs with the command's key, and sends it again
// after an answer that shows the request started no run: 429, or 503 with
// outcome none. After a 503 with outcome none or a 409 that says the key is
// closed with no run, it sends the request again with a new key, because
// no request with the closed key starts a run. After an answer that does
// not decide, a transport error, a timeout, or a 503 without outcome none,
// it asks the outcome route. The key makes each request of the command
// start one run at most.
func (a *App) startRemoteRun(ctx context.Context, r *remote, body []byte, key string, asJSON bool) (*server.RunInfo, error) {
	hdr := http.Header{server.ApplyKeyHeader: {key}}
	var last []byte
	for try := range remoteTries {
		if try > 0 && !wait(ctx, try) {
			return a.askOutcome(ctx, r, key, asJSON)
		}
		status, b, err := r.once(ctx, http.MethodPost, "/v1/runs", body, remoteTimeout, hdr)
		if _, redirected := errors.AsType[*redirectError](err); redirected {
			return nil, err
		}
		last = b
		switch {
		case err != nil:
			if !asJSON {
				fmt.Fprintf(a.humanOutput(), "The apply request got no answer: %v\nAsking writ serve whether it started a run.\n", escapeHuman(err.Error(), false))
			}
			return a.askOutcome(ctx, r, key, asJSON)
		case status == http.StatusAccepted:
			var started server.RemoteRun
			if err := json.Unmarshal(b, &started); err != nil || started.Run == nil {
				return nil, fmt.Errorf("writ serve answered 202 with a body that is not %s", server.FormatRemoteRun)
			}
			return started.Run, nil
		case keyClosed(status, b):
			if !asJSON {
				fmt.Fprintf(a.humanOutput(), "writ serve closed the key %s with no run. Sending the request again with a new key.\n", key)
			}
			key = server.NewApplyKey(a.opts.Now())
			hdr.Set(server.ApplyKeyHeader, key)
			continue
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable && outcomeOf(b) == server.OutcomeNone:
			continue
		case status == http.StatusServiceUnavailable:
			if !asJSON {
				fmt.Fprintln(a.humanOutput(), "writ serve answered 503 and did not say that no run started. Asking it whether one did.")
			}
			return a.askOutcome(ctx, r, key, asJSON)
		default:
			return nil, a.remoteFailed(asJSON, b)
		}
	}
	return nil, a.remoteFailed(asJSON, last)
}

// outcomeOf is the outcome field of a refusal, "" for none.
func outcomeOf(doc []byte) string {
	var head struct {
		Outcome string `json:"outcome"`
	}
	_ = json.Unmarshal(doc, &head)
	return head.Outcome
}

// keyClosed reports whether an answer of POST /v1/runs says that its key
// is closed with no run: a 409, or a 503 with outcome none, with
// key_closed. A 503 with outcome unknown never replaces the key.
func keyClosed(status int, doc []byte) bool {
	var head struct {
		Outcome   string `json:"outcome"`
		KeyClosed bool   `json:"key_closed"`
	}
	if json.Unmarshal(doc, &head) != nil || !head.KeyClosed {
		return false
	}
	return status == http.StatusConflict || status == http.StatusServiceUnavailable && head.Outcome == server.OutcomeNone
}

// wait waits before try, as call does, and reports false when ctx ended.
func wait(ctx context.Context, try int) bool {
	t := time.NewTimer(time.Duration(1<<try) * time.Second)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// askOutcome asks the outcome route whether a key started a run, and asks
// again after a transport error, a 503, or a 429, up to remoteTries times.
// started returns the run. none prints that no run started. When the route
// does not answer, it prints the key and the command that asks later.
func (a *App) askOutcome(ctx context.Context, r *remote, key string, asJSON bool) (*server.RunInfo, error) {
	path := "/v1/apply-requests/" + url.PathEscape(key) + "/outcome"
	for try := range remoteTries {
		if try > 0 && !wait(ctx, try) {
			break
		}
		status, b, err := r.once(ctx, http.MethodPost, path, nil, remoteTimeout, nil)
		if err != nil || status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
			continue
		}
		var doc server.ApplyOutcome
		if status != http.StatusOK || json.Unmarshal(b, &doc) != nil || doc.Format != server.FormatOutcome {
			if !asJSON {
				_ = a.remoteFailed(false, b)
			}
			break
		}
		switch {
		case doc.Outcome == server.OutcomeStarted && doc.Run != nil:
			return doc.Run, nil
		case doc.Outcome == server.OutcomeNone:
			if asJSON {
				a.opts.Stdout.Write(b)
				return nil, ErrFailed
			}
			fmt.Fprintf(a.humanOutput(), "error: state-io: no run started: writ serve closed the key %s of this command's requests with no run; run the command again\n", key)
			return nil, ErrFailed
		}
		break
	}
	msg := fmt.Sprintf("writ serve did not say whether the apply request with the key %s started a run; ask later with: writ show --remote --request %s", key, key)
	if asJSON {
		doc, _ := json.Marshal(server.ApplyOutcome{Format: server.FormatOutcome, Key: key, Outcome: server.OutcomeUnknown,
			Diagnostics: []server.Diagnostic{{Severity: "error", Class: "state-io", Message: msg}}})
		a.opts.Stdout.Write(append(doc, '\n'))
	} else {
		fmt.Fprintf(a.humanOutput(), "error: state-io: %s\n", msg)
	}
	return nil, ErrFailed
}

// showRequest is writ show --remote --request KEY: the run an apply
// request started, or that none did. The question closes a key with no
// run. It exits 0 only for a run that started and applied.
func (a *App) showRequest(ctx context.Context, r *remote, o ShowOptions) error {
	if _, err := server.ParseApplyKey(o.Request); err != nil {
		return usagef("--request: %v", err)
	}
	status, b, err := r.call(ctx, http.MethodPost, "/v1/apply-requests/"+url.PathEscape(o.Request)+"/outcome", nil, remoteTimeout)
	if err != nil {
		return err
	}
	var doc server.ApplyOutcome
	if status != http.StatusOK {
		return a.remoteFailed(o.JSON, b)
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Format != server.FormatOutcome {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatOutcome)
	}
	if o.JSON {
		if _, err := a.opts.Stdout.Write(b); err != nil {
			return err
		}
		if doc.Run == nil || doc.Run.Status != "applied" {
			return ErrFailed
		}
		return nil
	}
	if doc.Outcome != server.OutcomeStarted || doc.Run == nil {
		fmt.Fprintf(a.humanOutput(), "No run started for the apply request with the key %s. The key is closed, so no request with it starts one.\n", o.Request)
		return ErrFailed
	}
	fmt.Fprintf(a.humanOutput(), "The apply request with the key %s started run %s.\n", o.Request, doc.Run.ID)
	run, err := r.get(ctx, "/v1/runs/"+url.PathEscape(doc.Run.ID), remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(false, re.doc)
	}
	if err != nil {
		return err
	}
	return a.printRemoteRun(false, run)
}

// errDetached means the first interrupt stopped the client following a
// run, which goes on.
var errDetached = errors.New("stopped following the run")

// waitRun polls a run until it ends, or until the first interrupt. It
// follows for remoteDeadline, counted from its own start, not from the
// run's.
func (a *App) waitRun(ctx context.Context, r *remote, id string) ([]byte, error) {
	deadline := time.Now().Add(remoteDeadline)
	path := "/v1/runs/" + url.PathEscape(id) + "?wait=" + remotePollWait.String()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	detached := make(chan struct{})
	go func() {
		select {
		case <-a.opts.Drain:
			close(detached)
			cancel()
		case <-wctx.Done():
		}
	}()
	for {
		doc, err := r.get(wctx, path, remotePollWait+remoteTimeout)
		select {
		case <-detached:
			return nil, errDetached
		default:
		}
		if err != nil {
			if re, ok := errors.AsType[*remoteError](err); ok {
				return re.doc, nil
			}
			return nil, err
		}
		var run server.RemoteRun
		if err := json.Unmarshal(doc, &run); err != nil || run.Run == nil {
			return nil, fmt.Errorf("writ serve sent a run that is not %s", server.FormatRemoteRun)
		}
		if run.Run.Status != "applying" {
			return doc, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("run %s still applies after %s; follow it with writ show --remote %s", id, remoteDeadline, id)
		}
	}
}

// printRemoteRun prints a writ.remote-run/v0 document: as is with --json,
// else the apply's text and a line that names the run.
func (a *App) printRemoteRun(asJSON bool, doc []byte) error {
	var run server.RemoteRun
	if err := json.Unmarshal(doc, &run); err != nil {
		return fmt.Errorf("writ serve sent a run that is not %s", server.FormatRemoteRun)
	}
	if asJSON {
		if _, err := a.opts.Stdout.Write(doc); err != nil {
			return err
		}
	} else {
		out := a.humanOutput()
		for _, d := range run.Diagnostics {
			fmt.Fprintf(out, "%s: %s: %s\n", d.Severity, d.Class, escapeHuman(d.Message, false))
		}
		io.WriteString(out, run.Text)
		if r := run.Run; r != nil {
			switch r.Status {
			case "applying":
				fmt.Fprintf(out, "Run %s of plan %s for %s (%s) is still applying, started %s.\n", r.ID, r.Plan, r.Env, escapeHuman(r.Stack, false), r.StartedAt)
			default:
				fmt.Fprintf(out, "Run %s of plan %s for %s (%s), by %s%s%s: %s at %s.\n", r.ID, r.Plan, r.Env, escapeHuman(r.Stack, false),
					escapeHuman(r.Actor.Display, false), approvedWords(r), overrideWords(r), r.Status, r.EndedAt)
			}
		}
	}
	if !run.OK || run.Run == nil || run.Run.Status != "applied" {
		return ErrFailed
	}
	return nil
}
