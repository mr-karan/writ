//go:build linux

package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hashLog records whether binaryHash trusted a sidecar instead of reading.
type hashLog struct {
	mu      sync.Mutex
	skipped int
}

func (h *hashLog) Enabled(context.Context, slog.Level) bool { return true }
func (h *hashLog) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *hashLog) WithGroup(string) slog.Handler            { return h }
func (h *hashLog) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "provider binary unchanged since it was hashed" {
		h.mu.Lock()
		h.skipped++
		h.mu.Unlock()
	}
	return nil
}

// hashTrusting calls binaryHash with trustStamp and reports whether it
// skipped reading the file.
func hashTrusting(t *testing.T, bin string) ([32]byte, bool) {
	t.Helper()
	h := &hashLog{}
	sum, err := binaryHash(bin, true, slog.New(h))
	if err != nil {
		t.Fatal(err)
	}
	return sum, h.skipped == 1
}

func TestBinaryHashStamp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	content := []byte("provider binary, version one\n")
	mutations := []struct {
		name   string
		mutate func(t *testing.T, path string)
		want   []byte // content after the mutation
	}{
		{"nothing changed", func(*testing.T, string) {}, content},
		{"content rewritten with its size and mtime kept", func(t *testing.T, path string) {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.ToUpper(content), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
				t.Fatal(err)
			}
		}, bytes.ToUpper(content)},
		{"touched", func(t *testing.T, path string) {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				t.Fatal(err)
			}
		}, content},
		{"chmod", func(t *testing.T, path string) {
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, content},
	}
	paths := make([]string, len(mutations))
	for i := range mutations {
		paths[i] = filepath.Join(dir, "bin"+string(rune('a'+i)))
		if err := os.WriteFile(paths[i], content, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a file hashed within the racy window is hashed again", func(t *testing.T) {
		if _, err := binaryHash(paths[0], false, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
		if _, skipped := hashTrusting(t, paths[0]); skipped {
			t.Error("trusted a sidecar written in the same racy window as the file's ctime")
		}
	})

	time.Sleep(racyMargin + 100*time.Millisecond)
	for _, p := range paths {
		if _, err := binaryHash(p, false, slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
	}
	for i, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			m.mutate(t, paths[i])
			sum, skipped := hashTrusting(t, paths[i])
			if want := sha256.Sum256(m.want); sum != want {
				t.Errorf("hash %x, want %x", sum, want)
			}
			if wantSkip := i == 0; skipped != wantSkip {
				t.Errorf("skipped reading the file = %v, want %v", skipped, wantSkip)
			}
		})
	}

	t.Run("a damaged sidecar is ignored", func(t *testing.T) {
		if err := os.WriteFile(paths[0]+stampSuffix, []byte("sha256=zz\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sum, skipped := hashTrusting(t, paths[0])
		if skipped || sum != sha256.Sum256(content) {
			t.Errorf("hash %x, skipped %v; want a fresh hash of the file", sum, skipped)
		}
	})
}

func TestPoolRejectsUnauthenticatedStamp(t *testing.T) {
	for _, name := range []string{"missing mac", "forged mac", "different key", "public key file"} {
		t.Run(name, func(t *testing.T) {
			config := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", config)
			keyPath := filepath.Join(config, "writ", "stamp.key")
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				t.Fatal(err)
			}
			key := bytes.Repeat([]byte{1}, 32)
			if err := os.WriteFile(keyPath, key, 0o600); err != nil {
				t.Fatal(err)
			}
			dirs := Dirs{Cache: t.TempDir()}
			bin := dirs.BinaryPath(tlsSrc, "4.4.2", CurrentPlatform())
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			pinned := sha256.Sum256([]byte("original provider"))
			if err := os.WriteFile(bin, []byte("replaced provider"), 0o755); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(bin)
			if err != nil {
				t.Fatal(err)
			}
			st, ok := stampOf(fi)
			if !ok {
				t.Fatal("no file identity")
			}
			// A cache writer can claim any verification time, including one outside the racy window.
			payload := fmt.Sprintf("sha256=%x dev=%d ino=%d size=%d mtime=%d ctime=%d verified=%d\n", pinned, st.dev, st.ino, st.size, st.mtime, st.ctime, st.ctime+2*racyMargin.Nanoseconds())
			data := []byte(payload)
			if name != "missing mac" {
				signingKey := key
				if name == "different key" {
					signingKey = bytes.Repeat([]byte{2}, 32)
				}
				mac := hmac.New(sha256.New, signingKey)
				mac.Write(data)
				signature := mac.Sum(nil)
				if name == "forged mac" {
					signature = make([]byte, sha256.Size)
				}
				data = fmt.Appendf(data, "mac=%x\n", signature)
			}
			if name == "public key file" {
				if err := os.Chmod(keyPath, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(bin+stampSuffix, data, 0o644); err != nil {
				t.Fatal(err)
			}
			pool := NewPool(PoolOptions{Dirs: dirs})
			pool.Add(&Lock{entries: []LockEntry{{Source: tlsSrc, Version: "4.4.2", Platform: CurrentPlatform(), Binary: pinned}}}, []Config{{Source: tlsSrc, Version: "4.4.2"}})
			defer pool.Close(context.Background())
			_, err = pool.Client(context.Background(), keyOf(tlsSrc))
			if err == nil || !strings.Contains(err.Error(), "does not match its hash in writ.lock") {
				t.Fatalf("Client error = %v, want binary hash mismatch", err)
			}
			if pool.Started() != 0 {
				t.Fatal("started a replaced provider")
			}
		})
	}
}

func TestBinaryHashUnavailableStampKey(t *testing.T) {
	for _, name := range []string{"missing", "unreadable", "short", "public directory"} {
		t.Run(name, func(t *testing.T) {
			config := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", config)
			bin := filepath.Join(t.TempDir(), "provider")
			content := []byte("provider binary")
			if err := os.WriteFile(bin, content, 0o755); err != nil {
				t.Fatal(err)
			}
			time.Sleep(racyMargin + 100*time.Millisecond)
			if _, err := binaryHash(bin, false, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			keyPath := filepath.Join(config, "writ", "stamp.key")
			var err error
			switch name {
			case "missing":
				err = os.Rename(keyPath, keyPath+".saved")
			case "unreadable":
				err = os.Chmod(keyPath, 0)
			case "short":
				err = os.WriteFile(keyPath, []byte("short"), 0o600)
			case "public directory":
				err = os.Chmod(filepath.Dir(keyPath), 0o777)
			}
			if err != nil {
				t.Fatal(err)
			}
			sum, skipped := hashTrusting(t, bin)
			if skipped || sum != sha256.Sum256(content) {
				t.Fatal("did not hash the binary with an unavailable key")
			}
		})
	}
}

func TestConcurrentStampKeyCreation(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	const count = 16
	paths := make([]string, count)
	sums := make([][32]byte, count)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), "provider")
		if err := os.WriteFile(paths[i], []byte(fmt.Sprintf("provider %d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(racyMargin + 100*time.Millisecond)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range paths {
		wg.Go(func() { sums[i], errs[i] = binaryHash(paths[i], false, slog.New(slog.DiscardHandler)) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		sum, skipped := hashTrusting(t, paths[i])
		if !skipped || sum != sums[i] {
			t.Fatalf("writer %d did not use the published key", i)
		}
	}
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(config, "writ"), 0o700},
		{filepath.Join(config, "writ", "stamp.key"), 0o600},
	} {
		fi, err := os.Stat(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != entry.mode {
			t.Errorf("mode = %o, want %o", fi.Mode().Perm(), entry.mode)
		}
	}
}
