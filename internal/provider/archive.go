package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mr-karan/writ/internal/schema"
)

func (d Dirs) archivePath(src schema.Source, version, platform string) string {
	return filepath.Join(d.Cache, "archives", host(src), src.Namespace, src.Type, version, platform+".zip")
}

func (in installer) pinArchive(ctx context.Context, lock *Lock, req Requirement) (LockEntry, error) {
	if err := checkPathPart("version", req.Version); err != nil {
		return LockEntry{}, err
	}
	if e, ok := lock.find(req.Source, req.Version, in.platform); ok {
		return e, req.admit(in.platform, e.Archive)
	}
	path := in.dirs.archivePath(req.Source, req.Version, in.platform)
	var archive [32]byte
	if in.offline {
		data, err := os.ReadFile(path + ".sum")
		if err != nil {
			return LockEntry{}, fmt.Errorf("%s %s %s: no verified cached archive; install it online once to pin %s", req.Source, req.Version, in.platform, in.platform)
		}
		archive, err = parseHash(string(data), "archive")
		if err != nil {
			return LockEntry{}, err
		}
		if err := req.admit(in.platform, archive); err != nil {
			return LockEntry{}, err
		}
	} else {
		rel, err := (registry{http: in.http}).release(ctx, req.Source, req.Version, in.platform)
		if err != nil {
			return LockEntry{}, err
		}
		// The download must hash to rel.archive before it reaches the
		// cache, so an archive the requirement does not list is never
		// fetched.
		if err := req.admit(in.platform, rel.archive); err != nil {
			return LockEntry{}, err
		}
		archive = rel.archive
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return LockEntry{}, err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".archive-*.zip")
		if err != nil {
			return LockEntry{}, err
		}
		defer os.Remove(tmp.Name())
		_, sum, fetchErr := in.fetch(ctx, rel, tmp)
		closeErr := tmp.Close()
		if fetchErr != nil {
			return LockEntry{}, fetchErr
		}
		if closeErr != nil {
			return LockEntry{}, closeErr
		}
		if sum != archive {
			return LockEntry{}, fmt.Errorf("%s %s %s: archive sha256 does not match the registry's", req.Source, req.Version, in.platform)
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return LockEntry{}, err
		}
		if err := writeFileAtomic(path+".sum", []byte(fmt.Sprintf("archive=sha256:%x", archive)), 0o644); err != nil {
			return LockEntry{}, err
		}
	}
	binary, err := hashArchive(path, req.Source.Type, archive)
	if err != nil {
		if in.offline {
			return LockEntry{}, fmt.Errorf("%s %s %s: %w; install it online once to pin %s", req.Source, req.Version, in.platform, err, in.platform)
		}
		return LockEntry{}, fmt.Errorf("%s %s %s: %w", req.Source, req.Version, in.platform, err)
	}
	return LockEntry{Source: req.Source, Version: req.Version, Platform: in.platform, Archive: archive, Binary: binary}, nil
}

func hashArchive(path, typ string, want [32]byte) ([32]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	if info.Size() > maxArchive {
		return [32]byte{}, fmt.Errorf("archive is larger than %d bytes", maxArchive)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxArchive+1))
	if err != nil {
		return [32]byte{}, err
	}
	if n > maxArchive {
		return [32]byte{}, fmt.Errorf("archive is larger than %d bytes", maxArchive)
	}
	if [32]byte(h.Sum(nil)) != want {
		return [32]byte{}, fmt.Errorf("cached archive sha256 does not match its verified hash; install it online")
	}
	rc, err := archiveBinary(f, n, typ)
	if err != nil {
		return [32]byte{}, err
	}
	defer rc.Close()
	h.Reset()
	n, err = io.Copy(h, io.LimitReader(rc, maxBinary+1))
	if err != nil {
		return [32]byte{}, err
	}
	if n > maxBinary {
		return [32]byte{}, fmt.Errorf("provider binary is larger than %d bytes", maxBinary)
	}
	return [32]byte(h.Sum(nil)), nil
}
