package provider

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// stampSuffix names the sidecar that records a verified hash of a provider
// binary together with the file's identity at the time it was read.
const stampSuffix = ".verified"

// racyMargin bounds the filesystem's timestamp granularity plus the lag of
// the kernel's coarse clock that stamps ctime. FAT, the coarsest filesystem
// Linux mounts, keeps 2 s timestamps; ext4, xfs, and btrfs keep nanoseconds
// taken from a clock that ticks every few milliseconds.
const racyMargin = 2 * time.Second

// fileStamp is the identity of one version of a file's content, read without
// reading the content. Times are nanoseconds since the Unix epoch.
type fileStamp struct {
	dev, ino     uint64
	size         int64
	mtime, ctime int64
}

type stampRecord struct {
	sum      [32]byte
	stamp    fileStamp
	verified int64
}

// binaryHash returns the SHA-256 of the file at bin. With trustStamp, a
// sidecar written by an earlier call stands in for reading the file when
// stampedHash accepts it. Every call that reads the file writes a new sidecar.
func binaryHash(bin string, trustStamp bool, log *slog.Logger) ([32]byte, error) {
	if trustStamp {
		if sum, ok := stampedHash(bin); ok {
			log.Debug("provider binary unchanged since it was hashed", "path", bin)
			return sum, nil
		}
	}
	f, err := os.Open(bin)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	// The clock is read before the stat so that any change after the stat
	// gets a ctime at or after this time, less the margin.
	verified := time.Now()
	fi, err := f.Stat()
	if err != nil {
		return [32]byte{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	sum := [32]byte(h.Sum(nil))
	key, keyErr := stampKey(true)
	if st, ok := stampOf(fi); ok && keyErr == nil {
		rec := stampRecord{sum: sum, stamp: st, verified: verified.UnixNano()}
		if err := writeFileAtomic(bin+stampSuffix, rec.signed(key), 0o644); err != nil {
			log.Debug("could not record the provider binary's hash", "path", bin, "error", err)
		}
	}
	return sum, nil
}

// stampedHash returns the hash recorded in bin's sidecar when the file still
// has the device, inode, size, mtime, and ctime it had when it was hashed,
// and that ctime is older than the hash by more than racyMargin. The record
// must authenticate under the private per-user key outside the cache.
//
// Why this is sound where size, mtime, and inode alone are not: any write,
// truncate, chmod, chown, or rename onto the file sets its ctime to the
// kernel's current time, and no unprivileged call sets ctime to any other
// value. utimensat sets mtime back, but stamps ctime with the time of that
// call. So a file whose ctime still equals the recorded one has not changed
// since the stat that preceded the hash, unless the change fell in the same
// clock tick as the recorded ctime. The racy-timestamp rule, as in git's
// index, rules that out: a change after the stat gets a ctime no earlier than
// the stat less racyMargin, so a recorded ctime older than the stat by more
// than racyMargin can never be repeated by a later change.
//
// The MAC authenticates all recorded fields with a per-user key outside the
// cache. This guards against accidental changes and a binary replaced through
// cache-only access, including a cache restored on another machine. It does
// not guard against a process running as the same user, which can read the key
// and already replace Writ itself. Setting the system clock back needs privilege.
func stampedHash(bin string) ([32]byte, bool) {
	data, err := os.ReadFile(bin + stampSuffix)
	if err != nil {
		return [32]byte{}, false
	}
	key, err := stampKey(false)
	if err != nil {
		return [32]byte{}, false
	}
	payload, signature, ok := bytes.Cut(data, []byte("mac="))
	if !ok {
		return [32]byte{}, false
	}
	var mac []byte
	if _, err := fmt.Sscanf(string(signature), "%x\n", &mac); err != nil {
		return [32]byte{}, false
	}
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	if !hmac.Equal(mac, h.Sum(nil)) {
		return [32]byte{}, false
	}
	rec, err := parseStamp(payload)
	if err != nil {
		return [32]byte{}, false
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return [32]byte{}, false
	}
	st, ok := stampOf(fi)
	if !ok || st != rec.stamp || rec.stamp.ctime >= rec.verified-racyMargin.Nanoseconds() {
		return [32]byte{}, false
	}
	return rec.sum, true
}

const stampFormat = "sha256=%x dev=%d ino=%d size=%d mtime=%d ctime=%d verified=%d\n"

func (r stampRecord) encode() []byte {
	return fmt.Appendf(nil, stampFormat, r.sum, r.stamp.dev, r.stamp.ino, r.stamp.size, r.stamp.mtime, r.stamp.ctime, r.verified)
}

func parseStamp(data []byte) (stampRecord, error) {
	var r stampRecord
	var sum []byte
	_, err := fmt.Sscanf(string(data), stampFormat, &sum, &r.stamp.dev, &r.stamp.ino, &r.stamp.size, &r.stamp.mtime, &r.stamp.ctime, &r.verified)
	if err != nil {
		return stampRecord{}, err
	}
	if len(sum) != len(r.sum) {
		return stampRecord{}, fmt.Errorf("hash has %d bytes, want %d", len(sum), len(r.sum))
	}
	copy(r.sum[:], sum)
	return r, nil
}

func (r stampRecord) signed(key []byte) []byte {
	data := r.encode()
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return fmt.Appendf(data, "mac=%x\n", h.Sum(nil))
}

func stampKey(create bool) ([]byte, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "writ")
	path := filepath.Join(dir, "stamp.key")
	key, err := readStampKey(path)
	if !create || !os.IsNotExist(err) {
		return key, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("stamp key directory is not private")
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".stamp-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(key)
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Linking publishes a complete key without replacing another creator's key.
	if err := os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return nil, err
	}
	return readStampKey(path)
}

func readStampKey(path string) ([]byte, error) {
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("stamp key directory is not private")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() & ^os.FileMode(0o600) != 0 || fi.Size() != 32 {
		return nil, fmt.Errorf("stamp key must be a private 32-byte file")
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("stamp key must have 32 bytes")
	}
	return key, nil
}
