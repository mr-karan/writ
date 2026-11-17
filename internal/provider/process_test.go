package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A provider reads its credentials from the environment Writ runs in, as
// under Terraform, while Writ's plugin settings and log switches still win,
// and Writ's own settings and a parent plugin host's stay out.
func TestProviderInheritsEnvironment(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	bin := filepath.Join(dir, "provider")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s' \"$PROVIDER_ENV_PROBE\" \"$TF_LOG_SDK\" \"$WRIT_STATE\" \"$PLUGIN_CLIENT_CERT\" > '" + out + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVIDER_ENV_PROBE", "from-parent")
	t.Setenv("TF_LOG_SDK", "trace")
	t.Setenv("WRIT_STATE", "postgres://writ:secret@db/writ")
	t.Setenv("PLUGIN_CLIENT_CERT", "a-host-cert")

	if _, err := start(context.Background(), bin, Launch{}); err == nil {
		t.Fatal("a provider that sends no handshake must fail to start")
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(seen), "from-parent|off||"; got != want {
		t.Fatalf("provider saw %q, want %q", got, want)
	}
}

// Under OwnEnv a provider sees the launch's environment and its own
// directory, and nothing of Writ's: writ serve hands a provider one run's
// credentials this way.
func TestProviderOwnEnvironment(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "seen")
	bin := filepath.Join(dir, "provider")
	script := "#!/bin/sh\nprintf '%s|%s|%s|%s' \"$RUN_CREDENTIAL\" \"$PROVIDER_ENV_PROBE\" \"$HOME\" \"$(pwd)\" > '" + out + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PROVIDER_ENV_PROBE", "from-writ")
	t.Setenv("AWS_ACCESS_KEY_ID", "the-host-key")

	if _, err := start(context.Background(), bin, Launch{OwnEnv: true, Env: []string{"RUN_CREDENTIAL=one-run"}}); err == nil {
		t.Fatal("a provider that sends no handshake must fail to start")
	}
	seen, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Split(string(seen), "|")
	if len(f) != 4 || f[0] != "one-run" || f[1] != "" || f[2] == "" || f[2] != f[3] {
		t.Fatalf("provider saw %q, want the run's credential, nothing of writ's, and HOME as its directory", seen)
	}
	if _, err := os.Stat(f[2]); !os.IsNotExist(err) {
		t.Fatalf("the provider's directory %s is left: %v", f[2], err)
	}
}
