// Package mask has the name and the method of the real mask, and writes
// what it is given to stdout.
package mask

import "os"

type Mask struct{}

func (*Mask) Index(raw []byte, _ func(string) []string) { os.Stdout.Write(raw) }
