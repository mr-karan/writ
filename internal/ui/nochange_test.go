package ui_test

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// uiNoChangeJSON is a plan whose one resource change is a no-op.
const uiNoChangeJSON = `{"format_version":"1.2","terraform_version":"1.13.9",
 "resource_changes":[
  {"address":"tls_private_key.ca_key","mode":"managed","type":"tls_private_key","name":"ca_key","provider_name":"registry.opentofu.org/hashicorp/tls",
   "change":{"actions":["no-op"],"before":{"ecdsa_curve":"P256"},"after":{"ecdsa_curve":"P256"},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}
 ]}`

// sidebar is the text of a page's sidebar, its spaces folded.
func sidebar(t *testing.T, doc *html.Node) string {
	t.Helper()
	var side *html.Node
	each(doc, func(n *html.Node) {
		if side == nil && n.Data == "aside" && hasClass(n, "side") {
			side = n
		}
	})
	if side == nil {
		t.Fatal("the page has no sidebar")
	}
	return strings.Join(strings.Fields(text(side)), " ")
}

// A plan that changes nothing shows as no-changes in a quiet pill: its
// page offers no approval and no apply, its stations end at the plan, the
// sidebar counts it nowhere, and the plans list finds it by its status.
// A plan that waits, made next, shows that the sidebar does count.
func TestNoChangePlanPage(t *testing.T) {
	f := newFixture(t)
	f.tofu.mu.Lock()
	f.tofu.doc = func() string { return uiNoChangeJSON }
	f.tofu.mu.Unlock()
	id := f.plan(nil)
	b := f.browser()

	p := b.signIn("/ui/plans/" + id)
	checkPage(t, p, "")
	if !strings.Contains(p.body, `class="stamp stamp-none status-no-changes"`) {
		t.Error("the plan's pill is not the quiet no-changes one")
	}
	panel := strings.Join(strings.Fields(text(p.doc)), " ")
	for _, want := range []string{"This plan takes no approval.", "Approve void not needed", "Apply void not needed"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the plan page lacks %q", want)
		}
	}
	for _, deny := range []string{"writ apply --remote", "the apply job", "You made this plan", "No approval rule matches", "Approve this plan"} {
		if strings.Contains(panel, deny) {
			t.Errorf("the plan page says %q", deny)
		}
	}
	side := sidebar(t, p.doc)
	for _, deny := range []string{"approved", "no-changes", "pending"} {
		if strings.Contains(side, deny) {
			t.Errorf("the sidebar counts the plan as %q: %s", deny, side)
		}
	}

	list := b.get("/ui/plans?env=prod")
	checkPage(t, list, "")
	if !strings.Contains(list.body, `href="/ui/plans?env=prod&amp;status=no-changes"`) {
		t.Error("the Status picker has no no-changes")
	}
	if l := b.get("/ui/plans?env=prod&status=no-changes"); !strings.Contains(l.body, id) || !strings.Contains(l.body, "status-no-changes") {
		t.Error("the plans of status no-changes do not list the plan")
	}
	if l := b.get("/ui/plans?env=prod&status=approved"); strings.Contains(l.body, id) {
		t.Error("the approved plans list the plan")
	}
	if a := b.get("/ui/approvals?env=prod"); strings.Contains(a.body, id) {
		t.Error("the approvals list the plan")
	}

	f.tofu.mu.Lock()
	f.tofu.doc = nil
	f.tofu.mu.Unlock()
	f.plan(nil)
	if side := sidebar(t, b.get("/ui/plans/"+id).doc); !strings.Contains(side, "Approvals1 waiting") || !strings.Contains(side, "network 1 pending") {
		t.Errorf("the sidebar does not count the plan that waits: %s", side)
	}
}
