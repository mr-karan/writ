package ui_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// auditUIFixture configures three people roles on the control UI:
// - nora: reads network@prod only
// - rita: reads every stack in every environment
// - root: admin on every stack in every environment
func auditUIFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureOn(t, controltest.SQLite, `
team "net_readers" {
  issuer = "sso"
  match  = { groups = ["acme/net-readers"] }
}

team "all_readers" {
  issuer = "sso"
  match  = { groups = ["acme/all-readers"] }
}

team "admins" {
  issuer = "sso"
  match  = { groups = ["acme/admins"] }
}

grant {
  teams  = ["net_readers"]
  stacks = ["network"]
  envs   = ["prod"]
  can    = ["read"]
}

grant {
  teams  = ["all_readers"]
  stacks = ["**"]
  envs   = ["*"]
  can    = ["read"]
}

grant {
  teams  = ["admins"]
  stacks = ["**"]
  envs   = ["*"]
  can    = ["admin"]
}
`)
}

func signInGroup(f *fixture, sub, name, group string) *browser {
	f.t.Helper()
	claims, info := oidctest.GitLabPerson()
	claims["sub"], claims["preferred_username"], claims["groups_direct"] = sub, name, []string{group}
	info["sub"], info["preferred_username"], info["groups"] = sub, name, []string{"acme", group}
	f.is.SetPerson(claims, info)
	b := f.browser()
	b.signIn("/ui/")
	return b
}

// /ui/audit enforces the same per-row visibility rules as GET /v1/audit:
// a reader of one stack sees only that stack's rows and env-wide rows of
// that env; a reader of every stack sees no server-wide rows; only an
// admin on every stack in every environment sees server-wide rows.
func TestAuditPageAuthorization(t *testing.T) {
	f := auditUIFixture(t)
	at := f.clock.Now().Add(-time.Hour).Truncate(time.Second)
	for i, e := range []control.Entry{
		{Request: "AUDNET00000000000000000001", Env: "prod", Stacks: []string{"network"}, Op: "plan", Method: "POST", Route: "/v1/plans", Status: 202, Principal: "alice-ci", PrincipalKind: "ci-job", Person: "gitlab/user/1001", Decision: control.Allowed},
		{Request: "AUDAPP00000000000000000002", Env: "prod", Stacks: []string{"app"}, Op: "plan", Method: "POST", Route: "/v1/plans", Status: 202, Principal: "bob-dev", PrincipalKind: "person", Person: "gitlab/user/2002", Decision: control.Allowed},
		{Request: "AUDENV00000000000000000003", Env: "prod", Op: "read", Method: "GET", Route: "/v1/applies", Status: 200, Principal: "carol", PrincipalKind: "person", Person: "gitlab/user/3003", Decision: control.Allowed},
		{Request: "AUDSTG00000000000000000004", Env: "staging", Stacks: []string{"network"}, Op: "read", Method: "GET", Route: "/v1/applies", Status: 200, Principal: "dave", PrincipalKind: "person", Person: "gitlab/user/4004", Decision: control.Allowed},
		{Request: "AUDSRV00000000000000000005", Kind: "started", Decision: control.NoDecision, Reason: "writ audit-test-srv"},
	} {
		e.At = at.Add(time.Duration(i) * time.Second)
		if e.Kind == "" {
			e.Kind = "request"
		}
		if err := f.store.Audit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	nora := signInGroup(f, "7001", "nora", "acme/net-readers")
	rita := signInGroup(f, "7002", "rita", "acme/all-readers")
	root := signInGroup(f, "7003", "root", "acme/admins")

	for _, tc := range []struct {
		name string
		b    *browser
		want []string
		deny []string
	}{
		{
			name: "nora reads network@prod only",
			b:    nora,
			want: []string{"AUDNET00000000000000000001", "AUDENV00000000000000000003", "alice-ci", `href="#i-ci"`},
			deny: []string{"AUDAPP00000000000000000002", "AUDSTG00000000000000000004", "AUDSRV00000000000000000005", "writ audit-test-srv"},
		},
		{
			name: "rita reads every stack but not server-wide rows",
			b:    rita,
			want: []string{"AUDNET00000000000000000001", "AUDAPP00000000000000000002", "AUDENV00000000000000000003", "AUDSTG00000000000000000004"},
			deny: []string{"AUDSRV00000000000000000005", "writ audit-test-srv"},
		},
		{
			name: "root admin sees server-wide rows too",
			b:    root,
			want: []string{"AUDNET00000000000000000001", "AUDAPP00000000000000000002", "AUDSRV00000000000000000005", "writ audit-test-srv"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The reads show too, so every row the rules allow shows.
			p := tc.b.get("/ui/audit?reads=1")
			if p.status != http.StatusOK {
				t.Fatalf("GET /ui/audit?reads=1: %d %s", p.status, text(p.doc))
			}
			checkPage(t, p, "")
			for _, w := range tc.want {
				if !strings.Contains(p.body, w) {
					t.Errorf("/ui/audit lacks %q", w)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(p.body, d) {
					t.Errorf("/ui/audit leaks %q", d)
				}
			}
		})
	}

	// Filtering on an unreadable stack yields an empty page with no cursor.
	p := nora.get("/ui/audit?env=prod&stack=app")
	if p.status != http.StatusOK || strings.Contains(p.body, "AUDAPP00000000000000000002") || strings.Contains(p.body, "Older entries") {
		t.Fatalf("nora filtering on app@prod: %d", p.status)
	}
	checkPage(t, p, "")
}

// Mirror of TestAuditReadNeverShowsWhatARequestSent for /ui/audit: a secret
// sent in a token, path, query, body, or method, or in a filter of the page
// itself, never appears on /ui/audit, even for an admin. Refused rows show
// "reason withheld" apart from empty reasons, state-write hashes are
// stripped, and client addresses never appear.
func TestAuditPageNeverLeaksRequestSecrets(t *testing.T) {
	f := auditUIFixture(t)
	secret := "audit~ui~canary~" + rand.Text()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots}}}
	ciTok := f.token(nil)
	for _, req := range []struct {
		method, path, token, body string
	}{
		{"GET", "/v1/whoami", secret, ""},
		{"GET", "/v1/plans/" + secret, ciTok, ""},
		{"GET", "/v1/" + secret, ciTok, ""},
		{"GET", "/v1/applies?env=" + secret, ciTok, ""},
		{"GET", "/v1/applies?env=prod&stack=" + secret, ciTok, ""},
		{"POST", "/v1/plans", ciTok, `{"format":"` + secret + `"}`},
		{"POST", "/v1/runs", ciTok, `{"format":"writ.run-request/v1","plan":"` + secret + `"}`},
		{secret, "/v1/whoami", ciTok, ""},
	} {
		r, err := http.NewRequest(req.method, f.url+req.path, strings.NewReader(req.body))
		if err != nil {
			t.Fatal(err)
		}
		if req.token != "" {
			r.Header.Set("Authorization", "Bearer "+req.token)
		}
		if req.body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Seed a state-write row with a sha256 hash and an allowed row with an
	// empty reason, plus a client address on both.
	sum := sha256.Sum256([]byte("secret-state-payload"))
	hashHex := hex.EncodeToString(sum[:])
	now := f.clock.Now().Add(-time.Minute).Truncate(time.Second)
	for _, e := range []control.Entry{
		{At: now, Request: "STATEWR0000000000000000001", Kind: "request", Method: "POST", Route: "/v1/tfstate/{stack}", Status: 200, Remote: "198.51.100.42", Env: "prod", Stacks: []string{"network"}, Op: "state-write", Decision: control.Allowed, Reason: "state version 1, serial 1, lineage lin-1, sha256 " + hashHex},
		{At: now.Add(time.Second), Request: "EMPTYRS0000000000000000002", Kind: "request", Method: "GET", Route: "/v1/applies", Status: 200, Remote: "198.51.100.43", Env: "prod", Stacks: []string{"network"}, Op: "read", Principal: "nora", PrincipalKind: "person", Person: "gitlab/user/7001", Decision: control.Allowed, Reason: ""},
	} {
		if err := f.store.Audit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	root := signInGroup(f, "7003", "root", "acme/admins")
	for _, q := range []string{"plan=", "env=prod&stack=", "person=", "env=", "op=", "cursor=", "since=", "until="} {
		root.get("/ui/audit?" + q + url.QueryEscape(secret))
	}
	// The control database holds the secret, so the page must hold it back.
	held := false
	for _, e := range controltest.Audit(t, f.loc, controltest.AuditFilter{Limit: 1000}) {
		held = held || strings.Contains(e.Reason, secret)
	}
	if !held {
		t.Fatal("no audit row quotes the secret; the test no longer proves the page holds it back")
	}
	// Read every page, with the reads, so no row the secret's requests
	// left is past the first or left out.
	var body strings.Builder
	for next, pages := "/ui/audit?reads=1", 0; next != ""; pages++ {
		if pages == 20 {
			t.Fatal("/ui/audit has over 20 pages")
		}
		p := root.get(next)
		if p.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", next, p.status, text(p.doc))
		}
		checkPage(t, p, "")
		body.WriteString(p.body)
		next = ""
		each(p.doc, func(n *html.Node) {
			if n.Data == "a" && strings.TrimSpace(text(n)) == "Older entries" {
				next, _ = attr(n, "href")
			}
		})
	}
	all := body.String()
	for _, forbidden := range []string{secret, hashHex, "sha256", "198.51.100.42", "198.51.100.43"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("/ui/audit leaks %q", forbidden)
		}
	}
	if !strings.Contains(all, "reason withheld") {
		t.Error("/ui/audit does not mark refused rows as reason withheld")
	}
	if !strings.Contains(all, "state version 1, serial 1, lineage lin-1") {
		t.Error("/ui/audit missing state-write reason (with hash stripped)")
	}
	if !strings.Contains(all, "OTHER /v1/whoami") {
		t.Error("/ui/audit does not show nonstandard method as OTHER")
	}
	// The allowed row with an empty reason does not say "reason withheld".
	emptyRow := false
	for _, tr := range strings.Split(all, "<tr")[1:] {
		tr, _, _ = strings.Cut(tr, "</tr>")
		if strings.Contains(tr, "EMPTYRS0000000000000000002") {
			emptyRow = true
			if strings.Contains(tr, "reason withheld") {
				t.Errorf("allowed row with empty reason shows reason withheld: %s", tr)
			}
		}
	}
	if !emptyRow {
		t.Error("/ui/audit lacks the allowed row with an empty reason")
	}
}

// /ui/audit filters work as GET parameters with scripts off, pages with the
// sealed cursor ("Older entries"), and is linked from the sidebar, plan
// pages, and run pages.
func TestAuditPageFiltersPagingAndLinks(t *testing.T) {
	f := auditUIFixture(t)
	planID := f.plan(nil)
	rita := signInGroup(f, "7002", "rita", "acme/all-readers")
	root := signInGroup(f, "7003", "root", "acme/admins")

	// Sidebar links to /ui/audit, and plan/run pages have "Audit trail".
	planPage := rita.get("/ui/plans/" + planID)
	if planPage.status != http.StatusOK {
		t.Fatalf("plan page: %d", planPage.status)
	}
	checkPage(t, planPage, "")
	for _, want := range []string{`href="/ui/audit"`, `href="/ui/audit?plan=` + planID + `">Audit trail</a>`} {
		if !strings.Contains(planPage.body, want) {
			t.Errorf("plan page lacks %s", want)
		}
	}

	// Approve and apply to get a run page with an Audit trail link.
	f.is.SetPerson(oidctest.GitLabPerson())
	approver := f.browser()
	approver.signIn("/ui/")
	if p := approver.post("/ui/plans/"+planID+"/approve", url.Values{}, "same-origin"); p.status != http.StatusOK {
		t.Fatalf("approve: %d", p.status)
	}
	runID := f.apply(planID)
	runPage := rita.get("/ui/runs/" + runID)
	if runPage.status != http.StatusOK {
		t.Fatalf("run page: %d", runPage.status)
	}
	checkPage(t, runPage, "")
	if want := `href="/ui/audit?run=` + runID + `">Audit trail</a>`; !strings.Contains(runPage.body, want) {
		t.Errorf("run page lacks %s", want)
	}

	// Seed 55 rows in a clean time window to verify every filter and cursor paging.
	base := f.clock.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for i := range 55 {
		e := control.Entry{
			At:            base.Add(time.Duration(i) * time.Second),
			Request:       "PAGE" + strings.Repeat("A", 20) + string(rune('A'+i/26)) + string(rune('A'+i%26)),
			Kind:          "request",
			Method:        "GET",
			Route:         "/v1/plans",
			Status:        200,
			Env:           "prod",
			Stacks:        []string{"network"},
			Op:            "read",
			Principal:     "alice",
			PrincipalKind: "person",
			Person:        "gitlab/user/1001",
			Decision:      control.Allowed,
		}
		if err := f.store.Audit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	since := base.Format(time.RFC3339)
	until := base.Add(time.Minute).Format(time.RFC3339)
	q := url.Values{"env": {"prod"}, "stack": {"network"}, "person": {"gitlab/user/1001"}, "op": {"read"}, "since": {since}, "until": {until}}
	p1 := root.get("/ui/audit?" + q.Encode())
	if p1.status != http.StatusOK {
		t.Fatalf("filtered audit page 1: %d %s", p1.status, text(p1.doc))
	}
	checkPage(t, p1, "")
	if !strings.Contains(p1.body, "Older entries") {
		t.Fatalf("page 1 with 55 matching rows lacks Older entries link")
	}

	// Follow the "Older entries" sealed cursor link.
	var nextHref string
	each(p1.doc, func(n *html.Node) {
		if n.Data == "a" && strings.TrimSpace(text(n)) == "Older entries" {
			nextHref, _ = attr(n, "href")
		}
	})
	if nextHref == "" || !strings.Contains(nextHref, "cursor=") {
		t.Fatalf("Older entries href: %q", nextHref)
	}
	p2 := root.get(nextHref)
	if p2.status != http.StatusOK {
		t.Fatalf("filtered audit page 2: %d %s", p2.status, text(p2.doc))
	}
	checkPage(t, p2, "")
	if strings.Contains(p2.body, "Older entries") {
		t.Errorf("page 2 still has Older entries when only 5 rows remain")
	}

	// Filtering by plan and by run both render cleanly and pass render checks.
	for _, path := range []string{"/ui/audit?plan=" + planID, "/ui/audit?run=" + runID} {
		p := root.get(path)
		if p.status != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, p.status, text(p.doc))
		}
		checkPage(t, p, "")
	}
	// JSON link returns writ.audit/v0.
	if body := root.read("/ui/audit?env=prod&as=json"); !strings.Contains(string(body), `"format":"`+server.FormatAudit+`"`) {
		t.Errorf("/ui/audit?as=json: %s", body)
	}
}

// /ui/audit shows only the changes by default: the reads that its own
// pages, their static files, and CI polls write are left out, as GET
// /v1/audit?changes=true leaves them out. Show reads, a link that works
// with scripts off, shows every row, and every link and the filter form
// of that view keep it. An op filter shows that op's rows, reads too. The
// By column keeps a principal and its person id on one line each.
func TestAuditPageShowsChangesUnlessReadsAreAsked(t *testing.T) {
	f := auditUIFixture(t)
	at := f.clock.Now().Add(-time.Hour).Truncate(time.Second)
	for i, e := range []control.Entry{
		{Request: "CHGPLAN0000000000000000001", Env: "prod", Stacks: []string{"network"}, Op: "plan", Method: "POST", Route: "/v1/plans", Status: 202, Principal: "tester (dev)", PrincipalKind: "person", Person: "dev/tester", Decision: control.Allowed},
		{Request: "CHGREAD0000000000000000002", Env: "prod", Stacks: []string{"network"}, Op: "read", Method: "GET", Route: "/v1/applies", Status: 200, Principal: "tester (dev)", PrincipalKind: "person", Person: "dev/tester", Decision: control.Allowed},
		{Request: "CHGSTAT0000000000000000003", Method: "GET", Route: "/ui/static/", Status: 200, Decision: control.NoDecision},
		{Request: "CHGAPLY0000000000000000004", Env: "prod", Stacks: []string{"network"}, Op: "apply", Method: "POST", Route: "/v1/runs", Status: 202, Principal: "tester (dev)", PrincipalKind: "person", Person: "dev/tester", Decision: control.Allowed},
	} {
		e.At, e.Kind = at.Add(time.Duration(i)*time.Second), "request"
		if err := f.store.Audit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	root := signInGroup(f, "7003", "root", "acme/admins")
	has := func(p page, ids ...string) string {
		var out []string
		for _, id := range ids {
			if strings.Contains(p.body, id) {
				out = append(out, id[:7])
			}
		}
		return strings.Join(out, " ")
	}
	ids := []string{"CHGPLAN0000000000000000001", "CHGREAD0000000000000000002", "CHGSTAT0000000000000000003", "CHGAPLY0000000000000000004"}
	window := "&since=" + url.QueryEscape(at.Format(time.RFC3339))

	changes := root.get("/ui/audit?since=" + url.QueryEscape(at.Format(time.RFC3339)))
	checkPage(t, changes, "")
	if got := has(changes, ids...); got != "CHGPLAN CHGAPLY" {
		t.Errorf("the default view shows %s, want CHGPLAN CHGAPLY", got)
	}
	if !strings.Contains(changes.body, `<p>CLI <code>writ audit --remote --changes --since`) {
		t.Errorf("the default view's command lacks --changes")
	}
	var readsHref string
	each(changes.doc, func(n *html.Node) {
		if n.Data == "a" && strings.TrimSpace(text(n)) == "Show reads" {
			readsHref, _ = attr(n, "href")
		}
	})
	if want := "/ui/audit?reads=1" + window; readsHref != want {
		t.Fatalf("Show reads links %q, want %q", readsHref, want)
	}
	if !strings.Contains(changes.body, `aria-current="true">Changes</a>`) || strings.Contains(changes.body, `name="reads"`) {
		t.Errorf("the default view's toggle or form:\n%s", changes.body)
	}

	reads := root.get(readsHref)
	checkPage(t, reads, "")
	if got := has(reads, ids...); got != "CHGPLAN CHGREAD CHGSTAT CHGAPLY" {
		t.Errorf("Show reads shows %s, want every row", got)
	}
	if !strings.Contains(reads.body, `aria-current="true">Show reads</a>`) || !strings.Contains(reads.body, `<input type="hidden" name="reads" value="1">`) {
		t.Errorf("the reads view's toggle or form:\n%s", reads.body)
	}
	if strings.Contains(reads.body, "--changes") {
		t.Errorf("the reads view's command has --changes")
	}
	// Every link of the reads view to the page keeps reads=1, but Changes
	// and Clear filters.
	each(reads.doc, func(n *html.Node) {
		href, _ := attr(n, "href")
		if n.Data != "a" || !strings.HasPrefix(href, "/ui/audit?") {
			return
		}
		if label := strings.TrimSpace(text(n)); label != "Changes" && !strings.Contains(href, "reads=1") {
			t.Errorf("the link %q, %s, drops reads=1", label, href)
		}
	})

	// An op filter shows that op's rows, reads too, and no toggle.
	op := root.get("/ui/audit?op=read" + window)
	if got := has(op, ids...); got != "CHGREAD" || strings.Contains(op.body, "Show reads") {
		t.Errorf("op=read shows %s", got)
	}

	// The By cell keeps "tester (dev)" and dev/tester on a line each.
	if !strings.Contains(reads.body, `<td data-label="By" class="audit-by">`) {
		t.Error("the By cell lacks audit-by")
	}
}
