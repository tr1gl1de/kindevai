package diskusage

import "syscall"

func usage(st *syscall.Statfs_t) Usage {
	size := st.Frsize
	if size <= 0 {
		size = st.Bsize
	}
	return Usage{TotalBytes: clamp(st.Blocks, size), AvailableBytes: clamp(st.Bavail, size), FreeBytes: clamp(st.Bfree, size)}
}
