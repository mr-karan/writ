package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

// DefaultAuditLimit is how many rows `writ audit` lists without --limit.
const DefaultAuditLimit = 50

// AuditOptions are the filters of `writ audit --remote`. The environment
// is --env.
type AuditOptions struct {
	Stack, Plan, Run, Person, Op string
	// Since and Until are RFC 3339 times.
	Since, Until string
	Cursor       string
	Limit        int
	JSON         bool
}

// RemoteAudit is `writ audit --remote`: the rows of writ serve's audit log
// that the token may see, newest first, as GET /v1/audit filters them.
func (a *App) RemoteAudit(ctx context.Context, o AuditOptions) (err error) {
	defer escapeError(&err, o.JSON)
	if o.Limit < 1 || o.Limit > 200 {
		return usagef("--limit must be 1 to 200, got %d", o.Limit)
	}
	if o.Stack != "" {
		if a.opts.Env == "" {
			return usagef("--stack needs --env: a stack is named within an environment")
		}
		if err := server.CheckStackName(o.Stack); err != nil {
			return usagef("--stack: %v", err)
		}
	}
	for _, t := range []struct{ flag, v string }{{"--since", o.Since}, {"--until", o.Until}} {
		if _, err := time.Parse(time.RFC3339, t.v); t.v != "" && err != nil {
			return usagef("%s must be an RFC 3339 time, such as 2026-11-19T09:30:00Z", t.flag)
		}
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(o.Limit)}}
	for k, v := range map[string]string{"env": a.opts.Env, "stack": o.Stack, "plan": o.Plan, "run": o.Run, "person": o.Person,
		"op": o.Op, "since": o.Since, "until": o.Until, "cursor": o.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	doc, err := r.get(ctx, "/v1/audit?"+q.Encode(), remoteTimeout)
	if re, ok := errors.AsType[*remoteError](err); ok {
		return a.remoteFailed(o.JSON, re.doc)
	}
	if err != nil {
		return err
	}
	if o.JSON {
		_, err := a.opts.Stdout.Write(doc)
		return err
	}
	var page server.AuditDoc
	if err := json.Unmarshal(doc, &page); err != nil || page.Format != server.FormatAudit {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatAudit)
	}
	out := a.humanOutput()
	if len(page.Rows) == 0 && page.Next == "" {
		fmt.Fprintln(out, "No audit row that this token may see matches.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tBY\tOP\tON\tWHAT\tDECISION\tPLAN\tRUN\tREASON")
	for _, row := range page.Rows {
		what := row.Kind
		if row.Route != "" {
			what = strings.TrimSpace(row.Method + " " + row.Route + " " + strconv.Itoa(row.Status))
		}
		reason := row.Reason
		if row.ReasonWithheld {
			reason = "(withheld)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", row.At, escapeHuman(dash(row.Principal), false), dash(row.Op),
			escapeHuman(auditTarget(row), false), escapeHuman(what, false), row.Decision, dash(row.Plan), dash(row.Run), escapeHuman(dash(reason), false))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.Next != "" {
		fmt.Fprintf(out, "More rows match: run the command again with --cursor %s\n", page.Next)
	}
	return nil
}

// auditTarget is what a row concerns: its stack@env, its environment, or
// - for a row of no environment.
func auditTarget(row server.AuditRow) string {
	switch {
	case row.Env == "":
		return "-"
	case len(row.Stacks) == 0:
		return row.Env
	}
	return strings.Join(row.Stacks, ",") + "@" + row.Env
}
