package provider

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mr-karan/writ/internal/schema"
)

// fakeRegistry serves acme/fake 1.0.0 for the local and a foreign platform over
// the registry protocol. The binary is not runnable, so tests seed the schema
// cache for it and Install never starts it.
type fakeRegistry struct {
	srv           *httptest.Server
	src           schema.Source
	zip           []byte
	foreignMarker string
	foreignBinary []byte
	foreignZip    []byte
	binary        []byte
	shasum        string // what the download endpoint claims
	metadata      atomic.Int32
	downloads     atomic.Int32
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{binary: []byte("not really a provider\n")}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string][]byte{"LICENSE": []byte("MPL"), "terraform-provider-fake_v1.0.0_x5": f.binary} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.zip = buf.Bytes()
	f.foreignMarker = filepath.Join(t.TempDir(), "foreign-started")
	f.foreignBinary = []byte(fmt.Sprintf("#!/bin/sh\necho started > %q\nexit 99\n", f.foreignMarker))
	var foreign bytes.Buffer
	fzw := zip.NewWriter(&foreign)
	fw, err := fzw.Create("terraform-provider-fake_v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(f.foreignBinary); err != nil {
		t.Fatal(err)
	}
	if err := fzw.Close(); err != nil {
		t.Fatal(err)
	}
	f.foreignZip = foreign.Bytes()

	sum := sha256.Sum256(f.zip)
	f.shasum = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/terraform.json", func(w http.ResponseWriter, r *http.Request) {
		f.metadata.Add(1)
		w.Write([]byte(`{"providers.v1": "/v1/providers/"}`))
	})
	mux.HandleFunc("GET /v1/providers/acme/fake/versions", func(w http.ResponseWriter, r *http.Request) {
		f.metadata.Add(1)
		platforms := []registryPlatform{}
		for _, p := range []string{CurrentPlatform(), foreignPlatform()} {
			os, arch, _ := strings.Cut(p, "_")
			platforms = append(platforms, registryPlatform{OS: os, Arch: arch})
		}
		json.NewEncoder(w).Encode(registryVersions{Versions: []registryVersion{{Version: "1.0.0", Protocols: []string{"5.0"}, Platforms: platforms}}})
	})
	mux.HandleFunc("GET /v1/providers/acme/fake/1.0.0/download/{os}/{arch}", func(w http.ResponseWriter, r *http.Request) {
		f.metadata.Add(1)
		shasum, url := f.shasum, "/files/fake.zip"
		if r.PathValue("os")+"_"+r.PathValue("arch") != CurrentPlatform() {
			sum := sha256.Sum256(f.foreignZip)
			shasum, url = hex.EncodeToString(sum[:]), "/files/darwin.zip"
			if f.shasum == strings.Repeat("ab", 32) {
				shasum = f.shasum
			}
		}
		json.NewEncoder(w).Encode(registryDownload{
			Protocols:   []string{"5.0"},
			Filename:    "terraform-provider-fake_1.0.0_linux_amd64.zip",
			DownloadURL: url,
			Shasum:      shasum,
		})
	})
	mux.HandleFunc("GET /files/fake.zip", func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		w.Write(f.zip)
	})
	mux.HandleFunc("GET /files/darwin.zip", func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		w.Write(f.foreignZip)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	f.src = schema.Source{Host: f.srv.Listener.Addr().String(), Namespace: "acme", Type: "fake"}
	return f
}

func (f *fakeRegistry) req() Requirement {
	return Requirement{Name: "fake", Source: f.src, Version: "1.0.0"}
}

func (f *fakeRegistry) options(dirs Dirs) InstallOptions {
	return InstallOptions{Dirs: dirs, Platforms: []string{CurrentPlatform()}, HTTP: f.srv.Client()}
}

// seedSchema caches a schema for the fake binary, as if an earlier install
// had fetched it.
func (f *fakeRegistry) seedSchema(t *testing.T, dirs Dirs) {
	t.Helper()
	cs := &cachedSchema{Protocol: 5, Resources: map[string]rawResource{
		"fake_thing": {Block: rawBlock{Attributes: []rawAttribute{{Name: "id", Type: json.RawMessage(`"string"`), Computed: true}}}},
	}}
	if err := writeSchemaCache(dirs, sha256.Sum256(f.binary), cs); err != nil {
		t.Fatal(err)
	}
}

func TestInstallFromRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	dirs := Dirs{Cache: t.TempDir()}
	f.seedSchema(t, dirs)
	ctx := context.Background()

	lock, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{f.req()})
	if err != nil {
		t.Fatal(err)
	}
	e, ok := lock.find(f.src, "1.0.0", CurrentPlatform())
	if !ok {
		t.Fatalf("lock has no entry:\n%s", lock.Encode())
	}
	if e.Archive != sha256.Sum256(f.zip) {
		t.Errorf("archive hash %x, want the zip's %x", e.Archive, sha256.Sum256(f.zip))
	}
	if e.Binary != sha256.Sum256(f.binary) {
		t.Errorf("binary hash %x, want the extracted binary's %x", e.Binary, sha256.Sum256(f.binary))
	}
	bin := dirs.BinaryPath(f.src, "1.0.0", CurrentPlatform())
	got, err := os.ReadFile(bin)
	if err != nil || !bytes.Equal(got, f.binary) {
		t.Fatalf("installed binary = %q, %v", got, err)
	}
	if info, _ := os.Stat(bin); info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode %v is not executable", info.Mode())
	}
	if n := f.downloads.Load(); n != 1 {
		t.Errorf("downloaded %d times, want 1", n)
	}

	t.Run("pinned and cached needs no network", func(t *testing.T) {
		before := f.metadata.Load() + f.downloads.Load()
		again, err := Install(ctx, f.options(dirs), lock, []Requirement{f.req()})
		if err != nil {
			t.Fatal(err)
		}
		if after := f.metadata.Load() + f.downloads.Load(); after != before {
			t.Errorf("made %d registry requests, want none", after-before)
		}
		if again.Digest() != lock.Digest() {
			t.Errorf("lock changed:\n%s", again.Encode())
		}
	})

	t.Run("cached but unpinned is not downloaded again", func(t *testing.T) {
		again, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{f.req()})
		if err != nil {
			t.Fatal(err)
		}
		if n := f.downloads.Load(); n != 1 {
			t.Errorf("downloaded %d times in all, want 1", n)
		}
		if again.Digest() != lock.Digest() {
			t.Errorf("lock differs from the first install:\n%s", again.Encode())
		}
	})

	t.Run("a changed binary is downloaded again", func(t *testing.T) {
		if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Install(ctx, f.options(dirs), lock, []Requirement{f.req()}); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(bin); !bytes.Equal(got, f.binary) {
			t.Errorf("binary after reinstall = %q", got)
		}
		if n := f.downloads.Load(); n != 2 {
			t.Errorf("downloaded %d times in all, want 2", n)
		}
	})
}

func TestInstallRefusesBadHashes(t *testing.T) {
	ctx := context.Background()
	t.Run("archive differs from the registry's shasum", func(t *testing.T) {
		f := newFakeRegistry(t)
		f.shasum = strings.Repeat("ab", 32)
		dirs := Dirs{Cache: t.TempDir()}
		f.seedSchema(t, dirs)
		_, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{f.req()})
		if err == nil || !strings.Contains(err.Error(), "does not match the registry's") {
			t.Fatalf("Install error = %v, want a hash mismatch", err)
		}
		if _, err := os.Stat(dirs.BinaryPath(f.src, "1.0.0", CurrentPlatform())); err == nil {
			t.Error("a binary from an unverified archive was installed")
		}
	})
	t.Run("registry differs from the pin", func(t *testing.T) {
		f := newFakeRegistry(t)
		dirs := Dirs{Cache: t.TempDir()}
		f.seedSchema(t, dirs)
		pinned := (&Lock{}).with(LockEntry{Source: f.src, Version: "1.0.0", Platform: CurrentPlatform(), Archive: hash(1), Binary: hash(2)})
		_, err := Install(ctx, f.options(dirs), pinned, []Requirement{f.req()})
		if err == nil || !strings.Contains(err.Error(), "never re-pins") {
			t.Fatalf("Install error = %v, want a refusal to re-pin", err)
		}
		if n := f.downloads.Load(); n != 0 {
			t.Errorf("downloaded %d times, want none", n)
		}
	})
	t.Run("version not in the registry", func(t *testing.T) {
		f := newFakeRegistry(t)
		req := f.req()
		req.Version = "2.0.0"
		_, err := Install(ctx, f.options(Dirs{Cache: t.TempDir()}), &Lock{}, []Requirement{req})
		if err == nil || !strings.Contains(err.Error(), "has no version 2.0.0") {
			t.Fatalf("Install error = %v", err)
		}
	})
}

// The binary is not runnable and no schema is cached for it in a fresh
// cache, so a refusal that is ErrUnlistedArchive came before any start.
func TestInstallRefusesAnUnlistedArchive(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	listed := f.req()
	listed.Archives = [][32]byte{hash(1), sha256.Sum256(f.zip)}
	unlisted := f.req()
	unlisted.Archives = [][32]byte{hash(1)}
	refused := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnlistedArchive) || !strings.Contains(err.Error(), fmt.Sprintf("zh:%x", sha256.Sum256(f.zip))) {
			t.Fatalf("Install error = %v, want a refusal naming the archive's hash", err)
		}
	}

	t.Run("a download", func(t *testing.T) {
		dirs := Dirs{Cache: t.TempDir()}
		_, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{unlisted})
		refused(t, err)
		if n := f.downloads.Load(); n != 0 {
			t.Errorf("downloaded %d times, want none", n)
		}
		if _, err := os.Stat(dirs.BinaryPath(f.src, "1.0.0", CurrentPlatform())); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the binary of an unlisted archive was extracted: %v", err)
		}
	})

	dirs := Dirs{Cache: t.TempDir()}
	f.seedSchema(t, dirs)
	lock, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{listed})
	if err != nil {
		t.Fatalf("Install of a listed archive: %v", err)
	}
	t.Run("a pinned cached binary", func(t *testing.T) {
		_, err := Install(ctx, f.options(dirs), lock, []Requirement{unlisted})
		refused(t, err)
	})
	t.Run("an unpinned cached binary offline", func(t *testing.T) {
		opts := InstallOptions{Dirs: dirs, HTTP: &http.Client{Transport: noNetwork{t}}, Offline: true}
		_, err := Install(ctx, opts, &Lock{}, []Requirement{unlisted})
		refused(t, err)
	})
	t.Run("another platform", func(t *testing.T) {
		cache := t.TempDir()
		opts := f.options(Dirs{Cache: cache})
		opts.Platforms = []string{foreignPlatform()}
		before := f.downloads.Load()
		_, err := Install(ctx, opts, &Lock{}, []Requirement{listed})
		if !errors.Is(err, ErrUnlistedArchive) {
			t.Fatalf("Install error = %v, want the foreign archive refused", err)
		}
		if n := f.downloads.Load() - before; n != 0 {
			t.Errorf("downloaded the unlisted foreign archive %d times, want none", n)
		}
		if files := cachedFiles(t, cache); len(files) != 0 {
			t.Errorf("the cache holds %q after the refusal, want nothing", files)
		}
	})
	t.Run("another platform whose registry lies", func(t *testing.T) {
		lying := newFakeRegistry(t)
		lying.shasum = strings.Repeat("ab", 32)
		req := lying.req()
		req.Archives = [][32]byte{[32]byte(bytes.Repeat([]byte{0xab}, 32))}
		cache := t.TempDir()
		opts := lying.options(Dirs{Cache: cache})
		opts.Platforms = []string{foreignPlatform()}
		_, err := Install(ctx, opts, &Lock{}, []Requirement{req})
		if err == nil || !strings.Contains(err.Error(), "does not match the registry's") {
			t.Fatalf("Install error = %v, want the archive refused for its hash", err)
		}
		if files := cachedFiles(t, cache); len(files) != 0 {
			t.Errorf("the cache holds %q after the refusal, want nothing", files)
		}
	})
}

// cachedFiles is every file under dir that is not a directory.
func cachedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// noNetwork fails the test on any HTTP request.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("offline install made a request to %s", r.URL.Redacted())
	return nil, errors.New("offline")
}

func TestInstallOffline(t *testing.T) {
	ctx := context.Background()
	f := newFakeRegistry(t)
	dirs := Dirs{Cache: t.TempDir()}
	f.seedSchema(t, dirs)
	online, err := Install(ctx, f.options(dirs), &Lock{}, []Requirement{f.req()})
	if err != nil {
		t.Fatal(err)
	}
	offline := func(t *testing.T, dirs Dirs) InstallOptions {
		return InstallOptions{Dirs: dirs, Platforms: []string{CurrentPlatform()}, HTTP: &http.Client{Transport: noNetwork{t}}, Offline: true}
	}
	bin := dirs.BinaryPath(f.src, "1.0.0", CurrentPlatform())

	t.Run("unpinned and cached", func(t *testing.T) {
		lock, err := Install(ctx, offline(t, dirs), &Lock{}, []Requirement{f.req()})
		if err != nil {
			t.Fatal(err)
		}
		if lock.Digest() != online.Digest() {
			t.Errorf("offline lock:\n%s\nwant the online one:\n%s", lock.Encode(), online.Encode())
		}
	})
	t.Run("pinned and cached", func(t *testing.T) {
		lock, err := Install(ctx, offline(t, dirs), online, []Requirement{f.req()})
		if err != nil {
			t.Fatal(err)
		}
		if lock.Digest() != online.Digest() {
			t.Errorf("lock changed:\n%s", lock.Encode())
		}
	})
	t.Run("not cached", func(t *testing.T) {
		_, err := Install(ctx, offline(t, Dirs{Cache: t.TempDir()}), &Lock{}, []Requirement{f.req()})
		if err == nil || !strings.Contains(err.Error(), "acme/fake 1.0.0 "+CurrentPlatform()+" is not in the provider cache") {
			t.Fatalf("Install error = %v, want one naming the missing version", err)
		}
	})
	t.Run("pinned to another binary", func(t *testing.T) {
		pinned := (&Lock{}).with(LockEntry{Source: f.src, Version: "1.0.0", Platform: CurrentPlatform(), Archive: hash(1), Binary: hash(2)})
		_, err := Install(ctx, offline(t, dirs), pinned, []Requirement{f.req()})
		if err == nil || !strings.Contains(err.Error(), "does not match its hash in writ.lock") {
			t.Fatalf("Install error = %v, want a pin mismatch", err)
		}
	})
	t.Run("changed since it was extracted", func(t *testing.T) {
		if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Install(ctx, offline(t, dirs), &Lock{}, []Requirement{f.req()})
		if err == nil || !strings.Contains(err.Error(), "changed since it was extracted") {
			t.Fatalf("Install error = %v, want the cached binary refused", err)
		}
	})
}

func TestSchemasReportsMissing(t *testing.T) {
	dirs := Dirs{Cache: t.TempDir()}
	pinned := LockEntry{Source: tlsSrc, Version: "4.4.2", Platform: CurrentPlatform(), Archive: hash(1), Binary: hash(2)}
	lock := (&Lock{}).with(pinned)
	reqs := []Requirement{
		{Name: "tls", Source: tlsSrc, Version: "4.4.2"},       // pinned, schema not cached
		{Name: "random", Source: randomSrc, Version: "3.9.1"}, // not pinned
	}
	_, err := Schemas(dirs, lock, reqs)
	missing, ok := err.(*MissingError)
	if !ok {
		t.Fatalf("Schemas error = %v, want a *MissingError", err)
	}
	if len(missing.Missing) != 2 {
		t.Errorf("missing %v, want both requirements", missing.Missing)
	}
	if !strings.Contains(err.Error(), "install them online") {
		t.Errorf("error %q does not say how to fix it", err)
	}
}

func TestDefaultDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WRIT_CACHE_DIR", dir)
	d, err := DefaultDirs()
	if err != nil || d.Cache != dir {
		t.Errorf("DefaultDirs = %+v, %v; want %s", d, err, dir)
	}
}

func TestInstallPlatforms(t *testing.T) {
	f := newFakeRegistry(t)
	dirs := Dirs{Cache: t.TempDir()}
	f.seedSchema(t, dirs)
	opts := f.options(dirs)
	opts.Platforms = []string{CurrentPlatform(), foreignPlatform()}
	opts.Launch = Launch{}
	ctx := context.Background()
	local, err := Install(ctx, f.options(dirs), nil, []Requirement{f.req()})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := Install(ctx, opts, local, []Requirement{f.req()})
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Entries()) != 2 {
		t.Fatalf("lock: %s", lock.Encode())
	}
	for _, e := range lock.Entries() {
		binary, archive := f.binary, f.zip
		if e.Platform == foreignPlatform() {
			binary, archive = f.foreignBinary, f.foreignZip
		}
		if e.Binary != sha256.Sum256(binary) || e.Archive != sha256.Sum256(archive) {
			t.Fatalf("incorrect hashes: %s", e.Line())
		}
	}
	if !bytes.Contains(lock.Encode(), local.Encode()) {
		t.Fatal("existing row changed")
	}
	// No runnable foreign binary exists, and no schema is needed for it.
	if _, err := os.Stat(f.foreignMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign binary started: %v", err)
	}
	info, err := os.Stat(dirs.archivePath(f.src, "1.0.0", foreignPlatform()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 != 0 {
		t.Fatal("foreign archive is executable")
	}
	foreign := dirs.BinaryPath(f.src, "1.0.0", foreignPlatform())
	if _, err := os.Stat(foreign); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign binary extracted: %v", err)
	}
	before := f.metadata.Load() + f.downloads.Load()
	again, err := Install(ctx, opts, lock, []Requirement{f.req()})
	if err != nil || again.Digest() != lock.Digest() {
		t.Fatalf("re-init: %v", err)
	}
	if f.metadata.Load()+f.downloads.Load() != before {
		t.Fatal("re-init used network")
	}
	opts.Offline = true
	again, err = Install(ctx, opts, local, []Requirement{f.req()})
	if err != nil || again.Digest() != lock.Digest() {
		t.Fatalf("cached foreign archive: %v", err)
	}
	opts.Dirs = Dirs{Cache: t.TempDir()}
	opts.Platforms = []string{foreignPlatform()}
	_, err = Install(ctx, opts, nil, []Requirement{f.req()})
	if err == nil || !strings.Contains(err.Error(), foreignPlatform()) || !strings.Contains(err.Error(), "install it online once to pin "+foreignPlatform()) {
		t.Fatalf("offline error: %v", err)
	}
	// A pinned foreign platform does not need an archive or schema, even offline.
	again, err = Install(ctx, opts, lock, []Requirement{f.req()})
	if err != nil || again.Digest() != lock.Digest() {
		t.Fatalf("pinned foreign platform: %v", err)
	}
	opts.Offline = false
	f.shasum = strings.Repeat("ab", 32)
	_, err = Install(ctx, opts, nil, []Requirement{f.req()})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("foreign archive mismatch: %v", err)
	}
}

// TestValidatePlatform pins the platform names Install accepts: a
// platform becomes part of a download path, so anything but a known
// os_arch pair is refused.
func TestValidatePlatform(t *testing.T) {
	for _, tc := range []struct {
		platform string
		valid    bool
	}{
		{"linux_amd64", true}, {"darwin_arm64", true}, {"linux_arm64", true},
		{"", false}, {"linux", false}, {"_amd64", false},
		{"../linux_amd64", false}, {"linux_amd64_extra", false}, {"linux_amd64/..", false},
		{"linuz_amd64", false}, {"linux_bogus", false},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			if err := validatePlatform(tc.platform); (err == nil) != tc.valid {
				t.Fatalf("validatePlatform(%q): %v", tc.platform, err)
			}
		})
	}
}

func TestForeignArchiveBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxArchive + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = hashArchive(path, "fake", [32]byte{})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(maxArchive)) {
		t.Fatalf("archive bound: %v", err)
	}
}

func foreignPlatform() string {
	if CurrentPlatform() == "darwin_arm64" {
		return "linux_amd64"
	}
	return "darwin_arm64"
}
