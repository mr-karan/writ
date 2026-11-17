package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
)

// planned makes one plan of network@prod and returns its id.
func (f *fixture) planned() string {
	f.t.Helper()
	tok := f.mrToken(nil, nil)
	r := f.do("POST", "/v1/plans", tok, planRequest(nil))
	ref := r.doc["plan"].(map[string]any)["ref"].(string)
	got := f.do("GET", "/v1/plans/"+ref+"?wait=10s", tok, nil)
	return got.doc["plan"].(map[string]any)["id"].(string)
}

func TestConfigEndpoint(t *testing.T) {
	f := newFixture(t, func(s string) string {
		s = strings.Replace(s, "can = [\"read\", \"plan\"]\n}\n", `can = ["read", "plan"]
}

grant {
  teams = ["reviewers"]
  stacks = ["*"]
  envs = ["*"]
  can = ["admin"]
}
`, 1)
		return strings.Replace(s, "key = \"prod.key\"\n", "key = \"prod.key\"\n  provider_env = { DB_PASSWORD = \"db_password\" }\n", 1)
	})
	r := f.do("GET", "/v1/config", f.token(nil), nil)
	var doc server.ConfigDoc
	if err := json.Unmarshal(r.body, &doc); err != nil || r.status != 200 || !doc.OK {
		t.Fatalf("config %d %s", r.status, r.body)
	}
	if len(doc.Teams) != 2 || len(doc.Grants) != 3 || doc.Grants[0].Grant != 1 || doc.Envs[0].Name != "prod" {
		t.Fatalf("config %s", r.body)
	}
	if doc.PlanTTL != "24h0m0s" || doc.SessionTTL != "12h0m0s" || doc.Admin != nil {
		t.Fatalf("config %s", r.body)
	}
	for _, leak := range []string{"postgres://", "prod.key", "db_password", f.secret, "issuer.pem"} {
		if strings.Contains(string(r.body), leak) {
			t.Errorf("the config document holds %q", leak)
		}
	}
	admin := f.do("GET", "/v1/config", f.token(map[string]any{"user_access_level": "maintainer"}), nil)
	if err := json.Unmarshal(admin.body, &doc); err != nil || doc.Admin == nil || doc.Admin.Limits.Body != 16<<20 {
		t.Fatalf("config for admin %s", admin.body)
	}
	if r := f.do("GET", "/v1/config", f.token(map[string]any{"project_id": "1"}), nil); r.status != 403 || r.class() != "denied" || r.doc["format"] != "writ.config/v0" {
		t.Fatalf("config without read: %d %s", r.status, r.body)
	}
}

// Call sends a control UI read through the API's own handler: the same
// grants decide it, and the audit row names the browser session.
func TestCallAsSession(t *testing.T) {
	f := planFixture(t, withForge(t, ""))
	f.addInfra()
	f.planned()
	sess := &server.Session{ID: "S1", Display: "bob", Person: "gitlab/user/2002", Issuer: "sso", Subject: "2002", Teams: []string{"infra_mr"}, EndsAt: time.Now().Add(time.Hour)}
	r := httptest.NewRequest("GET", "/ui/", nil)
	status, body := f.srv.Call(r, sess, "/v1/plans?env=prod")
	if status != 200 || !strings.Contains(string(body), `"format":"writ.remote-plans/v0"`) {
		t.Fatalf("call %d %s", status, body)
	}
	rows := f.audit(controltest.AuditFilter{Person: "gitlab/user/2002"})
	if len(rows) != 1 || rows[0].Via != "browser session" || rows[0].PrincipalKind != "person" || rows[0].Route != "/v1/plans" || rows[0].Decision != control.Allowed {
		t.Fatalf("audit %+v", rows)
	}
	status, body = f.srv.Call(r, &server.Session{Display: "eve", Person: "x/user/9", Teams: []string{}}, "/v1/index?env=prod&stack=network")
	if status != http.StatusForbidden || !strings.Contains(string(body), `"class":"denied"`) {
		t.Fatalf("a session in no team read the state index: %d %s", status, body)
	}
	if status, _ := f.srv.Call(r, sess, "/ui/"); status != http.StatusBadRequest {
		t.Fatalf("Call reached a non-API route: %d", status)
	}
	whoami := mustCall(t, f, r, sess, "/v1/whoami")
	if !strings.Contains(whoami, `"kind":"person"`) || !strings.Contains(whoami, `"display":"bob"`) {
		t.Fatalf("whoami of a session %s", whoami)
	}
}

func mustCall(t *testing.T, f *fixture, r *http.Request, sess *server.Session, target string) string {
	t.Helper()
	status, body := f.srv.Call(r, sess, target)
	if status != 200 {
		t.Fatalf("%s: %d %s", target, status, body)
	}
	return string(body)
}
