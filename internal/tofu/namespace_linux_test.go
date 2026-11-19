package tofu

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// namespaceArgs makes the user, network, mount, and pid namespaces that
// the tests which run this test binary again run it in.
var namespaceArgs = []string{"--user", "--map-root-user", "--map-users=auto", "--map-groups=auto", "--net", "--pid", "--fork", "--mount-proc"}

// enterOwnRoot makes a tmpfs at dir the child's root directory, with the
// system's directories, /dev, /proc, this package's directory, and extra
// bound in. In the user namespace every directory of the host,
// / among them, belongs to the overflow uid, which stands for every host
// uid the namespace does not map, so New refuses a scratch root under any
// of them. The tmpfs belongs to root of the namespace, which is writ. It
// becomes the root by pivot_root, not chroot, because the kernel lets no
// chrooted process make the user namespace of a run's view.
func enterOwnRoot(t *testing.T, dir string, extra ...string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "mode=0755"); err != nil {
		t.Fatalf("mount a tmpfs at %s: %v", dir, err)
	}
	for _, p := range append([]string{"/nix", "/bin", "/usr", "/lib", "/lib64", "/etc", "/dev", "/proc", wd}, extra...) {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		at := filepath.Join(dir, p)
		if err := os.MkdirAll(at, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mount(p, at, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			t.Fatalf("bind %s: %v", p, err)
		}
	}
	tmp := filepath.Join(dir, "tmp")
	// A checkout under /tmp, bound above, made tmp already.
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	pivotRoot(t, dir)
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	os.Setenv("TMPDIR", "/tmp")
}

// pivotRoot makes dir, a mount point, the root directory and drops the
// old root.
func pivotRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		t.Fatalf("pivot_root to %s: %v", dir, err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		t.Fatalf("drop the old root: %v", err)
	}
	if err := os.Chdir("/"); err != nil {
		t.Fatal(err)
	}
}
