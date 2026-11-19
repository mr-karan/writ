package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/server"
)

// plansStandIn answers GET /v1/plans with a page of two plans, one a
// person started and one a CI job, and records each query it was sent.
func plansStandIn(t *testing.T, next string) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	plans := []server.PlanInfo{
		{ID: "5be0c1d27f3a4e968b2d0a71c4f9e352", Status: "pending", Env: "prod", Stacks: []server.PlanStack{{Stack: "network"}}, StartedBy: "ci",
			Author: server.Principal{Display: "gitlab acme/infra #1849213 (alice)"}, RequestedAt: "2026-11-19T10:02:00Z"},
		{ID: "c0ffee00c0ffee00c0ffee00c0ffee00", Status: "applied", Env: "prod", Stacks: []server.PlanStack{{Stack: "app"}}, StartedBy: "person",
			Author: server.Principal{Display: "carol"}, RequestedAt: "2026-11-19T09:40:00Z"},
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/plans" {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(server.RemotePlans{Format: server.FormatRemotePlans, OK: true, Plans: plans, Next: next, Diagnostics: []server.Diagnostic{}})
	}))
	t.Cleanup(s.Close)
	return s, &queries
}

// writ plans --remote lists the plans of an environment with what started
// each, passes its filters to GET /v1/plans, and with --json prints the
// document writ serve sent.
func TestRemotePlans(t *testing.T) {
	s, queries := plansStandIn(t, "")
	var out bytes.Buffer
	a := standInApp(t, s.URL, &out)
	a.opts.Env = "prod"
	if err := a.RemotePlans(context.Background(), PlansOptions{Stack: "network", Status: "pending", StartedBy: "ci", Limit: 20}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got, want := (*queries)[0], "env=prod&limit=20&stack=network&started_by=ci&status=pending"; got != want {
		t.Errorf("GET /v1/plans?%s, want ?%s", got, want)
	}
	want := `PLAN                              REQUESTED             STACK    STATUS   STARTED BY  BY
5be0c1d27f3a4e968b2d0a71c4f9e352  2026-11-19T10:02:00Z  network  pending  ci          gitlab acme/infra #1849213 (alice)
c0ffee00c0ffee00c0ffee00c0ffee00  2026-11-19T09:40:00Z  app      applied  person      carol
`
	if out.String() != want {
		t.Errorf("writ plans --remote printed\n%s\nwant\n%s", out.String(), want)
	}

	out.Reset()
	if err := a.RemotePlans(context.Background(), PlansOptions{Limit: 50, JSON: true}); err != nil {
		t.Fatal(err)
	}
	var doc server.RemotePlans
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Format != server.FormatRemotePlans || len(doc.Plans) != 2 || doc.Plans[0].StartedBy != "ci" {
		t.Errorf("writ plans --remote --json printed %s", out.String())
	}

	s, _ = plansStandIn(t, "Q1VSU09S")
	out.Reset()
	a = standInApp(t, s.URL, &out)
	a.opts.Env = "prod"
	if err := a.RemotePlans(context.Background(), PlansOptions{Limit: 2}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "More plans match: run the command again with --cursor Q1VSU09S\n") {
		t.Errorf("a page with more plans after it ends\n%s", out.String())
	}
}

// --started-by takes person or ci; a drift check is no plan of this list,
// and the refusal says where drift checks are.
func TestRemotePlansStartedByUsage(t *testing.T) {
	s, queries := plansStandIn(t, "")
	var out bytes.Buffer
	a := standInApp(t, s.URL, &out)
	a.opts.Env = "prod"
	for value, says := range map[string]string{"drift": "drift checks are not plans, and GET /v1/drift lists them", "robot": "--started-by must be person or ci"} {
		err := a.RemotePlans(context.Background(), PlansOptions{StartedBy: value, Limit: 50})
		if _, ok := err.(*UsageError); !ok || !strings.Contains(err.Error(), says) {
			t.Errorf("--started-by %s: %v, want a usage error that says %q", value, err, says)
		}
	}
	if len(*queries) != 0 {
		t.Errorf("a refused command line asked writ serve %v", *queries)
	}
}
