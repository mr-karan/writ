package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/server"
)

// The stations say where a plan is: a plan that failed to plan never
// reaches approval, a plan whose apply failed shows the apply failed,
// and a spent plan will never apply. The approvals a plan needs are not
// known until it is made: the server tallies a planning plan against the
// rules of a plan with no risks, so its need may read 0 or more, and
// neither is the plan's.
func TestPlanSteps(t *testing.T) {
	one := server.PlanApprovals{Need: 1}
	given := server.PlanApprovals{Need: 1, ApprovedBy: []server.ApprovalInfo{{Display: "rev"}}}
	for _, tc := range []struct {
		name string
		plan server.PlanInfo
		want string
	}{
		{"planning", server.PlanInfo{Status: "planning"}, "Plan now planning, Approve todo not known yet, Apply todo "},
		{"planning under a rule of every plan", server.PlanInfo{Status: "planning", Approvals: one}, "Plan now planning, Approve todo not known yet, Apply todo "},
		{"pending", server.PlanInfo{ID: "p", Status: "pending", Approvals: one}, "Plan done made, Approve next 0 of 1, Apply todo "},
		{"no rule", server.PlanInfo{ID: "p", Status: "approved"}, "Plan done made, Approve done none needed, Apply next "},
		{"approved", server.PlanInfo{ID: "p", Status: "approved", Approvals: given}, "Plan done made, Approve done 1 of 1, Apply next "},
		{"applying", server.PlanInfo{ID: "p", Status: "applying", Approvals: given, Run: "r"}, "Plan done made, Approve done 1 of 1, Apply now applying"},
		{"applied", server.PlanInfo{ID: "p", Status: "applied", Approvals: given, Run: "r"}, "Plan done made, Approve done 1 of 1, Apply done applied"},
		{"refused by a policy", server.PlanInfo{Status: "refused", Approvals: one}, "Plan failed refused, Approve void , Apply void "},
		{"failed to plan", server.PlanInfo{Status: "failed"}, "Plan failed failed, Approve void , Apply void "},
		{"apply failed", server.PlanInfo{ID: "p", Status: "failed", Approvals: given, Run: "r"}, "Plan done made, Approve done 1 of 1, Apply failed failed"},
		{"superseded after approval", server.PlanInfo{ID: "p", Status: "superseded", Approvals: given}, "Plan done made, Approve done 1 of 1, Apply void superseded"},
		{"superseded unapproved", server.PlanInfo{ID: "p", Status: "superseded", Approvals: one}, "Plan done made, Approve void 0 of 1, Apply void superseded"},
		{"the same as an earlier plan", server.PlanInfo{Status: "superseded", SupersededBy: "q"}, "Plan done made, Approve void , Apply void superseded"},
		{"expired unapproved", server.PlanInfo{ID: "p", Status: "expired", Approvals: one}, "Plan done made, Approve void 0 of 1, Apply void expired"},
		{"expired approved", server.PlanInfo{ID: "p", Status: "expired", Approvals: given}, "Plan done made, Approve done 1 of 1, Apply void expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, s := range planSteps(&tc.plan) {
				note := s.Note
				if s.Need != nil {
					note = fmt.Sprintf("%d of %d", s.Given.N, s.Need.N)
				}
				got = append(got, s.Name+" "+s.State+" "+note)
			}
			if g := strings.Join(got, ", "); g != tc.want {
				t.Errorf("got  %s\nwant %s", g, tc.want)
			}
		})
	}
}

// The plan page's decision panel says what the approval rules ask of the
// plan only once they were asked, and words the plan's run from where the
// run is, as the apply station does.
func TestPlanDecision(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	one := server.PlanApprovals{Need: 1, Rules: []int{1}}
	given := server.PlanApprovals{Need: 1, Rules: []int{1}, ApprovedBy: []server.ApprovalInfo{{Display: "rev", At: "2026-11-16T10:00:00Z"}}}
	const notAsked, noRule, needsOne = "This plan takes no approval.", "No approval rule matches this plan.", "It needs 1 approval from someone other than its author (rule 1 )."
	for _, tc := range []struct {
		name       string
		plan       server.PlanInfo
		want, deny []string
	}{
		{"planning", server.PlanInfo{Status: "planning"},
			[]string{"Writ asks the approval rules of this plan once it is made.", "Approve todo not known yet"}, []string{noRule, "none needed"}},
		{"planning under a rule of every plan", server.PlanInfo{Status: "planning", Approvals: one},
			[]string{"Writ asks the approval rules of this plan once it is made.", "Approve todo not known yet"}, []string{needsOne, "0 of 1"}},
		{"pending", server.PlanInfo{ID: id, Status: "pending", Approvals: one}, []string{needsOne, "Approve next 0 of 1"}, []string{"Run "}},
		{"approved by no rule", server.PlanInfo{ID: id, Status: "approved"}, []string{noRule, "Approve done none needed"}, nil},
		{"approved", server.PlanInfo{ID: id, Status: "approved", Approvals: given}, []string{needsOne, "Approve done 1 of 1"}, nil},
		{"applying", server.PlanInfo{ID: id, Status: "applying", Approvals: given, Run: "run1"},
			[]string{needsOne, "Apply now applying", "Run run1 is applying this plan."}, []string{"applied this plan"}},
		{"applied", server.PlanInfo{ID: id, Status: "applied", Approvals: given, Run: "run1"},
			[]string{needsOne, "Apply done applied", "Run run1 applied this plan."}, nil},
		{"apply failed", server.PlanInfo{ID: id, Status: "failed", Approvals: given, Run: "run1"},
			[]string{needsOne, "Apply failed failed", "Run run1 failed to apply this plan."}, []string{"applied this plan"}},
		{"refused", server.PlanInfo{Status: "refused", Approvals: one}, []string{notAsked}, []string{needsOne, "0 of 1"}},
		{"failed to plan", server.PlanInfo{Status: "failed"}, []string{notAsked}, []string{noRule, "none needed"}},
		{"superseded", server.PlanInfo{ID: id, Status: "superseded", Approvals: given}, []string{needsOne, "Apply void superseded"}, nil},
		{"the same as an earlier plan", server.PlanInfo{Status: "superseded", SupersededBy: id}, []string{notAsked}, []string{noRule, "none needed"}},
		{"expired", server.PlanInfo{ID: id, Status: "expired", Approvals: one}, []string{needsOne, "Approve void 0 of 1", "Apply void expired"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.plan
			p.Ref, p.Env, p.Stacks, p.Origin = "ref1", "prod", []server.PlanStack{{Stack: "app"}}, json.RawMessage(`{}`)
			p.Author, p.Source = server.Principal{Person: "gitlab/user/1001", Display: "alice"}, server.PlanSourceInfo{Kind: "commit"}
			var b bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&b, "plan", &page{Session: &server.Session{Person: "gitlab/user/2002"}, Data: newPlanView(&p, "", 1)}); err != nil {
				t.Fatal(err)
			}
			panel := decisionText(t, &b)
			for _, w := range tc.want {
				if !strings.Contains(panel, w) {
					t.Errorf("the decision lacks %q:\n%s", w, panel)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(panel, d) {
					t.Errorf("the decision says %q:\n%s", d, panel)
				}
			}
		})
	}
}

// decisionText is the text of a plan page's decision panel, its spaces
// folded.
func decisionText(t *testing.T, page *bytes.Buffer) string {
	t.Helper()
	root, err := html.Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	var found *html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "class" && strings.Contains(" "+a.Val+" ", " decision ") {
				found = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	if found == nil {
		t.Fatal("the page has no decision panel")
	}
	var b strings.Builder
	var text func(*html.Node)
	text = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data + " ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			text(c)
		}
	}
	text(found)
	return strings.Join(strings.Fields(b.String()), " ")
}

// Every status a plan, a run, a drift check, a policy decision, or the
// server's health can have renders as a stamp of its tone, so none
// renders as a plain grey stamp, and a status no table knows does.
func TestStatusStamps(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		t.Fatal(err)
	}
	tones := map[string]string{
		"planning": "run", "applying": "run",
		"pending": "wait", "drifted": "wait", "warned": "wait",
		"approved": "ok", "applied": "ok", "clean": "ok", "ok": "ok", "passed": "ok",
		"refused": "bad", "failed": "bad", "failing": "bad", "denied": "bad",
		"expired": "void", "superseded": "void",
		"unheard-of": "",
	}
	for _, s := range server.PlanStatuses {
		if _, ok := tones[s]; !ok {
			t.Errorf("plan status %s is missing from the test", s)
		}
	}
	for status, tone := range tones {
		t.Run(status, func(t *testing.T) {
			class := "stamp"
			if tone != "" {
				class += " stamp-" + tone
			}
			want := fmt.Sprintf(`<span class="%s status-%s"><span class="dot" aria-hidden="true"></span>%s</span>`, class, status, status)
			var b bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&b, "status", status); err != nil {
				t.Fatal(err)
			}
			if b.String() != want {
				t.Errorf("got  %s\nwant %s", b.String(), want)
			}
		})
	}
}

// The plan page offers its reader the approval of a live plan of a
// commit that someone else made, tells the author they cannot approve
// their own, says a speculative plan takes no approval, and shows the
// apply command while the plan is live.
func TestPlanOffer(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		t.Fatal(err)
	}
	const approve, own, speculative, apply = "Approve this plan", "You made this plan, so you cannot approve it.", "A speculative plan, of files a client sent", "writ apply --remote"
	me, other := "gitlab/user/2002", "gitlab/user/1001"
	for _, tc := range []struct {
		status, kind, author string
		want                 []string
	}{
		{"pending", "commit", other, []string{approve, apply}},
		{"approved", "commit", other, []string{approve, apply}},
		{"pending", "commit", me, []string{own, apply}},
		{"approved", "commit", me, []string{own, apply}},
		{"pending", "speculative", other, []string{speculative, apply}},
		{"applied", "speculative", other, []string{speculative}},
		{"planning", "commit", other, nil},
		{"applying", "commit", other, nil},
		{"applied", "commit", other, nil},
		{"refused", "commit", other, nil},
		{"failed", "commit", other, nil},
		{"expired", "commit", other, nil},
		{"superseded", "commit", other, nil},
	} {
		t.Run(tc.status+" "+tc.kind+" "+tc.author, func(t *testing.T) {
			p := server.PlanInfo{ID: strings.Repeat("a", 32), Ref: "ref1", Env: "prod", Status: tc.status, Stacks: []server.PlanStack{{Stack: "app"}}, Origin: json.RawMessage(`{}`),
				Author: server.Principal{Person: tc.author, Display: "alice"}, Source: server.PlanSourceInfo{Kind: tc.kind}}
			var b bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&b, "plan", &page{Session: &server.Session{Person: me}, Data: newPlanView(&p, "", 1)}); err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{approve, own, speculative, apply} {
				if got, want := strings.Contains(b.String(), line), slices.Contains(tc.want, line); got != want {
					t.Errorf("the page shows %q: %v, want %v", line, got, want)
				}
			}
		})
	}
}

// A note is a flex box, so its text sits in one child beside its icon:
// text and a time loose in it would be set apart, as "Expired 19:34 UTC
// . It can no longer be applied." was.
func TestNoteHoldsItsTextInOneBox(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	for _, p := range []server.PlanInfo{
		{ID: id, Status: "expired", ExpiresAt: "2026-11-18T19:34:00Z"},
		{ID: id, Status: "superseded", SupersededWhy: "a newer plan of app was approved"},
	} {
		t.Run(p.Status, func(t *testing.T) {
			p.Ref, p.Env, p.Stacks, p.Origin = "ref1", "prod", []server.PlanStack{{Stack: "app"}}, json.RawMessage(`{}`)
			var b bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&b, "plan", &page{Session: &server.Session{Person: "gitlab/user/2002"}, Data: newPlanView(&p, "", 1)}); err != nil {
				t.Fatal(err)
			}
			root, err := html.Parse(&b)
			if err != nil {
				t.Fatal(err)
			}
			notes := 0
			var walk func(*html.Node)
			walk = func(n *html.Node) {
				if n.Type == html.ElementNode && slices.Contains(strings.Fields(attrOf(n, "class")), "note") {
					notes++
					var boxes []string
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						switch {
						case c.Type == html.TextNode && strings.TrimSpace(c.Data) != "":
							boxes = append(boxes, fmt.Sprintf("%q", c.Data))
						case c.Type == html.ElementNode && c.Data != "svg":
							boxes = append(boxes, "<"+c.Data+">")
						}
					}
					if len(boxes) > 1 {
						t.Errorf("a note sets apart %s", strings.Join(boxes, ", "))
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
			}
			walk(root)
			if notes == 0 {
				t.Error("the page shows no note")
			}
		})
	}
}

// attrOf is n's attribute key, "" when it has none.
func attrOf(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
