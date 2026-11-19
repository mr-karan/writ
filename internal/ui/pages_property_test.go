package ui_test

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/sqldb"
)

// pageEstates are the overview's estates and one where the reader's own
// plan waits behind 200 newer plans of others, which a read of the newest
// 200 leaves out.
var pageEstates = append(slices.Clone(overviewEstates), estate{
	name: "yours behind 200 others",
	plans: []planPart{
		{env: "prod", stack: "web-old", n: 1, status: "pending", by: "platform"},
		{env: "prod", stack: "web-new", n: 200, status: "pending", by: "web"},
	},
})

// The pages a list page shows, by the limit writ serve reads them with.
const (
	plansPage = 50
	runsPage  = 50
	waitRead  = 200
)

// TestPagesProperty renders the approvals, plans, and runs of each
// environment, the three views of some stacks, the repositories, Getting
// started, access, who you are, and settings, for readers of different
// grants over estates of more than 200 plans, on SQLite and Postgres. It
// holds every number of template n in each page's main part to a
// brute-force count of the rows the reader may read, each listing to its
// number, and each sentence of nones to the rows it rests on, and runs
// checkWords on every page. The repositories, access, who you are, and
// settings read nothing of an estate, so they are checked for each reader
// of the first estate only. TestPagesPropertyCrowdsStaging and
// TestPagesPropertyCrowdsEveryList run the crowded estates.
func TestPagesProperty(t *testing.T) {
	pagesProperty(t, estatesWhere(pageEstates, func(name string) bool { return !slices.Contains(crowded, name) }), true)
}

// TestPagesPropertyCrowdsStaging is TestPagesProperty over prod's 201
// stacks and plans of each kind.
func TestPagesPropertyCrowdsStaging(t *testing.T) {
	pagesProperty(t, estatesWhere(pageEstates, func(name string) bool { return name == crowded[0] }), false)
}

// TestPagesPropertyCrowdsEveryList is TestPagesProperty over the web
// stacks that pass 200 in every list.
func TestPagesPropertyCrowdsEveryList(t *testing.T) {
	pagesProperty(t, estatesWhere(pageEstates, func(name string) bool { return name == crowded[1] }), false)
}

// pagesProperty checks the pages over estates, and the pages that read
// nothing of an estate over the first when config is set.
func pagesProperty(t *testing.T, estates []estate, config bool) {
	if len(estates) == 0 {
		t.Fatal("no estate to check")
	}
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		for i, est := range estates {
			t.Run(est.name, func(t *testing.T) {
				f := newFixtureOn(t, e, overviewGrants)
				raw := controltest.Raw(t, f.loc)
				seedEstate(t, f, raw, est)
				for _, rd := range overviewReaders {
					t.Run(rd.name, func(t *testing.T) {
						b := signInAs(f, rd)
						pc := &pageCheck{t: t, b: b, rd: rd, w: countTruth(t, raw, f.clock.Now(), rd), rows: readRows(t, raw, f.clock.Now(), rd)}
						for _, env := range rd.envs {
							pc.approvals(env)
							for _, status := range []string{"", "pending", "approved", "applied", "expired"} {
								pc.plans(env, "", status)
							}
							pc.runs(env)
							for _, stack := range pc.someStacks(env) {
								pc.plans(env, stack, "")
								pc.stack(env, stack)
							}
						}
						pc.start()
						if config && i == 0 {
							pc.repos()
							pc.config()
						}
					})
				}
			})
		}
	})
}

// planRow and runRow are the rows of a plan and a run the reader may read.
type planRow struct {
	env, stack, status, person string
	at                         int64
}

type runRow struct{ env, stack string }

type estateRows struct {
	plans []planRow
	runs  []runRow
	// indexed are the stacks whose state an index row names.
	indexed map[stackKey]bool
}

// readRows reads the plans, the runs, and the indexed stacks rd may read,
// each plan with the status showsNow gives it.
func readRows(t *testing.T, raw *sqldb.DB, now time.Time, rd overviewReader) estateRows {
	t.Helper()
	reads := func(env, stack string) bool { return slices.Contains(rd.envs, env) && rd.reads(env, stack) }
	var out estateRows
	_, plans := selectRows(t, raw, `SELECT env, stack, status, expires_at, author_person, requested_at, `+approvalsOf+` FROM plans`)
	for _, p := range plans {
		r := planRow{env: p[0].(string), stack: p[1].(string), status: showsNow(p[2].(string), p[3], p[6].(int64), now), person: p[4].(string), at: p[5].(int64)}
		if reads(r.env, r.stack) {
			out.plans = append(out.plans, r)
		}
	}
	_, runs := selectRows(t, raw, `SELECT env, stack FROM runs`)
	for _, r := range runs {
		if env, stack := r[0].(string), r[1].(string); reads(env, stack) {
			out.runs = append(out.runs, runRow{env, stack})
		}
	}
	out.indexed = map[stackKey]bool{}
	_, idx := selectRows(t, raw, `SELECT env, stack FROM state_index_stacks`)
	for _, r := range idx {
		if env, stack := r[0].(string), r[1].(string); reads(env, stack) {
			out.indexed[stackKey{env, stack}] = true
		}
	}
	return out
}

func (r estateRows) countPlans(env, stack, status string) int {
	n := 0
	for _, p := range r.plans {
		if p.env == env && (stack == "" || p.stack == stack) && (status == "" || p.status == status) {
			n++
		}
	}
	return n
}

func (r estateRows) countRuns(env, stack string) int {
	n := 0
	for _, x := range r.runs {
		if x.env == env && (stack == "" || x.stack == stack) {
			n++
		}
	}
	return n
}

type pageCheck struct {
	t    *testing.T
	b    *browser
	rd   overviewReader
	w    *truth
	rows estateRows
}

// figureRule names a number of template n in a page's main part and
// counts what it counts, or reports false for a number it has no rule for.
// A number that is a field of one record, such as a plan's changes, is the
// record's own and counts as field.
type figureRule func(n *html.Node) (what string, want int, ok bool)

const field = -1

// page gets path, runs checkWords with the sentences of nones judged by
// facts, and holds each number of template n in the main part by rule.
func (pc *pageCheck) page(path string, facts map[string]int, rule figureRule) *html.Node {
	t := pc.t
	t.Helper()
	p := pc.b.get(path)
	if p.status != 200 || p.doc == nil {
		t.Fatalf("%s: %d", path, p.status)
	}
	view := pc.w.sum(pc.rd.envs...)
	checkWords(t, path, p.doc, p.served, pc.w.names(), func(key string, at *html.Node) bool {
		c := sayCtx{view: view, w: pc.w, page: facts}
		if env := envOf(at); env != "" {
			e := pc.w.sum(env)
			c.env = &e
		}
		return guards[key](c)
	})
	main := first(p.doc, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "main" })
	for _, n := range everyOf(main, classed("n")) {
		f, ok := readFig(n)
		if !ok {
			continue // checkWords fails it
		}
		what, want, ok := rule(n)
		if !ok {
			t.Errorf("(p) %s: a number the page check has no rule for: %s in %q", path, f, folded(n.Parent))
			continue
		}
		if want != field && (f.n > want || !f.floor && f.n != want) {
			t.Errorf("(p) %s: %s is %s, of %d the reader may read", path, what, f, want)
		}
	}
	return main
}

// rowsOf counts the rows of the tables in root.
func rowsOf(root *html.Node) int {
	if root == nil {
		return 0
	}
	return len(everyOf(root, func(n *html.Node) bool { return n.Data == "tr" && up(n, isTag("tbody")) != nil }))
}

func (pc *pageCheck) approvals(env string) {
	pending, mine := pc.rows.countPlans(env, "", "pending"), 0
	for _, p := range pc.rows.plans {
		if p.env == env && p.status == "pending" && p.person == pc.rd.person() {
			mine++
		}
	}
	path := "/ui/approvals?env=" + env
	main := pc.page(path, map[string]int{"others": pending - mine, "mine": mine}, func(n *html.Node) (string, int, bool) {
		if h := up(n, isTag("h2")); h != nil {
			switch id, _ := attr(h, "id"); id {
			case "h-others":
				return "plans by others that wait", pending - mine, true
			case "h-mine":
				return "plans of the reader that wait", mine, true
			}
		}
		if up(n, classed("note")) != nil {
			return "plans that wait writ serve read", min(pending, waitRead), true
		}
		if up(n, isTag("td")) != nil {
			return "a plan's changes", field, true
		}
		return "", 0, false
	})
	for _, id := range []string{"h-others", "h-mine"} {
		sec := first(main, labelledBy(id))
		f, _ := readFig(first(first(sec, isTag("h2")), classed("n")))
		if got := rowsOf(sec); got != f.n {
			pc.t.Errorf("(p) %s: %s says %s and lists %d", path, id, f, got)
		}
	}
	if cut := pending > waitRead; cut != (first(main, classed("note")) != nil) {
		pc.t.Errorf("(p) %s: %d plans wait and the page says it read only some: %v", path, pending, !cut)
	}
}

func (pc *pageCheck) plans(env, stack, status string) {
	count := pc.rows.countPlans(env, stack, status)
	path := "/ui/plans?" + url.Values{"env": {env}, "stack": {stack}, "status": {status}}.Encode()
	view, facts := "plans that match", map[string]int{"plans": count}
	if stack != "" && status == "" {
		path = "/ui/stack?" + url.Values{"env": {env}, "stack": {stack}, "view": {"plans"}}.Encode()
		view = "plans of " + stack
		facts = pc.stackFacts(env, stack)
		facts["plans"] = count
	}
	drift := pc.driftRule(env, stack)
	main := pc.page(path, facts, func(n *html.Node) (string, int, bool) {
		if up(n, classed("muted")) != nil && up(n, isTag("table")) == nil {
			return "the " + view + " the page lists", min(count, plansPage), true
		}
		if up(n, isTag("td")) != nil {
			return "a plan's changes", field, true
		}
		return drift(n)
	})
	if got := rowsOf(main); got != min(count, plansPage) {
		pc.t.Errorf("(p) %s: lists %d of %d plans that match", path, got, count)
	}
	more := slices.ContainsFunc(everyOf(main, isTag("a")), func(a *html.Node) bool { return folded(a) == "Older plans" })
	if more != (count > plansPage) {
		pc.t.Errorf("(p) %s: %d plans match and the page links older plans: %v", path, count, more)
	}
}

func (pc *pageCheck) runs(env string) {
	count := pc.rows.countRuns(env, "")
	path := "/ui/runs?env=" + env
	main := pc.page(path, map[string]int{"runs": count}, func(n *html.Node) (string, int, bool) {
		if up(n, labelledBy("h-applies")) != nil && up(n, isTag("h2")) != nil {
			return "runs", count, true
		}
		return "", 0, false
	})
	if got := rowsOf(main); got != min(count, runsPage) {
		pc.t.Errorf("(p) %s: lists %d of %d runs", path, got, count)
	}
}

// someStacks are stacks of env to open: the first and last the reader may
// read with state, one a plan names and none has state of, and one that
// does not exist.
func (pc *pageCheck) someStacks(env string) []string {
	var state, planned []string
	for k, s := range pc.w.stacks {
		switch {
		case k.env != env:
		case s.state:
			state = append(state, k.stack)
		case s.live() > 0:
			planned = append(planned, k.stack)
		}
	}
	slices.Sort(state)
	slices.Sort(planned)
	out := []string{"web-missing"}
	if len(state) > 0 {
		out = append(out, state[0], state[len(state)-1])
	}
	if len(planned) > 0 {
		out = append(out, planned[0])
	}
	return slices.Compact(out)
}

// stackFacts are what the sentences of a stack's page rest on: whether it
// has state, an index, and a drift check, and how many resources.
func (pc *pageCheck) stackFacts(env, stack string) map[string]int {
	s := pc.w.stacks[stackKey{env, stack}]
	if s == nil {
		s = &stackTruth{plans: map[string]int{}}
	}
	return map[string]int{"state": b2i(s.state), "indexed": b2i(pc.rows.indexed[stackKey{env, stack}]), "resources": s.resources, "checked": b2i(s.check != "")}
}

// stack checks a stack's four views. Plans goes through pc.plans.
func (pc *pageCheck) stack(env, stack string) {
	facts := pc.stackFacts(env, stack)
	base := url.Values{"env": {env}, "stack": {stack}}
	drift := pc.driftRule(env, stack)
	main := pc.page("/ui/stack?"+base.Encode(), facts, drift)
	if s := pc.w.stacks[stackKey{env, stack}]; s != nil && s.resources > 0 {
		sum := 0
		for _, n := range everyOf(first(main, classed("svc-strip")), classed("n")) {
			f, _ := readFig(n)
			sum += f.n
		}
		if sum != s.resources {
			pc.t.Errorf("(p) %s@%s resources: the strip counts %d of %d", stack, env, sum, s.resources)
		}
	}
	base.Set("view", "drift")
	pc.page("/ui/stack?"+base.Encode(), facts, drift)
	runs := pc.rows.countRuns(env, stack)
	facts["runs"] = runs
	base.Set("view", "runs")
	main = pc.page("/ui/stack?"+base.Encode(), facts, drift)
	if got := rowsOf(main); got != min(runs, runsPage) {
		pc.t.Errorf("(p) %s@%s runs: lists %d of %d", stack, env, got, runs)
	}
}

// driftRule names the numbers every view of a stack shows, and those of
// its drift and resources views: the resources its index holds, the
// changes its newest check found outside OpenTofu and in the code, and
// those shown. A count of the resources view is of the index, the one
// document of the view, so it is a field; stack checks the strip's sum.
func (pc *pageCheck) driftRule(env, stack string) figureRule {
	return func(n *html.Node) (string, int, bool) {
		s := pc.w.stacks[stackKey{env, stack}]
		if s == nil {
			return "", 0, false
		}
		switch {
		case up(n, classed("page-head")) != nil:
			return "resources of the index", s.resources, true
		case up(n, classed("tabs")) != nil:
			return "changes the drift check found", s.driftOut + s.driftCode, true
		case up(n, isTag("h2")) != nil && up(n, labelledBy("h-drift")) != nil:
			return "changes the drift check found", s.driftOut + s.driftCode, true
		case up(n, classed("svc-strip")) != nil, up(n, classed("bar")) != nil, up(n, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "index-rows" }) != nil:
			return "resources of a category, a group, or the drift check", field, true
		case up(n, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "matches" }) != nil:
			return "resources that match", field, true
		}
		m := up(n, classed("meta"))
		if m == nil || up(m, labelledBy("h-drift")) == nil {
			return "", 0, false
		}
		switch slices.Index(everyOf(m, classed("n")), n) {
		case 0:
			return "changes outside OpenTofu", s.driftOut, true
		case 1:
			return "changes the code would make", s.driftCode, true
		case 2:
			return "changes shown", s.driftOut + s.driftCode, true
		}
		return "", 0, false
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// repos holds the stacks counted of each repository to what writ serve
// found at its head, and each repository's page to the same.
func (pc *pageCheck) repos() {
	found := map[string]int{}
	for _, name := range pc.repoNames() {
		var doc struct {
			Stacks []json.RawMessage `json:"stacks"`
		}
		pc.getJSON("/ui/repos/"+name+"?as=json", &doc)
		found[name] = len(doc.Stacks)
		pc.page("/ui/repos/"+name, map[string]int{"found": len(doc.Stacks)}, func(n *html.Node) (string, int, bool) {
			if up(n, labelledBy("h-found")) != nil && up(n, isTag("h2")) != nil {
				return "stacks found", len(doc.Stacks), true
			}
			if up(n, isTag("dd")) != nil {
				return "files of the commit read", field, true
			}
			return "", 0, false
		})
	}
	pc.page("/ui/repos", map[string]int{}, func(n *html.Node) (string, int, bool) {
		tr := up(n, isTag("tr"))
		if tr == nil {
			return "", 0, false
		}
		name := folded(first(first(tr, isTag("th")), isTag("a")))
		want, ok := found[name]
		return "stacks found in " + name, want, ok
	})
}

func (pc *pageCheck) repoNames() []string {
	var doc struct {
		Repos []struct {
			Name string `json:"name"`
		} `json:"repos"`
	}
	pc.getJSON("/ui/repos?as=json", &doc)
	var names []string
	for _, r := range doc.Repos {
		names = append(names, r.Name)
	}
	return names
}

func (pc *pageCheck) getJSON(path string, into any) {
	pc.t.Helper()
	p := pc.b.get(path)
	if p.status != 200 || json.Unmarshal([]byte(p.body), into) != nil {
		pc.t.Fatalf("%s: %d %.300s", path, p.status, p.body)
	}
}

// start holds Getting started's counts: the stacks found in every
// repository, those served, and the plans of every environment.
func (pc *pageCheck) start() {
	stacks, served := 0, 0
	for _, name := range pc.repoNames() {
		var doc struct {
			Stacks []struct {
				Served []string `json:"served"`
			} `json:"stacks"`
		}
		pc.getJSON("/ui/repos/"+name+"?as=json", &doc)
		for _, s := range doc.Stacks {
			stacks++
			if len(s.Served) > 0 {
				served++
			}
		}
	}
	plans := 0
	for _, env := range pc.rd.envs {
		plans += pc.rows.countPlans(env, "", "")
	}
	pc.page("/ui/start", map[string]int{"plans": plans}, func(n *html.Node) (string, int, bool) {
		li := up(n, isTag("li"))
		if li == nil {
			return "", 0, false
		}
		switch label := folded(first(li, isTag("strong"))); {
		case label == "Stacks found" && slices.Index(everyOf(li, classed("n")), n) == 0:
			return "stacks found", stacks, true
		case label == "Stacks found":
			return "stacks served", served, true
		case label == "First plan":
			return "plans", plans, true
		}
		return "", 0, false
	})
}

// config holds access to the config it reads, who you are to the session
// it reads, and settings to the words it may say.
func (pc *pageCheck) config() {
	var cfg struct {
		Grants []json.RawMessage `json:"grants"`
		Teams  []json.RawMessage `json:"teams"`
	}
	pc.getJSON("/ui/access?as=json", &cfg)
	pc.page("/ui/access", map[string]int{}, func(n *html.Node) (string, int, bool) {
		h := up(n, isTag("h2"))
		if h == nil {
			return "", 0, false
		}
		switch id, _ := attr(h, "id"); id {
		case "h-grants":
			return "grants", len(cfg.Grants), true
		case "h-teams":
			return "teams", len(cfg.Teams), true
		}
		return "", 0, false
	})
	var me struct {
		Teams  []string          `json:"teams"`
		Grants []json.RawMessage `json:"grants"`
	}
	pc.getJSON("/ui/whoami?as=json", &me)
	facts := map[string]int{"teams": len(me.Teams), "grants": len(me.Grants)}
	pc.page("/ui/whoami", facts, func(n *html.Node) (string, int, bool) {
		if h := up(n, isTag("h2")); h != nil && strings.HasPrefix(folded(h), "What you may do") {
			return "the reader's grants", len(me.Grants), true
		}
		return "", 0, false
	})
	pc.page("/ui/settings", facts, func(*html.Node) (string, int, bool) { return "", 0, false })
}
