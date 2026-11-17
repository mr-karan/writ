package tfstate_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/control/pgtest"
	"github.com/mr-karan/writ/internal/tfstate"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

const env = "prod"

type fixture struct {
	t     *testing.T
	store *control.Store
	loc   string
	srv   *httptest.Server
	now   time.Time
	b     *tfstate.Backend
}

func newFixture(t *testing.T, e controltest.Engine, opts tfstate.Options) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	f.store, f.loc = controltest.Open(t, e)
	opts.Store = f.store
	if opts.Key == nil {
		opts.Key = keys(map[string]*tfstate.Key{env: newKey(t)})
	}
	opts.Now = func() time.Time { return f.now }
	var err error
	if f.b, err = tfstate.New(opts); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(serve(f.b))
	t.Cleanup(f.srv.Close)
	return f
}

// serve is b as writ serve mounts it, with a new audit row for each
// request.
func serve(b *tfstate.Backend) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.Serve(w, r, control.Entry{Request: rand.Text(), Kind: "request", Method: r.Method, Decision: control.NoDecision}, func(control.Entry) error { return nil })
	})
}

func newKey(t *testing.T) *tfstate.Key {
	t.Helper()
	k, err := tfstate.NewKey([]byte(rand.Text() + "abcdef")[:tfstate.KeySize])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// keys looks an environment's key up in m.
func keys(m map[string]*tfstate.Key) func(string) (*tfstate.Key, error) {
	return func(env string) (*tfstate.Key, error) {
		if k, ok := m[env]; ok {
			return k, nil
		}
		return nil, fmt.Errorf("no key for env %s", env)
	}
}

// token issues a token for run on stack that lives an hour.
func (f *fixture) token(run, stack string, scope control.StateScope) string {
	f.t.Helper()
	tok, err := f.b.Issue(context.Background(), control.StateToken{RunID: run, Env: env, Stack: stack, Scope: scope, ExpiresAt: f.now.Add(time.Hour)})
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

// addr is the state address of stack, as a run's environment names it.
func (f *fixture) addr(stack string) string {
	for _, kv := range tfstate.ClientEnv(f.srv.URL, stack, "unused") {
		if v, ok := strings.CutPrefix(kv, "TF_HTTP_ADDRESS="); ok {
			return v
		}
	}
	f.t.Fatal("ClientEnv names no TF_HTTP_ADDRESS")
	return ""
}

// do sends one request as tofu's http backend does and returns the status
// and the body.
func (f *fixture) do(method, url, token, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if token != "" {
		req.SetBasicAuth("writ", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func (f *fixture) want(method, url, token, body string, status int) string {
	f.t.Helper()
	got, text := f.do(method, url, token, body)
	if got != status {
		f.t.Fatalf("%s %s: %d %s, want %d", method, url, got, text, status)
	}
	return text
}

func stateJSON(serial int, lineage, canary string) string {
	return fmt.Sprintf(`{"version":4,"terraform_version":"1.13.9","serial":%d,"lineage":%q,"outputs":{"c":{"value":%q,"type":"string"}},"resources":[]}`,
		serial, lineage, canary)
}

func lockJSON(id string) string {
	return fmt.Sprintf(`{"ID":%q,"Operation":"OperationTypeApply","Info":"","Who":"writ@run","Version":"1.13.9","Created":"2026-11-13T10:00:00Z","Path":""}`, id)
}

// versions returns the version numbers stored for stack, oldest first.
func (f *fixture) versions(stack string) []int64 {
	f.t.Helper()
	rows, err := controltest.Raw(f.t, f.loc).QueryContext(context.Background(),
		"SELECT version FROM tfstate_versions WHERE env = ? AND stack = ? ORDER BY version", env, stack)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func TestTokens(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{})
		plan := f.token("run-plan", "app", control.StatePlan)
		apply := f.token("run-apply", "app", control.StateApply)
		other := f.token("run-other", "db", control.StateApply)
		app := f.addr("app")
		cases := []struct {
			name, method, url, token, body string
			status                         int
		}{
			{"no credentials", "GET", app, "", "", http.StatusUnauthorized},
			{"an unknown token", "GET", app, "not-a-token", "", http.StatusUnauthorized},
			{"a plan reads", "GET", app, plan, "", http.StatusNotFound},
			{"an apply reads", "GET", app, apply, "", http.StatusNotFound},
			{"a plan cannot write", "POST", app + "?ID=x", plan, stateJSON(1, "l", "c"), http.StatusForbidden},
			{"a plan cannot lock", "LOCK", app + "/lock", plan, lockJSON("x"), http.StatusForbidden},
			{"a plan cannot unlock", "UNLOCK", app + "/lock", plan, lockJSON("x"), http.StatusForbidden},
			{"another stack's token cannot read", "GET", app, other, "", http.StatusForbidden},
			{"another stack's token cannot lock", "LOCK", app + "/lock", other, lockJSON("x"), http.StatusForbidden},
			{"another stack's token cannot write", "POST", app + "?ID=x", other, stateJSON(1, "l", "c"), http.StatusForbidden},
			{"an apply locks", "LOCK", app + "/lock", apply, lockJSON("x"), http.StatusOK},
			{"an unknown method", "DELETE", app, apply, "", http.StatusMethodNotAllowed},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got, body := f.do(c.method, c.url, c.token, c.body); got != c.status {
					t.Errorf("%s: %d %s, want %d", c.name, got, body, c.status)
				}
			})
		}

		t.Run("a token stops at its expiry", func(t *testing.T) {
			f.now = f.now.Add(time.Hour)
			defer func() { f.now = f.now.Add(-time.Hour) }()
			f.want("GET", app, plan, "", http.StatusUnauthorized)
		})
		t.Run("a token stops when its run ends, and the run's lock goes", func(t *testing.T) {
			if err := f.b.End(context.Background(), "run-apply"); err != nil {
				t.Fatal(err)
			}
			f.want("GET", app, apply, "", http.StatusUnauthorized)
			next := f.token("run-next", "app", control.StateApply)
			f.want("LOCK", app+"/lock", next, lockJSON("y"), http.StatusOK)
		})
		t.Run("a token for an env with no key is refused", func(t *testing.T) {
			_, err := f.b.Issue(context.Background(), control.StateToken{RunID: "r", Env: "staging", Stack: "app", Scope: control.StatePlan, ExpiresAt: f.now.Add(time.Hour)})
			if err == nil {
				t.Error("Issue made a token for an env with no state key")
			}
		})
	})
}

func TestLockAndWrite(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{})
		app := f.addr("app")
		a := f.token("run-a", "app", control.StateApply)
		b := f.token("run-b", "app", control.StateApply)
		plan := f.token("run-p", "app", control.StatePlan)
		state := stateJSON(1, "lineage-1", "canary")

		f.want("LOCK", app+"/lock", a, lockJSON("lock-a"), http.StatusOK)
		f.want("LOCK", app+"/lock", a, lockJSON("lock-a"), http.StatusOK)
		if held := f.want("LOCK", app+"/lock", b, lockJSON("lock-b"), http.StatusLocked); held != lockJSON("lock-a") {
			t.Errorf("a refused LOCK returned %s, want the held lock %s", held, lockJSON("lock-a"))
		}
		f.want("LOCK", app+"/lock", a, lockJSON("lock-a2"), http.StatusLocked)
		f.want("POST", app+"?ID=lock-a", b, state, http.StatusConflict)
		f.want("UNLOCK", app+"/lock", b, lockJSON("lock-a"), http.StatusLocked)
		f.want("POST", app, a, state, http.StatusConflict)
		f.want("POST", app+"?ID=lock-b", a, state, http.StatusConflict)
		if got := f.versions("app"); len(got) != 0 {
			t.Fatalf("refused writes stored versions %v", got)
		}

		f.want("POST", app+"?ID=lock-a", a, state, http.StatusOK)
		if got := f.want("GET", app, plan, "", http.StatusOK); got != state {
			t.Errorf("GET returned %s, want the state written, %s", got, state)
		}
		f.want("UNLOCK", app+"/lock", a, lockJSON("lock-a"), http.StatusOK)
		f.want("POST", app+"?ID=lock-a", a, stateJSON(2, "lineage-1", "c"), http.StatusConflict)
		f.want("LOCK", app+"/lock", b, lockJSON("lock-b"), http.StatusOK)
		// A force-unlock sends no lock id.
		f.want("UNLOCK", app+"/lock", b, "", http.StatusOK)
		f.want("LOCK", app+"/lock", a, lockJSON("lock-a3"), http.StatusOK)
	})
}

func TestStaleWrites(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{})
		app := f.addr("app")
		a := f.token("run-a", "app", control.StateApply)
		f.want("LOCK", app+"/lock", a, lockJSON("l"), http.StatusOK)
		post := app + "?ID=l"
		f.want("POST", post, a, stateJSON(3, "one", "x"), http.StatusOK)
		// A retried request repeats the newest version and stores nothing.
		f.want("POST", post, a, stateJSON(3, "one", "x"), http.StatusOK)
		for _, c := range []struct{ name, body string }{
			{"the same serial with other bytes", stateJSON(3, "one", "y")},
			{"a lower serial", stateJSON(2, "one", "x")},
			{"another lineage", stateJSON(4, "two", "x")},
		} {
			if got, body := f.do("POST", post, a, c.body); got != http.StatusConflict {
				t.Errorf("%s: %d %s, want %d", c.name, got, body, http.StatusConflict)
			}
		}
		f.want("POST", post, a, stateJSON(4, "one", "x"), http.StatusOK)
		if got := f.versions("app"); len(got) != 2 {
			t.Errorf("stored versions %v, want 2", got)
		}
	})
}

func TestKeepsBoundedVersions(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{Keep: 2})
		app := f.addr("app")
		a := f.token("run-a", "app", control.StateApply)
		f.want("LOCK", app+"/lock", a, lockJSON("l"), http.StatusOK)
		for serial := 1; serial <= 4; serial++ {
			f.want("POST", app+"?ID=l", a, stateJSON(serial, "one", "x"), http.StatusOK)
		}
		if got := f.versions("app"); len(got) != 2 || got[0] != 3 || got[1] != 4 {
			t.Errorf("stored versions %v, want [3 4]", got)
		}
	})
}

func TestStatesAreSealed(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{})
		const canary = "canary-7f3c9e1a"
		for _, stack := range []string{"app", "db"} {
			a := f.token("run-"+stack, stack, control.StateApply)
			f.want("LOCK", f.addr(stack)+"/lock", a, lockJSON("l"), http.StatusOK)
			f.want("POST", f.addr(stack)+"?ID=l", a, stateJSON(1, "one", canary), http.StatusOK)
		}
		raw := controltest.Raw(t, f.loc)
		var sealed []byte
		if err := raw.QueryRowContext(context.Background(), "SELECT sealed FROM tfstate_versions WHERE stack = 'app'").Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sealed, []byte(canary)) || bytes.Contains(sealed, []byte(`"serial"`)) {
			t.Fatal("the stored state is not sealed")
		}

		t.Run("a sealed state opens only for its own stack", func(t *testing.T) {
			if _, err := raw.ExecContext(context.Background(), "UPDATE tfstate_versions SET sealed = ? WHERE stack = 'db'", sealed); err != nil {
				t.Fatal(err)
			}
			f.want("GET", f.addr("db"), f.token("run-p", "db", control.StatePlan), "", http.StatusInternalServerError)
		})
		t.Run("another key opens nothing", func(t *testing.T) {
			g, err := tfstate.New(tfstate.Options{Store: f.store, Key: keys(map[string]*tfstate.Key{env: newKey(t)})})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(serve(g))
			defer srv.Close()
			tok, err := g.Issue(context.Background(), control.StateToken{RunID: "r", Env: env, Stack: "app", Scope: control.StatePlan, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			f.want("GET", srv.URL+"/v1/tfstate/app", tok, "", http.StatusInternalServerError)
		})
	})
}

func TestStackNameWithSlash(t *testing.T) {
	f := newFixture(t, controltest.SQLite, tfstate.Options{})
	nested := f.addr("net/vpc")
	if !strings.HasSuffix(nested, "/v1/tfstate/net%2Fvpc") {
		t.Fatalf("the address of net/vpc is %s", nested)
	}
	a := f.token("run-a", "net/vpc", control.StateApply)
	f.want("LOCK", nested+"/lock", a, lockJSON("l"), http.StatusOK)
	f.want("POST", nested+"?ID=l", a, stateJSON(1, "one", "x"), http.StatusOK)
	f.want("GET", nested, a, "", http.StatusOK)
	f.want("GET", f.addr("net"), f.token("run-n", "net", control.StatePlan), "", http.StatusNotFound)
	f.want("GET", nested, f.token("run-n2", "net", control.StatePlan), "", http.StatusForbidden)
}

func TestBadBodies(t *testing.T) {
	f := newFixture(t, controltest.SQLite, tfstate.Options{MaxState: 256})
	app := f.addr("app")
	a := f.token("run-a", "app", control.StateApply)
	f.want("LOCK", app+"/lock", a, `{"Who":"x"}`, http.StatusBadRequest)
	f.want("LOCK", app+"/lock", a, `not json`, http.StatusBadRequest)
	f.want("LOCK", app+"/lock", a, lockJSON("l"), http.StatusOK)
	for _, c := range []struct {
		name, body string
		status     int
	}{
		{"not JSON", "not json", http.StatusBadRequest},
		{"no serial", `{"lineage":"one"}`, http.StatusBadRequest},
		{"no lineage", `{"serial":1}`, http.StatusBadRequest},
		{"a negative serial", `{"serial":-1,"lineage":"one"}`, http.StatusBadRequest},
		{"too large", `{"serial":1,"lineage":"one","pad":"` + strings.Repeat("x", 256) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		if got, body := f.do("POST", app+"?ID=l", a, c.body); got != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, got, body, c.status)
		}
	}
	if got := f.versions("app"); len(got) != 0 {
		t.Errorf("refused writes stored versions %v", got)
	}
}

// endOnRead ends a run the first time the server reads the request body,
// which it does after the request's token was checked.
type endOnRead struct {
	end  func()
	body io.Reader
}

func (e *endOnRead) Read(p []byte) (int, error) {
	if e.end != nil {
		e.end()
		e.end = nil
	}
	return e.body.Read(p)
}

func TestRunEndedWhileReadingBody(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		for _, c := range []struct {
			name, method, path, body string
			locked                   bool
		}{
			{"a LOCK cannot take the lock after its run ended", "LOCK", "/v1/tfstate/app/lock", lockJSON("l"), false},
			{"a POST cannot write after its run ended", "POST", "/v1/tfstate/app?ID=l", stateJSON(1, "one", "x"), true},
			{"an UNLOCK cannot act after its run ended", "UNLOCK", "/v1/tfstate/app/lock", lockJSON("l"), true},
		} {
			t.Run(c.name, func(t *testing.T) {
				f := newFixture(t, e, tfstate.Options{})
				a := f.token("run-a", "app", control.StateApply)
				if c.locked {
					f.want("LOCK", f.addr("app")+"/lock", a, lockJSON("l"), http.StatusOK)
				}
				end := func() {
					if err := f.b.End(context.Background(), "run-a"); err != nil {
						t.Error(err)
					}
				}
				req := httptest.NewRequest(c.method, c.path, &endOnRead{end: end, body: strings.NewReader(c.body)})
				req.SetBasicAuth("writ", a)
				rec := httptest.NewRecorder()
				serve(f.b).ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("%s after End: %d %s, want %d", c.method, rec.Code, rec.Body, http.StatusUnauthorized)
				}
				if got := f.versions("app"); len(got) != 0 {
					t.Errorf("a write after End stored versions %v", got)
				}
				f.want("LOCK", f.addr("app")+"/lock", f.token("run-next", "app", control.StateApply), lockJSON("next"), http.StatusOK)
			})
		}
	})
}

func TestSealedStateIsBoundToItsVersion(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		f := newFixture(t, e, tfstate.Options{})
		app := f.addr("app")
		a := f.token("run-a", "app", control.StateApply)
		f.want("LOCK", app+"/lock", a, lockJSON("l"), http.StatusOK)
		f.want("POST", app+"?ID=l", a, stateJSON(1, "one", "x"), http.StatusOK)
		f.want("POST", app+"?ID=l", a, stateJSON(2, "one", "y"), http.StatusOK)
		raw := controltest.Raw(t, f.loc)
		if _, err := raw.ExecContext(context.Background(),
			"UPDATE tfstate_versions SET sealed = (SELECT sealed FROM tfstate_versions WHERE stack = 'app' AND version = 1) WHERE stack = 'app' AND version = 2"); err != nil {
			t.Fatal(err)
		}
		if got, body := f.do("GET", app, f.token("run-p", "app", control.StatePlan), ""); got != http.StatusInternalServerError {
			t.Errorf("GET of version 2 holding version 1's sealed bytes: %d %s, want %d", got, body, http.StatusInternalServerError)
		}
	})
}

func TestOpenKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", env+".key")
	first, err := tfstate.OpenKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != tfstate.KeySize || st.Mode().Perm() != 0o600 {
		t.Fatalf("the key file OpenKey made: %v, %v; want %d bytes of mode 0600", st, err, tfstate.KeySize)
	}
	f := newFixture(t, controltest.SQLite, tfstate.Options{Key: keys(map[string]*tfstate.Key{env: first})})
	a := f.token("run-a", "app", control.StateApply)
	f.want("LOCK", f.addr("app")+"/lock", a, lockJSON("l"), http.StatusOK)
	f.want("POST", f.addr("app")+"?ID=l", a, stateJSON(1, "one", "c"), http.StatusOK)

	again, err := tfstate.OpenKey(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := tfstate.New(tfstate.Options{Store: f.store, Key: keys(map[string]*tfstate.Key{env: again})})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(serve(g))
	defer srv.Close()
	tok, err := g.Issue(context.Background(), control.StateToken{RunID: "r", Env: env, Stack: "app", Scope: control.StatePlan, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.want("GET", srv.URL+"/v1/tfstate/app", tok, "", http.StatusOK); got != stateJSON(1, "one", "c") {
		t.Fatalf("the state under the key opened again: %s", got)
	}

	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tfstate.OpenKey(path); err == nil {
		t.Fatal("OpenKey accepted a damaged key file")
	}
	if b, _ := os.ReadFile(path); string(b) != "short" {
		t.Fatal("OpenKey replaced a damaged key file")
	}
}
