package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"text/tabwriter"

	"github.com/mr-karan/writ/internal/server"
)

// DefaultPlansLimit is how many plans `writ plans` lists without --limit.
const DefaultPlansLimit = 50

// PlansOptions are the filters of `writ plans --remote`. The environment
// is --env.
type PlansOptions struct {
	Stack, Status string
	// StartedBy is person or ci: what started the plans to list.
	StartedBy string
	Cursor    string
	Limit     int
	JSON      bool
}

// RemotePlans is `writ plans --remote`: the plans writ serve keeps in one
// environment that the token may read, newest first, with what started
// each, as GET /v1/plans filters them.
func (a *App) RemotePlans(ctx context.Context, o PlansOptions) (err error) {
	defer escapeError(&err, o.JSON)
	if o.Limit < 1 || o.Limit > 200 {
		return usagef("--limit must be 1 to 200, got %d", o.Limit)
	}
	switch o.StartedBy {
	case "", server.StartedByPerson, server.StartedByCI:
	case server.StartedByDrift:
		return usagef("--started-by must be person or ci: drift checks are not plans, and GET /v1/drift lists them")
	default:
		return usagef("--started-by must be person or ci, got %q", o.StartedBy)
	}
	if o.Status != "" && !slices.Contains(server.PlanStatuses, o.Status) {
		return usagef("--status must be one of %v, got %q", server.PlanStatuses, o.Status)
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	env := a.opts.Env
	if env == "" {
		if !r.dev {
			return usagef("--remote needs --env: writ serve lists the plans of one environment")
		}
		env = server.DevEnv
	}
	q := url.Values{"env": {env}, "limit": {strconv.Itoa(o.Limit)}}
	if o.Stack != "" {
		if err := server.CheckStackName(o.Stack); err != nil {
			return usagef("--stack: %v", err)
		}
	}
	for k, v := range map[string]string{"stack": o.Stack, "status": o.Status, "started_by": o.StartedBy, "cursor": o.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	doc, err := r.get(ctx, "/v1/plans?"+q.Encode(), remoteTimeout)
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
	var page server.RemotePlans
	if err := json.Unmarshal(doc, &page); err != nil || page.Format != server.FormatRemotePlans {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatRemotePlans)
	}
	out := a.humanOutput()
	if len(page.Plans) == 0 && page.Next == "" {
		fmt.Fprintf(out, "No plan in %s that this token may read matches.\n", env)
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PLAN\tREQUESTED\tSTACK\tSTATUS\tSTARTED BY\tBY")
	for _, p := range page.Plans {
		stack := ""
		if len(p.Stacks) > 0 {
			stack = p.Stacks[0].Stack
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", cmp.Or(p.ID, p.Ref), p.RequestedAt, escapeHuman(stack, true), p.Status, p.StartedBy,
			escapeHuman(cmp.Or(p.Author.Display, p.Author.Person), true))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if page.Next != "" {
		fmt.Fprintf(out, "More plans match: run the command again with --cursor %s\n", page.Next)
	}
	return nil
}
