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

// yesStandIn is a control plane for writ apply --remote --yes: it plans a
// commit as approved with changes, starts a run, and answers the run's
// poll as applied, or holds the poll open until the client goes when hold
// is set.
func yesStandIn(t *testing.T, changes int, hold bool) *httptest.Server {
	t.Helper()
	const id = "d3f700d860a97483a7ad509c31fe9e28"
	plan := &server.PlanInfo{Ref: "r1", ID: id, Status: "approved", Env: "prod", Stacks: []server.PlanStack{{Stack: "network", Changes: changes}}}
	run := &server.RunInfo{ID: standInRun, Plan: id, Status: "applying", Env: "prod", Stack: "network",
		Actor: server.Principal{Display: "alice"}, ApprovedBy: []server.ApprovalInfo{}, NamedBy: "plan", StartedAt: "2026-11-11T10:00:00Z"}
	reply := func(w http.ResponseWriter, status int, doc any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/plans":
			reply(w, http.StatusAccepted, server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: &server.PlanInfo{Ref: "r1", Status: "planning"}, Diagnostics: []server.Diagnostic{}})
		case r.Method == "GET" && r.URL.Path == "/v1/plans/r1":
			reply(w, http.StatusOK, server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: plan, Text: "1 to change.\n", Diagnostics: []server.Diagnostic{}})
		case r.Method == "POST" && r.URL.Path == "/v1/runs":
			reply(w, http.StatusAccepted, server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Run: run, Document: json.RawMessage("null"), Diagnostics: []server.Diagnostic{}})
		case r.Method == "GET" && r.URL.Path == "/v1/runs/"+standInRun:
			if hold {
				<-r.Context().Done()
				return
			}
			done := *run
			done.Status, done.EndedAt = "applied", "2026-11-11T10:00:05Z"
			reply(w, http.StatusOK, server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Run: &done, Document: json.RawMessage(`{"ok":true}`),
				Text: "Applied 1 change.\n", Diagnostics: []server.Diagnostic{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func yesOptions() RemoteApplyOptions {
	commit := strings.Repeat("a", 40)
	return RemoteApplyOptions{JSON: true, Yes: true, Stack: "network", Commit: commit,
		Plan: RemotePlanOptions{RemoteOptions: RemoteOptions{Stack: "network"}, JSON: true, Commit: commit}}
}

// formats decodes stdout as a stream of JSON documents and returns each
// one's format.
func formats(t *testing.T, out []byte) []string {
	t.Helper()
	var got []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var doc struct{ Format string }
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("stdout is not JSON documents: %v\n%s", err, out)
		}
		got = append(got, doc.Format)
	}
	return got
}

// writ apply --remote --yes --json prints one document, the run, when the
// plan is approved: also with no resource change, since outputs may change.
func TestRemoteApplyYesJSONPrintsOneDocument(t *testing.T) {
	for _, c := range []struct {
		changes int
		want    string
	}{{1, server.FormatRemoteRun}, {0, server.FormatRemoteRun}} {
		s := yesStandIn(t, c.changes, false)
		var out bytes.Buffer
		a := standInApp(t, s.URL, &out)
		a.opts.Env = "prod"
		if err := a.RemoteApply(context.Background(), yesOptions()); err != nil {
			t.Fatalf("changes %d: %v\n%s", c.changes, err, out.String())
		}
		if got := formats(t, out.Bytes()); len(got) != 1 || got[0] != c.want {
			t.Errorf("changes %d: stdout holds %v, want one %s", c.changes, got, c.want)
		}
	}
}

// An interrupt while writ apply --remote --json follows its run writes
// nothing on stdout and returns an error, so writ writes the failure
// document.
func TestRemoteApplyJSONDetachIsAnError(t *testing.T) {
	s := yesStandIn(t, 1, true)
	var out bytes.Buffer
	a := standInApp(t, s.URL, &out)
	a.opts.Env = "prod"
	drain := make(chan struct{})
	a.opts.Drain = drain
	o := yesOptions()
	o.Yes, o.ID = false, "d3f700d860a97483a7ad509c31fe9e28"
	done := make(chan error, 1)
	go func() { done <- a.RemoteApply(context.Background(), o) }()
	close(drain)
	err := <-done
	if err == nil || errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "writ show --remote "+standInRun) {
		t.Fatalf("err %v, want an error that names the run", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout holds %q, want nothing before the failure document", out.String())
	}
}

// A refusal whose body is not JSON, such as a proxy's error page, is not
// copied to stdout: it is an error, so writ writes the failure document.
func TestRemoteApplyRefusalNotJSON(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	t.Cleanup(s.Close)
	for _, asJSON := range []bool{true, false} {
		var out bytes.Buffer
		a := standInApp(t, s.URL, &out)
		a.opts.Env = "prod"
		err := a.RemoteApply(context.Background(), RemoteApplyOptions{JSON: asJSON, ID: "d3f700d860a97483a7ad509c31fe9e28"})
		if err == nil || errors.Is(err, ErrFailed) {
			t.Fatalf("json %v: err %v, want an error that is not ErrFailed", asJSON, err)
		}
		if strings.Contains(out.String(), "<html>") {
			t.Fatalf("json %v: stdout holds the proxy's page:\n%s", asJSON, out.String())
		}
	}
}

// noChangeStandIn is a control plane whose plan of network changes no
// resource: a commit plans as no-changes, and an apply after a merge
// answers 200 with no run. runs counts the requests to POST /v1/runs.
func noChangeStandIn(t *testing.T, runs *int) *httptest.Server {
	t.Helper()
	const id = "d3f700d860a97483a7ad509c31fe9e28"
	plan := &server.PlanInfo{Ref: "r1", ID: id, Status: "no-changes", Env: "prod", Stacks: []server.PlanStack{{Stack: "network"}},
		Source: server.PlanSourceInfo{Kind: server.SourceCommit, Repo: "infra", Commit: strings.Repeat("a", 40)}}
	reply := func(w http.ResponseWriter, status int, doc any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/plans":
			reply(w, http.StatusAccepted, server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: &server.PlanInfo{Ref: "r1", Status: "planning"}, Diagnostics: []server.Diagnostic{}})
		case r.Method == "GET" && r.URL.Path == "/v1/plans/r1":
			reply(w, http.StatusOK, server.RemotePlan{Format: server.FormatRemotePlan, OK: true, Plan: plan, Text: "No changes.\n", Diagnostics: []server.Diagnostic{}})
		case r.Method == "POST" && r.URL.Path == "/v1/runs":
			*runs++
			reply(w, http.StatusOK, server.RemoteRun{Format: server.FormatRemoteRun, OK: true, Plan: id, Document: json.RawMessage("null"), Diagnostics: []server.Diagnostic{},
				Text: "Plan " + id + " of network@prod has the sources of infra at aaaaaaaa and changes no resource: there is nothing to apply.\n"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// The apply job after a merge whose plan changes no resource passes: writ
// apply --remote --merged says there is nothing to apply and exits 0, and
// with --json prints the one document writ serve sent.
func TestRemoteApplyMergedNothingToApply(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var runs int
		s := noChangeStandIn(t, &runs)
		var out bytes.Buffer
		a := standInApp(t, s.URL, &out)
		a.opts.Env = "prod"
		if err := a.RemoteApply(context.Background(), RemoteApplyOptions{JSON: asJSON, Merged: true, Stack: "network"}); err != nil {
			t.Fatalf("json %v: %v\n%s", asJSON, err, out.String())
		}
		if runs != 1 {
			t.Fatalf("json %v: %d requests to apply", asJSON, runs)
		}
		if asJSON {
			if got := formats(t, out.Bytes()); len(got) != 1 || got[0] != server.FormatRemoteRun {
				t.Errorf("stdout holds %v, want one %s", got, server.FormatRemoteRun)
			}
		} else if !strings.Contains(out.String(), "there is nothing to apply") {
			t.Errorf("stdout %q", out.String())
		}
	}
}

// writ apply --remote --yes of a commit whose plan changes no resource
// prints the plan, starts no run, and exits 0.
func TestRemoteApplyYesNothingToApply(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		var runs int
		s := noChangeStandIn(t, &runs)
		var out bytes.Buffer
		a := standInApp(t, s.URL, &out)
		a.opts.Env = "prod"
		o := yesOptions()
		o.JSON, o.Plan.JSON = asJSON, asJSON
		if err := a.RemoteApply(context.Background(), o); err != nil {
			t.Fatalf("json %v: %v\n%s", asJSON, err, out.String())
		}
		if runs != 0 {
			t.Fatalf("json %v: %d requests to apply", asJSON, runs)
		}
		if asJSON {
			if got := formats(t, out.Bytes()); len(got) != 1 || got[0] != server.FormatRemotePlan {
				t.Errorf("stdout holds %v, want one %s", got, server.FormatRemotePlan)
			}
		} else if !strings.Contains(out.String(), "changes no resource") || strings.Contains(out.String(), "writ apply --remote") {
			t.Errorf("stdout %q", out.String())
		}
	}
}

func TestRemoteApplyChangedRunResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"nothing to apply", http.StatusOK},
		{"started run", http.StatusAccepted},
		{"polled run", http.StatusOK},
		{"key outcome", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(server.VersionHeader, "9.9.9")
				w.Header().Set("Content-Type", "application/json")
				if tc.name == "polled run" && r.Method == http.MethodPost {
					w.WriteHeader(http.StatusAccepted)
					_ = json.NewEncoder(w).Encode(server.RemoteRun{Format: server.FormatRemoteRun, OK: true,
						Run: &server.RunInfo{ID: standInRun, Status: "applying"}})
					return
				}
				if tc.name == "key outcome" && r.URL.Path == "/v1/runs" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"format":"writ.changed/v99","ok":true,"execution":{"id":"other","status":"applied"}}`))
			}))
			defer s.Close()
			for _, asJSON := range []bool{false, true} {
				var out bytes.Buffer
				a := standInApp(t, s.URL, &out)
				a.opts.Env = "prod"
				err := a.RemoteApply(context.Background(), RemoteApplyOptions{JSON: asJSON, Merged: true, Stack: "network"})
				if err == nil || !strings.Contains(err.Error(), "install writ 9.9.9") {
					t.Errorf("json %v: got %v, want the version-skew error", asJSON, err)
				}
				if asJSON && out.Len() != 0 {
					t.Errorf("json mode printed an incompatible response: %s", out.String())
				}
			}
		})
	}
}
