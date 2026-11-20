package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/server"
)

// newerServer answers every request with status and doc, and names its
// version in X-Writ-Version when version is set.
func newerServer(t *testing.T, version string, status int, doc any) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if version != "" {
			w.Header().Set(server.VersionHeader, version)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(s.Close)
	return s
}

// A remote command that gets an answer it does not know never exits 1 with
// nothing said. writ 0.1.5 met writ serve 0.1.7's 200 for a merged apply
// with nothing to apply, a status it did not expect, passed the body to
// remoteFailed, which printed the diagnostics of a body with none, and
// exited 1 silently. An answer that is no refusal is now an error, which
// names both versions when writ serve sent its own, so writ prints it and,
// with --json, the failure document.
func TestRemoteUnknownAnswerIsAnError(t *testing.T) {
	const id = "d3f700d860a97483a7ad509c31fe9e28"
	nothing := server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Plan: id, Document: json.RawMessage("null"), Diagnostics: []server.Diagnostic{}}
	for _, c := range []struct {
		name    string
		version string
		status  int
		doc     any
		run     func(a *App, asJSON bool) error
		want    string
	}{
		{"an apply answered with a status writ does not know", "v0.1.7", http.StatusCreated, nothing,
			func(a *App, asJSON bool) error {
				return a.RemoteApply(context.Background(), RemoteApplyOptions{JSON: asJSON, Merged: true, Stack: "network"})
			},
			"writ serve is v0.1.7 and this writ is v0.1.5: install writ v0.1.7"},
		{"an approval refused with no error", "", http.StatusConflict, map[string]any{"format": server.FormatApprove, "ok": false, "diagnostics": []any{}},
			func(a *App, asJSON bool) error {
				return a.Approve(context.Background(), ApproveOptions{JSON: asJSON, ID: id})
			},
			"writ serve may be newer than this writ, v0.1.5"},
		{"a plan that is ok and names none", "v0.1.5", http.StatusOK, map[string]any{"format": server.FormatRemotePlan, "ok": true, "plan": nil, "diagnostics": []any{}},
			func(a *App, asJSON bool) error {
				return a.Show(context.Background(), ShowOptions{JSON: asJSON, ID: id})
			},
			"writ serve is v0.1.5 and this writ is v0.1.5"},
	} {
		for _, asJSON := range []bool{false, true} {
			t.Run(c.name, func(t *testing.T) {
				s := newerServer(t, c.version, c.status, c.doc)
				var out, stderr bytes.Buffer
				a := standInApp(t, s.URL, &out)
				a.opts.Env, a.opts.Version, a.opts.Stderr = "prod", "v0.1.5", &stderr
				err := c.run(a, asJSON)
				if err == nil || errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "which this writ cannot use") || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("json %v: err %v, want an error with %q", asJSON, err, c.want)
				}
				if out.Len() != 0 {
					t.Fatalf("json %v: stdout holds %q, want nothing before the failure document", asJSON, out.String())
				}
			})
		}
	}
}

// writ warns on stderr, once, when writ serve is a newer feature release:
// a newer major or minor version, or before 1.0 a newer patch version.
func TestServerVersionSkewWarns(t *testing.T) {
	whoami := server.Whoami{Format: server.FormatWhoami, OK: true, Principal: &server.Principal{Kind: "ci-job", Display: "job 1"}, Teams: []string{}, Diagnostics: []server.Diagnostic{}}
	for _, c := range []struct {
		server, client string
		warn           bool
	}{
		{"v0.1.7", "v0.1.5", true},
		{"0.1.7", "v0.1.5", true},
		{"v0.2.0", "v0.1.9", true},
		{"v1.0.0", "v0.9.0", true},
		{"v1.3.0", "v1.2.4", true},
		{"v1.2.9", "v1.2.4", false},
		{"v0.1.5", "v0.1.5", false},
		{"v0.1.5", "v0.1.7", false},
		{"v0.1.7", "dev", false},
		{"dev", "v0.1.5", false},
		{"v0.1.8-rc.1", "v0.1.7", true},
	} {
		s := newerServer(t, c.server, http.StatusOK, whoami)
		var out, stderr bytes.Buffer
		a := standInApp(t, s.URL, &out)
		a.opts.Version, a.opts.Stderr = c.client, &stderr
		for range 2 {
			if err := a.Whoami(context.Background(), false); err != nil {
				t.Fatalf("server %s, client %s: %v", c.server, c.client, err)
			}
		}
		got := stderr.String()
		if !c.warn {
			if got != "" {
				t.Errorf("server %s, client %s: stderr %q, want nothing", c.server, c.client, got)
			}
			continue
		}
		want := "writ: warning: writ serve is " + c.server + " and this writ is " + c.client + ": a newer writ serve can answer in ways an older writ cannot read; install writ " + c.server + ", the version of writ serve\n"
		if got != want {
			t.Errorf("server %s, client %s: stderr\n%q\nwant once\n%q", c.server, c.client, got, want)
		}
	}
}
