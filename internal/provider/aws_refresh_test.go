package provider

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/awsrole/ststest"
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/tfplugin5"
	"github.com/mr-karan/writ/internal/value"
)

const refreshChildEnv = "WRIT_TEST_REFRESH_CHILD"

// TestAWSProviderRefreshesCredentials runs the real hashicorp/aws 6.70.0
// with credentials from writ serve's endpoint that last five seconds. One
// provider process calls STS, waits until the credentials have expired,
// and calls STS again. The second call must carry credentials the endpoint
// assumed anew, which is what lets an apply outlive its first credentials.
// It runs in a network namespace with only loopback, so nothing reaches
// AWS, and skips without unshare or without the provider in the cache.
func TestAWSProviderRefreshesCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the real hashicorp/aws provider")
	}
	dirs, err := DefaultDirs()
	if err != nil {
		t.Skip(err)
	}
	src := schema.Source{Host: "registry.opentofu.org", Namespace: "hashicorp", Type: "aws"}
	if _, err := os.Stat(dirs.BinaryPath(src, "6.70.0", CurrentPlatform())); err != nil {
		t.Skipf("hashicorp/aws 6.70.0 is not in the provider cache: %v", err)
	}
	if os.Getenv(refreshChildEnv) == "" {
		if err := exec.Command("unshare", "-Urn", "true").Run(); err != nil {
			t.Skipf("unshare -Urn is unavailable: %v", err)
		}
		cmd := exec.Command("unshare", "-Urn", "sh", "-c", `ip link set lo up && exec "$@"`, "sh",
			os.Args[0], "-test.run=^TestAWSProviderRefreshesCredentials$", "-test.v", "-test.count=1")
		cmd.Env = append(os.Environ(), refreshChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("in the namespace:\n%s", out)
		if err != nil || !strings.Contains(string(out), "--- PASS: TestAWSProviderRefreshesCredentials") {
			t.Fatalf("the test failed in the namespace: %v", err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lock, err := Install(ctx, InstallOptions{Dirs: dirs, Offline: true}, nil, []Requirement{{Name: "aws", Source: src, Version: "6.70.0"}})
	if err != nil {
		t.Fatal(err)
	}
	st := ststest.New()
	defer st.Close()
	st.SetLifetime(5 * time.Second)
	// The control plane's own STS client: the SDK with keys in the
	// environment, as on a host with keys rather than an instance role.
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIACONTROLPLANE0001")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "control-plane-secret")
	t.Setenv("AWS_CONFIG_FILE", "/dev/null")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", "/dev/null")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	assumer, err := awsrole.NewSTS(ctx, awsrole.STSOptions{Region: "us-east-1", Endpoint: st.URL})
	if err != nil {
		t.Fatal(err)
	}
	b, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: assumer, RefreshBefore: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const session = "writ-ci-REFRESHTEST"
	run, err := b.Start(ctx, awsrole.Session{Role: "arn:aws:iam::222222222222:role/writ-plan", Name: session, SourceIdentity: "gitlab-4242-1", Duration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer run.End()

	cfg := value.ObjectVal(map[string]value.Value{
		"region":    str("us-east-1"),
		"endpoints": list(obj(map[string]value.Value{"sts": str(st.URL)})),
	})
	pool := NewPool(PoolOptions{
		Dirs: dirs, Lock: lock,
		Configs: []Config{{Source: src, Version: "6.70.0", Value: cfg}},
		Launch:  Launch{OwnEnv: true, Env: append([]string{"AWS_EC2_METADATA_DISABLED=true"}, run.Env()...)},
	})
	defer closePool(t, pool)
	// Configuring the provider checks its credentials with
	// GetCallerIdentity.
	c, err := pool.Client(ctx, ConfigKey{Source: src, Version: "6.70.0"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	got := readCallerIdentity(ctx, t, dirs, lock, src, c)
	if arn := got.Get("arn"); !value.Equal(arn, str("arn:aws:sts::222222222222:assumed-role/writ-plan/"+session)) {
		t.Errorf("aws_caller_identity arn = %s", arn)
	}

	var keys []string
	for _, r := range st.Requests() {
		if r.Action != "GetCallerIdentity" {
			continue
		}
		issued, ok := st.Issued(r.AccessKeyID)
		if !ok || issued.SessionName != session || issued.SessionToken != r.SecurityToken {
			t.Fatalf("the provider signed with %s, which STS did not issue to the run", r.AccessKeyID)
		}
		if len(keys) == 0 || keys[len(keys)-1] != r.AccessKeyID {
			keys = append(keys, r.AccessKeyID)
		}
	}
	t.Logf("the provider's GetCallerIdentity calls were signed by %v", keys)
	if len(keys) < 2 {
		t.Fatalf("the provider used one set of credentials, %v, past its expiry", keys)
	}
}

// readCallerIdentity reads the aws_caller_identity data source over protocol
// 5, which makes the provider call STS GetCallerIdentity with the
// credentials it holds now. Writ itself reads no data source: OpenTofu does.
func readCallerIdentity(ctx context.Context, t *testing.T, dirs Dirs, lock *Lock, src schema.Source, c *Client) value.Value {
	t.Helper()
	const typ = "aws_caller_identity"
	entry, ok := lock.Find(src, "6.70.0")
	if !ok {
		t.Fatalf("no lock entry for %s", src)
	}
	cache, err := openSchemaCache(dirs, entry.Binary)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.dataSource(typ)
	if err != nil {
		t.Fatal(err)
	}
	block, ty, err := convertBlock(raw.Block)
	if err != nil {
		t.Fatal(err)
	}
	w := wireBlock{block: block, ty: ty}
	config, err := w.encode(obj(nil))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tfplugin5.NewProviderClient(c.proc.conn).ReadDataSource(ctx, &tfplugin5.ReadDataSource_Request{TypeName: typ, Config: &tfplugin5.DynamicValue{Msgpack: config}})
	if err != nil {
		t.Fatal(err)
	}
	if diags := c.diags(w, v5Diags(resp.GetDiagnostics())); hasError(diags) {
		t.Fatalf("ReadDataSource %s: %+v", typ, diags)
	}
	state, err := w.decode(v5Dynamic(resp.GetState()))
	if err != nil {
		t.Fatal(err)
	}
	return state
}
