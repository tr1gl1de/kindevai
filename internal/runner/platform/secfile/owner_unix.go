//go:build unix

package secfile

import (
	"io/fs"
	"os"
	"syscall"
)

func ownedByMe(st fs.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Geteuid()
}
