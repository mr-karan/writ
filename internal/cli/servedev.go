package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/ui"
)

// DevListen is the address writ serve --dev listens on by default.
const DevListen = "127.0.0.1:7340"

// DevClientFile is the file in the dev data directory that holds the dev
// control plane's URL and token, readable only by its user.
const DevClientFile = "client.json"

// devClientFormat is the format of DevClientFile.
const devClientFormat = "writ.dev-client/v0"

// devClient is what DevClientFile holds.
type devClient struct {
	Format string `json:"format"`
	URL    string `json:"url"`
	Token  string `json:"token"`
	User   string `json:"user"`
	PID    int    `json:"pid"`
}

// DevServeOptions are the flags of `writ serve --dev`.
type DevServeOptions struct {
	// Data is the data directory; empty means DevDir.
	Data string
	// Listen is a loopback address; empty means DevListen. Port 0 picks
	// one, which client.json records.
	Listen string
	// Database is a postgres:// URL for the control database and every
	// stack's state; empty means SQLite files under Data.
	Database string
	// Config is an HCL file of forge and repo blocks and poll_every, for repositories
	// at a forge; empty for none.
	Config  string
	Version string
}

// devDir is the dev control plane's data directory: $XDG_DATA_HOME/writ/dev,
// else ~/.local/share/writ/dev. A client finds the dev control plane only
// there.
func (a *App) devDir() (string, error) {
	base, _ := a.opts.LookupEnv("XDG_DATA_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the home directory for the dev control plane: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "writ", "dev"), nil
}

// ServeDev runs writ serve --dev until ctx ends: a control plane for this
// user alone, on loopback, with the same API and code as a team's.
func (a *App) ServeDev(ctx context.Context, o DevServeOptions) error {
	start := time.Now()
	data := o.Data
	if data == "" {
		d, err := a.devDir()
		if err != nil {
			return err
		}
		data = d
	}
	data, err := filepath.Abs(data)
	if err != nil {
		return err
	}
	listen := o.Listen
	if listen == "" {
		listen = DevListen
	}
	if err := checkLoopback(listen); err != nil {
		return err
	}
	var extra *server.Config
	if o.Config != "" {
		src, err := os.ReadFile(o.Config)
		if err != nil {
			return usagef("read the config: %v", err)
		}
		var diags diag.List
		extra, diags = server.ParseDevConfig(o.Config, src)
		printDiags(a.humanOutput(), diags)
		if diags.HasErrors() {
			return ErrFailed
		}
	}
	database := o.Database
	if database == "" {
		database = filepath.Join(data, "control.db")
	} else if err := control.CheckURL(database); err != nil {
		return usagef("--database: %v", err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return fmt.Errorf("make the data directory: %w", err)
	}
	if err := os.Chmod(data, 0o700); err != nil {
		return fmt.Errorf("make the data directory readable only by its user: %w", err)
	}
	clientPath := filepath.Join(data, DevClientFile)
	if running, ok := devRunning(ctx, clientPath); ok {
		return usagef("a dev control plane already runs at %s for %s; stop it first, or pass --data", running, data)
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	url := "http://" + ln.Addr().String()
	log := slog.New(slog.NewTextHandler(a.opts.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store, err := control.Open(ctx, control.Options{Location: database})
	if err != nil {
		return err
	}
	defer store.Close()
	token, err := newDevToken()
	if err != nil {
		return err
	}
	user := a.osUser()
	cfg := server.DevConfig(server.DevOptions{URL: url, Listen: ln.Addr().String(), Database: database, Data: data, Extra: extra})
	runner, closeTofu, err := serveTofu(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeTofu()
	srv, err := server.New(ctx, server.Options{
		Config: cfg, Store: store, Tofu: runner, DevToken: token, DevUser: user, Version: o.Version, Log: log, Now: a.opts.Now,
	})
	if err != nil {
		return err
	}
	u, err := ui.New(ui.Options{Server: srv, Log: log, Now: a.opts.Now})
	if err != nil {
		srv.Close()
		return err
	}
	u.Mount()
	if err := writeDevClient(clientPath, devClient{Format: devClientFormat, URL: url, Token: token, User: user, PID: os.Getpid()}); err != nil {
		srv.Close()
		return err
	}
	defer removeDevClient(clientPath, token)
	out := a.humanOutput()
	where := "control.db, state/, keys/"
	if o.Database != "" {
		where = "keys/; control database and state in " + redactURL(o.Database)
	}
	fmt.Fprintf(out, "writ serve --dev: %s, for %s only\n", url, user)
	fmt.Fprintf(out, "data: %s (%s)\n", data, where)
	if runner != nil {
		fmt.Fprintf(out, "OpenTofu runs get PATH=%s and this shell's TF_VAR_ variables, and no other variable of this shell.\n", devRunPath(cfg.Tofu))
	}
	if len(cfg.Forges) > 0 {
		names := make([]string, len(cfg.Forges))
		for i, f := range cfg.Forges {
			names[i] = f.Name + " (" + f.URL + ")"
		}
		fmt.Fprintf(out, "forges: %s; repositories polled every %s\n", strings.Join(names, ", "), cfg.PollEvery)
	}
	fmt.Fprintf(out, "control UI: %s (works once, for 10 minutes; writ ui prints another)\n", srv.DevLink().URL)
	return srv.ServeOn(ctx, ln, func(string) {
		fmt.Fprintf(out, "ready in %d ms\n", time.Since(start).Milliseconds())
	})
}

// devRunPath is the PATH of a dev OpenTofu run: the base PATH, after the
// directory of tofu.git when the tofu block sets it, as tofu.Runner puts
// it there for each step.
func devRunPath(t *server.Tofu) string {
	const base = "/usr/local/bin:/usr/bin:/bin"
	if t.Git == "" {
		return base
	}
	git, err := tofuProgram(t.Git)
	if err != nil {
		return base
	}
	if git, err = filepath.Abs(git); err != nil {
		return base
	}
	return filepath.Dir(git) + ":" + base
}

// checkLoopback refuses an address that is not on loopback: the dev
// control plane serves one user on one machine.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return usagef("--listen %s: %v", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return usagef("--listen %s is not a loopback address; writ serve --dev serves this machine's user alone", addr)
	}
	return nil
}

// newDevToken makes the dev token: 32 random bytes.
func newDevToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "writd_" + hex.EncodeToString(b), nil
}

// WriteDevClient writes the client.json of a dev control plane at url,
// with its token, into the data directory dir, as writ serve --dev does.
// A test's in-process server writes one so the CLI finds it.
func WriteDevClient(dir, url, token, user string) error {
	return writeDevClient(filepath.Join(dir, DevClientFile), devClient{Format: devClientFormat, URL: url, Token: token, User: user, PID: os.Getpid()})
}

// writeDevClient writes client.json with mode 0600, by a rename so no
// reader sees half of it.
func writeDevClient(path string, c devClient) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeDevClient removes client.json when it still holds token, so a
// later dev control plane's file stays.
func removeDevClient(path, token string) {
	c, err := readDevClient(path)
	if err == nil && c.Token == token {
		os.Remove(path)
	}
}

func readDevClient(path string) (devClient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return devClient{}, err
	}
	var c devClient
	if err := json.Unmarshal(b, &c); err != nil || c.Format != devClientFormat || c.URL == "" || c.Token == "" {
		return devClient{}, fmt.Errorf("%s is not a %s document", path, devClientFormat)
	}
	return c, nil
}

// devRunning reports the URL of the dev control plane client.json names,
// when one answers there.
func devRunning(ctx context.Context, path string) (string, bool) {
	c, err := readDevClient(path)
	if err != nil {
		return "", false
	}
	return c.URL, answersDev(ctx, c)
}

// answersDev reports whether the dev control plane c names answers GET
// /v1/server at its URL. While the process that wrote c lives, it waits as
// long as any request does: each answer waits for its audit row's commit,
// which takes seconds on a busy disk. When that process is gone, the port
// may belong to another program, so it waits only devProbeTimeout. A dev
// control plane listens on loopback, so when none runs the connection is
// refused at once.
func answersDev(ctx context.Context, c devClient) bool {
	wait := devProbeTimeout
	if processAlive(c.PID) {
		wait = remoteTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.URL, "/")+"/v1/server", nil)
	if err != nil {
		return false
	}
	resp, err := apiClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var info server.ServerInfo
	if resp.StatusCode != http.StatusOK || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&info) != nil {
		return false
	}
	return info.Format == server.FormatServer && info.Dev
}

// osUser returns the name of the user writ runs as.
func (a *App) osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name, ok := a.opts.LookupEnv("USER"); ok && name != "" {
		return name
	}
	return "uid-" + strconv.Itoa(os.Getuid())
}

// devProbeTimeout bounds the probe of a dev control plane whose process is
// gone, whose port another program may hold.
const devProbeTimeout = 2 * time.Second

// processAlive reports whether a process with this id exists. A process
// of another user counts.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
