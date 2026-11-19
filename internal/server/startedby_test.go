package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// A plan says what started it: ci for a CI job's token, person for a
// person's session. GET /v1/plans filters on it, alone and with a status,
// and refuses drift, whose checks GET /v1/drift lists, and any other
// value.
func TestPlansStartedBy(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := tofuFixtureOn(t, e, &fakeTofu{plan: createOnly}, uidLines(t), withApprovals(t))
		f.addInfra()
		sha := f.gitlab.Commit(4242, "mr", changed("resource \"random_pet\" \"pet2\" {}\n"))
		ci := f.mrPlan(sha, "mr", "network", "1849213")
		sess := &server.Session{ID: "S1", Display: "carol", Person: "gitlab/user/3003", Issuer: "sso", Subject: "3003", Teams: []string{"infra_mr"}, EndsAt: time.Now().Add(time.Hour)}
		mine := f.gitlab.Commit(4242, "carol", changed("resource \"random_pet\" \"pet3\" {}\n"))
		body, _ := io.ReadAll(commitPlan(map[string]any{"repo": "infra", "commit": mine, "ref": "refs/heads/carol"}))
		status, out := f.srv.Post(httptest.NewRequest("POST", "/ui/", nil), sess, "/v1/plans", body)
		var started server.RemotePlan
		if err := json.Unmarshal(out, &started); status != http.StatusAccepted || err != nil || started.Plan == nil {
			t.Fatalf("carol's plan: %d %s", status, out)
		}
		if started.Plan.StartedBy != server.StartedByPerson {
			t.Errorf("POST /v1/plans of a session: started_by %q", started.Plan.StartedBy)
		}
		person := f.waitPlan(f.token(nil), started.Plan.Ref)
		if ci.Plan.StartedBy != server.StartedByCI || person.Plan.StartedBy != server.StartedByPerson {
			t.Fatalf("started_by of the CI job's plan %q, of carol's %q", ci.Plan.StartedBy, person.Plan.StartedBy)
		}
		tok := f.token(nil)
		for _, c := range []struct{ query, want string }{
			{"", person.Plan.ID + " " + ci.Plan.ID},
			{"&started_by=ci", ci.Plan.ID},
			{"&started_by=person", person.Plan.ID},
			{"&started_by=person&status=pending", person.Plan.ID},
			{"&started_by=ci&status=applied", ""},
		} {
			r := f.do("GET", "/v1/plans?env=prod"+c.query, tok, nil)
			var doc server.RemotePlans
			if err := json.Unmarshal(r.body, &doc); r.status != http.StatusOK || err != nil {
				t.Fatalf("%s: %d %s", c.query, r.status, r.body)
			}
			var ids []string
			for _, p := range doc.Plans {
				ids = append(ids, p.ID)
				if want := map[string]string{ci.Plan.ID: "ci", person.Plan.ID: "person"}[p.ID]; p.StartedBy != want {
					t.Errorf("%s: plan %s started_by %q, want %q", c.query, p.ID, p.StartedBy, want)
				}
			}
			if got := strings.Join(ids, " "); got != c.want {
				t.Errorf("GET /v1/plans?env=prod%s: %q, want %q", c.query, got, c.want)
			}
		}
		for value, says := range map[string]string{"drift": "GET /v1/drift", "robot": "person or ci"} {
			if r := f.do("GET", "/v1/plans?env=prod&started_by="+value, tok, nil); r.status != http.StatusBadRequest || r.class() != "usage" || !strings.Contains(string(r.body), says) {
				t.Errorf("started_by=%s: %d %s", value, r.status, r.body)
			}
		}
	})
}
