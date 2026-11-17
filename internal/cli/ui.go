package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mr-karan/writ/internal/server"
)

// UI is `writ ui`: it prints the control UI's address, and on the dev
// control plane a sign-in link that works once, for 10 minutes.
func (a *App) UI(ctx context.Context, asJSON bool) (err error) {
	defer escapeError(&err, asJSON)
	r, err := a.remote(ctx)
	if err != nil {
		return err
	}
	if !r.dev {
		if asJSON {
			return writeJSON(a.opts.Stdout, server.UILink{Format: server.FormatUILink, OK: true, URL: r.base + "/ui/", Diagnostics: []server.Diagnostic{}})
		}
		fmt.Fprintf(a.humanOutput(), "control UI: %s/ui/\n", r.base)
		return nil
	}
	status, resp, err := r.call(ctx, http.MethodPost, "/v1/dev/ui-link", []byte("{}"), remoteTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return a.remoteFailed(asJSON, resp)
	}
	if asJSON {
		_, err := a.opts.Stdout.Write(resp)
		return err
	}
	var link server.UILink
	if err := json.Unmarshal(resp, &link); err != nil {
		return err
	}
	fmt.Fprintf(a.humanOutput(), "control UI: %s\n", link.URL)
	return nil
}
