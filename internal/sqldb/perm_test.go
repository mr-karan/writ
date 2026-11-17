//go:build unix

package sqldb_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/sqldb"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func writeRow(t *testing.T, db *sqldb.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS t (x integer)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t (x) VALUES (1)"); err != nil {
		t.Fatal(err)
	}
}

// A database created under a permissive umask, and the -wal and -shm files
// SQLite creates beside it, are readable by their owner only.
func TestOpenSQLiteCreatesPrivateFiles(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	for _, m := range []sqldb.SQLiteMode{sqldb.Durable, sqldb.Scratch} {
		t.Run(map[sqldb.SQLiteMode]string{sqldb.Durable: "durable", sqldb.Scratch: "scratch"}[m], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sqldb.OpenSQLite(path, m, "test database")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			writeRow(t, db)
			for _, p := range []string{path, path + "-wal", path + "-shm"} {
				if got := mode(t, p); got != 0o600 {
					t.Errorf("%s has mode %o, want 600", filepath.Base(p), got)
				}
			}
		})
	}
}

// A database an older writ created with mode 0644 is tightened when a
// writable open finds it, along with its -wal and -shm files. A read-only
// open changes nothing.
func TestOpenSQLiteTightensExistingFiles(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	writeRow(t, db)
	defer db.Close()
	files := []string{path, path + "-wal", path + "-shm"}
	for _, p := range files {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	ro, err := sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := ro.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatal(err)
	}
	ro.Close()
	if got := mode(t, path); got != 0o444 {
		t.Fatalf("read changed database mode to %o", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if got := mode(t, p); got != 0o644 {
			t.Fatalf("a read-only open changed %s to %o", filepath.Base(p), got)
		}
	}
	rw, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	for _, p := range files {
		if got := mode(t, p); got != 0o600 {
			t.Errorf("%s has mode %o after a writable open, want 600", filepath.Base(p), got)
		}
	}
}

// A writable open creates the missing directories of the path with mode 0700.
func TestOpenSQLiteCreatesPrivateDirectory(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	dir := filepath.Join(t.TempDir(), ".writ", "prod")
	db, err := sqldb.OpenSQLite(filepath.Join(dir, "state.db"), sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if got := mode(t, d); got != 0o700 {
			t.Errorf("%s has mode %o, want 700", d, got)
		}
	}
}

func TestReadOnlyFileAllowsLaterWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	writeRow(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	ro, err := sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	count := func(want int) {
		t.Helper()
		var got int
		if err := ro.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("reader saw %d rows, want %d", got, want)
		}
	}
	count(1)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read changed database")
	}
	if got := mode(t, path); got != 0o444 {
		t.Fatalf("read changed database mode to %o", got)
	}
	// Repeated read commands reopen an empty WAL, which SQLite chmods again.
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err = sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	count(1)
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	rw, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	writeRow(t, rw)
	var got int
	if err := rw.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&got); err != nil || got != 2 {
		t.Fatalf("writer rows %d: %v", got, err)
	}
}

func TestWriterHealsExistingReadOnlySidecars(t *testing.T) {
	for _, perm := range []os.FileMode{0o400, 0o444} {
		t.Run(fmt.Sprintf("%o", perm), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
			if err != nil {
				t.Fatal(err)
			}
			writeRow(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if err := os.WriteFile(path+suffix, nil, perm); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			writeRow(t, writer)
			for _, suffix := range []string{"-wal", "-shm"} {
				if got := mode(t, path+suffix); got != 0o600 {
					t.Errorf("%s mode %o, want 600", suffix, got)
				}
			}
		})
	}
}

func TestWriterRefusesSymlinkSidecar(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			target := filepath.Join(dir, "unrelated")
			if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path+suffix); err != nil {
				t.Fatal(err)
			}
			writer, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
			if err == nil {
				writer.Close()
				t.Error("accepted symlink sidecar")
			}
			if got := mode(t, target); got != 0o644 {
				t.Errorf("symlink target mode %o, want 644", got)
			}
		})
	}
}

func TestReadOnlyKilledReaderHeals(t *testing.T) {
	if path := os.Getenv("WRIT_SQLITE_CRASH_READER"); path != "" {
		reader, err := sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		for range 2 {
			rows, err := reader.QueryContext(context.Background(), "SELECT x FROM t")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("missing row: %v", rows.Err())
			}
		}
		fmt.Println("two connections ready")
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		return
	}
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	writeRow(t, writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestReadOnlyKilledReaderHeals$")
	cmd.Env = append(os.Environ(), "WRIT_SQLITE_CRASH_READER="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "two connections ready" {
		t.Fatalf("reader: %q %v %s", line, err, &stderr)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	waited = true
	if err == nil {
		t.Fatal("reader survived SIGKILL")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	writer, err = sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writeRow(t, writer)
}

func TestReadOnlyConnectionsKeepProcessLocks(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc/locks")
	}
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := sqldb.OpenSQLite(path, sqldb.Durable, "test database")
	if err != nil {
		t.Fatal(err)
	}
	writeRow(t, writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	first, err := sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	rows, err := first.QueryContext(context.Background(), "SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing row: %v", rows.Err())
	}
	info, err := os.Stat(path + "-shm")
	if err != nil {
		t.Fatal(err)
	}
	inode := ":" + strconv.FormatUint(uint64(info.Sys().(*syscall.Stat_t).Ino), 10)
	locks := func() int {
		t.Helper()
		data, err := os.ReadFile("/proc/locks")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 5 && fields[1] == "POSIX" && fields[4] == strconv.Itoa(os.Getpid()) && strings.HasSuffix(fields[5], inode) {
				count++
			}
		}
		return count
	}
	before := locks()
	if before == 0 {
		t.Fatal("first reader holds no SHM locks")
	}
	second, err := sqldb.OpenSQLite(path, sqldb.ReadOnly, "test database")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := second.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		second.Close()
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if after := locks(); after != before {
		t.Fatalf("closing second reader changed first reader's SHM locks from %d to %d", before, after)
	}
}
