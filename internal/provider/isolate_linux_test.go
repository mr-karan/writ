package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// The isolation test runs this package's test binary twice more: once as
// the control plane inside a user and network namespace, and from there as
// a provider under another uid. These variables carry its settings.
const (
	isolationChildEnv = "WRIT_TEST_ISOLATION_CHILD"
	probeEnv          = "WRIT_TEST_PROBE"
	testBinsEnv       = "WRIT_TEST_BINS"
)

// metadataRules is the firewall rule that docs/serve.md asks for, with the
// uid range the test uses.
const metadataRules = `table inet writ {
  chain out {
    type filter hook output priority 0;
    meta skuid 61000-61063 ip daddr 169.254.169.254 drop
    meta skuid 61000-61063 ip6 daddr fd00:ec2::254 drop
  }
}
`

// namespaceArgs makes the namespaces the isolation test runs in. Every
// run by the same user maps the same subordinate uids, so provider uid
// 61000 is the same host uid in each concurrent run. A pid namespace with
// its own /proc keeps one run from seeing, and failing to reap, another
// run's providers.
var namespaceArgs = []string{"--user", "--map-root-user", "--map-users=auto", "--map-groups=auto", "--net", "--pid", "--fork", "--mount-proc"}

// isolationPrelude readies the network namespace as root of the user
// namespace: the metadata addresses on loopback, low ports for anyone, and
// the firewall rule. Then it becomes uid 1000 with only the capabilities
// systemd's unit gives writ serve, ambient as AmbientCapabilities makes
// them, under no_new_privs as NoNewPrivileges sets.
const isolationPrelude = `set -e
ip link set lo up
ip addr add 169.254.169.254/32 dev lo
ip -6 addr add fd00:ec2::254/128 dev lo
echo 0 > /proc/sys/net/ipv4/ip_unprivileged_port_start
nft -f "$1"
shift
exec setpriv --reuid=1000 --regid=1000 --clear-groups --inh-caps=+setuid,+setgid,+kill --ambient-caps=+setuid,+setgid,+kill --no-new-privs "$@"
`

func init() {
	switch os.Getenv(probeEnv) {
	case "":
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	default:
		probe()
	}
}

// probeReport is what a provider could reach.
type probeReport struct {
	UID, GID int
	Groups   []int
	Env      []string
	Key      string
	Environ  string
	IMDS4    string
	IMDS6    string
}

// probe runs as a provider: it tries what a hostile provider would try,
// reports it on stderr, and then becomes hashicorp/random, which answers
// the handshake.
func probe() {
	r := probeReport{UID: os.Getuid(), GID: os.Getgid(), Env: os.Environ()}
	r.Groups, _ = os.Getgroups()
	r.Key = tryRead(os.Getenv("PROBE_KEY"))
	r.Environ = tryRead("/proc/" + os.Getenv("PROBE_WRIT_PID") + "/environ")
	r.IMDS4 = tryGet("169.254.169.254:80")
	r.IMDS6 = tryGet("[fd00:ec2::254]:80")
	b, _ := json.Marshal(r)
	fmt.Fprintf(os.Stderr, "probe %s\n", b)
	bin := os.Getenv("PROBE_EXEC")
	err := syscall.Exec(bin, []string{bin}, os.Environ())
	fmt.Fprintf(os.Stderr, "exec %s: %v\n", bin, err)
	os.Exit(1)
}

func tryRead(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return "read " + strconv.Itoa(len(b)) + " bytes"
}

func tryGet(addr string) string {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/latest/meta-data/iam/security-credentials/")
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return "reached: " + string(b)
}

// TestProviderIsolation proves on this machine what docs/serve.md says of
// providers that run as their own users: the user switch works for a
// process like writ serve under systemd, the handshake works across the
// two users, and the provider cannot read the state key, read writ's
// environment, or reach the instance metadata addresses past the firewall
// rule, while writ can. It needs unshare with subordinate uids, setpriv,
// ip, and nft (`just test-serve` provides nft), and skips without them.
func TestProviderIsolation(t *testing.T) {
	if os.Getenv(isolationChildEnv) != "" {
		isolationChild(t)
		return
	}
	for _, bin := range []string{"unshare", "setpriv", "ip", "nft"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("needs %s: run just test-serve", bin)
		}
	}
	if out, err := exec.Command("unshare", append(slices.Clone(namespaceArgs), "true")...).CombinedOutput(); err != nil {
		t.Skipf("needs a user namespace with subordinate uids: %v %s", err, out)
	}
	dirs, lock := installed(t)
	e, ok := lock.Find(randomSrc, "3.9.1")
	if !ok {
		t.Fatal("random 3.9.1 is not installed")
	}
	// Uid 1000 in the namespace is a subordinate uid on the host, so it
	// can read nothing of this user's. The binaries go where any user may
	// run them, as tofu.plugin_cache must be for writ serve.
	bins, err := os.MkdirTemp("/tmp", "writ-bins-")
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
	copyExec(t, self, filepath.Join(bins, "probe"))
	copyExec(t, dirs.BinaryPath(randomSrc, "3.9.1", e.Platform), filepath.Join(bins, "random"))
	rules := filepath.Join(bins, "rules.nft")
	if err := os.WriteFile(rules, []byte(metadataRules), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", append(slices.Clone(namespaceArgs),
		"sh", "-c", isolationPrelude, "sh", rules,
		filepath.Join(bins, "probe"), "-test.run=^TestProviderIsolation$", "-test.v", "-test.count=1")...)
	cmd.Env = append(os.Environ(), isolationChildEnv+"=1", testBinsEnv+"="+bins, "TMPDIR=/tmp")
	out, err := cmd.CombinedOutput()
	t.Logf("in the namespace:\n%s", out)
	switch {
	case strings.Contains(string(out), "--- SKIP: TestProviderIsolation"):
		t.Skip("the test skipped in the namespace")
	case err != nil || !strings.Contains(string(out), "--- PASS: TestProviderIsolation"):
		t.Fatalf("the test failed in the namespace: %v", err)
	}
}

// isolationChild is the test proper, as uid 1000 with writ serve's
// capabilities in the namespace the parent made.
func isolationChild(t *testing.T) {
	if os.Getuid() != 1000 {
		t.Fatalf("the child runs as uid %d, not 1000", os.Getuid())
	}
	user := User{UID: 61000, GID: 61000}

	// The fake instance metadata service answers writ, as the real one
	// answers the host.
	for _, addr := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80"} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "writ-host-role")
		})}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}

	// writ's own files: a key only writ may read, and a password in writ's
	// environment, as a database URL with a password would be.
	keys := filepath.Join(t.TempDir(), "keys")
	if err := os.Mkdir(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(keys, "prod.key")
	if err := os.WriteFile(key, []byte("state-key-material"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Setenv("PGPASSWORD", "writ-database-password")

	bins := os.Getenv(testBinsEnv)
	probeBin, randomBin := filepath.Join(bins, "probe"), filepath.Join(bins, "random")

	if got := tryRead(key); !strings.HasPrefix(got, "read ") {
		t.Fatalf("writ cannot read its own key: %s", got)
	}
	for _, addr := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80"} {
		if got := tryGet(addr); got != "reached: writ-host-role" {
			t.Fatalf("writ cannot reach %s, so the test proves nothing: %s", addr, got)
		}
	}

	runEnv := []string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:1/creds",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN=run-token",
		"PROBE_KEY=" + key,
		"PROBE_WRIT_PID=" + strconv.Itoa(os.Getpid()),
		"PROBE_EXEC=" + randomBin,
		probeEnv + "=1",
	}
	launch := Launch{OwnEnv: true, Env: runEnv, User: &user}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	t.Run("a capability writ holds would reach the provider", func(t *testing.T) {
		_, err := start(ctx, probeBin, launch)
		if err == nil || !strings.Contains(err.Error(), "started with capabilities") {
			t.Fatalf("start before PrepareIsolation: %v, want a refusal for capabilities", err)
		}
		requireNoProcesses(t, user.UID)
	})

	if err := PrepareIsolation(); errors.Is(err, syscall.ENOTSUP) {
		t.Skip("PrepareIsolation needs a binary without cgo; the race detector uses cgo")
	} else if err != nil {
		t.Fatal(err)
	}
	if _, inh, amb, err := selfCaps(); err != nil || inh != 0 || amb != 0 {
		t.Fatalf("after PrepareIsolation: inheritable %x, ambient %x, %v", inh, amb, err)
	}

	t.Run("the provider runs apart from writ", func(t *testing.T) {
		p, err := start(ctx, probeBin, launch)
		if err != nil {
			t.Fatal(err)
		}
		dir := p.socketDir
		if st, err := os.Stat(dir); err != nil || st.Sys().(*syscall.Stat_t).Uid != user.UID || st.Mode().Perm() != 0o700 {
			t.Fatalf("the provider's directory: %v %v", st, err)
		}
		// A gRPC call over the socket that belongs to the provider's user.
		sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
		defer scancel()
		if _, err := p.client().getSchema(sctx); err != nil {
			t.Fatalf("GetProviderSchema across users: %v", err)
		}
		if err := p.shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the provider's directory is left: %v", err)
		}
		r := probeOf(t, p.stderr.String())
		if r.UID != int(user.UID) || r.GID != int(user.GID) || len(r.Groups) != 0 {
			t.Errorf("the provider ran as uid %d gid %d groups %v", r.UID, r.GID, r.Groups)
		}
		want := append(slices.Clone(runEnv), "HOME="+dir, "TMPDIR="+dir, "TF_PLUGIN_MAGIC_COOKIE="+magicCookie,
			"PLUGIN_PROTOCOL_VERSIONS=5,6", "PLUGIN_UNIX_SOCKET_DIR="+dir, "TF_LOG_SDK=off", "TF_LOG_SDK_PROTO=off", "TF_LOG_SDK_FRAMEWORK=off")
		slices.Sort(want)
		slices.Sort(r.Env)
		if !slices.Equal(r.Env, want) {
			t.Errorf("the provider's environment:\n%s\nwant:\n%s", strings.Join(r.Env, "\n"), strings.Join(want, "\n"))
		}
		for name, got := range map[string]string{"the state key": r.Key, "writ's environment": r.Environ} {
			if !strings.Contains(got, "permission denied") {
				t.Errorf("the provider read %s: %s", name, got)
			}
		}
		// The rule drops the packets, so the connection times out. writ
		// reached the same addresses above.
		for name, got := range map[string]string{"169.254.169.254": r.IMDS4, "fd00:ec2::254": r.IMDS6} {
			if strings.HasPrefix(got, "reached") || !strings.Contains(got, "Timeout exceeded") {
				t.Errorf("the provider reached %s: %s", name, got)
			}
		}
		t.Logf("the provider saw: key %q, environ %q, %q, %q", r.Key, r.Environ, r.IMDS4, r.IMDS6)
	})

	t.Run("writ kills a provider of another user", func(t *testing.T) {
		p, err := start(ctx, probeBin, launch)
		if err != nil {
			t.Fatal(err)
		}
		p.kill()
		if _, err := os.Stat(p.socketDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the killed provider's directory is left: %v", err)
		}
		requireNoProcesses(t, user.UID)
	})

	t.Run("Reap ends what a provider left behind", func(t *testing.T) {
		left := exec.Command(probeBin)
		left.Env = []string{probeEnv + "=sleep"}
		setProcAttr(left, &user)
		if err := left.Start(); err != nil {
			t.Fatal(err)
		}
		if err := runas.Reap(user.UID); err != nil {
			t.Fatal(err)
		}
		if err := left.Wait(); err == nil || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("the process a provider left: %v, want killed", err)
		}
		requireNoProcesses(t, user.UID)
	})
}

func copyExec(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func probeOf(t *testing.T, stderr string) probeReport {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(stderr))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if b, ok := strings.CutPrefix(sc.Text(), "probe "); ok {
			var r probeReport
			if err := json.Unmarshal([]byte(b), &r); err != nil {
				t.Fatal(err)
			}
			return r
		}
	}
	t.Fatalf("the provider reported nothing:\n%s", stderr)
	return probeReport{}
}

func requireNoProcesses(t *testing.T, uid uint32) {
	t.Helper()
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		st, err := runas.ProcStatus(pid)
		if err == nil && !strings.HasPrefix(st["State"], "Z") && strings.Fields(st["Uid"])[0] == strconv.FormatUint(uint64(uid), 10) {
			t.Fatalf("process %d of uid %d is still running: %s", pid, uid, st["Name"])
		}
	}
}
