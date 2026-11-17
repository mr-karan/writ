package provider

import (
	"io/fs"
	"syscall"
)

func stampOf(fi fs.FileInfo) (fileStamp, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileStamp{}, false
	}
	return fileStamp{
		dev:   uint64(st.Dev),
		ino:   st.Ino,
		size:  st.Size,
		mtime: st.Mtim.Nano(),
		ctime: st.Ctim.Nano(),
	}, true
}
