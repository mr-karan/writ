package server_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// auditFixture is a control plane with a principal for each way to see
// the audit log: alice (infra_mr) reads every stack in prod, nora reads
// network@prod only, rita reads every stack in every environment, root
// has admin on every stack in every environment, and zed has no grant.
func auditFixture(t *testing.T) (*fixture, map[string]string) {
	t.Helper()
	f := newFixture(t, func(s string) string {
		return s + `
team "net_readers" {
  issuer = "gitlab"
  match = { user_login = ["nora"] }
}

team "all_readers" {
  issuer = "gitlab"
  match = { user_login = ["rita"] }
}

team "admins" {
  issuer = "gitlab"
  match = { user_login = ["root"] }
}

grant {
  teams = ["net_readers"]
  stacks = ["network"]
  envs = ["prod"]
  can = ["read"]
}

grant {
  teams = ["all_readers"]
  stacks = ["**"]
  envs = ["*"]
  can = ["read"]
}

grant {
  teams = ["admins"]
  stacks = ["**"]
  envs = ["*"]
  can = ["admin"]
}

limits {
  requests_per_second = 1000
  requests_per_address = 1000
}
`
	})
	// A project of no team, so only the user_login teams match.
	as := func(login string) string { return f.token(map[string]any{"user_login": login, "project_id": "5000"}) }
	return f, map[string]string{"alice": f.token(nil), "nora": as("nora"), "rita": as("rita"), "root": as("root"), "zed": as("zed")}
}

// auditPage is GET /v1/audit with query, as tok.
func (f *fixture) auditPage(tok string, query url.Values) (server.AuditDoc, response) {
	f.t.Helper()
	r := f.do("GET", "/v1/audit?"+query.Encode(), tok, nil)
	var doc server.AuditDoc
	if err := json.Unmarshal(r.body, &doc); err != nil {
		f.t.Fatalf("GET /v1/audit?%s: %v: %s", query.Encode(), err, r.body)
	}
	return doc, r
}

// auditAll follows every page of GET /v1/audit with query, as tok.
func (f *fixture) auditAll(tok string, query url.Values) ([]server.AuditRow, []byte) {
	f.t.Helper()
	var rows []server.AuditRow
	var bodies []byte
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	for range 100 {
		doc, r := f.auditPage(tok, q)
		if r.status != 200 || !doc.OK || doc.Format != server.FormatAudit {
			f.t.Fatalf("GET /v1/audit?%s: %d %s", q.Encode(), r.status, r.body)
		}
		rows, bodies = append(rows, doc.Rows...), append(bodies, r.body...)
		if doc.Next == "" {
			return rows, bodies
		}
		q.Set("cursor", doc.Next)
	}
	f.t.Fatal("the pages never end")
	return nil, nil
}

// seed writes rows of kind "t" with the request ids, envs, and stacks of
// rows, at one-second steps from at, oldest first.
func (f *fixture) seed(at time.Time, rows ...control.Entry) {
	f.t.Helper()
	for i, e := range rows {
		e.At, e.Kind = at.Add(time.Duration(i)*time.Second), "t"
		if e.Decision == "" {
			e.Decision = control.Allowed
		}
		if err := f.store.Audit(context.Background(), e); err != nil {
			f.t.Fatal(err)
		}
	}
}

// seeded are the request ids of rows of kind "t", in order.
func seeded(rows []server.AuditRow) string {
	var out []string
	for _, r := range rows {
		if r.Kind == "t" {
			out = append(out, r.Request)
		}
	}
	return strings.Join(out, " ")
}

// A principal sees a row of a stack@env only when it may read every stack
// the row names there, a row of an environment and no stack when it may
// read a stack there, and a row of no environment, or of every one, only
// with admin on every stack in every environment.
func TestAuditReadSeesOnlyWhatItMayRead(t *testing.T) {
	f, tok := auditFixture(t)
	f.seed(time.Now().Add(-time.Hour),
		control.Entry{Request: "net", Env: "prod", Stacks: []string{"network"}, Op: "plan"},
		control.Entry{Request: "app", Env: "prod", Stacks: []string{"app"}, Op: "plan"},
		control.Entry{Request: "both", Env: "prod", Stacks: []string{"app", "network"}, Op: "read"},
		control.Entry{Request: "env", Env: "prod", Op: "read"},
		control.Entry{Request: "other-env", Env: "staging", Stacks: []string{"network"}, Op: "read"},
		control.Entry{Request: "server", Op: "admin", Decision: control.NoDecision},
		control.Entry{Request: "every-env", Env: "*", Op: "admin"},
	)
	for _, tc := range []struct{ who, want string }{
		{"nora", "env net"},
		{"alice", "env both app net"},
		{"rita", "other-env env both app net"},
		{"root", "every-env server other-env env both app net"},
		{"zed", ""},
	} {
		t.Run(tc.who, func(t *testing.T) {
			rows, _ := f.auditAll(tok[tc.who], nil)
			if got := seeded(rows); got != tc.want {
				t.Fatalf("%s sees %q, want %q", tc.who, got, tc.want)
			}
			// Only root sees the server's own rows, such as its start.
			started := slices.ContainsFunc(rows, func(r server.AuditRow) bool { return r.Kind == "started" })
			if started != (tc.who == "root") {
				t.Fatalf("%s sees the start: %v", tc.who, started)
			}
		})
	}
	// A filter of a stack nora may not read answers, with no row.
	doc, r := f.auditPage(tok["nora"], url.Values{"env": {"prod"}, "stack": {"app"}})
	if r.status != 200 || len(doc.Rows) != 0 || doc.Next != "" {
		t.Fatalf("nora's read of app@prod: %d %s", r.status, r.body)
	}
	// The read is audited like any list: a read of the stack@env it names.
	rows := f.audit(controltest.AuditFilter{Request: r.header.Get("X-Request-Id")})
	if len(rows) != 1 || rows[0].Op != "read" || rows[0].Env != "prod" || strings.Join(rows[0].Stacks, ",") != "app" || rows[0].Decision != control.Allowed || rows[0].Route != "/v1/audit" {
		t.Fatalf("the read's own row: %+v", rows)
	}
}

func TestAuditReadFiltersAndPages(t *testing.T) {
	f, tok := auditFixture(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	plan, run := "0123456789abcdef0123456789abcdef", "RUNAAAAAAAAAAAAAAAAAAAAAAA"
	f.seed(at,
		control.Entry{Request: "a", Env: "prod", Stacks: []string{"network"}, Op: "plan", Person: "gitlab/user/1"},
		control.Entry{Request: "b", Env: "prod", Stacks: []string{"network"}, Op: "approve", Plan: plan, Person: "gitlab/user/2"},
		control.Entry{Request: "c", Env: "prod", Stacks: []string{"network"}, Op: "apply", Plan: plan, Run: run, Person: "gitlab/user/1"},
		control.Entry{Request: "d", Env: "prod", Stacks: []string{"app"}, Op: "state-write", Run: run},
		control.Entry{Request: "e", Env: "prod", Stacks: []string{"app"}, Op: "read", Person: "gitlab/user/2"},
	)
	for _, tc := range []struct {
		name  string
		query url.Values
		want  string
	}{
		{"a plan", url.Values{"plan": {plan}}, "c b"},
		{"a run", url.Values{"run": {run}}, "d c"},
		{"a stack in an environment", url.Values{"env": {"prod"}, "stack": {"app"}}, "e d"},
		{"a person", url.Values{"person": {"gitlab/user/1"}}, "c a"},
		{"an operation", url.Values{"op": {"approve"}}, "b"},
		{"since and until", url.Values{"since": {at.Add(time.Second).Format(time.RFC3339)}, "until": {at.Add(3 * time.Second).Format(time.RFC3339)}}, "c b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := f.auditAll(tok["rita"], tc.query)
			if got := seeded(rows); got != tc.want {
				t.Fatalf("rows %q, want %q", got, tc.want)
			}
		})
	}
	// Pages of two, newest first, each row once. The time range leaves
	// out the rows of this test's own reads.
	var got []string
	q := url.Values{"limit": {"2"}, "env": {"prod"}, "since": {at.Format(time.RFC3339)}, "until": {at.Add(time.Minute).Format(time.RFC3339)}}
	for range 10 {
		doc, r := f.auditPage(tok["rita"], q)
		if r.status != 200 || len(doc.Rows) > 2 {
			t.Fatalf("page: %d %s", r.status, r.body)
		}
		got = append(got, seeded(doc.Rows))
		if doc.Next == "" {
			break
		}
		q.Set("cursor", doc.Next)
	}
	if strings.Join(got, "|") != "e d|c b|a" {
		t.Fatalf("pages %q", got)
	}
	row := func() server.AuditRow {
		doc, _ := f.auditPage(tok["rita"], url.Values{"run": {run}, "limit": {"1"}})
		return doc.Rows[0]
	}()
	if row.Request != "d" || row.At != at.Add(3*time.Second).UTC().Format(time.RFC3339) || row.Op != "state-write" || row.Env != "prod" || strings.Join(row.Stacks, ",") != "app" || row.Run != run || row.Decision != "allowed" {
		t.Fatalf("a row as the API shows it: %+v", row)
	}
	for _, tc := range []struct {
		name  string
		query url.Values
		want  string
	}{
		{"limit 0", url.Values{"limit": {"0"}}, "limit must be 1 to 200"},
		{"limit 201", url.Values{"limit": {"201"}}, "limit must be 1 to 200"},
		{"a stack with no env", url.Values{"stack": {"network"}}, "stack needs env"},
		{"an env that is not a name", url.Values{"env": {"Prod!"}}, "is not one writ serve plans in"},
		{"an env writ serve does not serve", url.Values{"env": {"staging"}}, "is not one writ serve plans in"},
		{"a stack that is not a name", url.Values{"env": {"prod"}, "stack": {"../etc"}}, "stack name"},
		{"a plan that is not an id or a ref", url.Values{"plan": {"nope"}}, "plan must be"},
		{"a run that is not a run id", url.Values{"run": {"nope"}}, "run must be"},
		{"a person over 256 bytes", url.Values{"person": {strings.Repeat("p", 257)}}, "person is over 256 bytes"},
		{"an operation it does not know", url.Values{"op": {"nope"}}, "op must be one of"},
		{"since not RFC 3339", url.Values{"since": {"yesterday"}}, "since must be an RFC 3339 time"},
		{"until not RFC 3339", url.Values{"until": {"1700000000"}}, "until must be an RFC 3339 time"},
		{"until before since", url.Values{"since": {at.Format(time.RFC3339)}, "until": {at.Add(-time.Second).Format(time.RFC3339)}}, "until must be after since"},
		{"a cursor of no page", url.Values{"cursor": {"zzz"}}, "cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, r := f.auditPage(tok["rita"], tc.query)
			if r.status != 400 || r.class() != "usage" || doc.OK || doc.Format != server.FormatAudit || !strings.Contains(string(r.body), tc.want) {
				t.Fatalf("got %d %s, want 400 usage %q", r.status, r.body, tc.want)
			}
		})
	}
	if r := f.do("GET", "/v1/audit", "", nil); r.status != 401 || r.class() != "auth" || r.doc["format"] != server.FormatAudit {
		t.Fatalf("no token: %d %s", r.status, r.body)
	}
}

// A secret that a request carries never reaches GET /v1/audit, even for
// the principal that sees every row: in the token, the path, the query,
// or the body. The audit rows keep what writ serve answered the request,
// which can quote what it sent, so only the reasons writ serve composed
// itself are shown.
func TestAuditReadNeverShowsWhatARequestSent(t *testing.T) {
	f, tok := auditFixture(t)
	// The ~ makes the secret no env or stack name, which a row shows as
	// what it concerns once writ serve checked the name.
	secret := "audit~canary~" + rand.Text()
	for _, req := range []struct {
		method, path, token, body string
	}{
		{"GET", "/v1/whoami", secret, ""},
		{"GET", "/v1/plans/" + secret, tok["alice"], ""},
		{"GET", "/v1/" + secret, tok["alice"], ""},
		{"GET", "/v1/applies?env=" + secret, tok["alice"], ""},
		{"GET", "/v1/applies?env=prod&stack=" + secret, tok["alice"], ""},
		{"POST", "/v1/plans", tok["alice"], `{"format":"` + secret + `"}`},
		{"POST", "/v1/runs", tok["alice"], `{"format":"writ.run-request/v1","plan":"` + secret + `"}`},
		{"GET", "/v1/audit?plan=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?env=prod&stack=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?person=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?env=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?op=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?cursor=" + secret, tok["root"], ""},
		{"GET", "/v1/audit?since=" + secret, tok["root"], ""},
		{secret, "/v1/whoami", tok["alice"], ""},
	} {
		r := f.do(req.method, req.path, req.token, strings.NewReader(req.body))
		if r.status < 400 && !strings.Contains(req.path, "person=") {
			t.Fatalf("%s %s: %d %s", req.method, req.path, r.status, r.body)
		}
	}
	// The control database holds the secret, so the API must hold it back.
	held := false
	for _, e := range f.audit(controltest.AuditFilter{Limit: 1000}) {
		held = held || strings.Contains(e.Reason, secret)
	}
	if !held {
		t.Fatal("no audit row quotes the secret; the test no longer proves the API holds it back")
	}
	rows, bodies := f.auditAll(tok["root"], url.Values{"limit": {"200"}})
	if strings.Contains(string(bodies), secret) {
		t.Fatalf("GET /v1/audit shows the secret: %s", bodies)
	}
	withheld, other := 0, false
	for _, r := range rows {
		other = other || r.Method == "OTHER"
		if r.Decision != "allowed" && r.Decision != "none" {
			if r.Reason != "" {
				t.Fatalf("a %s row shows its reason: %+v", r.Decision, r)
			}
			if r.ReasonWithheld {
				withheld++
			}
		}
		if r.Kind == "started" && r.Reason != "writ test" {
			t.Fatalf("the start's reason, which writ serve wrote, is %q", r.Reason)
		}
	}
	if withheld == 0 {
		t.Fatalf("no row says its reason is withheld: %+v", rows)
	}
	if !other {
		t.Fatalf("no row shows the method of no standard set as OTHER: %+v", rows)
	}
}
