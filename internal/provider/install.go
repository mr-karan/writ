package provider

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mr-karan/writ/internal/schema"
)

const (
	installParallelism       = 4
	downloadTimeout          = 10 * time.Minute
	maxArchive               = 1 << 30
	maxBinary          int64 = 2 << 30
)

// Dirs locates the per-user cache shared by every stack:
//
//	<Cache>/providers/<host>/<namespace>/<type>/<version>/<os_arch>/terraform-provider-<type>_v<version>
//	<Cache>/providers/.../terraform-provider-<type>_v<version>.sum
//	<Cache>/schemas/v<format>/<binary sha256 hex>/ (see schemaDir)
//
// The .sum file records the archive and binary hashes of the cached binary,
// so a stack with no lock entry yet can reuse it without downloading.
type Dirs struct {
	Cache string
}

// DefaultDirs returns the cache at WRIT_CACHE_DIR, or at
// os.UserCacheDir()/writ when that is unset.
func DefaultDirs() (Dirs, error) {
	dir := os.Getenv("WRIT_CACHE_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return Dirs{}, fmt.Errorf("find the user cache directory: %w; set WRIT_CACHE_DIR", err)
		}
		dir = filepath.Join(base, "writ")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{Cache: abs}, nil
}

// BinaryPath is where the provider binary of src, version, and platform lives.
func (d Dirs) BinaryPath(src schema.Source, version, platform string) string {
	return filepath.Join(d.Cache, "providers", host(src), src.Namespace, src.Type, version, platform,
		"terraform-provider-"+src.Type+"_v"+version)
}

// InstallOptions configures Install.
type InstallOptions struct {
	Dirs Dirs
	HTTP *http.Client
	Log  *slog.Logger
	// Offline resolves every requirement from the cache alone, verified
	// against writ.lock or the hashes recorded when it was extracted, and
	// never contacts a registry. A version that is not cached is an error.
	Offline bool
	// Launch is how the providers start to report their schemas.
	Launch Launch
}

// Install installs each requirement for the current platform.
//
// A requirement pinned in lock whose cached binary matches the pinned hash
// needs no network. Otherwise Install asks the registry for the release,
// refuses an archive hash that differs from the pin (it never re-pins),
// downloads and verifies the archive unless the cache already holds it, and
// extracts the binary. A provider whose schema is not cached is started once
// to fetch it. With Offline, a requirement that would need the registry is an
// error instead. GPG verification of the SHA256SUMS signature is not in version
// 0; see the open questions in docs/architecture.md.
func Install(ctx context.Context, opts InstallOptions, lock *Lock, reqs []Requirement) (*Lock, error) {
	if lock == nil {
		lock = &Lock{}
	}
	in := installer{dirs: opts.Dirs, platform: CurrentPlatform(), http: opts.HTTP, log: opts.Log, offline: opts.Offline, launch: opts.Launch}
	if in.http == nil {
		in.http = &http.Client{Timeout: downloadTimeout}
	}
	if in.log == nil {
		in.log = slog.New(slog.DiscardHandler)
	}
	reqs = uniqueRequirements(reqs)
	entries := make([]LockEntry, len(reqs))
	errs := make([]error, len(entries))
	sem := make(chan struct{}, installParallelism)
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			entries[i], errs[i] = in.install(ctx, lock, req)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	out := &Lock{entries: slices.Clone(lock.entries)}
	for _, e := range entries {
		out = out.with(e)
	}
	return out, nil
}

func uniqueRequirements(reqs []Requirement) []Requirement {
	var out []Requirement
	seen := make(map[string]bool, len(reqs))
	for _, r := range reqs {
		k := r.Source.String() + " " + r.Version
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

type installer struct {
	dirs     Dirs
	platform string
	http     *http.Client
	log      *slog.Logger
	offline  bool
	launch   Launch
}

func (in installer) install(ctx context.Context, lock *Lock, req Requirement) (LockEntry, error) {
	if err := checkPathPart("version", req.Version); err != nil {
		return LockEntry{}, fmt.Errorf("%s: %w", req.Source, err)
	}
	bin := in.dirs.BinaryPath(req.Source, req.Version, in.platform)
	pinned, isPinned := lock.find(req.Source, req.Version, in.platform)
	if isPinned {
		if err := req.admit(in.platform, pinned.Archive); err != nil {
			return LockEntry{}, err
		}
		if sum, err := binaryHash(bin, false, in.log); err == nil && sum == pinned.Binary {
			in.log.Debug("provider already installed", "source", req.Source.String(), "version", req.Version)
			return pinned, in.ensureSchema(ctx, req, pinned, bin)
		}
	}
	if in.offline {
		return in.fromCache(ctx, req, bin, isPinned)
	}
	rel, err := registry{http: in.http}.release(ctx, req.Source, req.Version, in.platform)
	if err != nil {
		return LockEntry{}, err
	}
	if isPinned && rel.archive != pinned.Archive {
		return LockEntry{}, fmt.Errorf("%s %s %s: the registry's archive hash %x differs from %s (%x); writ never re-pins a version",
			req.Source, req.Version, in.platform, rel.archive, LockFile, pinned.Archive)
	}
	if err := req.admit(in.platform, rel.archive); err != nil {
		return LockEntry{}, err
	}
	entry := LockEntry{Source: req.Source, Version: req.Version, Platform: in.platform, Archive: rel.archive}
	if archive, binary, err := cachedBinary(bin, in.log); err == nil && archive == rel.archive {
		entry.Binary = binary
	} else if entry.Binary, err = in.download(ctx, req, rel, bin); err != nil {
		return LockEntry{}, err
	}
	if isPinned && entry.Binary != pinned.Binary {
		return LockEntry{}, fmt.Errorf("%s %s %s: the extracted binary's hash %x differs from %s (%x)",
			req.Source, req.Version, in.platform, entry.Binary, LockFile, pinned.Binary)
	}
	return entry, in.ensureSchema(ctx, req, entry, bin)
}

// fromCache pins a requirement to the cached binary and the archive hash its
// .sum file records, after checking the binary still has the hash recorded
// there. install has already found that a pinned requirement's binary is
// missing or differs from its pin.
func (in installer) fromCache(ctx context.Context, req Requirement, bin string, isPinned bool) (LockEntry, error) {
	what := fmt.Sprintf("%s %s %s", req.Source, req.Version, in.platform)
	if _, err := os.Stat(bin); errors.Is(err, fs.ErrNotExist) {
		return LockEntry{}, fmt.Errorf("%s is not in the provider cache at %s; install it online to download it", what, in.dirs.Cache)
	}
	if isPinned {
		return LockEntry{}, fmt.Errorf("%s: the cached binary %s does not match its hash in %s; install it online to download it again", what, bin, LockFile)
	}
	archive, binary, err := cachedBinary(bin, in.log)
	if err != nil {
		return LockEntry{}, fmt.Errorf("%s: %w; install it online to download it again", what, err)
	}
	if err := req.admit(in.platform, archive); err != nil {
		return LockEntry{}, err
	}
	entry := LockEntry{Source: req.Source, Version: req.Version, Platform: in.platform, Archive: archive, Binary: binary}
	in.log.Debug("using cached provider", "source", req.Source.String(), "version", req.Version)
	return entry, in.ensureSchema(ctx, req, entry, bin)
}

// cachedBinary returns the archive and binary hashes the .sum file recorded
// when bin was extracted, after checking that bin still has that hash.
func cachedBinary(bin string, log *slog.Logger) (archive, binary [32]byte, err error) {
	data, err := os.ReadFile(bin + ".sum")
	if err != nil {
		return archive, binary, err
	}
	f := strings.Fields(string(data))
	if len(f) != 2 {
		return archive, binary, fmt.Errorf("%s.sum is malformed", bin)
	}
	if archive, err = parseHash(f[0], "archive"); err != nil {
		return archive, binary, err
	}
	if binary, err = parseHash(f[1], "binary"); err != nil {
		return archive, binary, err
	}
	sum, err := binaryHash(bin, false, log)
	if err != nil {
		return archive, binary, err
	}
	if sum != binary {
		return archive, binary, fmt.Errorf("%s changed since it was extracted", bin)
	}
	return archive, binary, nil
}

// download fetches the release archive, checks its SHA-256 against the
// registry's, and extracts the provider binary to bin.
func (in installer) download(ctx context.Context, req Requirement, rel release, bin string) ([32]byte, error) {
	dir := filepath.Dir(bin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return [32]byte{}, err
	}
	zf, err := os.CreateTemp(dir, ".download-*.zip")
	if err != nil {
		return [32]byte{}, err
	}
	defer os.Remove(zf.Name())
	defer zf.Close()
	n, archive, err := in.fetch(ctx, rel, zf)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%s %s: download %s: %w", req.Source, req.Version, rel.url.Redacted(), err)
	}
	if archive != rel.archive {
		return [32]byte{}, fmt.Errorf("%s %s: archive sha256 %x does not match the registry's %x", req.Source, req.Version, archive, rel.archive)
	}
	sum, err := extractBinary(zf, n, req.Source.Type, bin)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%s %s: %w", req.Source, req.Version, err)
	}
	marker := fmt.Sprintf("archive=sha256:%x binary=sha256:%x\n", archive, sum)
	if err := writeFileAtomic(bin+".sum", []byte(marker), 0o644); err != nil {
		return [32]byte{}, err
	}
	in.log.Info("installed provider", "source", req.Source.String(), "version", req.Version, "platform", in.platform)
	return sum, nil
}

func (in installer) fetch(ctx context.Context, rel release, w io.Writer) (int64, [32]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.url.String(), nil)
	if err != nil {
		return 0, [32]byte{}, err
	}
	resp, err := in.http.Do(httpReq)
	if err != nil {
		return 0, [32]byte{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, [32]byte{}, errors.New(resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return 0, [32]byte{}, err
	}
	if n > maxArchive {
		return 0, [32]byte{}, fmt.Errorf("archive is larger than %d bytes", maxArchive)
	}
	return n, [32]byte(h.Sum(nil)), nil
}

// extractBinary writes the archive's terraform-provider-<type> file to bin
// through a temporary file and a rename, so a concurrent reader never runs a
// partial binary. Release archives name it with or without a version suffix,
// so it is found by prefix.
func extractBinary(r io.ReaderAt, size int64, typ, bin string) ([32]byte, error) {
	rc, err := archiveBinary(r, size, typ)
	if err != nil {
		return [32]byte{}, err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(bin), ".extract-*")
	if err != nil {
		return [32]byte{}, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(rc, maxBinary+1))
	if err == nil && n > maxBinary {
		err = fmt.Errorf("provider binary is larger than %d bytes", maxBinary)
	}
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return [32]byte{}, err
	}
	if err := os.Rename(tmp.Name(), bin); err != nil {
		return [32]byte{}, err
	}
	return [32]byte(h.Sum(nil)), nil
}

// ensureSchema caches the provider's schema unless a cached schema reads in
// full. Reading every type of a large provider costs a fraction of a second,
// which init can afford, and it lets init repair a damaged cache.
func (in installer) ensureSchema(ctx context.Context, req Requirement, entry LockEntry, bin string) error {
	cache, err := openSchemaCache(in.dirs, entry.Binary)
	if err == nil {
		p, _, err := cache.build(req.Source, req.Version)
		if err == nil {
			err = loadAll(p)
		}
		if err == nil {
			return nil
		}
		in.log.Warn("recaching unreadable provider schema", "source", req.Source.String(), "version", req.Version, "error", err)
	} else if !errors.Is(err, errNoSchema) {
		return err
	}
	cs, err := fetchSchema(ctx, bin, in.launch)
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Source, req.Version, err)
	}
	if _, _, err := buildSchema(req.Source, req.Version, cs); err != nil {
		return err
	}
	in.log.Info("cached provider schema", "source", req.Source.String(), "version", req.Version, "protocol", cs.Protocol)
	return writeSchemaCache(in.dirs, entry.Binary, cs)
}

// fetchSchema starts the provider once and asks for its schema.
func fetchSchema(ctx context.Context, bin string, launch Launch) (cs *cachedSchema, err error) {
	proc, err := start(ctx, bin, launch)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, proc.shutdown(context.WithoutCancel(ctx))) }()
	sctx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	cs, wd, err := proc.client().getSchema(sctx)
	if err != nil {
		return nil, proc.failed(fmt.Errorf("GetProviderSchema: %w", err))
	}
	diags := make([]Diag, len(wd))
	for i, d := range wd {
		diags[i] = Diag{Error: d.error, Summary: d.summary, Detail: d.detail, Path: wireBlock{}.path(d.path)}
	}
	if hasError(diags) {
		return nil, &Error{Op: "GetProviderSchema", Diags: diags}
	}
	return cs, nil
}

// checkPathPart rejects a version or platform that could step outside its
// cache directory.
func checkPathPart(what, s string) error {
	ok := s != "" && s != "." && s != ".." && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r))
	})
	if !ok {
		return fmt.Errorf("%s %q may hold only letters, digits, and . _ + -", what, s)
	}
	return nil
}

func archiveBinary(r io.ReaderAt, size int64, typ string) (io.ReadCloser, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, err
	}
	var found *zip.File
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "terraform-provider-"+typ) && !strings.ContainsAny(f.Name, `/\`) && f.Mode().IsRegular() {
			if found != nil {
				return nil, fmt.Errorf("archive holds two provider binaries, %s and %s", found.Name, f.Name)
			}
			found = f
		}
	}
	if found == nil {
		return nil, fmt.Errorf("archive holds no terraform-provider-%s binary", typ)
	}
	rc, err := found.Open()
	if err != nil {
		return nil, err
	}
	return rc, nil
}
