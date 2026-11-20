package tofu

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/mr-karan/writ/internal/runas"
)

// viewHelper is os.Args[0] of the copy of writ that makes a tofu process's
// filesystem view and then becomes tofu. writ starts it from
// /proc/self/exe, which names writ's own binary even after an upgrade
// replaced the file.
const viewHelper = "writ-tofu-view"

func init() {
	if len(os.Args) == 2 && os.Args[0] == viewHelper {
		err := enterView(os.Args[1])
		// enterView returns only on a failure, which writ reads from the
		// status pipe.
		status := os.NewFile(statusFD, "status")
		fmt.Fprint(status, err)
		os.Exit(1)
	}
}

// The helper's descriptors after stdin, stdout, and stderr: the status pipe,
// which closes when tofu starts and carries the error when the view fails,
// the run's directory and its home, which the view's binds of them must
// match, with a run's own network, its end of the socket pair through
// which it hands writ the listeners it makes in the run's namespace, and
// with a saved plan, the sealed file that holds it, which tofu inherits.
const (
	statusFD = 3
	runFD    = 4
	homeFD   = 5
	netFD    = 6
	planFD   = 7
)

// sealPlan puts plan in a memfd, sealed so that no process may write,
// grow, or shrink it, which every later step of sc inherits as planFD,
// and returns the path at which tofu opens it. Every file in the run's
// directory is the run's to change, as any process of the run may: a
// provider that show starts could write over a saved plan there after
// show read it and before writ or apply did.
func (sc *scratch) sealPlan(plan []byte) (string, error) {
	fd, err := unix.MemfdCreate("writ-saved-plan", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return "", fmt.Errorf("tofu: seal the saved plan: %w", err)
	}
	f := os.NewFile(uintptr(fd), "saved plan")
	if _, err := f.Write(plan); err != nil {
		f.Close()
		return "", fmt.Errorf("tofu: seal the saved plan: %w", err)
	}
	const seals = unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		f.Close()
		return "", fmt.Errorf("tofu: seal the saved plan: %w", err)
	}
	if sc.plan != nil {
		sc.plan.Close()
	}
	sc.plan = f
	return "/dev/fd/" + strconv.Itoa(planFD), nil
}

// viewTmp is where the view binds the run's home again, for TMPDIR. Each
// provider makes its go-plugin socket there, and a socket's path must fit
// sun_path's 108 bytes, which the run's directory at its host path, under
// a data directory of any length, need not. When a path the view holds,
// such as a scratch_dir the config sets, is at, under, or above it, the
// view takes the first of viewTmp-1, viewTmp-2, and so on that is not.
const viewTmp = "/writ-tmp"

// tmpTarget returns the first of viewTmp, viewTmp-1, ... that no bind's
// target is at, under, or above.
func tmpTarget(binds []bind) string {
	for i := 0; ; i++ {
		t := viewTmp
		if i > 0 {
			t = fmt.Sprintf("%s-%d", viewTmp, i)
		}
		if !slices.ContainsFunc(binds, func(b bind) bool {
			return b.Target == t || strings.HasPrefix(b.Target, t+"/") || strings.HasPrefix(t, b.Target+"/")
		}) {
			return t
		}
	}
}

// runTmp is the run's TMPDIR within its view.
func runTmp(string) (string, error) { return viewTmp, nil }

// viewSpec is what the helper reads from its argument.
type viewSpec struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	// Dir is tofu's working directory within the view.
	Dir   string `json:"dir"`
	Binds []bind `json:"binds"`
	User  *User  `json:"user,omitempty"`
	// Net starts the helper in a network namespace of its own, where it
	// listens on Ports of 127.0.0.1 and hands writ the listeners.
	Net   bool  `json:"net,omitempty"`
	Ports []int `json:"ports,omitempty"`
	// Seccomp installs writ's seccomp filter before the exec of tofu.
	Seccomp bool `json:"seccomp,omitempty"`
}

// bind puts Source of the host at Target in the view, read-only unless
// Write. FD, when set, is the helper's descriptor of the directory that
// writ found at Source, which the bind must hold.
type bind struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Write  bool   `json:"write,omitempty"`
	Device bool   `json:"device,omitempty"`
	FD     int    `json:"fd,omitempty"`
}

// prSetNoNewPrivs is PR_SET_NO_NEW_PRIVS, which package syscall lacks.
const prSetNoNewPrivs = 38

// nsswitch keeps glibc and Go's resolver to /etc/hosts and DNS, which the
// view holds. A host's own file may name sources, such as systemd-resolved
// or nscd, that the view leaves out.
const nsswitch = "hosts: files dns\n"

// startConfined starts cmd, whose Path and Args are tofu's, in a private
// view of the filesystem, and returns once tofu runs in it. Every tofu
// process starts here. The view is a new root directory, in new user,
// mount, and pid namespaces, that holds only the run's directory, which
// tofu may write, the plugin cache, which only Fill may write, tofu's
// binary, the view's tools, and the libraries they load, the CA
// certificates and resolver files, a few devices, and a /proc of the
// run's own processes. With c.net, the helper also starts in a new
// network namespace and writ serves c.net's ports there until the stop it
// returns is called. With c.cgroup, the helper starts in the step's
// cgroup. Where the kernel can run it, tofu runs under writ's seccomp
// filter (seccompFilter). A failure to make the view, the network, or the
// filter fails the step: tofu never runs without them.
func startConfined(cmd *exec.Cmd, c command) (stop func(), err error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	if c.user != nil && (c.user.UID == 0 || c.user.GID == 0) {
		return nil, fmt.Errorf("tofu: refuse to run as %s: id 0 is root", c.user)
	}
	if c.net != nil && c.view.run == nil {
		return nil, errors.New("tofu: a run's own network needs the run's directory")
	}
	bin, err := filepath.Abs(cmd.Path)
	if err != nil {
		return nil, err
	}
	binds, err := c.binds(bin)
	if err != nil {
		return nil, fmt.Errorf("tofu: list what the run's filesystem view holds: %w", err)
	}
	dir := "/"
	if c.view.run != nil {
		dir = filepath.Join(c.view.dir, c.view.work)
		tmp := binds[len(binds)-1].Target
		for i, e := range cmd.Env {
			if strings.HasPrefix(e, "TMPDIR=") {
				cmd.Env[i] = "TMPDIR=" + tmp
			}
		}
	}
	vs := viewSpec{Binary: bin, Args: cmd.Args, Dir: dir, Binds: binds, User: c.user, Seccomp: seccompReady() == nil}
	if c.net != nil {
		vs.Net, vs.Ports = true, c.net.ports()
	}
	spec, err := json.Marshal(vs)
	if err != nil {
		return nil, err
	}
	status, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer status.Close()
	cmd.Path, cmd.Args = "/proc/self/exe", []string{viewHelper, string(spec)}
	cmd.ExtraFiles = []*os.File{w}
	if c.view.run != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, c.view.run, c.view.home)
	}
	var lnRecv, lnSend *os.File
	if c.net != nil {
		if lnRecv, lnSend, err = socketPair(); err != nil {
			w.Close()
			return nil, fmt.Errorf("tofu: make the socket pair that hands writ the run's listeners: %w", err)
		}
		defer lnRecv.Close()
		cmd.ExtraFiles = append(cmd.ExtraFiles, lnSend)
	}
	if c.plan != nil {
		// A nil entry is a descriptor the helper does not have.
		for len(cmd.ExtraFiles) < planFD-3 {
			cmd.ExtraFiles = append(cmd.ExtraFiles, nil)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, c.plan)
	}
	cmd.SysProcAttr = viewAttr(c.user, c.net != nil)
	if c.cgroup != nil {
		// The helper starts in the step's cgroup, before it runs anything.
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, c.cgroup.fd()
	}
	err = runas.StartMapped(cmd)
	w.Close()
	if lnSend != nil {
		lnSend.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("tofu: start tofu in new user, mount, and pid namespaces, which its private filesystem view needs: %w", err)
	}
	stop = func() {}
	var lns []net.Listener
	var netErr error
	if c.net != nil {
		// The helper sends the listeners before it makes the view, or
		// exits with its error, which closes its end.
		lns, netErr = receiveListeners(lnRecv, vs.Ports)
	}
	msg, err := io.ReadAll(io.LimitReader(status, 4<<10))
	if err == nil && len(msg) == 0 && netErr == nil {
		if c.net != nil {
			stop = c.net.serve(lns, vs.Ports)
		}
		return stop, nil
	}
	for _, ln := range lns {
		ln.Close()
	}
	_ = killGroup(cmd.Process.Pid)
	_ = cmd.Wait()
	switch {
	case err != nil:
		return nil, fmt.Errorf("tofu: make the run's private filesystem view: %w", err)
	case len(msg) > 0:
		return nil, fmt.Errorf("tofu: make the run's private filesystem view: %s", msg)
	}
	return nil, fmt.Errorf("tofu: make the run's own network: %w", netErr)
}

// socketPair is a pair of connected Unix sockets that keep message
// boundaries, each closed on exec.
func socketPair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "listeners"), os.NewFile(uintptr(fds[1]), "listeners"), nil
}

// receiveListeners reads the listeners the helper made on ports of the
// run's 127.0.0.1, one message with one descriptor for each port, in
// order, and checks that each is that port's listener. The descriptors
// arrive closed on exec, so no other process writ starts inherits one.
func receiveListeners(f *os.File, ports []int) ([]net.Listener, error) {
	buf := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(4*len(ports)))
	n, oobn, _, _, err := syscall.Recvmsg(int(f.Fd()), buf, oob, syscall.MSG_CMSG_CLOEXEC)
	if err != nil {
		return nil, err
	}
	var fds []int
	if n > 0 {
		msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, err
		}
		for i := range msgs {
			got, err := syscall.ParseUnixRights(&msgs[i])
			if err != nil {
				return nil, err
			}
			fds = append(fds, got...)
		}
	}
	var lns []net.Listener
	fail := func(err error) ([]net.Listener, error) {
		for _, ln := range lns {
			ln.Close()
		}
		return nil, err
	}
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		files[i] = os.NewFile(uintptr(fd), "run listener")
		defer files[i].Close()
	}
	if n == 0 {
		return nil, errors.New("the view's helper ended before it handed writ the listeners")
	}
	if len(fds) != len(ports) || int(buf[0]) != len(ports) {
		return nil, fmt.Errorf("the view's helper handed writ %d listeners, not %d", len(fds), len(ports))
	}
	for i, f := range files {
		ln, err := net.FileListener(f)
		if err != nil {
			return fail(err)
		}
		lns = append(lns, ln)
		if want := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(ports[i])); ln.Addr().String() != want.String() {
			return fail(fmt.Errorf("the view's helper handed writ a listener on %s, not %s", ln.Addr(), want))
		}
	}
	return lns, nil
}

// viewAttr starts the helper in its own process group and namespaces, and
// with netns in a network namespace of its own too. Its
// user namespace maps uid and gid 0 to writ's own, so the helper keeps the
// namespace's capabilities through its exec and may make the view, and the
// run's ids to themselves, so it may become the run's user. Mapping ids
// not one's own needs CAP_SETUID and CAP_SETGID, which writ serve holds
// when it has provider uids. Without a run's user, setgroups stays denied,
// as a user namespace an unprivileged writ makes requires. The helper sets
// the parent-death signal itself: Go's check of it after a fork reads the
// parent's pid, which a new pid namespace hides.
func viewAttr(u *User, netns bool) *syscall.SysProcAttr {
	a := &syscall.SysProcAttr{
		Setpgid:     true,
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}},
	}
	if u != nil {
		a.UidMappings = append(a.UidMappings, syscall.SysProcIDMap{ContainerID: int(u.UID), HostID: int(u.UID), Size: 1})
		a.GidMappings = append(a.GidMappings, syscall.SysProcIDMap{ContainerID: int(u.GID), HostID: int(u.GID), Size: 1})
		a.GidMappingsEnableSetgroups = true
	}
	if netns {
		a.Cloneflags |= syscall.CLONE_NEWNET
	}
	return a
}

// caPaths are where Go's crypto/x509 looks for the system's CA bundle on
// Linux. The view holds the first that exists at the first of them.
var caPaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// binds lists the view's binds, each of a path with every symbolic link
// resolved at the path that names it.
func (c command) binds(bin string) ([]bind, error) {
	var out []bind
	add := func(target string, b bind) error {
		src, err := filepath.EvalSymlinks(target)
		if err != nil {
			return err
		}
		b.Source, b.Target = src, target
		out = append(out, b)
		return nil
	}
	files, err := closure(append([]string{bin}, c.view.tools...)...)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := add(f, bind{}); err != nil {
			return nil, err
		}
	}
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts", "/etc/ld.so.cache"} {
		if err := add(f, bind{}); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	for _, f := range caPaths {
		src, err := filepath.EvalSymlinks(f)
		if err == nil {
			out = append(out, bind{Source: src, Target: caPaths[0]})
			break
		}
	}
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom"} {
		if err := add(d, bind{Device: true}); err != nil {
			return nil, err
		}
	}
	if c.view.cache != "" {
		if err := add(c.view.cache, bind{Write: c.view.cacheWrite}); err != nil {
			return nil, fmt.Errorf("the plugin cache: %w", err)
		}
	}
	if c.view.run != nil {
		out = append(out,
			bind{Source: c.view.dir, Target: c.view.dir, Write: true, FD: runFD},
		)
		out = append(out, bind{Source: filepath.Join(c.view.dir, "home"), Target: tmpTarget(out), Write: true, FD: homeFD})
	}
	return out, nil
}

// closure lists bins and what exec of each loads: a script's interpreter,
// and an ELF file's dynamic loader and the libraries it needs, found as the
// loader finds them, through RUNPATH or RPATH, then the loader's own
// directory, where a Nix glibc looks, and then the system's library
// directories. Each is named by the path the loader opens.
func closure(bins ...string) ([]string, error) {
	var out []string
	var loaderDirs []string
	seen := map[string]bool{}
	var walk func(p string) error
	walk = func(p string) error {
		if seen[p] {
			return nil
		}
		seen[p] = true
		out = append(out, p)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		head := make([]byte, 256)
		n, _ := io.ReadFull(f, head)
		head = head[:n]
		if line, ok := strings.CutPrefix(string(head), "#!"); ok {
			line, _, _ = strings.Cut(line, "\n")
			fields := strings.Fields(line)
			if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
				return fmt.Errorf("%s names no interpreter by its full path", p)
			}
			return walk(fields[0])
		}
		ef, err := elf.NewFile(f)
		if err != nil {
			// Neither a script nor ELF, as a library's linker script is
			// not: what exec makes of it, it makes without other files.
			return nil
		}
		defer ef.Close()
		for _, prog := range ef.Progs {
			if prog.Type != elf.PT_INTERP {
				continue
			}
			b, err := io.ReadAll(prog.Open())
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			interp := strings.TrimRight(string(b), "\x00")
			loaderDirs = append(loaderDirs, filepath.Dir(interp))
			if real, err := filepath.EvalSymlinks(interp); err == nil {
				loaderDirs = append(loaderDirs, filepath.Dir(real))
			}
			if err := walk(interp); err != nil {
				return err
			}
		}
		libs, err := ef.ImportedLibraries()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		dirs := searchPath(ef, filepath.Dir(p), loaderDirs)
		for _, lib := range libs {
			found := ""
			for _, d := range dirs {
				if _, err := os.Stat(filepath.Join(d, lib)); err == nil {
					found = filepath.Join(d, lib)
					break
				}
			}
			if found == "" {
				return fmt.Errorf("%s needs %s, which is in none of %s", p, lib, strings.Join(dirs, ", "))
			}
			if err := walk(found); err != nil {
				return err
			}
		}
		return nil
	}
	for _, bin := range bins {
		if err := walk(bin); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// searchPath is where the dynamic loader looks for an ELF file's libraries.
func searchPath(ef *elf.File, origin string, loaderDirs []string) []string {
	var dirs []string
	paths, _ := ef.DynString(elf.DT_RUNPATH)
	if len(paths) == 0 {
		paths, _ = ef.DynString(elf.DT_RPATH)
	}
	for _, p := range paths {
		for d := range strings.SplitSeq(p, ":") {
			d = strings.ReplaceAll(strings.ReplaceAll(d, "${ORIGIN}", origin), "$ORIGIN", origin)
			if d != "" {
				dirs = append(dirs, d)
			}
		}
	}
	dirs = append(dirs, loaderDirs...)
	triplet := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[runtime.GOARCH]
	return append(dirs, "/lib/"+triplet, "/usr/lib/"+triplet, "/lib64", "/usr/lib64", "/lib", "/usr/lib")
}

// enterView runs in the helper, as uid 0 of its new user namespace, which
// is writ's own uid, with every capability there, and as pid 1 of its new
// pid namespace. It makes
// the view on a tmpfs mounted at /proc of its own mount namespace, makes
// that tmpfs its root, drops the old root, takes the run's user, drops
// every capability, installs writ's seccomp filter, and execs tofu. It
// returns only an error.
func enterView(arg string) error {
	runtime.LockOSThread()
	var spec viewSpec
	if err := json.Unmarshal([]byte(arg), &spec); err != nil {
		return err
	}
	if err := watchParent(); err != nil {
		return err
	}
	if spec.Net {
		if err := enterNet(spec.Ports); err != nil {
			return err
		}
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the mounts private: %w", err)
	}
	// /proc always exists, and nothing the view binds lies under it, so a
	// tmpfs over it hides no source. The old /proc stays mounted beneath,
	// which an older kernel needs to mount the view's own.
	const root = "/proc"
	if err := syscall.Mount("tmpfs", root, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0755"); err != nil {
		return fmt.Errorf("mount a tmpfs at %s for the new root: %w", root, err)
	}
	slices.SortFunc(spec.Binds, func(a, b bind) int { return strings.Compare(a.Target, b.Target) })
	var dirs []string
	for _, b := range spec.Binds {
		for _, d := range dirs {
			if strings.HasPrefix(b.Target, d+"/") {
				return fmt.Errorf("%s lies within %s, which the view also holds", b.Target, d)
			}
		}
		isDir, err := bindInto(root, b)
		if err != nil {
			return err
		}
		if isDir {
			dirs = append(dirs, b.Target)
		}
	}
	if err := os.MkdirAll(root+"/etc", 0o755); err != nil {
		return err
	}
	if err := writeFile(root+"/etc/nsswitch.conf", nsswitch); err != nil {
		return err
	}
	for link, to := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		if err := os.Symlink(to, root+"/dev/"+link); err != nil {
			return err
		}
	}
	if err := mountProc(root + "/proc"); err != nil {
		return err
	}
	if err := syscall.Chdir(root); err != nil {
		return err
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root to the new root: %w", err)
	}
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("drop the old root: %w", err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("make the new root read-only: %w", err)
	}
	if err := syscall.Chdir(spec.Dir); err != nil {
		return fmt.Errorf("enter the stack's directory: %w", err)
	}
	if err := dropPrivileges(spec.User); err != nil {
		return err
	}
	for _, fd := range []int{statusFD, runFD, homeFD} {
		syscall.CloseOnExec(fd)
	}
	if spec.Seccomp {
		if err := installSeccomp(); err != nil {
			return fmt.Errorf("install writ's seccomp filter: %w", err)
		}
	}
	return syscall.Exec(spec.Binary, spec.Args, os.Environ())
}

// enterNet runs in the helper, in its new network namespace, which holds
// only a loopback interface that is down. It brings loopback up, listens
// on each of ports of 127.0.0.1, hands writ the listeners in one message
// through netFD, and closes its copies. tofu then starts with those ports
// taken, so no process of the run can listen in writ's place.
func enterNet(ports []int) error {
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("bring up loopback in the run's network namespace: %w", err)
	}
	var fds []int
	defer func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}()
	for _, port := range ports {
		fd, err := listenLoopback(port)
		if err != nil {
			return fmt.Errorf("listen on 127.0.0.1:%d in the run's network namespace: %w", port, err)
		}
		fds = append(fds, fd)
	}
	var rights []byte
	if len(fds) > 0 {
		rights = syscall.UnixRights(fds...)
	}
	if err := syscall.Sendmsg(netFD, []byte{byte(len(fds))}, rights, nil, 0); err != nil {
		return fmt.Errorf("hand writ the listeners of the run's network namespace: %w", err)
	}
	return syscall.Close(netFD)
}

// ifreqFlags is struct ifreq with its ifr_flags member, as SIOCGIFFLAGS
// and SIOCSIFFLAGS read and write it: 40 bytes on a 64-bit kernel.
type ifreqFlags struct {
	name  [syscall.IFNAMSIZ]byte
	flags uint16
	_     [22]byte
}

// loopbackUp sets IFF_UP on lo, which a new network namespace makes down.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var req ifreqFlags
	copy(req.name[:], "lo")
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&req))); e != 0 {
		return e
	}
	req.flags |= syscall.IFF_UP
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&req))); e != 0 {
		return e
	}
	return nil
}

// listenLoopback returns a socket that listens on 127.0.0.1:port.
func listenLoopback(port int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	if err := syscall.Listen(fd, syscall.SOMAXCONN); err != nil {
		syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

// bindInto binds b at its target under root, and reports whether it is a
// directory.
func bindInto(root string, b bind) (bool, error) {
	st, err := os.Stat(b.Source)
	if err != nil {
		return false, err
	}
	at := root + b.Target
	if st.IsDir() {
		err = os.MkdirAll(at, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(at), 0o755); err == nil {
		err = writeFile(at, "")
	}
	if err != nil {
		return false, fmt.Errorf("make the mount point of %s: %w", b.Target, err)
	}
	if err := syscall.Mount(b.Source, at, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return false, fmt.Errorf("bind %s: %w", b.Target, err)
	}
	if b.FD != 0 {
		// writ found the directory by the path its descriptor had, which
		// the run may since have changed; the bind must hold that same
		// directory.
		var want, got syscall.Stat_t
		if err := syscall.Fstat(b.FD, &want); err != nil {
			return false, err
		}
		if err := syscall.Stat(at, &got); err != nil {
			return false, err
		}
		if want.Dev != got.Dev || want.Ino != got.Ino {
			return false, fmt.Errorf("%s is no longer the directory writ opened at %s", b.Target, b.Source)
		}
	}
	// A remount of a bind in a user namespace must keep the flags the
	// source's mount has, which the kernel locks.
	var fs syscall.Statfs_t
	if err := syscall.Statfs(at, &fs); err != nil {
		return false, err
	}
	const kept = syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME | syscall.MS_NODIRATIME | syscall.MS_RELATIME
	flags := uintptr(fs.Flags)&kept | syscall.MS_NOSUID
	if !b.Write {
		flags |= syscall.MS_RDONLY
	}
	if !b.Device {
		flags |= syscall.MS_NODEV
	}
	if err := syscall.Mount("", at, "", syscall.MS_BIND|syscall.MS_REMOUNT|flags, ""); err != nil {
		return false, fmt.Errorf("restrict the bind of %s: %w", b.Target, err)
	}
	return st.IsDir(), nil
}

// mountProc mounts a /proc of the helper's pid namespace, which holds only
// the run's processes. Its pid subset leaves out the kernel's other files,
// and Linux 7.2 and later mount it whatever covers writ's own /proc. An
// older kernel mounts only a whole /proc, and only when nothing locked
// covers part of writ's, as systemd's ProtectKernelTunables does. It is
// read-only on its own: the remount of / does not reach a separate mount.
func mountProc(at string) error {
	if err := os.MkdirAll(at, 0o555); err != nil {
		return err
	}
	const flags = syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	err := syscall.Mount("proc", at, "proc", flags, "subset=pid")
	if err == nil {
		return nil
	}
	if err2 := syscall.Mount("proc", at, "proc", flags, ""); err2 != nil {
		return fmt.Errorf("mount /proc for the run: the kernel refused (%v, and %v without subset=pid); Linux before 7.2 refuses it when a mount covers part of writ's /proc, as systemd's ProtectKernelTunables makes", err, err2)
	}
	return nil
}

// dropPrivileges takes u, when set, with no supplementary group, and
// empties the bounding set, so tofu starts with no capability even as uid
// 0 of the namespace. It sets the parent-death signal again, which a change
// of uid clears, and no_new_privs.
func dropPrivileges(u *User) error {
	for c := uintptr(0); c < 64; c++ {
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_CAPBSET_DROP, c, 0); e != 0 && e != syscall.EINVAL {
			return fmt.Errorf("drop capability %d from the bounding set: %w", c, e)
		}
	}
	if u != nil {
		if err := syscall.Setgroups(nil); err != nil {
			return fmt.Errorf("drop the supplementary groups: %w", err)
		}
		if err := syscall.Setresgid(int(u.GID), int(u.GID), int(u.GID)); err != nil {
			return fmt.Errorf("become gid %d: %w", u.GID, err)
		}
		if err := syscall.Setresuid(int(u.UID), int(u.UID), int(u.UID)); err != nil {
			return fmt.Errorf("become uid %d: %w", u.UID, err)
		}
	}
	if err := watchParent(); err != nil {
		return err
	}
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("set no_new_privs: %w", e)
	}
	return nil
}

// watchParent makes the kernel kill the helper when writ dies, then checks
// that writ was alive when it did: writ holds the read end of the status
// pipe, so the write end reports an error once writ is gone.
func watchParent() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0); e != 0 {
		return fmt.Errorf("set the parent-death signal: %w", e)
	}
	type pollFd struct {
		fd             int32
		events, revent int16
	}
	const pollOut, pollErr = 0x4, 0x8
	p := pollFd{fd: statusFD, events: pollOut}
	var now syscall.Timespec
	if _, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(&now)), 0, 0, 0); e != 0 {
		return fmt.Errorf("check that writ is alive: %w", e)
	}
	if p.revent&pollErr != 0 {
		os.Exit(1)
	}
	return nil
}

func writeFile(p, text string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	return errors.Join(err, f.Close())
}
