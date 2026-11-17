//go:build e2e && e2e_import && e2e_awsemu && linux

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestImportAWSEmulator adopts AWS objects that OpenTofu created on AWS
// Emulator, through the real hashicorp/aws provider: a launch template, a
// security group, a target group, an ALB, its listener, and a listener
// rule. writ import bulk must write code whose plan in an empty state only
// imports, with names that hold no ARN, and writ import accept must pass
// that plan. The parent removes every AWS_
// variable, builds writ, and runs the test again in a user, mount, and
// network namespace where only loopback exists and the usual credential
// files read as empty. The emulator, tofu, and writ run only there, and
// tofu installs providers only from its plugin cache. Run it with `just
// e2e-aws-emulator`, which puts the emulator on PATH.
func TestImportAWSEmulator(t *testing.T) {
	const child = "WRIT_E2E_AWS_EMULATOR_CHILD"
	if runtime.GOARCH != "amd64" {
		t.Skipf("AWS Emulator is built only for x86_64-linux, not linux/%s", runtime.GOARCH)
	}
	emulator, err := exec.LookPath("aws-emulator")
	if err != nil {
		t.Fatalf("this test needs AWS Emulator on PATH, as just e2e-aws-emulator gives: %v", err)
	}
	tofu, err := exec.LookPath("tofu")
	if err != nil {
		t.Fatalf("this test needs OpenTofu 1.13.9 on PATH, as just e2e-aws-emulator gives: %v", err)
	}
	if os.Getenv(child) == "" {
		masks, probe := credentialMasks(t)
		unsetAWS(t)
		h := newImportHarness(t, tofu)
		if _, err := os.Stat(filepath.Join(h.plugins, "registry.opentofu.org/hashicorp/aws/6.70.0/linux_amd64")); err != nil {
			t.Fatalf("hashicorp/aws 6.70.0 is not in OpenTofu's plugin cache %s, and the test installs providers from nowhere else: fill it first with `just e2e-aws-emulator-providers`: %v", h.plugins, err)
		}
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "unshare", "-rmn", self, "-test.run=^TestImportAWSEmulator$", "-test.v", "-test.count=1")
		cmd.Env = append(h.env(), child+"=1", childWritEnv+"="+h.writ,
			awsEmulatorMasksEnv+"="+strings.Join(masks, string(os.PathListSeparator)), awsEmulatorProbeEnv+"="+probe)
		out, err := cmd.CombinedOutput()
		t.Logf("in the namespace:\n%s", out)
		if err != nil || !strings.Contains(string(out), "--- PASS: TestImportAWSEmulator") {
			t.Fatalf("the test failed in the namespace: %v", err)
		}
		return
	}
	unsetAWS(t)
	maskCredentials(t, filepath.SplitList(os.Getenv(awsEmulatorMasksEnv)), os.Getenv(awsEmulatorProbeEnv))
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("loopback: %v %s", err, out)
	}
	requireOnlyLoopback(t)
	// The plugin cache is read as a filesystem mirror, so tofu never asks a
	// registry, and TF_PLUGIN_CACHE_DIR is empty, so tofu writes nothing
	// into the cache.
	h := &importHarness{tofu: tofu, writ: writBin, cache: os.Getenv("WRIT_CACHE_DIR"), tfrc: filepath.Join(t.TempDir(), "tofurc")}
	writeFiles(t, filepath.Dir(h.tfrc), map[string]string{"tofurc": fmt.Sprintf("provider_installation {\n  filesystem_mirror {\n    path = %q\n  }\n}\n", os.Getenv("TF_PLUGIN_CACHE_DIR"))})
	// No tool may find the caller's AWS files at their default paths.
	t.Setenv("HOME", t.TempDir())
	emu := startAWSEmulator(t, emulator)
	for k, v := range emu.env() {
		t.Setenv(k, v)
	}

	made := t.TempDir()
	writeFiles(t, made, map[string]string{".terraform.lock.hcl": readFile(t, "testdata/import-hcl/aws.lock.hcl"), "main.tf": awsHCLProviders + awsEmulatorProvider + awsEmulatorFixture})
	h.must(t, made, h.tofu, "init", "-input=false", "-lockfile=readonly", "-no-color")
	start := time.Now()
	if r := h.exec(t, made, h.tofu, "apply", "-auto-approve", "-input=false", "-json"); r.code != 0 {
		// AWS Emulator 1.0.385 gives a new security group in a VPC no
		// default egress rule, and the provider revokes that rule right
		// after the create. The group exists, so untainting it lets the
		// next apply add its rules in an update.
		retry, errs, err := egressRevokeOnly(r.stdout)
		if err != nil {
			t.Fatalf("read tofu apply -json: %v\n%s", err, r)
		}
		if !retry {
			t.Fatalf("tofu apply failed with %d errors:\n%s\n%s", len(errs), strings.Join(errs, "\n"), r.stderr)
		}
		t.Logf("the emulator gave the security group no default egress rule; untaint it and apply again: %s", errs[0])
		h.must(t, made, h.tofu, "untaint", "-no-color", "aws_security_group.web")
		h.must(t, made, h.tofu, "apply", "-auto-approve", "-input=false", "-no-color")
	}
	t.Logf("tofu created the objects in %s", time.Since(start).Round(time.Second))

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".terraform.lock.hcl": readFile(t, "testdata/import-hcl/aws.lock.hcl"),
		"main.tf":             awsHCLProviders + awsEmulatorProvider,
		"select.json":         h.must(t, made, h.tofu, "output", "-raw", "selection").stdout,
	})
	code, bulk := h.adopt(t, dir, 6, func(argv ...string) []string { return argv }, "select.json")
	for _, typ := range []string{"aws_launch_template", "aws_security_group", "aws_lb_target_group", "aws_lb", "aws_lb_listener", "aws_lb_listener_rule"} {
		if !strings.Contains(code, fmt.Sprintf("resource %q ", typ)) {
			t.Errorf("imported.tf declares no %s", typ)
		}
	}
	// The plan only imports, so bulk has no cause to doubt it. The
	// security group and the target group return a resource identity,
	// which bulk sends with each plan and the plan keeps.
	if strings.Contains(bulk, "could not prove") || strings.Contains(bulk, "resource identity") {
		t.Errorf("writ import bulk doubts a plan that only imports:\n%s", bulk)
	}
	// A listener and a rule have ARNs for ids. Their names come from the
	// load balancer, the protocol and port, and the priority.
	for _, want := range []string{`resource "aws_lb_listener" "aws_lb_listener_app_http_80" {`, `resource "aws_lb_listener_rule" "aws_lb_listener_rule_app_http_80_priority_10" {`} {
		if !strings.Contains(code, want) {
			t.Errorf("imported.tf lacks %s", want)
		}
	}
	var imports struct {
		Imports []struct {
			Name string  `json:"name"`
			Key  *string `json:"key"`
		} `json:"imports"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "imported.imports.json"))), &imports); err != nil {
		t.Fatal(err)
	}
	for _, imp := range imports.Imports {
		names := []string{imp.Name}
		if imp.Key != nil {
			names = append(names, *imp.Key)
		}
		for _, name := range names {
			if idInName.MatchString(name) {
				t.Errorf("the name %q holds part of an ARN or an account id", name)
			}
		}
	}
	if strings.Contains(code, emu.secretKey) {
		t.Error("imported.tf holds the emulator's secret key")
	}
	// adopt passes writ import accept the plan's JSON; the saved plan must
	// pass too.
	var accepted struct {
		OK      bool `json:"ok"`
		Imports int  `json:"imports"`
	}
	h.writJSON(t, &accepted, "import", "accept", "--imports", filepath.Join(dir, "imported.imports.json"), "--json", filepath.Join(dir, "import.plan"))
	if !accepted.OK || accepted.Imports != 6 {
		t.Fatalf("writ import accept of the saved plan = %+v, want 6 imports accepted", accepted)
	}
}

// idInName matches a name that holds part of an ARN, or a run of digits
// long enough to be part of an account id that a name cut short.
var idInName = regexp.MustCompile(`(?i)arn[:_]|elasticloadbalancing|[0-9]{6,}`)

// TestAWSEmulatorApplyRetry protects the one retry of the fixture's apply:
// only when the apply's sole error is the emulator's missing egress rule.
// egressRevoke is that error as tofu 1.13.9 printed it, without its
// timestamp, range, and snippet.
func TestAWSEmulatorApplyRetry(t *testing.T) {
	const (
		version      = `{"@level":"info","@message":"OpenTofu 1.13.9","@module":"tofu.ui","terraform":"1.13.9","type":"version","ui":"1.2"}`
		egressRevoke = `{"@level":"error","@message":"Error: revoking default IPv4 egress rule for Security Group (sg-00000000000000094): operation error EC2: RevokeSecurityGroupEgress, https response error StatusCode: 400, RequestID: 00000000-0000-0000-0000-00000000000a, api error InvalidPermission.NotFound: The specified rule does not exist in this security group.","@module":"tofu.ui","diagnostic":{"severity":"error","summary":"revoking default IPv4 egress rule for Security Group (sg-00000000000000094): operation error EC2: RevokeSecurityGroupEgress, https response error StatusCode: 400, RequestID: 00000000-0000-0000-0000-00000000000a, api error InvalidPermission.NotFound: The specified rule does not exist in this security group.","detail":"","address":"aws_security_group.web"},"type":"diagnostic"}`
		otherError   = `{"@level":"error","@message":"Error: creating ELBv2 application Load Balancer (app): operation error Elastic Load Balancing v2: CreateLoadBalancer, https response error StatusCode: 400, api error ValidationError: subnets","@module":"tofu.ui","diagnostic":{"severity":"error","summary":"creating ELBv2 application Load Balancer (app): operation error Elastic Load Balancing v2: CreateLoadBalancer, https response error StatusCode: 400, api error ValidationError: subnets","detail":"","address":"aws_lb.app"},"type":"diagnostic"}`
		warning      = `{"@level":"warn","@message":"Warning: Argument is deprecated","@module":"tofu.ui","diagnostic":{"severity":"warning","summary":"Argument is deprecated","detail":"","address":"aws_lb.app"},"type":"diagnostic"}`
	)
	for _, tc := range []struct {
		name      string
		lines     []string
		retry     bool
		errsShown int
	}{
		{"the emulator's error alone", []string{version, egressRevoke}, true, 1},
		{"the emulator's error and a warning", []string{version, warning, egressRevoke}, true, 1},
		{"the emulator's error and another error", []string{version, egressRevoke, otherError}, false, 2},
		{"another error and the emulator's error", []string{version, otherError, egressRevoke}, false, 2},
		{"another error alone", []string{version, otherError}, false, 1},
		{"the emulator's error on another address", []string{version, strings.Replace(egressRevoke, `"address":"aws_security_group.web"`, `"address":"aws_security_group.other"`, 1)}, false, 1},
		{"no error", []string{version, warning}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, errs, err := egressRevokeOnly(strings.Join(tc.lines, "\n") + "\n")
			if err != nil {
				t.Fatal(err)
			}
			if retry != tc.retry || len(errs) != tc.errsShown {
				t.Fatalf("egressRevokeOnly = %v with errors %q, want %v with %d errors", retry, errs, tc.retry, tc.errsShown)
			}
		})
	}
	if _, _, err := egressRevokeOnly("Error: not JSON\n"); err == nil {
		t.Fatal("egressRevokeOnly read a line that is not JSON")
	}
}

// requireOnlyLoopback fails the test unless this process is in a network
// namespace whose one interface is loopback, so nothing it starts can reach
// a host other than this one.
func requireOnlyLoopback(t *testing.T) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback == 0 {
			t.Fatalf("interface %s is not loopback: the test must run in a network namespace", i.Name)
		}
	}
	if c, err := net.DialTimeout("tcp", "192.0.2.1:443", 3*time.Second); err == nil {
		c.Close()
		t.Fatal("a connection out of the namespace succeeded")
	}
}

// awsEmulator is a running AWS Emulator on loopback and the files it wrote
// for its clients.
type awsEmulator struct {
	url, caFile, configFile, credentialsFile, secretKey string
}

// awsEmulatorPort is the emulator's default port. The namespace is new, so
// nothing else holds it.
const awsEmulatorPort = "9297"

// startAWSEmulator starts the emulator with its telemetry off and its
// files in $HOME, waits until it is healthy, and stops it when the test
// ends.
func startAWSEmulator(t *testing.T, bin string) *awsEmulator {
	t.Helper()
	home := os.Getenv("HOME")
	logPath := filepath.Join(t.TempDir(), "emulator.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	// --idle-timeout ends an emulator that outlives a killed test.
	cmd := exec.Command(bin, "--bind", "127.0.0.1", "--port", awsEmulatorPort, "--telemetry", "off", "--idle-timeout", "300")
	cmd.Env = append(os.Environ(), "EMULATOR_TELEMETRY=off")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// This goroutine alone receives the exit: the startup wait and the
	// cleanup both read done, which stays closed.
	var waitErr error
	done := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("the emulator, pid %d, did not exit after SIGKILL", cmd.Process.Pid)
			}
		}
		if t.Failed() {
			t.Logf("the emulator's log, last %d lines:\n%s", awsEmulatorLogTail, logTail(t, logPath, awsEmulatorLogTail))
		}
	})

	dir := filepath.Join(home, ".aws", "emulator", awsEmulatorPort)
	e := &awsEmulator{
		url:             "https://127.0.0.1:" + awsEmulatorPort,
		caFile:          filepath.Join(dir, "ca.pem"),
		configFile:      filepath.Join(dir, "config"),
		credentialsFile: filepath.Join(dir, "emulator_credentials"),
	}
	deadline := time.Now().Add(30 * time.Second)
	for !e.healthy() {
		select {
		case <-done:
			t.Fatalf("the emulator exited at startup: %v", waitErr)
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the emulator is not healthy after 30s")
		}
	}
	if log := readFile(t, logPath); !strings.Contains(log, "telemetry: disabled") {
		t.Fatalf("the emulator does not report its telemetry disabled:\n%s", log)
	}
	for _, line := range strings.Split(readFile(t, e.credentialsFile), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "aws_secret_access_key" {
			e.secretKey = strings.TrimSpace(v)
		}
	}
	if e.secretKey == "" {
		t.Fatalf("%s holds no aws_secret_access_key", e.credentialsFile)
	}
	return e
}

// healthy is whether the emulator answers its health check over TLS that
// its own CA verifies.
func (e *awsEmulator) healthy() bool {
	pem, err := os.ReadFile(e.caFile)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get(e.url + "/__admin/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// env is the standard AWS SDK settings that send tofu's and writ's aws
// provider to the emulator with its keys and CA.
func (e *awsEmulator) env() map[string]string {
	return map[string]string{
		"AWS_ENDPOINT_URL":            e.url,
		"AWS_CA_BUNDLE":               e.caFile,
		"AWS_CONFIG_FILE":             e.configFile,
		"AWS_SHARED_CREDENTIALS_FILE": e.credentialsFile,
		"AWS_REGION":                  "us-east-1",
		"AWS_EC2_METADATA_DISABLED":   "true",
	}
}

// awsEmulatorLogTail is how many of the emulator's last log lines a
// failure shows.
const awsEmulatorLogTail = 40

func logTail(t *testing.T, path string, n int) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(readFile(t, path), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// unsetAWS removes every AWS_ variable from this process until the test
// ends, so no real credentials, profile, or endpoint reaches anything it
// starts.
func unsetAWS(t *testing.T) {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AWS_") {
			t.Setenv(k, "")
			os.Unsetenv(k)
		}
	}
}

// The parent passes the child the paths to mask and the scratch home that
// holds the synthetic credential files.
const (
	awsEmulatorMasksEnv = "WRIT_E2E_AWS_EMULATOR_MASKS"
	awsEmulatorProbeEnv = "WRIT_E2E_AWS_EMULATOR_PROBE_HOME"
)

// credentialFiles are where cloud and registry tools look for credentials
// under a home directory.
var credentialFiles = []string{".aws", ".config/gcloud", ".azure", ".docker", ".netrc"}

// credentialMasks is every path the child must mask: the credential files
// under the caller's home, root's home, and a scratch home, and the files
// the caller's AWS_SHARED_CREDENTIALS_FILE and AWS_CONFIG_FILE name. The
// scratch home holds synthetic credentials, mode 0600, that the child must
// not be able to read. It returns the paths and the scratch home.
func credentialMasks(t *testing.T) (masks []string, probe string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	probe = t.TempDir()
	if err := os.Mkdir(filepath.Join(probe, ".aws"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range awsEmulatorProbeFiles {
		if err := os.WriteFile(filepath.Join(probe, name), []byte("synthetic-probe-credential\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{home, "/root", probe} {
		for _, name := range credentialFiles {
			masks = append(masks, filepath.Join(dir, name))
		}
	}
	for _, k := range []string{"AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE"} {
		if v := os.Getenv(k); v != "" {
			masks = append(masks, v)
		}
	}
	return masks, probe
}

// awsEmulatorProbeFiles are the synthetic credential files in the scratch
// home: one in a masked directory and one masked file.
var awsEmulatorProbeFiles = []string{".aws/credentials", ".netrc"}

// maskCredentials mounts an empty read-only tmpfs over each directory in
// paths and binds an empty file over each other file, in this process's
// own mount namespace, and requires that the synthetic files in the scratch
// home probe read as absent or empty. A path that does not exist or that
// this process cannot reach is left as it is.
func maskCredentials(t *testing.T, paths []string, probe string) {
	t.Helper()
	if probe == "" {
		t.Fatal("the parent named no scratch home with synthetic credentials")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	var masked []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			continue
		case err != nil:
			t.Fatal(err)
		case fi.IsDir():
			err = unix.Mount("tmpfs", p, "tmpfs", unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "size=4k,mode=0500")
		default:
			err = unix.Mount(empty, p, "", unix.MS_BIND, "")
		}
		if err != nil {
			t.Fatalf("mask %s: %v", p, err)
		}
		masked = append(masked, p)
	}
	t.Logf("masked %s", strings.Join(masked, ", "))
	for _, name := range awsEmulatorProbeFiles {
		b, err := os.ReadFile(filepath.Join(probe, name))
		if err == nil && len(b) > 0 {
			t.Fatalf("the synthetic credential file %s is readable in the namespace", name)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			t.Fatal(err)
		}
	}
}

// egressRevokeOnly reads the JSON lines of tofu apply -json and reports
// whether its errors are exactly the one that AWS Emulator 1.0.385 causes:
// the provider fails to revoke the default egress rule of
// aws_security_group.web, which the emulator never made. It returns each
// error's text.
func egressRevokeOnly(applyJSON string) (bool, []string, error) {
	var errs []string
	expected := false
	for _, line := range strings.Split(strings.TrimSpace(applyJSON), "\n") {
		var msg struct {
			Type       string `json:"type"`
			Diagnostic struct {
				Severity string `json:"severity"`
				Summary  string `json:"summary"`
				Detail   string `json:"detail"`
				Address  string `json:"address"`
			} `json:"diagnostic"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return false, nil, fmt.Errorf("line %q: %w", line, err)
		}
		d := msg.Diagnostic
		if msg.Type != "diagnostic" || d.Severity != "error" {
			continue
		}
		text := strings.TrimSpace(d.Summary + "\n" + d.Detail)
		errs = append(errs, d.Address+": "+text)
		expected = d.Address == "aws_security_group.web" && strings.Contains(text, "revoking default IPv4 egress rule") && strings.Contains(text, "InvalidPermission.NotFound")
	}
	return len(errs) == 1 && expected, errs, nil
}

const awsEmulatorProvider = `
provider "aws" {
  region                  = "us-east-1"
  skip_metadata_api_check = "true"
  skip_region_validation  = true
}
`

// awsEmulatorFixture is the objects to adopt and what they need, and their
// selection for writ import bulk.
const awsEmulatorFixture = `
resource "aws_vpc" "v" {
  cidr_block = "10.0.0.0/16"
}

resource "aws_subnet" "a" {
  vpc_id            = aws_vpc.v.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "us-east-1a"
}

resource "aws_subnet" "b" {
  vpc_id            = aws_vpc.v.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = "us-east-1b"
}

resource "aws_security_group" "web" {
  name        = "web"
  description = "web"
  vpc_id      = aws_vpc.v.id
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_lb_target_group" "app" {
  name     = "app"
  port     = 80
  protocol = "HTTP"
  vpc_id   = aws_vpc.v.id
}

resource "aws_lb" "app" {
  name               = "app"
  internal           = true
  load_balancer_type = "application"
  security_groups    = [aws_security_group.web.id]
  subnets            = [aws_subnet.a.id, aws_subnet.b.id]
  timeouts {
    create = "3m"
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.app.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

resource "aws_lb_listener_rule" "api" {
  listener_arn = aws_lb_listener.http.arn
  priority     = 10
  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
  condition {
    path_pattern {
      values = ["/api/*"]
    }
  }
}

resource "aws_launch_template" "app" {
  name                   = "app"
  image_id               = "ami-0123456789abcdef0"
  instance_type          = "t3.micro"
  vpc_security_group_ids = [aws_security_group.web.id]
}

output "selection" {
  value = jsonencode({ objects = [
    { type = "aws_launch_template", ids = [aws_launch_template.app.id] },
    { type = "aws_security_group", ids = [aws_security_group.web.id] },
    { type = "aws_lb_target_group", ids = [aws_lb_target_group.app.arn] },
    { type = "aws_lb", ids = [aws_lb.app.arn] },
    { type = "aws_lb_listener", ids = [aws_lb_listener.http.arn] },
    { type = "aws_lb_listener_rule", ids = [aws_lb_listener_rule.api.arn] },
  ] })
}
`
