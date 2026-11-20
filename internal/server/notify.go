package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/notify"
)

// A notification's summary is built from Writ's own words, names, counts,
// and diagnostic classes. It never quotes a diagnostic's message, a policy's
// reason, or anything else a run or a plan printed: those may hold a value
// the mask does not know, and a notification goes to a channel that may
// reach people with no grant on the stack.

// initNotifications starts the dispatcher of the config's notifiers, none
// when it has none. It reads each url_file and secret_file once.
func (s *Server) initNotifications(opts Options) error {
	targets, err := LoadNotifiers(s.cfg)
	if err != nil || len(targets) == 0 {
		return err
	}
	s.notifier = notify.New(notify.Options{
		Targets:   targets,
		QueueSize: opts.NotifyQueueSize,
		Timeout:   opts.NotifyTimeout,
		Backoff:   opts.NotifyBackoff,
		Version:   s.version,
		Log:       s.log.With("component", "notify"),
		Now:       s.now,
		OnResult:  s.recordNotification,
	})
	return nil
}

// recordNotification keeps how a delivery ended. It runs on the
// notifier's own goroutine, never on a plan's or an apply's.
func (s *Server) recordNotification(r notify.Result) {
	failed := r.Dropped
	if !r.OK && failed == 0 {
		failed = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.store.RecordNotification(ctx, control.NotificationResult{Name: r.Target, Event: string(r.Event), OK: r.OK, Failed: failed, Err: r.Err, At: r.At})
	if err != nil {
		s.log.Error("record a notification's delivery", "notifier", r.Target, "event", r.Event, "err", err)
	}
}

// emit queues ev for the notifiers whose events, envs, and stacks match.
// It never waits.
func (s *Server) emit(ev notify.Event) {
	if s.notifier == nil {
		return
	}
	ev.ID, ev.Server = newID(), s.cfg.URL
	if ev.At.IsZero() {
		ev.At = s.now()
	}
	ev.StartedBy.Display = clip(ev.StartedBy.Display, 200)
	ev.StartedBy.Person = clip(ev.StartedBy.Person, 200)
	s.notifier.Emit(ev)
}

// Who started a notification's action, for StartedBy.Kind.
const (
	startedBySystem  = "writ"
	startedByOutside = "outside"
)

var (
	driftActor   = notify.Actor{Kind: StartedByDrift, Display: "drift check"}
	outsideActor = notify.Actor{Kind: startedByOutside, Display: "a write outside Writ"}
)

// actorOf is who a plan or an apply ran for: a person or a CI job.
func actorOf(p *principal) notify.Actor {
	if p == driftPrincipal {
		return driftActor
	}
	return notify.Actor{Kind: control.StartedByOf(p.info.Kind), Display: p.info.Display, Person: p.info.Person}
}

// link is the control UI's page at path, "" when writ serve has no UI to
// link to.
func (s *Server) link(path string) string {
	if !s.cfg.UI || s.cfg.URL == "" {
		return ""
	}
	return s.cfg.URL + path
}

func (s *Server) planLink(key string) string { return s.link("/ui/plans/" + url.PathEscape(key)) }
func (s *Server) runLink(id string) string   { return s.link("/ui/runs/" + url.PathEscape(id)) }
func (s *Server) driftLink(env, stack string) string {
	return s.link("/ui/stack?" + url.Values{"env": {env}, "stack": {stack}, "view": {"drift"}}.Encode())
}

// notifyPlan emits plan-failed for a plan that failed or was refused, and
// approval-needed for one that waits on approvals.
func (s *Server) notifyPlan(ref, env, stack string, res control.PlanResult, p *principal) {
	key := cmpOr(res.ID, ref)
	ev := notify.Event{At: res.MadeAt, Stack: stack, Env: env, Status: string(res.Status), StartedBy: actorOf(p), Plan: key, URL: s.planLink(key)}
	switch {
	case res.Status == control.Failed || res.Status == control.RefusedPlan:
		ev.Event = notify.EventPlanFailed
		ev.Summary = "plan " + string(res.Status) + classWords(res.Document)
	case res.Status == control.Pending && res.Need > 0:
		ev.Event = notify.EventApprovalNeeded
		ev.Summary = fmt.Sprintf("plan needs %s: %s", approvalWords(res.Need), changeWords(res.Changes))
		if len(res.Risks) > 0 {
			ev.Summary += ", risks " + strings.Join(res.Risks, ", ")
		}
		ev.Counts = &notify.Counts{Changes: res.Changes, Need: res.Need}
	default:
		return
	}
	s.emit(ev)
}

// notifyRun emits applied or apply-failed for a run that ended.
func (s *Server) notifyRun(run control.Run, plan control.Plan, res control.RunResult, p *principal) {
	ev := notify.Event{At: res.EndedAt, Stack: plan.Stack, Env: plan.Env, Status: string(res.Status), StartedBy: actorOf(p), Plan: plan.ID, Run: run.ID, URL: s.runLink(run.ID)}
	switch res.Status {
	case control.RunApplied:
		ev.Event, ev.Summary = notify.EventApplied, "applied"
		var doc TofuApply
		if json.Unmarshal(res.Document, &doc) == nil && doc.Summary != nil {
			sum := doc.Summary
			ev.Summary = fmt.Sprintf("applied: %d added, %d changed, %d destroyed", sum.Add, sum.Change, sum.Remove)
			if sum.Import > 0 {
				ev.Summary += fmt.Sprintf(", %d imported", sum.Import)
			}
			ev.Counts = &notify.Counts{Added: sum.Add, Changed: sum.Change, Destroyed: sum.Remove, Imported: sum.Import}
		}
	case control.RunFailed:
		ev.Event, ev.Summary = notify.EventApplyFailed, "apply failed"+classWords(res.Document)
	default:
		return
	}
	s.emit(ev)
}

// notifyDrift emits drift for a check that found drift or found it gone.
// prev is the stack@env's last clean or drifted check: a notifier with
// drift_mode on-change hears only when the status differs from it.
func (s *Server) notifyDrift(c control.DriftCheck, prev control.DriftStatus) {
	ev := notify.Event{Event: notify.EventDrift, At: c.MadeAt, Stack: c.Stack, Env: c.Env, Status: string(c.Status), StartedBy: driftActor,
		Plan: c.PlanID, URL: s.driftLink(c.Env, c.Stack), DriftStateChanged: prev != c.Status}
	switch c.Status {
	case control.DriftDrifted:
		ev.Summary = fmt.Sprintf("drifted: %s outside Writ, %s in code", changeWords(c.Outside), changeWords(c.Code))
		ev.Counts = &notify.Counts{Outside: c.Outside, Code: c.Code}
	case control.DriftClean:
		if prev != control.DriftDrifted {
			// Clean after clean, or a first check that finds nothing, is
			// not news under either drift_mode.
			return
		}
		ev.Summary = "drift cleared: the state matches the code"
	default:
		return
	}
	s.emit(ev)
}

// notifyOutsideWrite emits outside-write for a serial of a stack's state on
// S3 that no apply of writ serve wrote. The store judges each serial once.
func (s *Server) notifyOutsideWrite(env, stack string, serial int64) {
	s.emit(notify.Event{Event: notify.EventOutsideWrite, Stack: stack, Env: env, Status: "outside-write",
		Summary: fmt.Sprintf("state serial %d was written outside Writ", serial), StartedBy: outsideActor,
		Counts: &notify.Counts{Serial: serial}, URL: s.driftLink(env, stack)})
}

// classRE is the shape of a diagnostic class: a word of Writ's own, never
// free text.
var classRE = regexp.MustCompile(`^[a-z][a-z-]{0,23}$`)

// classWords names the class of the first error diagnostic of a plan's or
// a run's document, as " (provider)", or "" when it has none.
func classWords(doc []byte) string {
	var d struct {
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(doc, &d) != nil {
		return ""
	}
	for _, dg := range d.Diagnostics {
		if dg.Severity == "error" && classRE.MatchString(dg.Class) {
			return " (" + dg.Class + ")"
		}
	}
	return ""
}

// changeWords counts changes in words: 1 change, 2 changes.
func changeWords(n int) string {
	if n == 1 {
		return "1 change"
	}
	return fmt.Sprintf("%d changes", n)
}

// clip cuts s to at most n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
