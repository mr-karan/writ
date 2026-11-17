//go:build race

package policy_test

// The race detector's heap needs address space at fixed places, which the
// evaluation process's cap on its address space can deny.
func init() { bigMemory = 8 << 30 }
