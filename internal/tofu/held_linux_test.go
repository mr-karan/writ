package tofu

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// deepDir makes depth directories, each in the one before, under a new
// temporary directory, each named with 250 of the letter c, and returns a
// descriptor of the deepest. /proc writes the whole path of a file there
// on each line of maps that maps it: about 60KB at a depth of 240.
func deepDir(t *testing.T, c byte, depth int) int {
	t.Helper()
	dir, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat(string(c), 250)
	for range depth {
		if err := unix.Mkdirat(dir, name, 0o700); err != nil {
			t.Fatal(err)
		}
		sub, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		unix.Close(dir)
		dir = sub
	}
	t.Cleanup(func() { unix.Close(dir) })
	return dir
}

// mapFile maps the file name of the directory dir, made with size bytes,
// times times, each mapping apart from the others, and unmaps each when t
// ends. With removed, it removes the file once it is mapped.
func mapFile(t *testing.T, dir int, name string, size, times int, removed bool) {
	t.Helper()
	fd, err := unix.Openat(dir, name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		t.Fatal(err)
	}
	for range times {
		m, err := unix.Mmap(fd, 0, size, unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unix.Munmap(m) })
	}
	if removed {
		if err := unix.Unlinkat(dir, name, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// A measure reads a bounded number of bytes of /proc/PID/maps. Past it,
// the step stops, as one over the limit does: each line of maps is as
// long as the path of the file it maps, and a run that maps a file under
// a deep path many times would otherwise make writ read gigabytes. Here
// 1000 mappings of one removed file make 60MB of maps.
func TestHeldMapsReadIsCapped(t *testing.T) {
	mapFile(t, deepDir(t, 'm', 240), "f", 4096, 1000, true)
	start := time.Now()
	msg := scratchStandIn(t).overScratch(1<<40, cgroupStandIn(t, 1))
	if msg == "" {
		t.Fatalf("a measure of 60MB of maps passed, after %v", time.Since(start))
	}
	t.Logf("after %v: %s", time.Since(start), msg)
}

// A measure reads the mappings of each process once, from one of its
// threads, which share them: read once a thread, a process of many
// threads would pass the bound on maps that it passes read once.
func TestHeldMapsReadOncePerProcess(t *testing.T) {
	// 600 mappings of a file under a path of 4KB: about 2.4MB of maps.
	mapFile(t, deepDir(t, 'o', 16), "f", 4096, 600, false)
	release := make(chan struct{})
	defer close(release)
	locked := make(chan struct{})
	for range 16 {
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			locked <- struct{}{}
			<-release
		}()
		<-locked
	}
	threads, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scratchStandIn(t).usage(1<<40, cgroupStandIn(t, 1), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("a measure of a process of %d threads failed: %v", len(threads), err)
	}
}

// A measure keeps a few bytes for each removed file it finds, not its
// path, which a run may make some 60KB long: 200 mapped removed files
// under such paths, 12MB of maps, cost writ 12MB of memory for each thread
// it read them from.
func TestHeldFilesCostLittleMemory(t *testing.T) {
	dir := deepDir(t, 'p', 240)
	for i := range 200 {
		mapFile(t, dir, "f"+strconv.Itoa(i), 4096, 1, true)
	}
	sc, cg := scratchStandIn(t), cgroupStandIn(t, 1)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n, err := sc.usage(1<<40, cg, time.Now().Add(time.Minute))
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if n < 200*scratchInode {
		t.Fatalf("the measure counted %d bytes, less than the 200 removed files", n)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 2<<20 {
		t.Fatalf("the measure allocated %d bytes for 200 removed files", alloc)
	}
}

// A measure of held files stops once what it found passes the limit, as a
// walk does, with each file counted as at least scratchInode: what writ
// keeps of the files it found grows only as far as the limit.
func TestHeldMeasureStopsPastTheLimit(t *testing.T) {
	dir, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dir)
	for i := range 64 {
		mapFile(t, dir, strconv.Itoa(i), 4096, 1, true)
	}
	const limit = 8 * scratchInode
	n, err := scratchStandIn(t).usage(limit, cgroupStandIn(t, 1), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n <= limit || n > limit+scratchInode {
		t.Fatalf("the measure counted %d bytes, not just past its limit of %d", n, limit)
	}
}

// One read of a process's maps meets the measure's deadline: a thread
// that keeps mapping past the line writ reads, and unmapping before it,
// kept a read with 100ms to run going for as long as it ran. Long paths
// make each line slow for the kernel to write. The read here has no bound
// on the bytes it reads, so only the deadline can stop it.
func TestHeldMapsReadMeetsItsDeadline(t *testing.T) {
	dir := deepDir(t, 'c', 240)
	fd, err := unix.Openat(dir, "f", unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	// Pages in a part of the address space that Go and the race detector
	// leave alone, a page apart so that no two mappings merge.
	const base, window = uintptr(0x6f0000000000), 2000
	mapAt := func(i int) error {
		_, _, e := unix.Syscall6(unix.SYS_MMAP, base+uintptr(i)*8192, 4096, unix.PROT_READ, unix.MAP_SHARED|unix.MAP_FIXED_NOREPLACE, uintptr(fd), 0)
		if e != 0 {
			return e
		}
		return nil
	}
	unmapAt := func(i int) { unix.Syscall(unix.SYS_MUNMAP, base+uintptr(i)*8192, 4096, 0) }
	for i := range window {
		if err := mapAt(i); err != nil {
			for j := range i {
				unmapAt(j)
			}
			t.Skipf("map a page at %#x: %v", base+uintptr(i)*8192, err)
		}
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		lo, hi := 0, window
		for end := time.Now().Add(15 * time.Second); time.Now().Before(end); {
			select {
			case <-stop:
				end = time.Now()
				continue
			default:
			}
			if mapAt(hi) == nil {
				hi++
			}
			unmapAt(lo)
			lo++
		}
		for i := lo; i < hi; i++ {
			unmapAt(i)
		}
	}()
	defer func() { close(stop); <-done }()
	h := newHeldFiles(1<<40, time.Now().Add(100*time.Millisecond))
	h.mapsLeft = 1 << 62
	start := time.Now()
	err = h.mappings(os.Getpid())
	if d := time.Since(start); !errors.Is(err, errScratchSlow) || d > 2*time.Second {
		t.Fatalf("a read of maps with 100ms to run returned %v after %v", err, d)
	}
}

// A mapping is the file of a descriptor when their inodes agree and
// either their devices or their paths do, which h knows by a hash: on
// btrfs, maps gives the filesystem's device and stat the subvolume's,
// and two subvolumes may hold files of one inode number.
func TestHeldFilesCountAMappedOpenFileOnce(t *testing.T) {
	const ino = 7
	dev, other := unix.Mkdev(8, 1), unix.Mkdev(0, 42)
	for _, c := range []struct {
		name      string
		dev       uint64
		ino       uint64
		path      string
		wantFiles int64
	}{
		{"the same device and path", dev, ino, "/run/f", 1},
		{"the same device, another path", dev, ino, "/run/g", 1},
		{"another device, the same path", other, ino, "/run/f", 1},
		{"another device and path", other, ino, "/run/g", 2},
		{"another inode", dev, ino + 1, "/run/f", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHeldFiles(1<<40, time.Now().Add(time.Minute))
			if err := h.addOpen(dev, ino, 0, []byte("/run/f")); err != nil {
				t.Fatal(err)
			}
			if err := h.addMapped(c.dev, c.ino, 0, []byte(c.path)); err != nil {
				t.Fatal(err)
			}
			if got := h.total(); got != c.wantFiles*scratchInode {
				t.Fatalf("an open file and a mapping counted %d bytes, not %d files", got, c.wantFiles)
			}
		})
	}
}
