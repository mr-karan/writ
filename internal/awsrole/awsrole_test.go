package awsrole_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/awsrole/ststest"
)

// controlPlane gives the test process an AWS identity that reads nothing
// of the user's: keys in the environment, no shared files, no instance
// metadata.
func controlPlane(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIACONTROLPLANE0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "control-plane-secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
}

// TestAssumeRoleRequest sends a real AssumeRole through the AWS SDK to the
// stand-in and checks each field of the request.
func TestAssumeRoleRequest(t *testing.T) {
	controlPlane(t)
	st := ststest.New()
	defer st.Close()
	c, err := awsrole.NewSTS(context.Background(), awsrole.STSOptions{Region: "eu-west-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	sess := awsrole.Session{
		Role:           "arn:aws:iam::222222222222:role/writ-plan",
		Name:           awsrole.SessionName("ci", "57EOKCIGJRZSC67UG4RY5UBKOK"),
		SourceIdentity: awsrole.SourceIdentity("gitlab-4242-1849213"),
		Duration:       time.Hour,
	}
	got, err := c.Assume(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	reqs := st.Requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	want := map[string]string{
		"Action": "AssumeRole", "Version": "2011-06-15", "RoleArn": sess.Role,
		"RoleSessionName": "writ-ci-57EOKCIGJRZSC67UG4RY5UBKOK", "SourceIdentity": "gitlab-4242-1849213", "DurationSeconds": "3600",
	}
	for k, v := range want {
		if r.Form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, r.Form.Get(k), v)
		}
	}
	if len(r.Form) != len(want) {
		t.Errorf("the request has fields %v, want only %v", r.Form, want)
	}
	if r.AccessKeyID != "AKIACONTROLPLANE0001" || r.Region != "eu-west-1" || r.Service != "sts" {
		t.Errorf("signed by %s for %s/%s, want the control plane's key for eu-west-1/sts", r.AccessKeyID, r.Region, r.Service)
	}
	issued, ok := st.Issued(got.AccessKeyID)
	if !ok || issued.SessionToken != got.SessionToken || issued.SecretAccessKey != got.SecretAccessKey || !issued.Expires.Equal(got.Expires) {
		t.Errorf("Assume returned %+v, STS issued %+v", got, issued)
	}

	st.Deny("arn:aws:iam::222222222222:role/writ-apply")
	sess.Role = "arn:aws:iam::222222222222:role/writ-apply"
	_, err = c.Assume(context.Background(), sess)
	var denied *awsrole.DeniedError
	if !errors.As(err, &denied) || denied.Role != sess.Role || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("a refused role: %v", err)
	}
}

func TestSessionName(t *testing.T) {
	tests := []struct{ name, run, want string }{
		{"ci", "57EOKCIGJRZSC67UG4RY5UBKOK", "writ-ci-57EOKCIGJRZSC67UG4RY5UBKOK"},
		{"alice", "RUN1", "writ-alice-RUN1"},
		{"Ana María Pérez", "RUN1", "writ-Ana-Mar-a-P-rez-RUN1"},
		{strings.Repeat("a", 80), "57EOKCIGJRZSC67UG4RY5UBKOK", "writ-" + strings.Repeat("a", 32) + "-57EOKCIGJRZSC67UG4RY5UBKOK"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := awsrole.SessionName(tt.name, tt.run)
			if got != tt.want || len(got) > 64 {
				t.Fatalf("SessionName(%q, %q) = %q (%d), want %q", tt.name, tt.run, got, len(got), tt.want)
			}
		})
	}
	if got := awsrole.SourceIdentity("gitlab.com/user/1001 " + strings.Repeat("x", 80)); len(got) != 64 || !strings.HasPrefix(got, "gitlab.com-user-1001-x") {
		t.Fatalf("SourceIdentity = %q", got)
	}
}

func TestCheckRoleARN(t *testing.T) {
	for _, ok := range []string{"arn:aws:iam::111111111111:role/writ-plan", "arn:aws-us-gov:iam::111111111111:role/path/writ-plan"} {
		if err := awsrole.CheckRoleARN(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "writ-plan", "arn:aws:iam::1111:role/x", "arn:aws:iam::111111111111:user/x", "arn:aws:sts::111111111111:role/x"} {
		if err := awsrole.CheckRoleARN(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

// fakeAssumer issues numbered credentials that expire after life, on the
// test's clock.
type fakeAssumer struct {
	mu    sync.Mutex
	now   func() time.Time
	life  time.Duration
	calls []awsrole.Session
	fail  bool
}

func (f *fakeAssumer) Assume(_ context.Context, s awsrole.Session) (awsrole.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return awsrole.Credentials{}, &awsrole.DeniedError{Role: s.Role, Err: errors.New("AccessDenied")}
	}
	f.calls = append(f.calls, s)
	n := len(f.calls)
	id := string(rune('0' + n))
	return awsrole.Credentials{AccessKeyID: "ASIA" + id, SecretAccessKey: "secret" + id, SessionToken: "token" + id, Expires: f.now().Add(f.life)}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fetch asks the endpoint as an AWS SDK does.
func fetch(t *testing.T, url, token string) (int, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, doc
}

func TestBroker(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	fa := &fakeAssumer{now: clk.now, life: time.Hour}
	b, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: fa, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sess := awsrole.Session{Role: "arn:aws:iam::222222222222:role/writ-plan", Name: "writ-ci-RUN1", SourceIdentity: "gitlab-4242-1", Duration: time.Hour}
	run, err := b.Start(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range run.Env() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	url, token := env["AWS_CONTAINER_CREDENTIALS_FULL_URI"], env["AWS_CONTAINER_AUTHORIZATION_TOKEN"]
	if !strings.HasPrefix(url, "http://127.0.0.1:") || len(token) < 40 || len(env) != 2 {
		t.Fatalf("Env() = %v", run.Env())
	}
	if len(fa.calls) != 1 || fa.calls[0] != sess {
		t.Fatalf("Start assumed %v, want %v once", fa.calls, sess)
	}

	t.Run("the run's token gets the run's credentials", func(t *testing.T) {
		status, doc := fetch(t, url, token)
		if status != 200 || doc["AccessKeyId"] != "ASIA1" || doc["SecretAccessKey"] != "secret1" || doc["Token"] != "token1" || doc["Expiration"] != "2026-11-10T13:00:00Z" || doc["AccountId"] != "222222222222" {
			t.Fatalf("%d %v", status, doc)
		}
	})
	t.Run("no token, or another, is refused", func(t *testing.T) {
		for _, tok := range []string{"", "Bearer " + token, token + "x"} {
			if status, doc := fetch(t, url, tok); status != 401 || doc["code"] != "AccessDenied" {
				t.Fatalf("token %q: %d %v", tok, status, doc)
			}
		}
	})
	t.Run("the role is assumed again before the credentials expire", func(t *testing.T) {
		clk.add(44 * time.Minute)
		if _, doc := fetch(t, url, token); doc["AccessKeyId"] != "ASIA1" {
			t.Fatalf("16 minutes before expiry: %v, want the first credentials", doc)
		}
		clk.add(2 * time.Minute)
		if _, doc := fetch(t, url, token); doc["AccessKeyId"] != "ASIA2" || doc["Expiration"] != "2026-11-10T13:46:00Z" {
			t.Fatalf("14 minutes before expiry: %v, want new credentials", doc)
		}
		if len(fa.calls) != 2 || fa.calls[1] != sess {
			t.Fatalf("the refresh assumed %v", fa.calls)
		}
		if got := run.Secrets(); !slices.Equal(got, []string{"secret1", "token1", "secret2", "token2"}) {
			t.Fatalf("Secrets() after a refresh = %q, want both credentials' secrets", got)
		}
	})
	t.Run("a failed refresh serves the old credentials while they last", func(t *testing.T) {
		fa.mu.Lock()
		fa.fail = true
		fa.mu.Unlock()
		clk.add(50 * time.Minute)
		if status, doc := fetch(t, url, token); status != 200 || doc["AccessKeyId"] != "ASIA2" {
			t.Fatalf("10 minutes before expiry: %d %v", status, doc)
		}
		clk.add(10 * time.Minute)
		if status, doc := fetch(t, url, token); status != 503 || doc["code"] != "AssumeRoleFailed" {
			t.Fatalf("at expiry: %d %v", status, doc)
		}
		fa.mu.Lock()
		fa.fail = false
		fa.mu.Unlock()
	})
	t.Run("two runs, two tokens", func(t *testing.T) {
		other, err := b.Start(context.Background(), awsrole.Session{Role: "arn:aws:iam::111111111111:role/writ-plan", Name: "writ-ci-RUN2", Duration: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer other.End()
		otherToken := strings.TrimPrefix(other.Env()[1], "AWS_CONTAINER_AUTHORIZATION_TOKEN=")
		_, a := fetch(t, url, token)
		_, o := fetch(t, url, otherToken)
		if a["AccessKeyId"] == o["AccessKeyId"] || o["AccountId"] != "111111111111" {
			t.Fatalf("two runs got %v and %v", a, o)
		}
	})
	t.Run("the token dies with the run", func(t *testing.T) {
		run.End()
		if status, doc := fetch(t, url, token); status != 401 || doc["code"] != "AccessDenied" {
			t.Fatalf("after End: %d %v", status, doc)
		}
	})
	t.Run("a role STS refuses fails at the start", func(t *testing.T) {
		fa.mu.Lock()
		fa.fail = true
		fa.mu.Unlock()
		if _, err := b.Start(context.Background(), sess); err == nil {
			t.Fatal("Start with a refused role succeeded")
		}
	})
	t.Run("only GET", func(t *testing.T) {
		resp, err := http.Post(url, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Fatalf("POST: %d", resp.StatusCode)
		}
	})
}

// The broker tells Assumed of every session it assumes, before it serves
// the credentials, so the control database records which sessions are
// Writ's own. A run whose session cannot be recorded does not start.
func TestBrokerRecordsSessions(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	fa := &fakeAssumer{now: clk.now, life: time.Hour}
	type record struct {
		name            string
		issued, expires time.Time
		sourceIdentity  string
	}
	var mu sync.Mutex
	var got []record
	var refuse bool
	b, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: fa, Now: clk.now,
		Assumed: func(_ context.Context, s awsrole.AssumedSession) error {
			mu.Lock()
			defer mu.Unlock()
			if refuse {
				return errors.New("the control database is down")
			}
			got = append(got, record{s.Name, s.From, s.Until, s.SourceIdentity})
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sess := awsrole.Session{Role: "arn:aws:iam::222222222222:role/writ-plan", Name: "writ-ci-RUN1", SourceIdentity: "gitlab-4242-1", Duration: time.Hour}
	run, err := b.Start(context.Background(), sess)
	if err != nil {
		t.Fatal(err)
	}
	defer run.End()
	start := clk.now()
	clk.add(46 * time.Minute)
	token := strings.TrimPrefix(run.Env()[1], "AWS_CONTAINER_AUTHORIZATION_TOKEN=")
	if status, _ := fetch(t, b.URL(), token); status != 200 {
		t.Fatalf("refresh: %d", status)
	}
	want := []record{
		{"writ-ci-RUN1", start, start.Add(time.Hour), "gitlab-4242-1"},
		{"writ-ci-RUN1", start.Add(46 * time.Minute), start.Add(106 * time.Minute), "gitlab-4242-1"},
	}
	mu.Lock()
	if !slices.Equal(got, want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
	refuse = true
	mu.Unlock()
	if _, err := b.Start(context.Background(), awsrole.Session{Role: sess.Role, Name: "writ-ci-RUN2", Duration: time.Hour}); err == nil {
		t.Fatal("a run whose session was not recorded started")
	}
}
