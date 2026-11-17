package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/server"
)

// StateAdoptOptions are the flags of `writ state adopt STACK`.
type StateAdoptOptions struct {
	JSON  bool
	Stack string
	// Version names a state version on the http backend, and Serial a
	// serial of a state on S3: exactly one is set.
	Version, Serial int64
	Reason          string
}

// adoptTimeout bounds an adoption, which runs tofu on the control plane
// within server.MaxWait.
const adoptTimeout = server.MaxWait + 15*time.Second

// StateAdopt is `writ state adopt`: writ serve seals a secret set with the
// newest state of a stack@env, which no apply of it sealed one with.
func (a *App) StateAdopt(ctx context.Context, o StateAdoptOptions) (err error) {
	defer escapeError(&err, o.JSON)
	switch {
	case (o.Version > 0) == (o.Serial > 0) || o.Version < 0 || o.Serial < 0:
		return usagef("state adopt names one state: --version N for a stack on the http backend, or --serial N for a stack on S3")
	case o.Reason == "":
		return usagef("state adopt needs --reason, such as the ticket and where the state came from")
	case a.opts.Env == "":
		return usagef("state adopt needs --env: writ serve keeps state for each environment")
	}
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	body, err := json.Marshal(server.StateAdoptRequest{Format: server.FormatStateAdoptRequest, Env: a.opts.Env, Stack: o.Stack, Version: o.Version, Serial: o.Serial, Reason: o.Reason})
	if err != nil {
		return err
	}
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/state/adopt", body, adoptTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return a.remoteFailed(o.JSON, resp)
	}
	var doc server.StateAdoptDoc
	if err := json.Unmarshal(resp, &doc); err != nil || doc.Format != server.FormatStateAdopt {
		return fmt.Errorf("writ serve sent a body that is not %s", server.FormatStateAdopt)
	}
	if o.JSON {
		_, err := a.opts.Stdout.Write(resp)
		return err
	}
	out := a.humanOutput()
	what := fmt.Sprintf("state version %d", doc.Version)
	if doc.Backend != "http" {
		what = fmt.Sprintf("serial %d", doc.Serial)
	}
	fmt.Fprintf(out, "Adopted %s of %s@%s, lineage %s: writ serve sealed a secret set with it, read with the providers of %s at %s.\n",
		what, escapeHuman(doc.Stack, false), doc.Env, escapeHuman(doc.Lineage, false), escapeHuman(doc.Repo, false), forge.Short(doc.Commit))
	if doc.Indexed {
		fmt.Fprintf(out, "The state index holds its %d resources.\n", doc.Resources)
	} else {
		fmt.Fprintln(out, "The state index did not take its resources; the next refresh reads them.")
	}
	for _, d := range doc.Diagnostics {
		fmt.Fprintf(out, "%s: %s\n", d.Severity, escapeHuman(d.Message, false))
	}
	return nil
}
