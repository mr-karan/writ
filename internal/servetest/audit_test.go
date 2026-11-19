package servetest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/cli"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/servetest"
)

// writ audit --remote prints the rows GET /v1/audit answers: a table, a
// line with the cursor while more rows match, and the document with
// --json.
func TestRemoteAudit(t *testing.T) {
	srv := servetest.Start(t, servetest.Options{})
	ctx := context.Background()
	at := time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)
	for i, e := range []control.Entry{
		{Request: "R1", Kind: "request", Method: "POST", Route: "/v1/plans", Status: 202, Principal: "tester (dev)", Op: "plan", Env: "dev", Stacks: []string{"app"}, Decision: control.Allowed},
		{Request: "R2", Kind: "request", Method: "POST", Route: "/v1/runs", Status: 403, Principal: "tester (dev)", Op: "apply", Env: "dev", Stacks: []string{"app"}, Decision: control.Denied, Reason: "what the request sent"},
		{Request: "R3", Kind: "state-unlock", Op: "state-unlock", Env: "dev", Stacks: []string{"db"}, Decision: control.NoDecision, Reason: "released"},
	} {
		e.At = at.Add(time.Duration(i) * time.Minute)
		if err := srv.Store.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	app := cli.New(srv.Client(cli.Options{Dir: t.TempDir(), Stdout: &out, Stderr: &out, Env: "dev"}, nil))
	window := cli.AuditOptions{Since: at.Format(time.RFC3339), Until: at.Add(time.Hour).Format(time.RFC3339), Limit: cli.DefaultAuditLimit}
	if err := app.RemoteAudit(ctx, window); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := strings.Join([]string{
		"AT                    BY            OP            ON      WHAT                   DECISION  PLAN  RUN  REASON",
		"2026-11-19T09:32:00Z  -             state-unlock  db@dev  state-unlock           none      -     -    released",
		"2026-11-19T09:31:00Z  tester (dev)  apply         app@dev  POST /v1/runs 403      denied    -     -    (withheld)",
		"2026-11-19T09:30:00Z  tester (dev)  plan          app@dev  POST /v1/plans 202     allowed   -     -    -",
	}, "\n") + "\n"
	if got := out.String(); squeeze(got) != squeeze(want) {
		t.Fatalf("table:\n%s\nwant:\n%s", got, want)
	}

	out.Reset()
	one := window
	one.Stack, one.Limit = "app", 1
	if err := app.RemoteAudit(ctx, one); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "POST /v1/runs 403") || !strings.HasPrefix(lines[2], "More rows match: run the command again with --cursor ") {
		t.Fatalf("one row of app@dev:\n%s", out.String())
	}
	out.Reset()
	one.Cursor = strings.TrimPrefix(lines[2], "More rows match: run the command again with --cursor ")
	if err := app.RemoteAudit(ctx, one); err != nil || !strings.Contains(out.String(), "POST /v1/plans 202") || strings.Contains(out.String(), "More rows") {
		t.Fatalf("the next page: %v\n%s", err, out.String())
	}

	out.Reset()
	window.JSON, window.Op = true, "apply"
	if err := app.RemoteAudit(ctx, window); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var doc server.AuditDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Format != server.FormatAudit || len(doc.Rows) != 1 || doc.Rows[0].Request != "R2" || !doc.Rows[0].ReasonWithheld || doc.Rows[0].Reason != "" {
		t.Fatalf("--json: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := app.RemoteAudit(ctx, cli.AuditOptions{Plan: "nope", Limit: 10}); err == nil || !strings.Contains(out.String(), "plan must be") {
		t.Fatalf("a plan that is not an id: %v\n%s", err, out.String())
	}
}

// squeeze folds each run of spaces to one, so a table compares by its
// cells rather than by the widths the tab writer chose.
func squeeze(s string) string {
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	return strings.Join(lines, "\n")
}
