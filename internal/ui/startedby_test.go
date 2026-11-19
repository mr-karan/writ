package ui_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/server"
)

// personPlan stores a pending plan of network in prod that the signed-in
// person carol started from her session, with the document and the
// source of plan id, and returns its id. No team of the fixture's
// people may plan, so the plan is written as writ serve writes one.
func (f *fixture) personPlan(id string) string {
	f.t.Helper()
	ctx := context.Background()
	src, err := f.store.Plan(ctx, id, f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	const ref, person = "CAR0LPLANCAR0LPLANCAR0LPLA", "c0ffee00c0ffee00c0ffee00c0ffee00"
	now := f.clock.Now()
	p := control.Plan{Ref: ref, Env: "prod", Stack: "network", RequestedAt: now, SourceDigest: src.SourceDigest, ConfigID: src.ConfigID, Source: src.Source,
		Author: control.Principal{Issuer: "sso", Subject: "3003", Person: "gitlab/user/3003", Display: "carol",
			Detail: json.RawMessage(`{"kind":"person","issuer":"sso","subject":"3003","display":"carol","person":"gitlab/user/3003"}`)},
		Teams: []string{"platform"}, Origin: json.RawMessage(`{"kind":"session"}`)}
	e := control.Entry{At: now, Request: ref, Kind: "request", Decision: control.Allowed}
	if err := f.store.CreatePlan(ctx, p, control.TokenUse{}, e); err != nil {
		f.t.Fatal(err)
	}
	res := control.PlanResult{ID: person, Status: control.Pending, MadeAt: now, ExpiresAt: now.Add(time.Hour), Risks: src.Risks, Changes: src.Changes,
		Document: src.Document, Text: src.Text, Saved: []byte("saved"), Need: 1, NeedRules: []int{1}, Tofu: src.Tofu}
	if err := f.store.FinishPlan(ctx, ref, res, control.Entry{At: now, Request: ref, Kind: "plan-finished", Decision: control.NoDecision}); err != nil {
		f.t.Fatal(err)
	}
	return person
}

// rowOf is the table row of page p that links plan id, "" when none does.
func rowOf(p page, id string) string {
	for _, row := range strings.Split(p.body, "<tr>")[1:] {
		row, _, _ = strings.Cut(row, "</tr>")
		if strings.Contains(row, `href="/ui/plans/`+id+`"`) {
			return row
		}
	}
	return ""
}

// Each list of plans marks what started a plan, a person or a CI job,
// with an icon and words for a screen reader; the plans list filters on
// it with a picker that works with no script; and a plan's page says it.
func TestPlansShowWhatStartedThem(t *testing.T) {
	f := newFixture(t)
	ci := f.plan(nil)
	person := f.personPlan(ci)
	b := f.browser()
	b.signIn("/ui/")
	cookie := ""
	for _, c := range b.c.Jar.Cookies(mustURL(f.url + "/ui/")) {
		if c.Name == "__Host-writ_session" {
			cookie = c.Value
		}
	}
	marks := map[string]string{ci: `href="#i-ci"/></svg><span class="sr-only">CI job </span>`, person: `href="#i-person"/></svg><span class="sr-only">Person </span>`}
	for _, c := range []struct {
		path  string
		shown []string
	}{
		{"/ui/plans?env=prod", []string{ci, person}},
		{"/ui/plans?env=prod&started_by=ci", []string{ci}},
		{"/ui/plans?env=prod&started_by=person", []string{person}},
		{"/ui/plans?env=prod&started_by=person&status=pending", []string{person}},
		{"/ui/approvals?env=prod", []string{ci, person}},
		{"/ui/stack?stack=network&env=prod&view=plans", []string{ci, person}},
	} {
		t.Run(c.path, func(t *testing.T) {
			p := b.get(c.path)
			if p.status != 200 {
				t.Fatalf("%d: %s", p.status, text(p.doc))
			}
			for id, mark := range marks {
				row := rowOf(p, id)
				switch shown := strings.Contains(strings.Join(c.shown, " "), id); {
				case shown && !strings.Contains(row, mark):
					t.Errorf("the row of plan %s lacks %s: %s", short(id), mark, row)
				case !shown && row != "":
					t.Errorf("the page lists plan %s", short(id))
				}
			}
			checkPage(t, p, cookie)
		})
	}

	p := b.get("/ui/plans?env=prod&started_by=ci&status=pending")
	for _, want := range []string{
		`<summary>Started by <b>ci</b></summary>`,
		`<input type="hidden" name="started_by" value="ci">`,
		`<a href="/ui/plans?env=prod&amp;started_by=person&amp;status=pending">person</a>`,
		`<a href="/ui/plans?env=prod&amp;status=pending">all</a>`,
		`<a href="/ui/plans?env=prod&amp;started_by=ci&amp;status=approved">approved</a>`,
	} {
		if !strings.Contains(p.body, want) {
			t.Errorf("the plans filtered by a CI job lack %s", want)
		}
	}
	if p := b.get("/ui/plans?env=prod&started_by=drift"); p.status != 400 || !strings.Contains(text(p.doc), "GET /v1/drift lists the drift checks") {
		t.Errorf("plans started by drift: %d %s", p.status, text(p.doc))
	}

	for id, says := range map[string]string{ci: "Started bya CI job", person: "Started bya person"} {
		p := b.get("/ui/plans/" + id)
		if p.status != 200 || !strings.Contains(text(p.doc), says) || !strings.Contains(p.body, marks[id]) {
			t.Errorf("the page of plan %s: %d, lacks %q or its mark: %s", short(id), p.status, says, text(p.doc))
		}
		checkPage(t, p, cookie)
	}
}

// A stack's drift view heads its last check as a drift check, with how
// often writ serve checks the stack when the config runs drift checks.
func TestTofuDriftCheckSaysWhatStartedIt(t *testing.T) {
	for _, c := range []struct {
		name  string
		every time.Duration
		head  string
	}{
		{"every 6h", 6 * time.Hour, "Drift check, every 6h"},
		{"no drift block", 0, "Drift check"},
	} {
		t.Run(c.name, func(t *testing.T) {
			x := newTofuUI(t, func(cfg *server.Config) { cfg.DriftEvery = c.every })
			x.driftCheck(second)
			body, doc := x.page("/ui/stack?stack=app&env=dev&view=drift")
			if got := words(first(doc, labelledBy("h-check"))); !strings.HasPrefix(got, c.head+" Status drifted") {
				t.Errorf("the drift check's card: %q, want it headed %q", got, c.head)
			}
			if c.every > 0 && !strings.Contains(body, `<time datetime="PT21600S">6h</time>`) {
				t.Error("the interval is not a time element of its duration")
			}
		})
	}
}
