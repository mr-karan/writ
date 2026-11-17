// Command hold stands in for a program that a stack runs inside the run's
// filesystem view, which holds no shell. It is built with CGO_ENABLED=0,
// so it needs no file but itself. "hold sleep" sleeps for an hour. "hold
// escape" starts "hold sleep" in a session of its own, prints "escaped",
// and exits, which leaves the copy behind once tofu's group is killed.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sleep":
		time.Sleep(time.Hour)
	case "escape":
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cmd := exec.Command(self, "sleep")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("escaped")
	default:
		os.Exit(2)
	}
}
