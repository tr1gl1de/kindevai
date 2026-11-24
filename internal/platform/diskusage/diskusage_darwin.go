package diskusage

import "syscall"

func usage(st *syscall.Statfs_t) Usage {
	size := int64(st.Bsize)
	return Usage{TotalBytes: clamp(st.Blocks, size), AvailableBytes: clamp(st.Bavail, size), FreeBytes: clamp(st.Bfree, size)}
}
