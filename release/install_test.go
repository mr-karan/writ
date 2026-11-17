// Package release tests install.sh, the script a new user pipes to sh.
// The tests run the real script with sh against a release directory that
// holds a stand-in writ, so they build no writ and need no network. The
// cases that sign or check a signature need minisign; nix develop has it.
package release

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// asset is the name install.sh downloads on this machine.
var asset = "writ-" + runtime.GOOS + "-" + runtime.GOARCH

// fakeWrit stands in for the binary: install.sh runs `writ version`.
const fakeWrit = "#!/bin/sh\necho writ v9.9.9\n"

// newRelease writes a release directory with binary as the writ for this
// machine and a SHA256SUMS line for it, and returns the directory.
func newRelease(t *testing.T, binary string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, asset), binary)
	writeSums(t, dir, binary)
	return dir
}

func writeSums(t *testing.T, dir, binary string) {
	t.Helper()
	sum := sha256.Sum256([]byte(binary))
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), fmt.Sprintf("%s  install.sh\n%s  %s\n", strings.Repeat("0", 64), hex.EncodeToString(sum[:]), asset))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// key is a minisign key pair made for one test.
type key struct{ secret, public string }

// exampleKey is a well-formed minisign public key whose secret key no test
// has, for the cases that never reach the check.
const exampleKey = "RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3"

func requireMinisign(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("minisign"); err != nil {
		t.Skip("minisign is not on PATH; nix develop has it")
	}
}

// newKey makes a minisign key pair without a password.
func newKey(t *testing.T) key {
	t.Helper()
	requireMinisign(t)
	dir := t.TempDir()
	sec, pub := filepath.Join(dir, "k.key"), filepath.Join(dir, "k.pub")
	run(t, "minisign", "-G", "-W", "-s", sec, "-p", pub)
	lines := strings.Split(strings.TrimSpace(readFile(t, pub)), "\n")
	return key{secret: sec, public: lines[len(lines)-1]}
}

// sign writes SHA256SUMS.minisig into the release directory.
func (k key) sign(t *testing.T, dir string) {
	t.Helper()
	run(t, "minisign", "-S", "-s", k.secret, "-m", filepath.Join(dir, "SHA256SUMS"))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// pathWithoutMinisign returns a PATH that has the tools install.sh uses and
// no minisign, as on a machine that never installed it.
func pathWithoutMinisign(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range []string{"sh", "uname", "sysctl", "mktemp", "rm", "cp", "cat", "awk", "sha256sum", "shasum", "cut", "mkdir", "chmod", "mv", "ln"} {
		if p, err := exec.LookPath(tool); err == nil {
			if err := os.Symlink(p, filepath.Join(dir, tool)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// installCmd returns install.sh run with sh, with only PATH and HOME from
// the test and the given variables.
func installCmd(t *testing.T, path string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	script, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append([]string{"PATH=" + path, "HOME=" + t.TempDir()}, env...)
	return cmd
}

// snapshot describes every entry under dir, without following symlinks,
// so a test can require that a refused install changed nothing.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			to, err := os.Readlink(path)
			got[rel] = "symlink to " + to
			return err
		case d.IsDir():
			got[rel] = "directory"
		default:
			got[rel] = "file " + readFile(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestInstall(t *testing.T) {
	tests := []struct {
		name string
		// binary is the writ of the release; default fakeWrit.
		binary string
		// unsigned skips signing; forge changes the release after signing.
		unsigned bool
		forge    func(t *testing.T, rel string)
		// noMinisign runs install.sh on a PATH without minisign.
		noMinisign bool
		// noKey gives install.sh no release key.
		noKey bool
		// noDir passes no --dir.
		noDir bool
		// prepare sets up the install directory, and outside, a directory
		// next to it, before the run.
		prepare func(t *testing.T, bin, outside string)
		env     []string
		args    []string
		wantErr string
		wantOut []string
	}{
		{name: "installs a signed binary that matches SHA256SUMS", wantOut: []string{"signature checked"}},
		{
			name: "replaces an older writ",
			prepare: func(t *testing.T, bin, _ string) {
				writeFile(t, filepath.Join(bin, "writ"), "#!/bin/sh\necho writ v0.0.1\n")
			},
		},
		{
			name: "installs into a relative --dir even with CDPATH set",
			// The test runs install.sh in a new directory, and CDPATH
			// names one that also has bin, which cd would pick and print.
			args: []string{"--dir", "bin"},
		},
		{
			name: "requires the version it was asked for",
			env:  []string{"WRIT_VERSION=v9.9.9"},
		},
		{
			name:    "refuses a --version with an @",
			args:    []string{"--version", "v9@bad"},
			wantErr: "version v9@bad has characters outside",
		},
		{
			name:    "refuses a WRIT_VERSION with an @",
			env:     []string{"WRIT_VERSION=v9@bad"},
			wantErr: "version v9@bad has characters outside",
		},
		{
			name:    "refuses a --version that is the placeholder",
			args:    []string{"--version", "@WRIT_VERSION@"},
			wantErr: "version @WRIT_VERSION@ has characters outside",
		},
		{
			name:    "refuses a WRIT_VERSION that is the placeholder",
			env:     []string{"WRIT_VERSION=@WRIT_VERSION@"},
			wantErr: "version @WRIT_VERSION@ has characters outside",
		},
		{
			name:    "refuses a WRIT_MINISIGN_PUBKEY that is the placeholder",
			env:     []string{"WRIT_MINISIGN_PUBKEY=@WRIT_MINISIGN_PUBKEY@"},
			wantErr: "the release key @WRIT_MINISIGN_PUBKEY@ is not a minisign public key",
		},
		{
			name:    "refuses a release key with an @",
			env:     []string{"WRIT_MINISIGN_PUBKEY=RWQ@bad"},
			wantErr: "the release key RWQ@bad is not a minisign public key",
		},
		{
			name:    "refuses a binary that reports another version",
			env:     []string{"WRIT_VERSION=v1.0.0"},
			wantErr: `reports "writ v9.9.9", not writ v1.0.0`,
		},
		{
			name:       "fails closed without minisign",
			unsigned:   true,
			noMinisign: true,
			wantErr:    "minisign is not installed, so the signature of SHA256SUMS cannot be checked; nothing installed. Install minisign",
		},
		{
			name:       "refuses a forged binary with a matching checksum and no signature when minisign is absent",
			binary:     "#!/bin/sh\necho writ v6.6.6-forged\n",
			unsigned:   true,
			noMinisign: true,
			wantErr:    "minisign is not installed",
		},
		{
			name:     "refuses a forged binary with a matching checksum and no signature",
			unsigned: true,
			wantErr:  "cannot get SHA256SUMS.minisig",
		},
		{
			name: "refuses a forged binary with a matching checksum and the old signature",
			forge: func(t *testing.T, rel string) {
				forged := "#!/bin/sh\necho writ v6.6.6-forged\n"
				writeFile(t, filepath.Join(rel, asset), forged)
				writeSums(t, rel, forged)
			},
			wantErr: "does not match the release key",
		},
		{
			name:    "refuses to check with no release key",
			noKey:   true,
			wantErr: "no release key to check the signature of SHA256SUMS with",
		},
		{
			name:    "refuses a release key that is not one",
			env:     []string{"WRIT_MINISIGN_PUBKEY=-x /etc/passwd"},
			wantErr: "is not a minisign public key",
		},
		{
			name:       "skips the signature only when told, and warns",
			unsigned:   true,
			noMinisign: true,
			noKey:      true,
			args:       []string{"--insecure-skip-signature"},
			wantOut:    []string{"WARNING: --insecure-skip-signature", "signature NOT checked"},
		},
		{
			name:     "checks the checksum when the signature is skipped",
			unsigned: true,
			forge: func(t *testing.T, rel string) {
				writeFile(t, filepath.Join(rel, asset), fakeWrit+"# changed\n")
			},
			args:    []string{"--insecure-skip-signature"},
			wantErr: "but SHA256SUMS says",
		},
		{
			name: "refuses a binary that does not match",
			forge: func(t *testing.T, rel string) {
				writeFile(t, filepath.Join(rel, asset), fakeWrit+"# changed\n")
			},
			wantErr: "but SHA256SUMS says",
		},
		{
			name:     "refuses a release with no line for this machine",
			unsigned: true,
			forge:    func(t *testing.T, rel string) { writeFile(t, filepath.Join(rel, "SHA256SUMS"), "") },
			args:     []string{"--insecure-skip-signature"},
			wantErr:  "SHA256SUMS has no line for " + asset,
		},
		{
			name:     "refuses a malformed line",
			unsigned: true,
			forge:    func(t *testing.T, rel string) { writeFile(t, filepath.Join(rel, "SHA256SUMS"), "abc  "+asset+"\n") },
			args:     []string{"--insecure-skip-signature"},
			wantErr:  "malformed line",
		},
		{
			name:    "refuses a release without SHA256SUMS",
			forge:   func(t *testing.T, rel string) { os.Remove(filepath.Join(rel, "SHA256SUMS")) },
			wantErr: "cannot get SHA256SUMS",
		},
		{
			name:    "refuses a download URL that is not https",
			env:     []string{"WRIT_BASE_URL=http://example.com"},
			wantErr: "must start with https://",
		},
		{
			name: "refuses a writ that is a symlink to a directory",
			prepare: func(t *testing.T, bin, outside string) {
				if err := os.Symlink(outside, filepath.Join(bin, "writ")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is a symlink",
		},
		{
			name: "refuses a writ that is a symlink to a file",
			prepare: func(t *testing.T, bin, outside string) {
				writeFile(t, filepath.Join(outside, "victim"), "keep me\n")
				if err := os.Symlink(filepath.Join(outside, "victim"), filepath.Join(bin, "writ")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is a symlink",
		},
		{
			name: "refuses a writ that is a directory",
			prepare: func(t *testing.T, bin, _ string) {
				if err := os.Mkdir(filepath.Join(bin, "writ"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "is not a regular file",
		},
		{
			name:   "fails and keeps the old writ when the new one does not run",
			binary: "#!/bin/sh\nexit 3\n",
			prepare: func(t *testing.T, bin, _ string) {
				writeFile(t, filepath.Join(bin, "writ"), "#!/bin/sh\necho writ v0.0.1\n")
			},
			wantErr: "the downloaded writ does not run on this machine; nothing installed",
		},
		{
			name:    "fails with no HOME and no --dir",
			noDir:   true,
			env:     []string{"HOME="},
			wantErr: "no install directory",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binary := tc.binary
			if binary == "" {
				binary = fakeWrit
			}
			rel := newRelease(t, binary)
			skipsSignature := slices.Contains(tc.args, "--insecure-skip-signature")
			if !tc.unsigned || (!tc.noMinisign && !skipsSignature) {
				requireMinisign(t)
			}
			public := exampleKey
			if !tc.unsigned {
				k := newKey(t)
				k.sign(t, rel)
				public = k.public
			}
			env := tc.env
			if !tc.noKey {
				env = append([]string{"WRIT_MINISIGN_PUBKEY=" + public}, env...)
			}
			if tc.forge != nil {
				tc.forge(t, rel)
			}
			path := os.Getenv("PATH")
			if tc.noMinisign {
				path = pathWithoutMinisign(t)
			}

			// The install directory sits in a fresh working directory, so a
			// relative --dir and an absolute one name the same place.
			cwd, outside := t.TempDir(), t.TempDir()
			bin := filepath.Join(cwd, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.prepare != nil {
				tc.prepare(t, bin, outside)
			}
			decoy := t.TempDir()
			if err := os.Mkdir(filepath.Join(decoy, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			beforeBin, beforeOutside := snapshot(t, bin), snapshot(t, outside)

			args := []string{"--from", rel}
			if !tc.noDir {
				args = append(args, "--dir", bin)
			}
			args = append(args, tc.args...)
			cmd := installCmd(t, path, append(env, "CDPATH="+decoy), args...)
			cmd.Dir = cwd
			b, err := cmd.CombinedOutput()
			out := string(b)

			if got := snapshot(t, outside); fmt.Sprint(got) != fmt.Sprint(beforeOutside) {
				t.Errorf("install.sh changed a directory outside --dir: %v, was %v", got, beforeOutside)
			}
			if got := snapshot(t, decoy); fmt.Sprint(got) != fmt.Sprint(map[string]string{".": "directory", "bin": "directory"}) {
				t.Errorf("install.sh wrote into the CDPATH directory: %v", got)
			}
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("install.sh succeeded, want an error with %q:\n%s", tc.wantErr, out)
				}
				if !strings.Contains(out, tc.wantErr) {
					t.Fatalf("output lacks %q:\n%s", tc.wantErr, out)
				}
				if got := snapshot(t, bin); fmt.Sprint(got) != fmt.Sprint(beforeBin) {
					t.Fatalf("a refused install changed %s: %v, was %v", bin, got, beforeBin)
				}
				return
			}
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%s", err, out)
			}
			for _, want := range append([]string{"installed writ v9.9.9 to " + filepath.Join(bin, "writ")}, tc.wantOut...) {
				if !strings.Contains(out, want) {
					t.Fatalf("output lacks %q:\n%s", want, out)
				}
			}
			if got, want := snapshot(t, bin), map[string]string{".": "directory", "writ": "file " + binary}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("install directory holds %v, want %v", got, want)
			}
			got, err := exec.Command(filepath.Join(bin, "writ")).Output()
			if err != nil || string(got) != "writ v9.9.9\n" {
				t.Fatalf("installed writ printed %q, %v", got, err)
			}
		})
	}
}

// TestInstallNeverWritesThroughASymlink plants a symlink at the name of the
// temporary file that an install.sh with a predictable name would use, its
// process ID, and requires the file the symlink names to stay unchanged.
func TestInstallNeverWritesThroughASymlink(t *testing.T) {
	k := newKey(t)
	rel := newRelease(t, fakeWrit)
	k.sign(t, rel)
	bin, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "victim")
	writeFile(t, victim, "keep me\n")

	// sh waits for a line, then execs install.sh, which keeps its process
	// ID, so the symlink is in place before install.sh starts.
	script, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `read -r line && exec sh "$@"`, "sh", script, "--from", rel, "--dir", bin)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "WRIT_MINISIGN_PUBKEY=" + k.public}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(bin, fmt.Sprintf(".writ.%d", cmd.Process.Pid))); err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(stdin)
	fmt.Fprintln(w, "go")
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	err = cmd.Wait()

	if got := readFile(t, victim); got != "keep me\n" {
		t.Fatalf("install.sh wrote through the planted symlink: the victim now holds %q", got)
	}
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, out.String())
	}
	fi, err := os.Lstat(filepath.Join(bin, "writ"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("writ is not a regular file: %v, %v", fi, err)
	}
	if got := readFile(t, filepath.Join(bin, "writ")); got != fakeWrit {
		t.Fatalf("writ holds %q", got)
	}
}

// TestStampedInstall stamps install.sh the way release/build.sh does. The
// stamped version must be enforced, so the check for an unstamped copy
// cannot clear a real version, and a placeholder that a user passes must
// be refused, so it cannot clear the stamped version either.
func TestStampedInstall(t *testing.T) {
	k := newKey(t)
	rel := newRelease(t, fakeWrit)
	k.sign(t, rel)
	src := readFile(t, "install.sh")
	stamped := strings.NewReplacer("@WRIT_VERSION@", "v1.0.0", "@WRIT_MINISIGN_PUBKEY@", k.public).Replace(src)
	if stamped == src {
		t.Fatal("install.sh has no placeholders to stamp")
	}
	script := filepath.Join(t.TempDir(), "install.sh")
	writeFile(t, script, stamped)
	tests := []struct {
		name    string
		env     []string
		args    []string
		wantErr string
	}{
		{name: "requires its version", wantErr: `reports "writ v9.9.9", not writ v1.0.0`},
		{
			name:    "refuses a --version that is the placeholder",
			args:    []string{"--version", "@WRIT_VERSION@"},
			wantErr: "version @WRIT_VERSION@ has characters outside",
		},
		{
			name:    "refuses a WRIT_VERSION that is the placeholder",
			env:     []string{"WRIT_VERSION=@WRIT_VERSION@"},
			wantErr: "version @WRIT_VERSION@ has characters outside",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			cmd := exec.Command("sh", append([]string{script, "--from", rel, "--dir", bin}, tc.args...)...)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, tc.env...)
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("install.sh = %v, want an error with %q:\n%s", err, tc.wantErr, out)
			}
			if entries, err := os.ReadDir(bin); err != nil || len(entries) != 0 {
				t.Fatalf("a refused install left %v in %s: %v", entries, bin, err)
			}
		})
	}
}

// shells returns sh and every other POSIX shell on PATH that install.sh
// should run under.
func shells(t *testing.T) []string {
	t.Helper()
	got := []string{"sh"}
	for _, name := range []string{"bash", "dash"} {
		if _, err := exec.LookPath(name); err == nil {
			got = append(got, name)
		}
	}
	return got
}

// hookedMv writes an mv into a new directory that runs hook, a shell
// snippet, when its last argument is target, and then runs the real mv
// with every argument. The directory goes first on PATH, so the hook runs
// in the gap between install.sh's last look at target and the rename.
func hookedMv(t *testing.T, target, hook string) string {
	t.Helper()
	real, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	if hook == "" {
		hook = ":"
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mv"), fmt.Sprintf("#!/bin/sh\nfor last; do :; done\nif [ \"$last\" = '%s' ]; then\n%s\nfi\nexec '%s' \"$@\"\n", target, hook, real))
	return dir
}

// TestInstallRenameNeverFollowsAPlantedSymlink makes writ a symlink to a
// directory outside --dir at the last moment before the rename, and
// requires that nothing lands outside --dir and no staged file is left.
func TestInstallRenameNeverFollowsAPlantedSymlink(t *testing.T) {
	k := newKey(t)
	rel := newRelease(t, fakeWrit)
	k.sign(t, rel)
	tests := []struct {
		name string
		// hook runs just before the rename; $last is the target.
		hook string
		// mv wraps the real mv, standing in for another platform's mv.
		mv      string
		wantErr string
		// wantWrit is what writ in --dir must be after the run.
		wantWrit string
	}{
		{
			name:     "a symlink to an outside directory is replaced, not followed",
			hook:     `ln -s "$OUTSIDE" "$last"`,
			wantWrit: "file " + fakeWrit,
		},
		{
			name:     "a symlink to an outside file is replaced, not followed",
			hook:     `ln -s "$OUTSIDE/victim" "$last"`,
			wantWrit: "file " + fakeWrit,
		},
		{
			name:     "a directory in its place fails the rename, and the staged file is removed",
			hook:     `mkdir "$last"`,
			wantErr:  "cannot move writ into place",
			wantWrit: "directory",
		},
		{
			name: "a BSD mv, which has -h and no -T, is used with -h",
			// GNU mv -T is what BSD mv -h does for a symlink at the target.
			mv: `for a; do case "$a" in -T) echo "mv: illegal option -- T" >&2; exit 64 ;; esac; done
if [ "$1" = -f ] && [ "$2" = -h ]; then shift 2; set -- -f -T "$@"; fi`,
			hook:     `ln -s "$OUTSIDE" "$last"`,
			wantWrit: "file " + fakeWrit,
		},
		{
			name:    "an mv with neither -T nor -h is refused before anything is written",
			mv:      `for a; do case "$a" in -T | -h) echo "mv: unknown option $a" >&2; exit 64 ;; esac; done`,
			wantErr: "this mv cannot rename onto a symlink without following it",
		},
	}
	for _, sh := range shells(t) {
		for _, tc := range tests {
			t.Run(sh+"/"+tc.name, func(t *testing.T) {
				bin, outside := t.TempDir(), t.TempDir()
				writeFile(t, filepath.Join(outside, "victim"), "keep me\n")
				target := filepath.Join(bin, "writ")
				hooks := hookedMv(t, target, tc.hook)
				if tc.mv != "" {
					// The platform stand-in runs first and passes on to the hook.
					writeFile(t, filepath.Join(hooks, "mv"), strings.Replace(readFile(t, filepath.Join(hooks, "mv")), "#!/bin/sh\n", "#!/bin/sh\n"+tc.mv+"\n", 1))
				}
				beforeOutside := snapshot(t, outside)

				script, err := filepath.Abs("install.sh")
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(sh, script, "--from", rel, "--dir", bin)
				cmd.Env = []string{"PATH=" + hooks + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "OUTSIDE=" + outside, "WRIT_MINISIGN_PUBKEY=" + k.public}
				b, err := cmd.CombinedOutput()
				out := string(b)

				if got := snapshot(t, outside); fmt.Sprint(got) != fmt.Sprint(beforeOutside) {
					t.Errorf("install.sh changed a directory outside --dir: %v, was %v\n%s", got, beforeOutside, out)
				}
				want := map[string]string{".": "directory"}
				if tc.wantWrit != "" {
					want["writ"] = tc.wantWrit
				}
				if got := snapshot(t, bin); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("install directory holds %v, want %v\n%s", got, want, out)
				}
				if tc.wantErr != "" {
					if err == nil || !strings.Contains(out, tc.wantErr) {
						t.Fatalf("got %v, want an error with %q:\n%s", err, tc.wantErr, out)
					}
					return
				}
				if err != nil || !strings.Contains(out, "installed writ v9.9.9 to "+target) {
					t.Fatalf("install.sh failed: %v\n%s", err, out)
				}
			})
		}
	}
}
