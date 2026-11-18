package ui_test

import (
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/ui"
)

// sayCtx is what a sentence of nones is judged by: the environments the
// overview shows, the tree's environment it is said of, and the rest. On
// a page but the overview, page holds the counts the page's sentences
// rest on, by name, counted from the rows the reader may read.
type sayCtx struct {
	view envTruth
	env  *envTruth
	w    *truth
	page map[string]int
}

// zero reports whether the page counts none of what, and fails a sentence
// said on a page that does not count it.
func (c sayCtx) zero(what string) bool {
	n, ok := c.page[what]
	return ok && n == 0
}

// fact reports whether the page knows what to be true.
func (c sayCtx) fact(what string) bool { return c.page[what] == 1 }

// guards are when each sentence of nones holds, by the DESIGN table.
var guards = map[string]func(c sayCtx) bool{
	"nothing":   func(c sayCtx) bool { return c.view.live() == 0 && c.view.stacks == 0 },
	"calm":      func(c sayCtx) bool { return c.view.driftAlerts+c.view.failed+c.view.expiring+c.w.pollsFailed == 0 },
	"steady":    func(c sayCtx) bool { return c.view.driftAlerts+c.view.failed+c.w.pollsFailed == 0 },
	"applying":  func(c sayCtx) bool { return c.view.plans["applying"] == 0 },
	"waiting":   func(c sayCtx) bool { return c.view.plans["pending"] == 0 },
	"stateless": func(c sayCtx) bool { return c.view.stacks == 0 },
	"no-stack":  func(c sayCtx) bool { return c.env != nil && c.env.tree == 0 },
	"no-repo":   func(c sayCtx) bool { return c.w.repos == 0 },
	"no-poll":   func(c sayCtx) bool { return c.w.repos > 0 && c.w.polled == 0 },

	"no-others":     func(c sayCtx) bool { return c.zero("others") },
	"no-mine":       func(c sayCtx) bool { return c.zero("mine") },
	"no-plan":       func(c sayCtx) bool { return c.zero("plans") },
	"no-run":        func(c sayCtx) bool { return c.zero("runs") },
	"no-stack-plan": func(c sayCtx) bool { return c.zero("plans") },
	"no-state":      func(c sayCtx) bool { return c.zero("state") },
	"unindexed":     func(c sayCtx) bool { return c.fact("state") && c.zero("indexed") },
	"no-resource":   func(c sayCtx) bool { return c.fact("indexed") && c.zero("resources") },
	"unchecked":     func(c sayCtx) bool { return c.zero("checked") },
	"no-secret":     func(c sayCtx) bool { return c.fact("unsealed") },
	"no-found":      func(c sayCtx) bool { return c.zero("found") },
	"no-team":       func(c sayCtx) bool { return c.zero("teams") },
	"no-grant":      func(c sayCtx) bool { return c.zero("grants") },
	"no-plan-yet":   func(c sayCtx) bool { return c.zero("plans") },
}

// noneTemplates are the templates of the pages of noneScope and the parts
// they call.
var noneTemplates = []string{"overview", "approvals", "plans", "runs", "stack", "repos", "repo", "access", "whoami", "settings", "start"}

// TestPagesSayNoneOnlyThroughNone holds the pages of noneScope and the
// sidebar to saying a thing is absent only through template func none,
// whose every sentence the render checks judge: a guard for each, and no
// such word written in a template's own text outside a listing, a field,
// or a message.
func TestPagesSayNoneOnlyThroughNone(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(ui.Nones)), slices.Sorted(maps.Keys(guards)); !slices.Equal(got, want) {
		t.Errorf("the sentences of none are %q and the render checks guard %q", got, want)
	}
	for _, name := range noneTemplates {
		text, err := ui.TemplateText(name)
		if err != nil {
			t.Fatal(err)
		}
		text = fieldsRE.ReplaceAllString(text, " ")
		text = regexp.MustCompile(`(?s)<span class="msg"[^>]*>.*?</span>`).ReplaceAllString(text, " ")
		text = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(text, " ")
		text = terms.ReplaceAllString(text, " ")
		for _, m := range noneWords.FindAllStringIndex(text, -1) {
			t.Errorf("%s says %q in its own words, not through none: %q", name, text[m[0]:m[1]], strings.Join(strings.Fields(text[max(0, m[0]-60):min(len(text), m[1]+60)]), " "))
		}
	}
}

// checkRender is property (d): the overview of every environment and of
// each one, and the sidebar, also filtered for each of finds. Every
// number, dot, and sentence that says a thing is absent is read from the
// page and held to the rows the reader may read. A number must come from
// template n and such a sentence from func none, so a new one fails here
// until a rule below judges it.
func checkRender(t *testing.T, b *browser, rd overviewReader, w *truth, finds []string) {
	t.Helper()
	type view struct {
		path string
		envs []string
		find string
	}
	views := []view{{"/ui/", rd.envs, ""}}
	for _, env := range rd.envs {
		views = append(views, view{"/ui/?env=" + env, []string{env}, ""})
	}
	for _, f := range finds {
		views = append(views, view{"/ui/?find=" + url.QueryEscape(f), rd.envs, f})
	}
	for _, v := range views {
		p := b.get(v.path)
		if p.status != 200 {
			t.Fatalf("%s: %d", v.path, p.status)
		}
		r := &reading{t: t, path: v.path, w: w, rd: rd, view: w.sum(v.envs...), viewEnvs: v.envs, find: v.find,
			listed: w.listed(), handled: map[*html.Node]bool{}}
		r.main = first(p.doc, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "main" })
		r.side = first(p.doc, classed("side"))
		if r.main == nil || r.side == nil {
			t.Fatalf("%s: no main or no sidebar", v.path)
		}
		r.figures()
		checkWords(t, v.path, p.doc, p.served, w.names(), func(key string, at *html.Node) bool {
			c := sayCtx{view: r.view, w: r.w}
			if env := envOf(at); env != "" {
				e := r.w.sum(env)
				c.env = &e
			}
			return guards[key](c)
		})
		r.tree()
		r.dots()
		r.lists()
	}
}

// reading is one page read by checkRender.
type reading struct {
	t          *testing.T
	path       string
	w          *truth
	rd         overviewReader
	view       envTruth
	viewEnvs   []string
	find       string
	listed     map[stackKey]bool
	main, side *html.Node
	// handled are the dots tree judged.
	handled map[*html.Node]bool
}

func (r *reading) errorf(format string, args ...any) {
	r.t.Helper()
	r.t.Errorf("(d) %s: "+format, append([]any{r.path}, args...)...)
}

// fig is a number of template n: exact, or a floor marked +.
type fig struct {
	n     int
	floor bool
}

func (f fig) String() string {
	if f.floor {
		return strconv.Itoa(f.n) + "+"
	}
	return strconv.Itoa(f.n)
}

var figRE = regexp.MustCompile(`^(\d+)(\+ or more)?$`)

func readFig(n *html.Node) (fig, bool) {
	m := figRE.FindStringSubmatch(folded(n))
	if m == nil {
		return fig{}, false
	}
	v, _ := strconv.Atoi(m[1])
	return fig{v, m[2] != ""}, true
}

// holds judges a number: exact, it is the truth; a floor, at most it.
func (r *reading) holds(what string, f fig, want int) {
	r.t.Helper()
	if f.n > want || !f.floor && f.n != want {
		r.errorf("%s is %s, of %d the reader may read", what, f, want)
	}
}

// figures judges every number of template n outside the tree's notes.
func (r *reading) figures() {
	for _, root := range []*html.Node{r.main, r.side} {
		for _, n := range everyOf(root, classed("n")) {
			if up(n, classed("t-note")) != nil {
				continue
			}
			f, ok := readFig(n)
			if !ok {
				r.errorf("template n wrote %q", folded(n))
				continue
			}
			what, want, ok := r.figureTruth(n)
			if !ok {
				r.errorf("a number checkRender has no rule for: %s in %q", f, folded(n.Parent))
				continue
			}
			r.holds(what, f, want)
		}
	}
}

// figureTruth names the number n and counts what it counts.
func (r *reading) figureTruth(n *html.Node) (string, int, bool) {
	v, w := r.view, r.w
	all := w.sum(r.rd.envs...)
	if p := up(n, classed("subtitle")); p != nil {
		switch slices.Index(everyOf(p, classed("n")), n) {
		case 0:
			return "environments", len(r.rd.envs), true
		case 1:
			return "stacks with state", v.stacks, true
		case 2:
			return "repositories", w.repos, true
		}
	}
	if s := up(n, classed("stat")); s != nil {
		label := folded(first(s, classed("stat-label")))
		i := -1
		if sub := up(n, classed("stat-sub")); sub != nil {
			i = slices.Index(everyOf(sub, classed("n")), n)
		}
		switch [2]any{label, i} {
		case [2]any{"Waiting for approval", -1}:
			return "plans waiting", v.plans["pending"], true
		case [2]any{"Waiting for approval", 0}:
			return "plans waiting by others", v.plans["pending"] - v.mine, true
		case [2]any{"Waiting for approval", 1}:
			return "plans waiting of the reader", v.mine, true
		case [2]any{"Applying", -1}:
			return "plans applying", v.plans["applying"], true
		case [2]any{"Applying", 0}:
			return "plans planning", v.plans["planning"], true
		case [2]any{"Failed", -1}:
			return "failed runs", v.failed, true
		case [2]any{"Drifted", -1}:
			return "drifted stacks", v.drifted, true
		case [2]any{"Drifted", 0}:
			return "checked stacks", v.checked, true
		}
		return "", 0, false
	}
	if h := up(n, isTag("h2")); h != nil {
		id, _ := attr(h, "id")
		switch id {
		case "h-alerts":
			return "active alerts", v.driftAlerts + v.failed + v.expiring + w.pollsFailed, true
		case "h-applying":
			return "Applying now", v.plans["applying"], true
		case "h-waiting":
			return "Waiting for approval", v.plans["pending"], true
		case "h-planning":
			return "Planning now", v.plans["planning"], true
		case "h-stacks":
			return "Stacks with state", v.stacks, true
		}
		return "", 0, false
	}
	if tr := up(n, isTag("tr")); tr != nil && up(tr, isTag("tbody")) != nil {
		k := stackKey{folded(first(tr, isTag("td"))), folded(first(tr, isTag("th")))}
		s := w.stacks[k]
		if s == nil {
			return "", 0, false
		}
		if d := up(n, classed("drift")); d != nil {
			switch slices.Index(everyOf(d, classed("n")), n) {
			case 0:
				return "resources changed outside OpenTofu of " + k.stack, s.driftOut, true
			case 1:
				return "resources the code would change of " + k.stack, s.driftCode, true
			}
			return "", 0, false
		}
		return "resources of " + k.stack, s.resources, true
	}
	if up(n, labelledBy("h-stacks")) != nil {
		return "stacks with state not listed", v.stacks - r.listedIn(r.viewEnvs...), true
	}
	if a := up(n, isTag("a")); a != nil && up(a, classed("sections")) != nil {
		switch href, _ := attr(a, "href"); href {
		case "/ui/approvals":
			return "the sidebar's plans waiting", all.plans["pending"], true
		case "/ui/runs":
			return "the sidebar's plans applying", all.plans["applying"], true
		case "/ui/repos":
			return "the sidebar's repositories", w.repos, true
		}
		return "", 0, false
	}
	if c := up(n, classed("count")); c != nil && up(c, isTag("summary")) != nil {
		if env := envOf(c); env != "" {
			return "stacks of " + env + " in the tree", r.w.sum(env).tree, true
		}
		return "", 0, false
	}
	if p := up(n, classed("tree-note")); p != nil {
		if env := envOf(p); env != "" {
			return "stacks with state of " + env + " not listed", r.unlisted(env), true
		}
		if strings.Contains(folded(p), "match") {
			return "stacks that match", r.matches(""), true
		}
		return "", 0, false
	}
	if up(n, classed("side-foot")) != nil {
		return "failed repository polls", w.pollsFailed, true
	}
	return "", 0, false
}

func (r *reading) listedIn(envs ...string) int {
	n := 0
	for k := range r.listed {
		if slices.Contains(envs, k.env) {
			n++
		}
	}
	return n
}

// unlisted counts the stacks with state of env that writ serve lists none
// of.
func (r *reading) unlisted(env string) int {
	return r.w.sum(env).stacks - r.listedIn(env)
}

// matches counts the stacks of the tree, of env or of every environment
// when env is "", whose name holds the filter.
func (r *reading) matches(env string) int {
	n := 0
	for k, s := range r.w.stacks {
		if (env == "" || k.env == env) && s.inTree() && r.match(k.stack) {
			n++
		}
	}
	return n
}

func (r *reading) match(stack string) bool {
	return strings.Contains(strings.ToLower(stack), strings.ToLower(r.find))
}

// notePart is a part of a tree note: a number and what it counts, or words.
type notePart struct {
	fig     fig
	counted bool
	word    string
}

var partRE = regexp.MustCompile(`^(\d+)(\+?) (.+)$`)

// note reads a tree note, and fails a number in it outside template n. A
// note set for a screen reader alone is read as one hears it.
func (r *reading) note(n *html.Node) []notePart {
	var out []notePart
	for _, s := range strings.Split(shown(n), " · ") {
		if m := partRE.FindStringSubmatch(s); m != nil {
			v, _ := strconv.Atoi(m[1])
			out = append(out, notePart{fig{v, m[2] != ""}, true, m[3]})
		} else {
			out = append(out, notePart{word: s})
		}
	}
	counted := 0
	for _, p := range out {
		if p.counted {
			counted++
		}
	}
	if figs := len(everyOf(n, classed("n"))); figs != counted {
		r.errorf("the note %q holds %d numbers of template n and %d numbers", visible(n), figs, counted)
	}
	return out
}

var liveStatuses = []string{"planning", "pending", "approved", "applying"}

// facts are what a dot may say of an environment or a stack, and the dot
// it says when every count is exact.
type facts struct {
	bad, run, wait bool
	want           string
}

func envFacts(e envTruth) facts {
	f := facts{bad: e.checkFailed > 0, run: e.plans["planning"]+e.plans["applying"] > 0, wait: e.plans["pending"]+e.plans["approved"]+e.drifted > 0}
	f.want = tone(f, []string{"bad", "run", "wait"}, e.checked > 0)
	return f
}

func stackFacts(s *stackTruth) facts {
	f := facts{bad: s.check == "failed", run: s.plans["planning"]+s.plans["applying"] > 0, wait: s.plans["pending"]+s.plans["approved"] > 0 || s.check == "drifted"}
	f.want = tone(f, []string{"run", "bad", "wait"}, s.check != "")
	return f
}

// tone is the first of order that f bears out, then ok when checked.
func tone(f facts, order []string, checked bool) string {
	for _, t := range order {
		if map[string]bool{"bad": f.bad, "run": f.run, "wait": f.wait}[t] {
			return t
		}
	}
	if checked {
		return "ok"
	}
	return "none"
}

// dot judges the tone of a dot. Beside exact counts it is the tone the
// rows give; beside a floor it may say only what the rows bear out, and
// never all clear.
func (r *reading) dot(what, got string, partial bool, f facts) {
	r.t.Helper()
	if !partial && got != f.want {
		r.errorf("the dot of %s is %s, want %s", what, got, f.want)
	}
	if partial && !map[string]bool{"bad": f.bad, "run": f.run, "wait": f.wait, "none": true}[got] {
		r.errorf("the dot of %s is %s beside counts that are floors, which the rows do not bear out", what, got)
	}
}

// counts judges the plan counts of a note, and, when none is a floor, the
// statuses it leaves out as none.
func (r *reading) counts(what string, parts []notePart, plans map[string]int) (partial bool) {
	r.t.Helper()
	said := map[string]bool{}
	for _, p := range parts {
		partial = partial || p.fig.floor
		if p.counted && slices.Contains(liveStatuses, p.word) {
			said[p.word] = true
			r.holds(what+" "+p.word, p.fig, plans[p.word])
		}
	}
	for _, s := range liveStatuses {
		if !partial && !said[s] && plans[s] > 0 {
			r.errorf("%s says no plan %s, of %d", what, s, plans[s])
		}
	}
	return partial
}

var matchRE = regexp.MustCompile(`^(\d+)(\+?) (listed )?stacks? match`)

// tree judges the filter's count and the sidebar's tree: each
// environment's count of stacks and dot, and each stack's note and dot,
// and the stacks it leaves out.
func (r *reading) tree() {
	if r.find != "" {
		line := visible(first(r.side, func(n *html.Node) bool { return hasClass(n, "tree-note") && strings.Contains(folded(n), "match") }))
		if m := matchRE.FindStringSubmatch(line); m == nil {
			r.errorf("the filter says %q", line)
		} else {
			n, _ := strconv.Atoi(m[1])
			r.holds("the filter's count", fig{n, m[2] != "" || m[3] != ""}, r.matches(""))
		}
	}
	var shownEnvs []string
	for _, li := range r.envNodes() {
		env := folded(first(li, classed("t-name")))
		shownEnvs = append(shownEnvs, env)
		e := r.w.sum(env)
		summary := first(li, isTag("summary"))
		// figures holds the count of stacks to the rows. It is a floor
		// when writ serve read only some plans of env, and so are the
		// counts the dot rests on.
		count, ok := readFig(first(first(summary, classed("count")), classed("n")))
		if !ok {
			r.errorf("the tree shows no count of the stacks of %s", env)
		}
		if t := visible(summary); t != env+" "+count.String() {
			r.errorf("the tree's %s says %q, not its name and its count of stacks", env, t)
		}
		d := first(summary, classed("dot"))
		r.handled[d] = true
		r.dot(env, toneOf(d), count.floor, envFacts(e))

		shown := map[string]bool{}
		for _, a := range everyOf(li, func(n *html.Node) bool { return n.Data == "a" && up(n, classed("stacks")) != nil }) {
			name := folded(first(a, classed("t-name")))
			what := name + "@" + env
			s := r.w.stacks[stackKey{env, name}]
			if s == nil || !s.inTree() || r.find != "" && !r.match(name) {
				r.errorf("the tree lists %s, which the reader may read no state or live plan of, or which the filter does not match", what)
				continue
			}
			shown[name] = true
			note := first(a, classed("t-note"))
			if hasClass(note, "sr-only") && toneOf(first(a, classed("dot"))) != "ok" {
				r.errorf("the tree hides the note of %s, whose dot is not all clear", what)
			}
			parts := r.note(note)
			partial := r.counts(what, parts, s.plans)
			for _, p := range parts {
				ok := map[string]bool{
					"no state yet": !s.state,
					"not checked":  s.state && s.check == "",
					"check failed": s.check == "failed",
					"clean":        s.check == "clean",
					"drifted":      s.check == "drifted",
				}[p.word]
				switch {
				case p.counted && p.word == "plans":
					r.holds(what+" plans", p.fig, s.live())
				case p.counted && slices.Contains(liveStatuses, p.word):
				case !ok:
					r.errorf("the note of %s says %q, which its rows do not bear out", what, p.word)
				}
			}
			d := first(a, classed("dot"))
			r.handled[d] = true
			r.dot(what, toneOf(d), partial, stackFacts(s))
		}

		said := slices.ContainsFunc(everyOf(li, classed("tree-note")), func(n *html.Node) bool { return strings.Contains(folded(n), "more with state") })
		if r.unlisted(env) > 0 && !said {
			r.errorf("the tree leaves %d stacks with state of %s out without a word", r.unlisted(env), env)
		}
		for k, s := range r.w.stacks {
			switch {
			case k.env != env || !s.inTree() || !r.match(k.stack) || shown[k.stack]:
			case s.state && !r.listed[k]:
			case !s.state && count.floor:
			default:
				r.errorf("the tree leaves %s@%s out without a word", k.stack, k.env)
			}
		}
	}
	for _, env := range r.rd.envs {
		if !slices.Contains(shownEnvs, env) && (r.find == "" || r.matches(env) > 0) {
			r.errorf("the tree leaves %s out, which holds %d stacks the filter matches", env, r.matches(env))
		}
	}
}

// envNodes are the environments of the tree.
func (r *reading) envNodes() []*html.Node {
	return everyOf(r.side, func(n *html.Node) bool { return n.Data == "li" && n.Parent != nil && hasClass(n.Parent, "envs") })
}

// dots judges every dot outside the tree and the listings' status pills.
func (r *reading) dots() {
	for _, root := range []*html.Node{r.main, r.side} {
		for _, d := range everyOf(root, classed("dot")) {
			if r.handled[d] || up(d, isTag("table")) != nil {
				continue
			}
			got := toneOf(d)
			switch {
			case up(d, classed("rows")) != nil:
				sec, _ := attr(up(d, isTag("section")), "aria-labelledby")
				if want := map[string]string{"h-applying": "run", "h-waiting": "wait", "h-planning": "run"}[sec]; got != want {
					r.errorf("a row of %s has a dot %s, want %s", sec, got, want)
				}
			case up(d, classed("stat")) != nil:
				stat := up(d, classed("stat"))
				label := folded(first(stat, classed("stat-label")))
				f, _ := readFig(first(first(stat, classed("stat-n")), classed("n")))
				says, n := map[string]string{"Waiting for approval": "wait", "Applying": "run", "Failed": "bad", "Drifted": "wait"}[label], map[string]int{
					"Waiting for approval": r.view.plans["pending"], "Applying": r.view.plans["applying"], "Failed": r.view.failed, "Drifted": r.view.drifted}[label]
				if got != says && got != "none" || got == says && n == 0 || got == "none" && n > 0 && !f.floor {
					r.errorf("the dot of %s is %s, of %d", label, got, n)
				}
			case up(d, classed("calm")) != nil:
				if !strings.Contains(folded(up(d, classed("calm"))), ui.Nones["calm"]) {
					r.errorf("an all-clear dot beside %q", folded(up(d, classed("calm"))))
				}
			case up(d, classed("side-foot")) != nil:
				w := r.w
				if !map[string]bool{"bad": w.pollsFailed > 0, "ok": w.polled > 0 && w.pollsFailed == 0, "none": w.polled == 0 && w.pollsFailed == 0}[got] {
					r.errorf("the repositories' dot is %s with %d polled, %d failed", got, w.polled, w.pollsFailed)
				}
			default:
				r.errorf("a dot checkRender has no rule for: %s beside %q", got, folded(d.Parent))
			}
		}
	}
}

// lists holds each list of the overview to the number above it, and the
// table of stacks with state to the stacks writ serve lists.
func (r *reading) lists() {
	for _, id := range []string{"h-alerts", "h-applying", "h-waiting", "h-planning"} {
		sec := first(r.main, labelledBy(id))
		if sec == nil {
			continue
		}
		f, _ := readFig(first(first(sec, isTag("h2")), classed("n")))
		rows := len(everyOf(sec, func(n *html.Node) bool {
			return n.Data == "li" && n.Parent != nil && (hasClass(n.Parent, "rows") || hasClass(n.Parent, "alerts"))
		}))
		if rows != f.n {
			r.errorf("%s says %s and lists %d", id, f, rows)
		}
	}
	if rows := len(everyOf(r.main, func(n *html.Node) bool { return n.Data == "tr" && up(n, isTag("tbody")) != nil })); rows != r.listedIn(r.viewEnvs...) {
		r.errorf("the table lists %d stacks with state, of %d writ serve lists", rows, r.listedIn(r.viewEnvs...))
	}
}

func envOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	li := up(n, func(a *html.Node) bool { return a.Data == "li" && a.Parent != nil && hasClass(a.Parent, "envs") })
	if li == nil {
		return ""
	}
	return folded(first(li, classed("t-name")))
}

func toneOf(d *html.Node) string {
	c, _ := attr(d, "class")
	for _, f := range strings.Fields(c) {
		if t, ok := strings.CutPrefix(f, "dot-"); ok {
			return t
		}
	}
	return ""
}

func hasClass(n *html.Node, c string) bool {
	v, _ := attr(n, "class")
	return slices.Contains(strings.Fields(v), c)
}

func classed(c string) func(*html.Node) bool {
	return func(n *html.Node) bool { return hasClass(n, c) }
}

func isTag(tag string) func(*html.Node) bool {
	return func(n *html.Node) bool { return n.Data == tag }
}

func labelledBy(id string) func(*html.Node) bool {
	return func(n *html.Node) bool { v, _ := attr(n, "aria-labelledby"); return n.Data == "section" && v == id }
}

// up is the nearest element above n that pred reports true for.
func up(n *html.Node, pred func(*html.Node) bool) *html.Node {
	for a := n.Parent; a != nil; a = a.Parent {
		if a.Type == html.ElementNode && pred(a) {
			return a
		}
	}
	return nil
}

func everyOf(root *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	if root == nil {
		return nil
	}
	each(root, func(n *html.Node) {
		if pred(n) {
			out = append(out, n)
		}
	})
	return out
}

func first(root *html.Node, pred func(*html.Node) bool) *html.Node {
	if all := everyOf(root, pred); len(all) > 0 {
		return all[0]
	}
	return nil
}

func folded(n *html.Node) string {
	if n == nil {
		return ""
	}
	return strings.Join(strings.Fields(text(n)), " ")
}

// visible is the text of n a sighted reader sees.
func visible(n *html.Node) string {
	if n == nil || hasClass(n, "sr-only") {
		return ""
	}
	return shown(n)
}

// shown is the text of n a sighted reader would see were n itself shown:
// it leaves out what lies under n for a screen reader only.
func shown(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if !hasClass(c, "sr-only") {
				walk(c)
			}
		}
	}
	if n != nil {
		walk(n)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
