package main

import (
	"io/fs"
	"syscall"
	"time"
)

type fileTimes struct {
	uid    int
	atime  time.Time
	mtime  time.Time
	blocks int64
}

func statTimes(fi fs.FileInfo) (fileTimes, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileTimes{}, false
	}
	return fileTimes{
		uid:    int(st.Uid),
		atime:  time.Unix(st.Atim.Sec, st.Atim.Nsec),
		mtime:  time.Unix(st.Mtim.Sec, st.Mtim.Nsec),
		blocks: st.Blocks,
	}, true
}
