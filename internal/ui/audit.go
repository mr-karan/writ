package ui

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

type auditData struct {
	Env, Stack, Person, Op, Plan, Run, Since, Until, Cursor string
	Envs, Ops                                               []string
	EnvPick, OpPick, TimePick                               picker
	Filtered, MoreOpen                                      bool
	// Reads shows the reads beside the changes. With no op filter the
	// page shows only the changes, as GET /v1/audit?changes=true does,
	// unless reads=1 asks for every row.
	Reads bool
	// ChangesHref and ReadsHref are the page with only the changes and
	// with the reads too.
	ChangesHref, ReadsHref string
	Rows                   []server.AuditRow
	Next                   string
}

// ReadsParam is the reads parameter of a link of the audit page: "1" when the
// page shows the reads, else "", which link leaves out.
func (d auditData) ReadsParam() string {
	if d.Reads {
		return "1"
	}
	return ""
}

// Changes reports whether the page shows only the changes.
func (d auditData) Changes() bool { return !d.Reads && d.Op == "" }

// audit shows the audit log from GET /v1/audit, newest first, with the same
// per-row authorization, withheld refusal reasons, stripped state hashes,
// route patterns, and sealed cursors as the API.
func (u *UI) audit(w http.ResponseWriter, r *http.Request, s *server.Session) {
	f := u.fetcher(w, r, s)
	q := r.URL.Query()
	d := auditData{
		Env:    q.Get("env"),
		Stack:  q.Get("stack"),
		Person: q.Get("person"),
		Op:     q.Get("op"),
		Plan:   q.Get("plan"),
		Run:    q.Get("run"),
		Since:  q.Get("since"),
		Until:  q.Get("until"),
		Cursor: q.Get("cursor"),
		Reads:  q.Get("reads") == "1",
		Ops:    server.AuditOps,
	}
	changes := ""
	if d.Changes() {
		changes = "true"
	}
	var doc server.AuditDoc
	target := link("/v1/audit",
		"env", d.Env, "stack", d.Stack, "person", d.Person, "op", d.Op, "changes", changes,
		"plan", d.Plan, "run", d.Run, "since", d.Since, "until", d.Until, "cursor", d.Cursor,
	)
	if !f.main(target, &doc) {
		return
	}
	d.Rows, d.Next = doc.Rows, doc.Next
	if envs, ok := f.envNames(); ok {
		d.Envs = envs
	}
	if d.Env != "" && !slices.Contains(d.Envs, d.Env) {
		d.Envs = append(slices.Clone(d.Envs), d.Env)
	}
	d.EnvPick = newPicker("Env", d.Env, d.Envs, func(e string) string {
		st := d.Stack
		if e == "" {
			st = ""
		}
		return link("/ui/audit", "env", e, "stack", st, "person", d.Person, "op", d.Op, "plan", d.Plan, "run", d.Run, "since", d.Since, "until", d.Until, "reads", d.ReadsParam())
	})
	d.OpPick = newPicker("Op", d.Op, server.AuditOps, func(op string) string {
		return link("/ui/audit", "env", d.Env, "stack", d.Stack, "person", d.Person, "op", op, "plan", d.Plan, "run", d.Run, "since", d.Since, "until", d.Until, "reads", d.ReadsParam())
	})
	d.ChangesHref = link("/ui/audit", "env", d.Env, "stack", d.Stack, "person", d.Person, "plan", d.Plan, "run", d.Run, "since", d.Since, "until", d.Until)
	d.ReadsHref = link("/ui/audit", "env", d.Env, "stack", d.Stack, "person", d.Person, "plan", d.Plan, "run", d.Run, "since", d.Since, "until", d.Until, "reads", "1")
	d.TimePick = auditTimePicker(u.now().UTC().Truncate(time.Second), d)
	d.MoreOpen = d.Stack != "" || d.Person != "" || d.Plan != "" || d.Run != "" || d.Since != "" || d.Until != ""
	d.Filtered = d.Env != "" || d.Op != "" || d.MoreOpen
	p := &page{
		Title:   "Audit log",
		Section: "audit",
		CLI:     auditCLI(d),
		Data:    d,
		Crumbs:  []crumb{{Name: "Audit log"}},
	}
	if d.Env != "" && d.Stack != "" {
		p.Here.Env, p.Here.Stack = d.Env, d.Stack
	}
	f.render("audit", p)
}

func auditTimePicker(now time.Time, d auditData) picker {
	current := "all"
	if d.Since != "" || d.Until != "" {
		current = "custom"
	}
	p := picker{Label: "Time", Value: current}
	p.Items = append(p.Items, pickItem{
		Name:    "all",
		Href:    link("/ui/audit", "env", d.Env, "stack", d.Stack, "person", d.Person, "op", d.Op, "plan", d.Plan, "run", d.Run, "reads", d.ReadsParam()),
		Current: d.Since == "" && d.Until == "",
	})
	for _, w := range []struct {
		name string
		d    time.Duration
	}{
		{"last hour", time.Hour},
		{"last day", 24 * time.Hour},
		{"last week", 7 * 24 * time.Hour},
		{"last month", 30 * 24 * time.Hour},
	} {
		since := now.Add(-w.d).Format(time.RFC3339)
		p.Items = append(p.Items, pickItem{
			Name: w.name,
			Href: link("/ui/audit", "env", d.Env, "stack", d.Stack, "person", d.Person, "op", d.Op, "plan", d.Plan, "run", d.Run, "since", since, "reads", d.ReadsParam()),
		})
	}
	return p
}

func auditCLI(d auditData) string {
	parts := []string{"writ audit --remote"}
	if d.Changes() {
		parts = append(parts, "--changes")
	}
	for _, kv := range [][2]string{
		{"--env", d.Env}, {"--stack", d.Stack}, {"--person", d.Person},
		{"--op", d.Op}, {"--plan", d.Plan}, {"--run", d.Run},
		{"--since", d.Since}, {"--until", d.Until}, {"--cursor", d.Cursor},
	} {
		if kv[1] != "" {
			parts = append(parts, kv[0], kv[1])
		}
	}
	return strings.Join(parts, " ")
}

// auditRecord names one audit row in writ.audit/v0 by its request id (and
// env when set, since pipeline approvals across environments share a
// request id).
func auditRecord(r server.AuditRow) string {
	if r.Request == "" {
		return ""
	}
	if r.Env != "" {
		return record("request", r.Request, "env", r.Env)
	}
	return record("request", r.Request)
}
