package importer

import (
	"github.com/mr-karan/writ/internal/schema"
	"github.com/mr-karan/writ/internal/value"
)

// inClearOf reports whether s refuses v, a value of blk that src holds,
// written in the clear for self.
func inClearOf(s *secretSet, blk schema.Block, v value.Value, self, _ *node) bool {
	return s.inClear(blk, v, self)
}
