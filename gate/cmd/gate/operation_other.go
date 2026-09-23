//go:build !linux

package main

import "os"

// lockHolders 只在 Linux 能从 /proc/locks 找持锁进程；其它平台退回通用提示。
func lockHolders(*os.File) []string { return nil }
