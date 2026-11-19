package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/runas"
)

// IsolationProbeEnv, set in writ's environment, makes writ run the
// isolation probe it names instead of a command. writ serve --check runs
// writ itself as a provider uid with it.
const IsolationProbeEnv = "WRIT_ISOLATION_PROBE"

// metadataAddrs are the instance metadata service's addresses on EC2.
var metadataAddrs = []string{"169.254.169.254:80", "[fd00:ec2::254]:80"}

const reachTimeout = 2 * time.Second

// probeRequest is what the probe tries as a provider's user.
type probeRequest struct {
	// Read are files and directories it must not read.
	Read []string `json:"read"`
	// Reach are addresses it must not reach.
	Reach []string `json:"reach"`
}

// probeResult maps each path or address to "" when the probe read or
// reached it, and otherwise to the error.
type probeResult struct {
	UID   int               `json:"uid"`
	Read  map[string]string `json:"read"`
	Reach map[string]string `json:"reach"`
}

// RunIsolationProbe is the probe: it tries each item of the request and
// writes what happened as JSON.
func RunIsolationProbe(req string, out io.Writer) int {
	var r probeRequest
	if err := json.Unmarshal([]byte(req), &r); err != nil {
		fmt.Fprintf(out, "writ: the isolation probe request: %v\n", err)
		return 2
	}
	res := probeResult{UID: os.Getuid(), Read: map[string]string{}, Reach: map[string]string{}}
	for _, p := range r.Read {
		res.Read[p] = errText(readOrList(p))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range r.Reach {
		wg.Go(func() {
			err := dial(a)
			mu.Lock()
			res.Reach[a] = errText(err)
			mu.Unlock()
		})
	}
	wg.Wait()
	if err := json.NewEncoder(out).Encode(res); err != nil {
		return 1
	}
	return 0
}

func readOrList(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	_, err = f.Read(make([]byte, 1))
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func dial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, reachTimeout)
	if err != nil {
		return err
	}
	return c.Close()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Check is one line of writ serve --check's isolation part.
type Check struct {
	What string
	// Err says why the check failed, "" when it passed.
	Err string
	// Moot marks a check that proves nothing on this host, such as the
	// metadata check where writ itself reaches no metadata service.
	Moot bool
}

// CheckIsolation runs exe, the writ binary, as the first provider uid
// with nothing of writ's environment. It checks that uid cannot read the
// state keys, the provider_env files, the TLS key, or the data directory,
// nor reach the
// instance metadata addresses that writ itself reaches. It needs the
// capabilities writ serve needs, and says so when it lacks them.
func CheckIsolation(ctx context.Context, cfg *Config, exe string) ([]Check, error) {
	r := cfg.ProviderUIDs
	if r == nil {
		return nil, nil
	}
	user := runas.User{UID: r.First, GID: r.First}
	secret := map[string]string{cfg.Data: "data"}
	if cfg.TLS != nil {
		secret[cfg.TLS.Key] = "tls.key"
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Envs)) {
		e := cfg.Envs[name]
		secret[e.Key] = "the state key of env " + name
		for v, p := range e.ProviderEnv {
			secret[p] = "provider_env " + v + " of env " + name
		}
	}
	req := probeRequest{Read: slices.Sorted(maps.Keys(secret)), Reach: metadataAddrs}
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := user.Command(ctx, exe)
	cmd.Env = append(slices.Clone(baseProviderEnv), IsolationProbeEnv+"="+string(b))
	cmd.Dir = "/"
	var out, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return nil, fmt.Errorf("run writ as provider uid %d: %w", user.UID, err)
	}
	var res probeResult
	if err := json.Unmarshal([]byte(out.String()), &res); err != nil || res.UID != int(user.UID) {
		return nil, fmt.Errorf("the probe as uid %d answered %q: %v", user.UID, out.String(), err)
	}
	var checks []Check
	for _, p := range req.Read {
		c := Check{What: fmt.Sprintf("uid %d cannot read %s, %s", user.UID, secret[p], p)}
		switch e := res.Read[p]; {
		case e == "":
			c.Err = "it can"
		case !strings.Contains(e, "permission denied"):
			c.Err, c.Moot = e, true
		}
		checks = append(checks, c)
	}
	for _, a := range req.Reach {
		c := Check{What: fmt.Sprintf("uid %d cannot reach %s", user.UID, a)}
		switch {
		case res.Reach[a] == "":
			c.Err = "it can"
		case dial(a) != nil:
			c.Err, c.Moot = "writ cannot reach it either, so this host has no metadata service there to protect", true
		}
		checks = append(checks, c)
	}
	return checks, nil
}
