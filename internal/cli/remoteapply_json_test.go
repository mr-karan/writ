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
