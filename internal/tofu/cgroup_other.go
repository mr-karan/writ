//go:build !linux

package tofu

import (
	"errors"
	"time"
)

var errNoCgroups = errors.New("cgroup limits need Linux")

func setUpCgroups() (cgroupSetup, error) { return cgroupSetup{}, errNoCgroups }
func checkCgroups() (cgroupSetup, error) { return cgroupSetup{}, errNoCgroups }
func seccompReady() error                { return errors.New("the seccomp filter needs Linux") }

// stepCgroup is a step's cgroup, which only Linux makes.
type stepCgroup struct{}

func newStepCgroup(cgroupSetup, Limits) (*stepCgroup, error)           { return nil, errNoCgroups }
func (*stepCgroup) kill() error                                        { return nil }
func (*stepCgroup) hit() (string, bool)                                { return "", false }
func (*stepCgroup) sharedHit() string                                  { return "" }
func (*stepCgroup) remove() error                                      { return nil }
func (*stepCgroup) heldRemoved(*User, int64, time.Time) (int64, error) { return 0, nil }
