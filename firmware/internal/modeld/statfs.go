package modeld

import "syscall"

// freeBytes 是 dir 所在文件系统对非特权用户可用的字节数（取不到为 0）。
func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
