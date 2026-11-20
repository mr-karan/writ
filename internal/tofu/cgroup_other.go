//go:build !linux

package tofu

import "errors"

var errNoCgroups = errors.New("cgroup limits need Linux")

func setUpCgroups() (string, error)   { return "", errNoCgroups }
func checkCgroups() (string, error)   { return "", errNoCgroups }
func seccompReady() error             { return errors.New("the seccomp filter needs Linux") }

// stepCgroup is a step's cgroup, which only Linux makes.
type stepCgroup struct{}

func newStepCgroup(string, Limits) (*stepCgroup, error) { return nil, errNoCgroups }
func (*stepCgroup) kill() error                         { return nil }
func (*stepCgroup) hit() string                         { return "" }
func (*stepCgroup) remove() error                       { return nil }
func (*stepCgroup) heldRemoved(uint64) (int64, error)   { return 0, nil }
