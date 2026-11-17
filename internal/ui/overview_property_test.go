package ui_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// overviewGrants adds two readers to the fixture's platform team, which
// reads every stack in every environment: one of the stacks named web*
// and network in every environment, and one of every stack in prod.
const overviewGrants = `
team "web_readers" {
  issuer = "sso"
  match  = { groups = ["acme/web"] }
}

grant {
  teams  = ["web_readers"]
  stacks = ["web*", "network"]
  envs   = ["*"]
  can    = ["read"]
}

team "prod_readers" {
  issuer = "sso"
  match  = { groups = ["acme/prod"] }
}

grant {
  teams  = ["prod_readers"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["read"]
}
`

// overviewReader is a person and what their grants let them read.
type overviewReader struct {
	name, group, sub string
	envs             []string
	reads            func(env, stack string) bool
}

// person is the reader's stable id: the sso issuer's identity and the
// reader's user id.
func (rd overviewReader) person() string { return "gitlab/user/" + rd.sub }

var overviewReaders = []overviewReader{
	{"platform", "acme/platform", "2002", []string{"prod", "staging"}, func(string, string) bool { return true }},
	{"web", "acme/web", "4004", []string{"prod", "staging"}, func(_, stack string) bool { return strings.HasPrefix(stack, "web") || stack == "network" }},
	{"prod", "acme/prod", "5005", []string{"prod"}, func(env, _ string) bool { return env == "prod" }},
}

// estate is what one property case seeds beside the fixture's own rows:
// stacks with state and their newest drift check, plans, and failed runs.
// A part of stacks with n of 1 names its stack prefix, a larger one
// prefix000, prefix001, and on. A part of failed runs is n of them on the
// stack prefix. Plans are seeded in the order listed, oldest first, so a
// part listed first is the one a cut leaves out. cut names a stack whose
// every plan a cut leaves out, which the sidebar's filter looks for.
type estate struct {
	name   string
	stacks []estatePart
	plans  []planPart
	failed []estatePart
	cut    string
}

type estatePart struct {
	env, prefix string
	n           int
	// check is the status of the stack's newest drift check, "" for none.
	check string
}

func (p estatePart) names() []string {
	if p.n == 1 {
		return []string{p.prefix}
	}
	out := make([]string, p.n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%03d", p.prefix, i)
	}
	return out
}

// planPart is n plans of stack@env in a stored status. A pending or
// approved plan expires in 12 hours unless expires says otherwise. One
// that shows approved carries an approval that counts, and one that shows
// pending none, so its status of now is the stored one unless shows names
// another, as a change of config that moved need would leave it. by names
// the reader who made them.
type planPart struct {
	env, stack string
	n          int
	status     string
	shows      string
	expires    time.Duration
	by         string
}

// overviewEstates hold more than 200 stacks, plans, drift alerts, and
// failed runs, plans of every status, and stacks whose only plans a cut
// leaves out, spread over prod and staging so each reader can read some
// of them and not others.
var overviewEstates = []estate{
	{
		// 201 pending plans of app the web reader may not read, 201 drift
		// alerts in prod ahead of staging's, and 402 stacks in prod listed
		// before staging's.
		name: "prod crowds staging",
		stacks: []estatePart{
			{"prod", "a", 201, "clean"},
			{"prod", "drift", 201, "drifted"},
			{"staging", "web-s1", 1, "failed"},
			{"staging", "web-s2", 1, "drifted"},
			{"staging", "net-s", 1, "clean"},
		},
		plans:  []planPart{{env: "prod", stack: "app", n: 201, status: "pending"}},
		failed: []estatePart{{"prod", "app", 201, ""}},
	},
	{
		// More than 200 of each that the web reader may read too, so the
		// cut lists are legitimately partial. web200 has state and a plan
		// that waits, and the list of stacks with state cuts it.
		name: "web stacks crowd every list",
		stacks: []estatePart{
			{"staging", "web", 201, "drifted"},
			{"prod", "app", 1, "failed"},
			{"prod", "web-p", 1, "clean"},
		},
		plans:  []planPart{{env: "prod", stack: "web-p", n: 201, status: "pending"}, {env: "staging", stack: "web200", n: 1, status: "pending"}},
		failed: []estatePart{{"prod", "web-p", 201, ""}, {"staging", "app", 3, ""}},
	},
	{
		// Few rows, so every value is exact and staging is all clear.
		name: "small",
		stacks: []estatePart{
			{"prod", "web-x", 1, "clean"},
			{"prod", "app", 1, "drifted"},
			{"staging", "web-y", 1, "clean"},
			{"staging", "db", 1, ""},
		},
	},
	{
		// A plan of every status, on stacks with state and on stacks with
		// none, some of each reader's own, one about to expire, and a stack
		// whose plans have all ended.
		name: "every status",
		stacks: []estatePart{
			{"prod", "web-s", 1, "clean"},
			{"prod", "app2", 1, "failed"},
			{"staging", "web-t", 1, "drifted"},
		},
		plans: []planPart{
			{env: "prod", stack: "web-s", n: 1, status: "planning"},
			{env: "prod", stack: "web-s", n: 2, status: "pending"},
			{env: "prod", stack: "web-s", n: 1, status: "pending", by: "web"},
			{env: "prod", stack: "app2", n: 1, status: "pending", by: "platform"},
			{env: "prod", stack: "web-s", n: 1, status: "approved"},
			{env: "prod", stack: "web-s", n: 1, status: "applying"},
			{env: "prod", stack: "web-s", n: 1, status: "pending", expires: -time.Hour},
			{env: "prod", stack: "net-old", n: 1, status: "applied"},
			{env: "prod", stack: "net-old", n: 1, status: "failed"},
			{env: "prod", stack: "net-old", n: 1, status: "refused"},
			{env: "prod", stack: "net-old", n: 1, status: "superseded"},
			{env: "prod", stack: "net-old", n: 1, status: "expired", expires: -2 * time.Hour},
			{env: "prod", stack: "net-old", n: 1, status: "approved", expires: -time.Hour},
			{env: "staging", stack: "web-new", n: 1, status: "pending", expires: time.Hour},
			{env: "staging", stack: "web-new", n: 1, status: "approved"},
			{env: "staging", stack: "app-new", n: 1, status: "applying"},
			{env: "staging", stack: "app-new", n: 1, status: "planning"},
			{env: "prod", stack: "web-s", n: 1, status: "approved", shows: "pending", by: "web"},
			{env: "staging", stack: "web-new", n: 1, status: "pending", shows: "approved"},
		},
		failed: []estatePart{{"staging", "web-t", 2, ""}},
	},
	{
		// One approved plan and no stack with state: something waits.
		name:  "approved alone",
		plans: []planPart{{env: "staging", stack: "web-ok", n: 1, status: "approved"}},
	},
	{
		// 200 newer pending plans of web-p push out the only plans of
		// web-lost, which has no state, and of web-c, which has state and
		// a clean check, while every stack with state is listed.
		name:   "a cut plan hides a stack",
		stacks: []estatePart{{"prod", "web-c", 1, "clean"}},
		plans: []planPart{
			{env: "prod", stack: "web-lost", n: 1, status: "pending"},
			{env: "prod", stack: "web-c", n: 1, status: "pending"},
			{env: "prod", stack: "web-p", n: 200, status: "pending"},
			{env: "staging", stack: "web-z", n: 1, status: "approved"},
		},
		cut: "web-lost",
	},
}

// TestOverviewProperty checks GET /v1/overview, the overview page, and the
// sidebar as properties over estates and readers, on SQLite and Postgres:
//
//	(a) the answer is the same with and without the rows the reader may
//	    not read;
//	(b) every exact count equals a count over the rows the reader may read;
//	(c) every value that leaves rows out is marked partial;
//	(d) every number, dot, and sentence the overview and the sidebar
//	    render holds of the rows the reader may read: checkRender.
//
// TestOverviewPropertyCrowded runs the same over the crowded estates.
func TestOverviewProperty(t *testing.T) {
	overviewProperty(t, estatesWhere(overviewEstates, func(name string) bool { return !slices.Contains(crowded, name) }))
}

// TestOverviewPropertyCrowded is TestOverviewProperty over the estates
// whose lists pass 200 rows.
func TestOverviewPropertyCrowded(t *testing.T) {
	overviewProperty(t, estatesWhere(overviewEstates, func(name string) bool { return slices.Contains(crowded, name) }))
}

// crowded are the estates whose lists pass 200 rows, the costliest to
// check. Each property runs them under tests of their own, so that a race
// run of each test stays short.
var crowded = []string{"prod crowds staging", "web stacks crowd every list"}

// estatesWhere are the estates of all whose name keep holds for.
func estatesWhere(all []estate, keep func(name string) bool) []estate {
	var out []estate
	for _, est := range all {
		if keep(est.name) {
			out = append(out, est)
		}
	}
	return out
}

func overviewProperty(t *testing.T, estates []estate) {
	if len(estates) == 0 {
		t.Fatal("no estate to check")
	}
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		for _, est := range estates {
			t.Run(est.name, func(t *testing.T) {
				f := newFixtureOn(t, e, overviewGrants)
				raw := controltest.Raw(t, f.loc)
				seedEstate(t, f, raw, est)
				for _, rd := range overviewReaders {
					t.Run(rd.name, func(t *testing.T) {
						b := signInAs(f, rd)
						full := b.get("/ui/?as=json")
						var doc overviewView
						if full.status != 200 || json.Unmarshal([]byte(full.body), &doc) != nil {
							t.Fatalf("GET /ui/?as=json: %d %s", full.status, full.body)
						}
						w := countTruth(t, raw, f.clock.Now(), rd)

						restore := hideUnreadable(t, raw, rd.reads)
						without := b.get("/ui/?as=json").body
						restore()
						sameAnswer(t, full.body, without)

						checkCounts(t, rd, doc, w)
						finds := []string{"zzz-missing"}
						if est.cut != "" && rd.reads("prod", est.cut) {
							finds = append(finds, est.cut)
						}
						if len(doc.Stacks) > 0 {
							finds = append(finds, doc.Stacks[len(doc.Stacks)/2].Stack)
						}
						checkRender(t, b, rd, w, finds)
					})
				}
			})
		}
	})
}

// overviewView is the part of writ.overview/v0 the property reads.
type overviewView struct {
	Envs []struct {
		Name        string         `json:"name"`
		Plans       map[string]int `json:"plans"`
		PlansMore   bool           `json:"plans_more"`
		AlertsMore  bool           `json:"alerts_more"`
		Failed      int            `json:"failed"`
		Stacks      int            `json:"stacks"`
		Checked     int            `json:"checked"`
		Drifted     int            `json:"drifted"`
		CheckFailed int            `json:"check_failed"`
	} `json:"envs"`
	Stacks []struct {
		Env   string          `json:"env"`
		Stack string          `json:"stack"`
		Drift json.RawMessage `json:"drift"`
	} `json:"stacks"`
	StacksMore bool `json:"stacks_more"`
	Alerts     []struct {
		Kind, Env, Stack string
	} `json:"alerts"`
	// AlertsMore is the field of the answer before alerts_more moved to
	// each environment. It is read so a check of the answer before holds
	// it to its own word.
	AlertsMore bool `json:"alerts_more"`
}

// withState counts the stacks of env listed with their state.
func (v overviewView) withState(env string) int {
	n := 0
	for _, st := range v.Stacks {
		if st.Env == env && !isNull(st.Drift) {
			n++
		}
	}
	return n
}

func isNull(m json.RawMessage) bool { return len(m) == 0 || string(m) == "null" }

// stackKey is a stack@env.
type stackKey struct{ env, stack string }

// stackTruth is what the rows of one stack@env hold: whether it has state,
// its newest drift check, a state written outside writ serve, and its
// live plans by status.
type stackTruth struct {
	state, outside bool
	check          string
	plans          map[string]int
	// resources counts the rows of the stack's state index, and driftOut
	// and driftCode the changes its newest drift check found outside
	// OpenTofu and in the code.
	resources, driftOut, driftCode int
}

func (s *stackTruth) live() int {
	n := 0
	for _, c := range s.plans {
		n += c
	}
	return n
}

// inTree reports whether the sidebar's tree should hold the stack: it has
// state or a live plan names it.
func (s *stackTruth) inTree() bool { return s.state || s.live() > 0 }

// truth is what the rows a reader may read hold, counted by brute force
// from the control database.
type truth struct {
	envs     []string
	stacks   map[stackKey]*stackTruth
	failed   map[string]int
	expiring map[string]int
	mine     map[string]int
	// repos counts the repositories, polled those polled at least once,
	// and pollsFailed those whose last poll failed.
	repos, polled, pollsFailed int
}

// envTruth sums the truth of one or more environments.
type envTruth struct {
	stacks, checked, drifted, checkFailed int
	driftAlerts, failed, expiring, mine   int
	tree                                  int
	plans                                 map[string]int
	state                                 map[string]bool
}

func (t envTruth) live() int {
	n := 0
	for _, c := range t.plans {
		n += c
	}
	return n
}

func (w *truth) sum(envs ...string) envTruth {
	out := envTruth{plans: map[string]int{}, state: map[string]bool{}}
	for k, s := range w.stacks {
		if !slices.Contains(envs, k.env) {
			continue
		}
		if s.state {
			out.stacks++
			out.state[k.stack] = true
			if s.check != "" {
				out.checked++
			}
			switch s.check {
			case "drifted":
				out.drifted++
				out.driftAlerts++
			case "failed":
				out.checkFailed++
				out.driftAlerts++
			}
			if s.outside {
				out.driftAlerts++
			}
		}
		if s.inTree() {
			out.tree++
		}
		for status, n := range s.plans {
			out.plans[status] += n
		}
	}
	for _, env := range envs {
		out.failed += w.failed[env]
		out.expiring += w.expiring[env]
		out.mine += w.mine[env]
	}
	return out
}

// names are the names the reader's pages may show: the environments and
// the stacks.
func (w *truth) names() []string {
	names := slices.Clone(w.envs)
	for k := range w.stacks {
		names = append(names, k.stack)
	}
	return names
}

// listed is the stacks with state the overview lists: the first 200 by
// env and stack.
func (w *truth) listed() map[stackKey]bool {
	var keys []stackKey
	for k, s := range w.stacks {
		if s.state {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b stackKey) int { return cmp.Or(cmp.Compare(a.env, b.env), cmp.Compare(a.stack, b.stack)) })
	out := map[stackKey]bool{}
	for _, k := range keys[:min(len(keys), 200)] {
		out[k] = true
	}
	return out
}

func seedEstate(t *testing.T, f *fixture, raw *sqldb.DB, est estate) {
	t.Helper()
	ctx := context.Background()
	applied := f.plan(nil)
	approver := f.browser()
	approver.signIn("/ui/")
	if p := approver.post("/ui/plans/"+applied+"/approve", url.Values{}, "same-origin"); p.status != 200 {
		t.Fatalf("approve: %d", p.status)
	}
	run := f.apply(applied)
	var status string
	if err := raw.QueryRowContext(ctx, "SELECT status FROM plans WHERE id = ?", applied).Scan(&status); err != nil || slices.Contains([]string{"planning", "pending", "approved", "applying"}, status) {
		t.Fatalf("the plan of the failed run is %q, %v; want a plan that is not live", status, err)
	}

	now := f.clock.Now().UnixMilli()
	n := 0
	// reserve returns the first of count numbers no other row took.
	reserve := func(count int) int { n += count; return n - count + 1 }
	for _, part := range est.stacks {
		var stacks, checks [][]any
		for i, name := range part.names() {
			stacks = append(stacks, []any{part.env, name, "http", 1, now})
			if part.check != "" {
				msg := ""
				if part.check == "failed" {
					msg = "the drift check of " + name + " failed"
				}
				checks = append(checks, []any{fmt.Sprintf("dc-%s-%s-%d", part.env, part.prefix, i), part.env, name, part.check, now - 1000, now - 500, msg})
			}
		}
		insertRows(t, raw, "state_index_stacks", []string{"env", "stack", "backend", "version", "indexed_at"}, stacks)
		insertRows(t, raw, "drift_checks", []string{"ref", "env", "stack", "status", "started_at", "made_at", "error"}, checks)
	}
	for _, part := range est.plans {
		k := reserve(part.n)
		var by *overviewReader
		if i := slices.IndexFunc(overviewReaders, func(rd overviewReader) bool { return rd.name == part.by }); i >= 0 {
			by = &overviewReaders[i]
		}
		copyRow(t, raw, "plans", applied, part.n, func(i int, col string, v any) any {
			switch col {
			case "status":
				return part.status
			case "expires_at":
				return now + cmp.Or(part.expires, 12*time.Hour).Milliseconds()
			case "superseded_by":
				if part.status == "superseded" {
					return applied
				}
			case "author_person", "author_subject", "author_display", "author":
				if by != nil {
					author, _ := json.Marshal(map[string]string{"kind": "person", "issuer": "sso", "subject": by.sub, "display": by.name, "person": by.person()})
					return map[string]string{"author_person": by.person(), "author_subject": by.sub, "author_display": by.name, "author": string(author)}[col]
				}
			}
			return planCopy(col, v, part.env, part.stack, k+i)
		})
		if cmp.Or(part.shows, part.status) == "approved" {
			for i := range part.n {
				if _, err := raw.ExecContext(ctx, `INSERT INTO approvals (plan_id, person, display, issuer, subject, via, approved_at)
					SELECT ?, person, display, issuer, subject, via, approved_at FROM approvals WHERE plan_id = ?`, fmt.Sprintf("id-%d", k+i), applied); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, part := range est.failed {
		k := reserve(part.n)
		copyRow(t, raw, "plans", applied, part.n, func(i int, col string, v any) any { return planCopy(col, v, part.env, part.prefix, k+i) })
		copyRow(t, raw, "runs", run, part.n, func(i int, col string, v any) any {
			switch col {
			case "id":
				return fmt.Sprintf("run-%d", k+i)
			case "plan_id":
				return fmt.Sprintf("id-%d", k+i)
			case "env":
				return part.env
			case "stack":
				return part.prefix
			case "status":
				return "failed"
			case "started_at", "ended_at":
				return v.(int64) + int64(k+i)
			}
			return v
		})
	}
}

// planCopy is column col of the kth copy of a plan, moved to stack@env.
func planCopy(col string, v any, env, stack string, k int) any {
	switch col {
	case "ref":
		return fmt.Sprintf("ref-%d", k)
	case "id":
		return fmt.Sprintf("id-%d", k)
	case "env":
		return env
	case "stack":
		return stack
	case "seq":
		return int64(k)
	case "requested_at":
		return v.(int64) + int64(k)
	}
	return v
}

// copyRow writes n copies of the row of table whose id is id, each with
// the columns edit gives the ith copy.
func copyRow(t *testing.T, raw *sqldb.DB, table, id string, n int, edit func(i int, col string, v any) any) {
	t.Helper()
	cols, rows := selectRows(t, raw, "SELECT * FROM "+table+" WHERE id = ?", id)
	if len(rows) != 1 {
		t.Fatalf("%d rows %s in %s", len(rows), id, table)
	}
	var out [][]any
	for i := range n {
		row := make([]any, len(cols))
		for j, c := range cols {
			row[j] = edit(i, c, rows[0][j])
		}
		out = append(out, row)
	}
	insertRows(t, raw, table, cols, out)
}

func selectRows(t *testing.T, raw *sqldb.DB, query string, args ...any) ([]string, [][]any) {
	t.Helper()
	rows, err := raw.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return cols, out
}

func insertRows(t *testing.T, raw *sqldb.DB, table string, cols []string, rows [][]any) {
	t.Helper()
	marks := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	for chunk := range slices.Chunk(rows, 100) {
		var tuples []string
		var args []any
		for _, row := range chunk {
			tuples = append(tuples, marks)
			args = append(args, row...)
		}
		if _, err := raw.ExecContext(context.Background(), "INSERT INTO "+table+" ("+strings.Join(cols, ", ")+") VALUES "+strings.Join(tuples, ", "), args...); err != nil {
			t.Fatal(err)
		}
	}
}

// stackTables hold rows by stack@env, in an order that deletes a run
// before its plan.
var stackTables = []string{"runs", "plans", "drift_checks", "state_index", "state_index_stacks", "tfstate_versions", "state_s3_stacks"}

// hideUnreadable deletes every row of a stack@env that reads refuses, and
// the approvals of its plans, and returns the func that writes them back.
// The fixture's own plan, run, and approval are of network in prod, which
// every reader may read, so no row that references a deleted one is left.
func hideUnreadable(t *testing.T, raw *sqldb.DB, reads func(env, stack string) bool) (restore func()) {
	t.Helper()
	ctx := context.Background()
	type saved struct {
		table string
		cols  []string
		rows  [][]any
	}
	var kept []saved
	hide := func(table, where string, args []any) {
		cols, rows := selectRows(t, raw, "SELECT * FROM "+table+where, args...)
		kept = append(kept, saved{table, cols, rows})
		if _, err := raw.ExecContext(ctx, "DELETE FROM "+table+where, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range stackTables {
		_, pairs := selectRows(t, raw, "SELECT DISTINCT env, stack FROM "+table)
		hidden := map[string][]any{}
		for _, p := range pairs {
			env, stack := p[0].(string), p[1].(string)
			if !reads(env, stack) {
				hidden[env] = append(hidden[env], stack)
			}
		}
		for env, stacks := range hidden {
			if env == "prod" && slices.Contains(stacks, any("network")) {
				t.Fatalf("a reader may not read network in prod, which other rows reference")
			}
			for chunk := range slices.Chunk(stacks, 200) {
				in := " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ") + ")"
				args := append([]any{env}, chunk...)
				if table == "plans" {
					hide("approvals", " WHERE plan_id IN (SELECT id FROM plans WHERE env = ? AND stack"+in+")", args)
				}
				hide(table, " WHERE env = ? AND stack"+in, args)
			}
		}
	}
	return func() {
		for _, s := range slices.Backward(kept) {
			cols, rows := s.cols, s.rows
			if s.table == "approvals" {
				// Postgres makes the id of an approval and refuses one given.
				i := slices.Index(cols, "id")
				cols = slices.Delete(slices.Clone(cols), i, i+1)
				rows = nil
				for _, r := range s.rows {
					rows = append(rows, slices.Delete(slices.Clone(r), i, i+1))
				}
			}
			insertRows(t, raw, s.table, cols, rows)
		}
	}
}

// expiringWindow is how soon a pending or approved plan expires to be an
// alert: 2 hours, a quarter of the fixture's plan lifetime or less.
const expiringWindow = 2 * time.Hour

// countTruth counts the rows rd may read, as writ serve should: the stacks
// with state and their newest drift check, the runs that failed in the
// last day, the live plans by status, and the repositories.
// approvalsOf selects the count of a plan's approvals not withdrawn.
const approvalsOf = `(SELECT count(*) FROM approvals WHERE plan_id = plans.id AND withdrawn_at IS NULL)`

// showsNow is the status a plan of the fixture shows now: a pending or
// approved plan past its expiry is expired, and one before it is approved
// when it has an approval, and pending if not. Every plan an estate seeds
// is a copy of one the fixture's approval rule asked one approval of, and
// every approval is by a person the fixture defines, so each counts.
func showsNow(status string, expires any, approvals int64, now time.Time) string {
	if status != "pending" && status != "approved" {
		return status
	}
	if exp, ok := expires.(int64); ok && exp <= now.UnixMilli() {
		return "expired"
	}
	if approvals > 0 {
		return "approved"
	}
	return "pending"
}

func countTruth(t *testing.T, raw *sqldb.DB, now time.Time, rd overviewReader) *truth {
	t.Helper()
	w := &truth{envs: rd.envs, stacks: map[stackKey]*stackTruth{}, failed: map[string]int{}, expiring: map[string]int{}, mine: map[string]int{}}
	at := func(env, stack string) *stackTruth {
		k := stackKey{env, stack}
		if w.stacks[k] == nil {
			w.stacks[k] = &stackTruth{plans: map[string]int{}}
		}
		return w.stacks[k]
	}
	reads := func(env, stack string) bool { return slices.Contains(rd.envs, env) && rd.reads(env, stack) }
	_, targets := selectRows(t, raw, `SELECT env, stack FROM state_index_stacks UNION SELECT env, stack FROM tfstate_versions UNION SELECT env, stack FROM state_s3_stacks`)
	for _, row := range targets {
		if env, stack := row[0].(string), row[1].(string); reads(env, stack) {
			at(env, stack).state = true
		}
	}
	_, checks := selectRows(t, raw, `SELECT env, stack, status, started_at, ref, outside, code FROM drift_checks WHERE status <> 'planning'`)
	type check struct {
		status        string
		at            int64
		ref           string
		outside, code int64
	}
	last := map[stackKey]check{}
	for _, c := range checks {
		k, got := stackKey{c[0].(string), c[1].(string)}, check{c[2].(string), c[3].(int64), c[4].(string), c[5].(int64), c[6].(int64)}
		if old, ok := last[k]; !ok || got.at > old.at || got.at == old.at && got.ref > old.ref {
			last[k] = got
		}
	}
	for k, c := range last {
		if s := w.stacks[k]; s != nil && s.state {
			s.check, s.driftOut, s.driftCode = c.status, int(c.outside), int(c.code)
		}
	}
	_, indexed := selectRows(t, raw, `SELECT env, stack FROM state_index`)
	for _, r := range indexed {
		if s := w.stacks[stackKey{r[0].(string), r[1].(string)}]; s != nil && s.state {
			s.resources++
		}
	}
	_, outside := selectRows(t, raw, `SELECT env, stack FROM state_index_stacks WHERE outside_serial > 0`)
	for _, o := range outside {
		if s := w.stacks[stackKey{o[0].(string), o[1].(string)}]; s != nil && s.state {
			s.outside = true
		}
	}
	_, runs := selectRows(t, raw, `SELECT env, stack FROM runs WHERE status = 'failed' AND ended_at > ?`, now.Add(-24*time.Hour).UnixMilli())
	for _, r := range runs {
		if reads(r[0].(string), r[1].(string)) {
			w.failed[r[0].(string)]++
		}
	}
	_, plans := selectRows(t, raw, `SELECT env, stack, status, expires_at, author_person, `+approvalsOf+` FROM plans WHERE status IN ('planning', 'pending', 'approved', 'applying')`)
	for _, p := range plans {
		env, stack, status := p[0].(string), p[1].(string), showsNow(p[2].(string), p[3], p[5].(int64), now)
		exp, _ := p[3].(int64)
		waits := status == "pending" || status == "approved"
		if !reads(env, stack) || status == "expired" {
			continue
		}
		at(env, stack).plans[status]++
		if status == "pending" && p[4] == rd.person() {
			w.mine[env]++
		}
		if waits && exp-now.UnixMilli() <= expiringWindow.Milliseconds() {
			w.expiring[env]++
		}
	}
	_, repos := selectRows(t, raw, `SELECT polled_at, error FROM repos`)
	for _, r := range repos {
		w.repos++
		if r[0] != nil {
			w.polled++
		}
		if r[1].(string) != "" {
			w.pollsFailed++
		}
	}
	return w
}

// sameAnswer is property (a): the rows a reader may not read change no
// field of the answer.
func sameAnswer(t *testing.T, full, without string) {
	t.Helper()
	var a, b map[string]any
	if json.Unmarshal([]byte(full), &a) != nil || json.Unmarshal([]byte(without), &b) != nil {
		t.Fatalf("(a) the answers are not JSON:\n%s\n%s", full, without)
	}
	for k := range a {
		if !reflect.DeepEqual(a[k], b[k]) {
			x, _ := json.Marshal(a[k])
			y, _ := json.Marshal(b[k])
			t.Errorf("(a) rows the reader may not read change %s:\nwith them    %.600s\nwithout them %.600s", k, x, y)
		}
	}
}

// checkCounts is properties (b) and (c) on the answer.
func checkCounts(t *testing.T, rd overviewReader, doc overviewView, w *truth) {
	t.Helper()
	var names []string
	for _, e := range doc.Envs {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, rd.envs) {
		t.Errorf("envs %v, want %v", names, rd.envs)
	}
	for _, e := range doc.Envs {
		tr := w.sum(e.Name)
		got := [5]int{e.Stacks, e.Checked, e.Drifted, e.CheckFailed, e.Failed}
		if want := [5]int{tr.stacks, tr.checked, tr.drifted, tr.checkFailed, tr.failed}; got != want {
			t.Errorf("(b) %s: stacks, checked, drifted, check_failed, failed %v, want %v", e.Name, got, want)
		}

		counted := [4]int{e.Plans["planning"], e.Plans["pending"], e.Plans["approved"], e.Plans["applying"]}
		real := [4]int{tr.plans["planning"], tr.plans["pending"], tr.plans["approved"], tr.plans["applying"]}
		cut := slices.ContainsFunc(real[:], func(n int) bool { return n > 200 })
		switch {
		case e.PlansMore && !cut:
			t.Errorf("(c) %s: plans_more with at most 200 readable plans of each status %v", e.Name, tr.plans)
		case !e.PlansMore && counted != real:
			t.Errorf("(b) %s: plans planning, pending, approved, applying %v, want %v", e.Name, counted, real)
		case e.PlansMore && slices.ContainsFunc([]int{0, 1, 2, 3}, func(i int) bool { return counted[i] > real[i] }):
			t.Errorf("(c) %s: plan floors %v above %v", e.Name, counted, real)
		}

		drift, runs := 0, 0
		for _, a := range doc.Alerts {
			switch {
			case a.Env != e.Name:
			case a.Kind == "run-failed":
				runs++
			case a.Kind == "drifted" || a.Kind == "drift-check-failed" || a.Kind == "outside-write":
				drift++
			}
		}
		marked := doc.AlertsMore || e.AlertsMore
		if drift > tr.driftAlerts || runs > tr.failed {
			t.Errorf("(b) %s: %d drift alerts and %d failed runs listed, of %d and %d", e.Name, drift, runs, tr.driftAlerts, tr.failed)
		}
		if !marked && (drift != tr.driftAlerts || runs != tr.failed) {
			t.Errorf("(c) %s: %d of %d drift alerts and %d of %d failed runs listed, and alerts_more is not set", e.Name, drift, tr.driftAlerts, runs, tr.failed)
		}

		listed := doc.withState(e.Name)
		if !doc.StacksMore && listed != tr.stacks {
			t.Errorf("(c) %s: %d of %d stacks with state listed, and stacks_more is not set", e.Name, listed, tr.stacks)
		}
	}
	for _, st := range doc.Stacks {
		if !rd.reads(st.Env, st.Stack) {
			t.Errorf("(a) the answer lists %s@%s", st.Stack, st.Env)
		}
		if s := w.stacks[stackKey{st.Env, st.Stack}]; isNull(st.Drift) && s != nil && s.state {
			t.Errorf("(c) %s@%s has state and the answer says it has none", st.Stack, st.Env)
		}
	}
}

// signInAs signs rd in to the control UI in a browser of its own.
func signInAs(f *fixture, rd overviewReader) *browser {
	f.t.Helper()
	claims, info := oidctest.GitLabPerson()
	claims["sub"], claims["preferred_username"], claims["groups_direct"] = rd.sub, rd.name, []string{rd.group}
	info["sub"], info["preferred_username"], info["groups"] = rd.sub, rd.name, []string{"acme", rd.group}
	f.is.SetPerson(claims, info)
	b := f.browser()
	b.signIn("/ui/")
	return b
}
