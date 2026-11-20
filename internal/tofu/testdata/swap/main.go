// Command swap is a provider that writes over the saved plan of the tofu
// command that started it, as any provider a stack pins could, and then
// becomes hashicorp/random from the plugin cache, which serves tofu in its
// place. It acts only in a stack directory that holds a file named swap,
// and it reads the saved plan's path from tofu's command line: the last
// argument of `tofu show -json PLAN` or `tofu apply ... PLAN`. It is built
// with CGO_ENABLED=0, so it needs no file but itself.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Swapped is what it writes in place of the saved plan.
const Swapped = "swapped by a provider\n"

func main() {
	if _, err := os.Stat("swap"); err == nil {
		swap()
	}
	random, _ := filepath.Glob(filepath.Join(os.Getenv("TF_PLUGIN_CACHE_DIR"), "registry.opentofu.org/hashicorp/random/*/*/terraform-provider-random*"))
	if len(random) == 0 {
		fmt.Fprintln(os.Stderr, "swap: no hashicorp/random in the plugin cache")
		os.Exit(1)
	}
	err := syscall.Exec(random[0], os.Args, os.Environ())
	fmt.Fprintln(os.Stderr, "swap:", err)
	os.Exit(1)
}

func swap() {
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getppid()) + "/cmdline")
	if err != nil {
		return
	}
	args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
	if len(args) < 3 {
		return
	}
	switch string(args[1]) {
	case "show", "apply":
		if plan := string(args[len(args)-1]); plan[0] != '-' {
			_ = os.WriteFile(plan, []byte(Swapped), 0o600)
		}
	}
}
