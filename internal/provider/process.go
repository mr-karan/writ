package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/runas"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// magicCookie is the go-plugin handshake value every Terraform provider
// checks before it serves.
const magicCookie = "d602bf8f470bc67ca7faa0386276bbdd4330efaf76d1a219cb4d6991ca9872b2"

const (
	handshakeTimeout = 10 * time.Second
	// shutdownGrace is how long a provider may take to exit after Shutdown
	// when the caller's context sets no deadline.
	shutdownGrace = 5 * time.Second
	maxMessage    = 64 << 20
	stderrTail    = 16 << 10
)

// process is one running provider binary and its gRPC connection.
type process struct {
	cmd       *exec.Cmd
	socketDir string
	user      *User // nil when the provider runs as writ's own user
	protocol  int   // 5 or 6, from the handshake
	conn      *grpc.ClientConn
	stderr    *tailBuffer
	// exited is closed once Wait returns; waitErr is set before that.
	exited  chan struct{}
	waitErr error
}

// start execs the binary and completes the go-plugin handshake.
//
// By default the provider inherits Writ's environment, as it does under
// Terraform, so the credential chains providers document work: AWS_PROFILE
// and ~/.aws, CI identity tokens, proxies. See providerEnv for what it does
// not inherit. launch can replace the environment and the user; see Launch.
// The plugin settings and the SDK log switches come after it and win.
// Without the switches a provider writes trace logs to stderr by the hundred
// kilobytes.
func start(ctx context.Context, binary string, launch Launch) (*process, error) {
	var sockDir string
	var err error
	if u := launch.User; u != nil {
		sockDir, err = mkdirAs(*u, isolatedSocketRoot)
	} else {
		sockDir, err = os.MkdirTemp(socketRoot(), "writ-")
	}
	if err != nil {
		return nil, fmt.Errorf("make the provider's directory: %w", err)
	}
	hs := &handshakeWriter{line: make(chan string, 1)}
	p := &process{socketDir: sockDir, user: launch.User, stderr: &tailBuffer{max: stderrTail}, exited: make(chan struct{})}
	p.cmd = exec.Command(binary)
	// exec keeps the last value of a repeated key.
	p.cmd.Env = append(launch.launchEnv(sockDir),
		"TF_PLUGIN_MAGIC_COOKIE="+magicCookie,
		"PLUGIN_PROTOCOL_VERSIONS=5,6",
		"PLUGIN_UNIX_SOCKET_DIR="+sockDir,
		"TF_LOG_SDK=off",
		"TF_LOG_SDK_PROTO=off",
		"TF_LOG_SDK_FRAMEWORK=off",
	)
	if launch.nomadPlanResponse {
		p.cmd.Env = append(p.cmd.Env, "WRIT_NOMAD_PLAN_RESPONSE=1")
	}
	if launch.OwnEnv {
		p.cmd.Dir = sockDir
	}
	// Writers, not pipes: Wait then waits for the copies to finish instead of
	// closing a pipe under a reader and dropping the last lines.
	p.cmd.Stdout = hs
	p.cmd.Stderr = p.stderr
	p.cmd.WaitDelay = time.Second
	setProcAttr(p.cmd, launch.User)
	if err := p.cmd.Start(); err != nil {
		p.removeDir()
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.exited)
	}()

	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()
	var line string
	select {
	case line = <-hs.line:
	case <-p.exited:
		p.kill()
		return nil, p.failed(fmt.Errorf("provider %s exited before the handshake: %v", binary, p.waitErr))
	case <-timer.C:
		p.kill()
		return nil, p.failed(fmt.Errorf("provider %s sent no handshake within %s", binary, handshakeTimeout))
	case <-ctx.Done():
		p.kill()
		return nil, ctx.Err()
	}
	network, addr, err := p.parseHandshake(line)
	if err != nil {
		p.kill()
		return nil, p.failed(fmt.Errorf("provider %s: %w", binary, err))
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessage), grpc.MaxCallSendMsgSize(maxMessage)),
	}
	target := "unix://" + addr
	switch {
	case network == "tcp":
		target = "passthrough:///" + addr
	case launch.User != nil:
		// The socket belongs to the provider's user, so only that user
		// may connect to it.
		if !strings.HasPrefix(addr, sockDir+"/") {
			p.kill()
			return nil, fmt.Errorf("provider %s: its socket %s is outside its directory %s", binary, addr, sockDir)
		}
		u := *launch.User
		target = "passthrough:///" + addr
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialAs(ctx, u, addr)
		}))
	}
	if u := launch.User; u != nil {
		if network != "unix" {
			p.kill()
			return nil, fmt.Errorf("provider %s offered %s; a provider that runs as another user must offer a unix socket", binary, network)
		}
		if err := runas.CheckIsolated(p.cmd.Process.Pid, runas.User(*u)); err != nil {
			p.kill()
			return nil, fmt.Errorf("provider %s: %w", binary, err)
		}
	}
	p.conn, err = grpc.NewClient(target, opts...)
	if err != nil {
		p.kill()
		return nil, fmt.Errorf("provider %s: dial %s: %w", binary, addr, err)
	}
	return p, nil
}

// removeDir removes the provider's directory, as its user when it has one.
func (p *process) removeDir() {
	if p.user != nil {
		removeAs(*p.user, p.socketDir)
		return
	}
	os.RemoveAll(p.socketDir)
}

// client returns the adapter for the protocol the provider chose in the
// handshake.
func (p *process) client() protocol {
	if p.protocol == 6 {
		return newV6(p.conn)
	}
	return newV5(p.conn)
}

// parseHandshake reads CORE|APP|NETWORK|ADDR|PROTOCOL|CERT, the go-plugin
// handshake line. CERT is empty because Writ does not ask for mTLS.
func (p *process) parseHandshake(line string) (network, addr string, err error) {
	f := strings.Split(strings.TrimSpace(line), "|")
	if len(f) < 6 {
		return "", "", fmt.Errorf("malformed handshake %q", line)
	}
	switch {
	case f[0] != "1":
		return "", "", fmt.Errorf("handshake %q: unsupported go-plugin core protocol %s", line, f[0])
	case f[1] != "5" && f[1] != "6":
		return "", "", fmt.Errorf("handshake %q: plugin protocol %s; writ speaks 5 and 6", line, f[1])
	case f[2] != "unix" && f[2] != "tcp":
		return "", "", fmt.Errorf("handshake %q: unsupported network %s", line, f[2])
	case f[4] != "grpc":
		return "", "", fmt.Errorf("handshake %q: provider does not speak grpc", line)
	case f[5] != "":
		return "", "", fmt.Errorf("handshake %q: provider offered a TLS certificate writ did not ask for", line)
	}
	p.protocol, _ = strconv.Atoi(f[1])
	return f[2], f[3], nil
}

// socketRoot is where per-process socket directories go. A unix socket path
// is limited to about 100 bytes, so a long TMPDIR falls back to /tmp.
func socketRoot() string {
	if d := os.TempDir(); len(d) <= 48 {
		return d
	}
	return "/tmp"
}

// providerEnv drops what a provider must not inherit: WRIT_ variables,
// which can carry a state URL with a password, and go-plugin's PLUGIN_
// settings from a host that started Writ, which would change how the
// provider serves (PLUGIN_CLIENT_CERT makes it offer TLS, which Writ
// refuses).
func providerEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "WRIT_") || strings.HasPrefix(kv, "PLUGIN_")
	})
}

// failed adds the provider's stderr to err, which is where a provider that
// crashes or refuses to start explains why.
func (p *process) failed(err error) *ProcessError {
	pe := &ProcessError{Err: err, Stderr: p.stderr.String()}
	select {
	case <-p.exited:
		pe.Exited = true
	default:
	}
	return pe
}

// codeOnly returns err with a gRPC status's message dropped. The message
// is the provider's text, and a shutdown or stop error goes to the log
// with no subject to judge it by, so only the code stays.
func codeOnly(err error) error {
	if st, ok := status.FromError(err); ok && err != nil {
		return status.Errorf(st.Code(), "the provider's message is withheld")
	}
	return err
}

// shutdown calls go-plugin's GRPCController.Shutdown, waits for the process
// to exit until ctx is done (or shutdownGrace without a deadline), then kills
// it. The provider exits before it answers, so Unavailable means success.
func (p *process) shutdown(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownGrace)
		defer cancel()
	}
	err := codeOnly(p.conn.Invoke(ctx, "/plugin.GRPCController/Shutdown", &emptypb.Empty{}, &emptypb.Empty{}))
	if status.Code(err) == codes.Unavailable {
		err = nil
	}
	p.conn.Close()
	select {
	case <-p.exited:
		p.removeDir()
		return nil
	case <-ctx.Done():
	}
	p.kill()
	return errors.Join(fmt.Errorf("provider %s did not exit after shutdown and was killed", filepath.Base(p.cmd.Path)), err)
}

// kill ends the process, waits for it, and removes its socket directory, which
// a killed provider leaves behind.
func (p *process) kill() {
	if p.conn != nil {
		p.conn.Close()
	}
	select {
	case <-p.exited:
	default:
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	p.removeDir()
}

// handshakeWriter is the provider's stdout. It hands the first line to start
// and discards the rest. exec.Cmd calls Write from one goroutine.
type handshakeWriter struct {
	buf  []byte
	sent bool
	line chan string
}

func (w *handshakeWriter) Write(b []byte) (int, error) {
	if w.sent {
		return len(b), nil
	}
	w.buf = append(w.buf, b...)
	if i := bytes.IndexByte(w.buf, '\n'); i >= 0 || len(w.buf) > 4096 {
		if i < 0 {
			i = len(w.buf)
		}
		w.line <- string(w.buf[:i])
		w.sent, w.buf = true, nil
	}
	return len(b), nil
}

// tailBuffer keeps the last max bytes written to it, so a chatty provider
// cannot grow memory and a crash report keeps its end.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
