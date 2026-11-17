// Package leak hands a state's raw show -json to sinks that are not the
// mask's Index, which TestStateOnlyToMask must refuse.
package leak

import (
	"os"

	"github.com/mr-karan/writ/internal/archtest/testdata/leak/mask"
	"github.com/mr-karan/writ/internal/tofu"
)

type sink struct{}

func (sink) Index(raw []byte) { os.Stdout.Write(raw) }

func toStdout(st tofu.State) { sink{}.Index(st.Unmasked) }

func toNamesake(st tofu.State) { new(mask.Mask).Index(st.Unmasked, nil) }
