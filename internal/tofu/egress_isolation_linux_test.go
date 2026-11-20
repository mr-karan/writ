package tofu

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mr-karan/writ/internal/awsrole"
	"github.com/mr-karan/writ/internal/egress"
)

// The egress isolation test runs this package's test binary again as the
// control plane inside user, network, and pid namespaces, as
// TestProviderIsolation in internal/provider does, and from there a probe
// in tofu's place as a provider uid. These variables carry its settings.
const (
	egressChildEnv = "WRIT_TEST_EGRESS_CHILD"
	egressBinsEnv  = "WRIT_TEST_EGRESS_BINS"
)

// egressNamespace makes the namespaces the test runs in. Every run by the
// same user maps the same subordinate uids, so provider uid 61000 is the
// same host uid in each.
var egressNamespace = []string{"--user", "--map-root-user", "--map-users=auto", "--map-groups=auto", "--net", "--pid", "--fork", "--mount-proc"}

// egressPrelude readies the network namespace as root of the user
// namespace: the instance metadata address and three test servers'
// addresses on loopback, which stand for the network writ serve reaches,
// the last a private one,
// and docs/serve.md's firewall rule. writ then runs as root of the
// namespace, as the tofu gate runs it. A writ that is not root of its
// user namespace, with only the capabilities systemd's unit gives it,
// cannot start a view once it has made files as a run's uid: the change
// of its filesystem uid leaves it non-dumpable, so the uid_map of the
// view's new namespace is root's to write.
const egressPrelude = `set -e
ip link set lo up
ip addr add 169.254.169.254/32 dev lo
ip addr add 192.0.2.10/32 dev lo
ip addr add 192.0.2.11/32 dev lo
ip addr add 10.0.0.10/32 dev lo
nft -f "$1"
shift
exec "$@"
`

const egressMetadataRules = `table inet writ {
  chain out {
    type filter hook output priority 0;
    meta skuid 61000-61063 ip daddr 169.254.169.254 drop
    meta skuid 61000-61063 ip6 daddr fd00:ec2::254 drop
  }
}
`

// TestRunEgressIsolation proves on this machine what docs/serve.md says of
// a run under egress_mode enforce: as a provider uid, in its own network
// namespace, it reaches a listed server through writ's egress proxy, is
// refused an unlisted one, the host's loopback, and the metadata address,
// reaches none of them around the proxy, and still gets the role's
// credentials from the broker with its token. A provider install, which
// runs as writ, where no firewall rule of the provider uids holds, gets a
// namespace of its own too, whose proxy lets it reach a public address
// and refuses a private one, the host's loopback, and the metadata
// address. Under egress_mode off the same run reaches the host's network
// as before, and the firewall rule still keeps it from the metadata
// address, which an install on writ's own network reaches. It needs
// unshare with subordinate uids, ip, and nft (`just test-serve` provides
// nft), and skips without them.
func TestRunEgressIsolation(t *testing.T) {
	if os.Getenv(egressChildEnv) != "" {
		egressChild(t)
		return
	}
	for _, bin := range []string{"unshare", "ip", "nft"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s: run just test-serve", bin)
		}
	}
	if out, err := exec.Command("unshare", append(slices.Clone(egressNamespace), "true")...).CombinedOutput(); err != nil {
		t.Skipf("needs a user namespace with subordinate uids: %v %s", err, out)
	}
	// The provider uid is a subordinate uid on the host, so the binary
	// goes where any user may run it.
	bins, err := os.MkdirTemp("/tmp", "writ-egress-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(bins) })
	if err := os.Chmod(bins, 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	test := filepath.Join(bins, "tofu.test")
	if err := os.WriteFile(test, b, 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake's name picks its behaviour. The scratch root is writable
	// by every user with the sticky bit, as tofu.scratch_dir allows.
	if err := os.Symlink(test, filepath.Join(bins, fakePrefix+"net")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"cache", "scratch"} {
		if err := os.Mkdir(filepath.Join(bins, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(bins, "scratch"), os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(bins, "rules.nft")
	if err := os.WriteFile(rules, []byte(egressMetadataRules), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(egressNamespace),
		"sh", "-c", egressPrelude, "sh", rules, test, "-test.run=^TestRunEgressIsolation$", "-test.v", "-test.count=1")...)
	cmd.Env = append(os.Environ(), egressChildEnv+"=1", egressBinsEnv+"="+bins, "TMPDIR=/tmp")
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespace:\n%s", out)
	switch {
	case strings.Contains(string(out), "--- SKIP: TestRunEgressIsolation"):
		t.Skip("the test skipped in the namespace")
	case err != nil || !strings.Contains(string(out), "--- PASS: TestRunEgressIsolation"):
		t.Fatalf("the test failed in the namespace: %v", err)
	}
}

// staticAssumer answers every role with the same credentials, as STS
// would answer the plan role.
type staticAssumer struct{}

func (staticAssumer) Assume(context.Context, awsrole.Session) (awsrole.Credentials, error) {
	return awsrole.Credentials{AccessKeyID: "ASIATEST000000000001", SecretAccessKey: "egress-test-secret", SessionToken: "egress-test-token", Expires: time.Now().Add(time.Hour)}, nil
}

// egressChild is the test proper, as root of the namespace the parent
// made.
func egressChild(t *testing.T) {
	if os.Getuid() != 0 {
		t.Fatalf("the child runs as uid %d, not root of its namespace", os.Getuid())
	}
	ctx := context.Background()
	bins := os.Getenv(egressBinsEnv)
	serve := func(addr, body string) string {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) }))
		}()
		return ln.Addr().String()
	}
	serve("192.0.2.10:8080", "listed")
	serve("192.0.2.11:8080", "unlisted")
	serve("10.0.0.10:8080", "private")
	serve("169.254.169.254:80", "metadata")
	// A service on the host's loopback, as writ serve's own API or a
	// Postgres listening there.
	hostLoop := serve("127.0.0.1:0", "host loopback")

	broker, err := awsrole.NewBroker(awsrole.BrokerOptions{Assumer: staticAssumer{}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	session, err := broker.Start(ctx, awsrole.Session{Role: "arn:aws:iam::000000000000:role/writ-plan", Name: "writ-ci-EGRESSTEST", SourceIdentity: "gitlab-1-1", Duration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer session.End()
	list, err := egress.ParseList([]string{"192.0.2.10:8080"})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := egress.New(egress.Options{Mode: egress.Enforce, List: list})
	if err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(bins, fakePrefix+"net")
	cache, scratch := filepath.Join(bins, "cache"), filepath.Join(bins, "scratch")
	// New checks the owner of the scratch root and of every directory
	// above it, and the user namespace shows /tmp as nobody's, so the
	// test opens the root itself.
	root, err := os.OpenRoot(scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	r := &Runner{opts: Options{Binary: bin, PluginCache: cache, ScratchRoot: scratch, Timeout: 2 * time.Minute, ApplyTimeout: 2 * time.Minute, MaxOutput: 1 << 20}, root: root}
	user := &User{UID: 61000, GID: 61000}
	plan := func(n *Net, env, creds []string) string {
		t.Helper()
		p, err := r.Plan(ctx, Stack{Files: fstest.MapFS{"main.tf": {Data: []byte("")}}, Dir: ".", Env: env, Creds: creds, User: user, Net: n})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		return string(p.Log)
	}
	// fill runs Fill with a probe in tofu's place, whose init fails so
	// that Fill returns what the probe saw.
	fill := func(n *Net, env []string) string {
		t.Helper()
		lock := []byte("provider \"registry.opentofu.org/hashicorp/random\" {\n  version = \"3.7.2\"\n  hashes = [\"h1:probe\"]\n}\n")
		_, err := r.Fill(ctx, lock, n, append(env, "PROBE_INIT_FAIL=1"))
		se, ok := errors.AsType[*StepError](err)
		if !ok {
			t.Fatalf("fill: %v", err)
		}
		return string(se.Stdout)
	}
	expect := func(t *testing.T, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w+"\n") {
				t.Errorf("the probe reported\n%s\nwithout %q", got, w)
			}
		}
	}

	t.Run("enforce", func(t *testing.T) {
		const proxyURL = "http://127.0.0.1:3128"
		env := []string{
			"HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "NO_PROXY=127.0.0.1,localhost,::1",
			"PROBE_GET=http://192.0.2.10:8080/,http://192.0.2.11:8080/",
			"PROBE_CONNECT=192.0.2.11:8080," + hostLoop + ",169.254.169.254:80,localhost:" + strings.Split(hostLoop, ":")[1],
			"PROBE_CREDS=1",
			"PROBE_DIAL=192.0.2.11:8080," + hostLoop + ",169.254.169.254:80",
		}
		n := &Net{Ports: map[int]http.Handler{3128: proxy, 3129: broker.Handler()}}
		got := plan(n, env, session.EnvAt("http://127.0.0.1:3129"+awsrole.CredentialsPath))
		port := strings.Split(hostLoop, ":")[1]
		expect(t, got,
			"interfaces: lo <nil>",
			"get http://192.0.2.10:8080/: 200 listed",
			"get http://192.0.2.11:8080/: 403 writ egress refused 192.0.2.11:8080: an IP address the list does not have",
			"connect 192.0.2.11:8080: 403 writ egress refused 192.0.2.11:8080: an IP address the list does not have",
			"connect "+hostLoop+": 403 writ egress refused "+hostLoop+": a loopback address",
			"connect 169.254.169.254:80: 403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is",
			"connect localhost:"+port+": 403 writ egress refused localhost:"+port+": not in the list",
			"creds: 200 ASIATEST000000000001",
			"dial 192.0.2.11:8080: connect: network is unreachable",
			"dial "+hostLoop+": connect: connection refused",
			"dial 169.254.169.254:80: connect: network is unreachable",
		)
		dests, _ := proxy.Destinations()
		t.Logf("the proxy recorded %+v", dests)
	})

	t.Run("install", func(t *testing.T) {
		install, err := egress.New(egress.Options{Mode: egress.Enforce, Install: true})
		if err != nil {
			t.Fatal(err)
		}
		const proxyURL = "http://127.0.0.1:3128"
		got := fill(&Net{Ports: map[int]http.Handler{3128: install}}, []string{
			"HTTP_PROXY=" + proxyURL, "HTTPS_PROXY=" + proxyURL, "NO_PROXY=127.0.0.1,localhost,::1",
			"PROBE_CONNECT=192.0.2.11:8080,10.0.0.10:8080," + hostLoop + ",169.254.169.254:80",
			"PROBE_DIAL=192.0.2.11:8080," + hostLoop + ",169.254.169.254:80",
		})
		expect(t, got,
			"interfaces: lo <nil>",
			"connect 192.0.2.11:8080: 200 Connection established",
			"connect 10.0.0.10:8080: 403 writ egress refused 10.0.0.10:8080: a private address, which a provider install never connects to",
			"connect "+hostLoop+": 403 writ egress refused "+hostLoop+": a loopback address",
			"connect 169.254.169.254:80: 403 writ egress refused 169.254.169.254:80: a link-local address, where the instance metadata service is",
			"dial 192.0.2.11:8080: connect: network is unreachable",
			"dial "+hostLoop+": connect: connection refused",
			"dial 169.254.169.254:80: connect: network is unreachable",
		)
		dests, _ := install.Destinations()
		t.Logf("the install's proxy recorded %+v", dests)
	})

	t.Run("off", func(t *testing.T) {
		env := []string{"PROBE_DIAL=192.0.2.11:8080," + hostLoop + ",169.254.169.254:80", "PROBE_CREDS=1"}
		got := plan(nil, env, session.Env())
		expect(t, got,
			"dial 192.0.2.11:8080: ok",
			"dial "+hostLoop+": ok",
			"dial 169.254.169.254:80: i/o timeout",
			"creds: 200 ASIATEST000000000001",
		)
		// writ itself reaches the metadata address, so the rule, not the
		// namespace, kept the provider uid from it, and an install on
		// writ's own network reaches it.
		c, err := net.DialTimeout("tcp", "169.254.169.254:80", 2*time.Second)
		if err != nil {
			t.Fatalf("writ cannot reach the metadata address either: %v", err)
		}
		c.Close()
		expect(t, fill(nil, []string{"PROBE_DIAL=10.0.0.10:8080," + hostLoop + ",169.254.169.254:80"}),
			"dial 10.0.0.10:8080: ok",
			"dial "+hostLoop+": ok",
			"dial 169.254.169.254:80: ok",
		)
	})
}
