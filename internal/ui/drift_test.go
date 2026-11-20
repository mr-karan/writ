package ui_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/oidc/oidctest"
	"github.com/mr-karan/writ/internal/server"
)

// A drift check's page shows its head (drift timer, interval, stack@env,
// commit, when, outcome), its changes (outside OpenTofu and in the code),
// or its error and adopt command when it failed. The Drift tab's kept
// checks and the overview's alerts link it, and /ui/plans/{its plan id}
// redirects to it.
func TestDriftCheckDetailPage(t *testing.T) {
	x := newTofuUI(t, func(cfg *server.Config) { cfg.DriftEvery = 6 * time.Hour })
	x.driftCheck(second)

	status, body := x.srv.Call(nilReq(), devSession(t, x.srv), "/v1/drift?env=dev&stack=app")
	var doc server.DriftDoc
	if status != http.StatusOK || json.Unmarshal(body, &doc) != nil || len(doc.Stacks) != 1 || doc.Stacks[0].Check == nil {
		t.Fatalf("GET /v1/drift: %d %s", status, body)
	}
	c := doc.Stacks[0].Check
	if c.Ref == "" || c.Plan == "" {
		t.Fatalf("drift check lacks ref or plan: %+v", c)
	}

	// Drift tab and overview alerts both link to /ui/drift/{ref}.
	driftTabHTML, driftTab := x.page("/ui/stack?stack=app&env=dev&view=drift")
	wantHref := `href="/ui/drift/` + c.Ref + `"`
	if !strings.Contains(driftTabHTML, wantHref) {
		t.Errorf("stack Drift tab lacks %s", wantHref)
	}
	if want := "Kept checks 1 The drift checks writ serve keeps of app in dev, newest first Check Status Commit Changes Checked " + c.Ref[:8] + " drifted " + x.commit[:8] + " 1 outside, 5 in code"; !strings.Contains(words(driftTab), want) {
		t.Errorf("stack Drift tab lacks %q:\n%s", want, words(driftTab))
	}
	overviewHTML, _ := x.page("/ui/")
	if !strings.Contains(overviewHTML, wantHref) {
		t.Errorf("overview alerts lack %s", wantHref)
	}

	// Detail page renders by ref and by plan id, and passes render checks.
	for _, id := range []string{c.Ref, c.Plan} {
		_, parsed := x.page("/ui/drift/" + id)
		text := words(parsed)
		for _, want := range []string{
			"Drift check " + c.Ref[:8] + " drifted",
			"app in dev",
			"every 6h",
			"Commit " + x.commit[:8] + " of demo",
			c.Plan + ", never applied",
			"Changes 6 1 changed outside OpenTofu, 5 that the code would change",
			"local_file.note changed outside OpenTofu - delete",
			"random_pet.rotated in the code -/+ replace id, keepers",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("/ui/drift/%s lacks %q:\n%s", id, want, text)
			}
		}
	}

	// /ui/plans/{drift plan id} redirects to /ui/drift/{id}.
	noFollow := &http.Client{Jar: x.c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(x.base + "/ui/plans/" + c.Plan)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/drift/"+c.Plan {
		t.Errorf("GET /ui/plans/%s: %d Location=%q, want 303 /ui/drift/%s", c.Plan, resp.StatusCode, resp.Header.Get("Location"), c.Plan)
	}

	// Failed drift check (no secret set) renders diagnostics and adopt hint.
	x.noSecretSet("staging")
	x.driftCheck(second)
	status, body = x.srv.Call(nilReq(), devSession(t, x.srv), "/v1/drift?env=staging&stack=app")
	var sdoc server.DriftDoc
	if status != http.StatusOK || json.Unmarshal(body, &sdoc) != nil || len(sdoc.Stacks) != 1 || sdoc.Stacks[0].Check == nil {
		t.Fatalf("GET /v1/drift staging: %d %s", status, body)
	}
	fc := sdoc.Stacks[0].Check
	_, fparsed := x.page("/ui/drift/" + fc.Ref)
	ftext := words(fparsed)
	for _, want := range []string{
		"Drift check " + fc.Ref[:8] + " failed",
		"drift unknown: no secret set for state version 1 of app@staging",
		"writ state adopt app --env staging --version 1 --reason REASON",
	} {
		if !strings.Contains(ftext, want) {
			t.Errorf("failed drift check page lacks %q:\n%s", want, ftext)
		}
	}

	// Clean drift check renders cleanly and passes render checks.
	const cleanRef = "CLEANCHECK0000000000000001"
	ctx := context.Background()
	now := time.Now()
	if err := x.store.BeginDrift(ctx, cleanRef, server.DevEnv, "app", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := x.store.FinishDrift(ctx, control.DriftCheck{Ref: cleanRef, Status: control.DriftClean, MadeAt: now.Add(2 * time.Second), PlanID: strings.Repeat("c", 32), Repo: "demo", Commit: x.commit}); err != nil {
		t.Fatal(err)
	}
	_, cparsed := x.page("/ui/drift/" + cleanRef)
	if !strings.Contains(words(cparsed), "Drift check "+cleanRef[:8]+" clean") {
		t.Errorf("clean drift check page: %s", words(cparsed))
	}
}

// A signed-in user who lacks read on a stack cannot view its drift check
// detail page, and /ui/plans/{drift id} does not reveal its existence.
func TestDriftCheckDetailEnforcesReadGrants(t *testing.T) {
	f := newFixtureOn(t, controltest.SQLite, `
team "limited" {
  issuer = "sso"
  match  = { groups = ["acme/limited"] }
}

grant {
  teams  = ["limited"]
  stacks = ["other"]
  envs   = ["prod"]
  can    = ["read"]
}
`)
	f.driftCheck()
	st, _, err := f.store.DriftStacks(context.Background(), "prod", "network", 1, func(string, string) bool { return true })
	if err != nil || len(st) != 1 || st[0].Last == nil {
		t.Fatalf("stored drift check: %+v %v", st, err)
	}
	ref, planID := st[0].Last.Ref, st[0].Last.PlanID

	// Authorized reader (platform) sees the detail page.
	authBrowser := f.browser()
	authBrowser.signIn("/ui/")
	p := authBrowser.get("/ui/drift/" + ref)
	if p.status != http.StatusOK || !strings.Contains(text(p.doc), "tls_private_key.ca_key") {
		t.Fatalf("authorized reader: %d %s", p.status, text(p.doc))
	}
	checkPage(t, p, "")

	// Limited reader (only reads other@prod) gets 403 on /ui/drift/{ref}
	// and 404 (no redirect) on /ui/plans/{planID}.
	claims, info := oidctest.GitLabPerson()
	claims["sub"], claims["preferred_username"], claims["groups_direct"] = "9999", "lim", []string{"acme/limited"}
	info["sub"], info["preferred_username"], info["groups"] = "9999", "lim", []string{"acme", "acme/limited"}
	f.is.SetPerson(claims, info)
	limBrowser := f.browser()
	limBrowser.signIn("/ui/")
	limBrowser.follow = false
	denied := limBrowser.get("/ui/drift/" + ref)
	if denied.status != http.StatusForbidden {
		t.Errorf("limited reader on /ui/drift/%s: %d, want 403", ref, denied.status)
	}
	notFound := limBrowser.get("/ui/plans/" + planID)
	if notFound.status != http.StatusNotFound {
		t.Errorf("limited reader on /ui/plans/%s: %d, want 404 (no existence leak)", planID, notFound.status)
	}
}

func nilReq() *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	return r
}
