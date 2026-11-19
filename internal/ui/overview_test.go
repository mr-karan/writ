package ui_test

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"golang.org/x/net/html"
)

// TestOverviewAndSidebarCount checks that the overview's cards and alerts
// and the sidebar's tree follow the plans and runs writ serve holds: a
// plan by someone else waits, then it is approved and its run fails.
func TestOverviewAndSidebarCount(t *testing.T) {
	f := newFixture(t)
	id := f.plan(nil)
	b := f.browser()
	b.signIn("/ui/")

	p := b.get("/ui/")
	if got := classTexts(p.doc, "stat"); !slices.Equal(got, []string{
		"Waiting for approval11 by others · 0 yours",
		"Applying00 planning",
		"Failed0runs in the last 24 hours",
		"Drifted0of 0 stacks checked",
	}) {
		t.Errorf("the stat cards with a plan that waits: %q", got)
	}
	if got := classTexts(p.doc, "row"); len(got) != 1 || !strings.HasPrefix(got[0], "network@prod") || !strings.Contains(got[0], "alice") {
		t.Errorf("the waiting list: %q", got)
	}
	if got := classTexts(p.doc, "stacks"); len(got) != 1 || got[0] != "network 1 pending" {
		t.Errorf("the sidebar's stacks: %q", got)
	}
	if got := classTexts(p.doc, "count"); !slices.Contains(got, "1 waiting") {
		t.Errorf("the sidebar's counts lack the plan that waits: %q", got)
	}

	if q := b.get("/ui/?find=NET"); len(classTexts(q.doc, "stacks")) != 1 {
		t.Errorf("the filter NET lost network: %s", text(q.doc))
	}
	if q := b.get("/ui/?find=queue"); len(classTexts(q.doc, "stacks")) != 0 || !strings.Contains(text(q.doc), "0 stacks match “queue”") {
		t.Errorf("the filter queue kept a stack: %s", text(q.doc))
	}

	if q := b.post("/ui/plans/"+id+"/approve", url.Values{}, "same-origin"); q.status != 200 {
		t.Fatalf("approve: %d", q.status)
	}
	run := f.apply(id)
	p = b.get("/ui/")
	if got := classTexts(p.doc, "stat"); len(got) != 4 || got[0] != "Waiting for approval00 by others · 0 yours" || got[2] != "Failed1runs in the last 24 hours" {
		t.Errorf("the stat cards after a failed run: %q", got)
	}
	alerts := classTexts(p.doc, "alerts")
	if len(alerts) != 1 || !strings.Contains(alerts[0], "Run failed network@prod") {
		t.Errorf("no alert of the failed run: %q", alerts)
	}
	found := false
	each(p.doc, func(n *html.Node) {
		if h, _ := attr(n, "href"); n.Data == "a" && h == "/ui/runs/"+run {
			found = true
		}
	})
	if !found {
		t.Errorf("no alert links the run %s", run)
	}
}

// TestOverviewPastTheList checks that the overview and the sidebar never
// read as all clear from a list writ serve cut short: network's drift,
// behind 200 stacks named before it, still counts and alerts, 200 more
// plans that wait than writ serve reads mark the counts as floors, and
// the tree says it left network out.
func TestOverviewPastTheList(t *testing.T) {
	f := newFixture(t)
	id := f.plan(nil)
	f.driftCheck()
	ctx := context.Background()
	for i := range 200 {
		if _, err := f.store.ReplaceIndex(ctx, control.IndexWrite{Env: "prod", Stack: fmt.Sprintf("a%03d", i), Backend: control.BackendHTTP, Version: 1, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	b := f.browser()
	b.signIn("/ui/")

	p := b.get("/ui/")
	if got := classTexts(p.doc, "stat"); len(got) != 4 || got[3] != "Drifted1of 2 stacks checked" {
		t.Errorf("the stat cards with network's drift past the list: %q", got)
	}
	if got := classTexts(p.doc, "alerts"); len(got) != 1 || !strings.Contains(got[0], "Drift found network@prod") {
		t.Errorf("no alert of network's drift: %q", got)
	}
	if body := strings.Join(strings.Fields(text(p.doc)), " "); strings.Contains(body, "No drift, no failed run in the last 24 hours, no plan about to expire") || !strings.Contains(body, "Stacks with state 202") {
		t.Errorf("the overview with 202 stacks: %s", body)
	}

	raw := controltest.Raw(t, f.loc)
	cols, err := raw.QueryContext(ctx, "SELECT * FROM plans WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	names, err := cols.Columns()
	cols.Close()
	if err != nil {
		t.Fatal(err)
	}
	pick := slices.Clone(names)
	for i, c := range pick {
		switch c {
		case "ref", "id":
			pick[i] = c + " || ?"
		case "seq":
			pick[i] = "seq - ?"
		}
	}
	for i := range 200 {
		suffix := fmt.Sprintf("-%d", i)
		args := []any{}
		for _, c := range names {
			switch c {
			case "ref", "id":
				args = append(args, suffix)
			case "seq":
				args = append(args, i+1)
			}
		}
		if _, err := raw.ExecContext(ctx, "INSERT INTO plans ("+strings.Join(names, ", ")+") SELECT "+strings.Join(pick, ", ")+" FROM plans WHERE id = ?", append(args, id)...); err != nil {
			t.Fatal(err)
		}
	}
	p = b.get("/ui/")
	if got := classTexts(p.doc, "stat"); len(got) != 4 || got[0] != "Waiting for approval200+ or more200+ or more by others · 0+ or more yours" {
		t.Errorf("the stat cards with 201 plans that wait: %q", got)
	}
	if got := classTexts(p.doc, "count"); !slices.Contains(got, "200+ or more waiting") {
		t.Errorf("the sidebar's counts with 201 plans that wait: %q", got)
	}
	// prod holds network and the 200 stacks named before it, and with
	// plans left unread its count of stacks is a floor. network has state
	// and comes after the 200 stacks listed, so the tree leaves it out and
	// says so, and never lists it as a stack with no state.
	if got := classTexts(p.doc, "envs"); len(got) != 1 || !strings.Contains(got[0], "prod 201+ or more stacks") || !strings.Contains(got[0], "1 more with state, not listed") ||
		strings.Contains(got[0], "network") {
		t.Errorf("the sidebar with 201 plans that wait: %q", got)
	}
}

// classTexts is the text of each element whose class list holds class.
func classTexts(doc *html.Node, class string) []string {
	var out []string
	each(doc, func(n *html.Node) {
		if c, _ := attr(n, "class"); slices.Contains(strings.Fields(c), class) {
			out = append(out, strings.Join(strings.Fields(text(n)), " "))
		}
	})
	return out
}
