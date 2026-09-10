//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func isReparse(os.FileInfo) bool { return false }
func lockFile(f *os.File, shared bool) error {
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	return syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
}
func isTerminalFile(f *os.File) bool { return isTerminal(f) }

func prepareSelfRemoval(c *fileChange, out, errOut io.Writer) (func() error, func(), error) {
	noop := func() {}
	if c == nil {
		return func() error { return nil }, noop, nil
	}
	if err := c.check(); err != nil {
		return nil, noop, err
	}
	return func() error {
		if err := c.check(); err != nil {
			return err
		}
		if err := os.Remove(c.path); err != nil {
			return err
		}
		if c.emptyParent {
			dir := filepath.Dir(c.path)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				return os.Remove(dir)
			}
			fmt.Fprintln(out, "保留非空安装目录："+dir)
		}
		return nil
	}, noop, nil
}
func printSelfRemovalResult(out io.Writer) {
	fmt.Fprintln(out, "gate 已卸载；保留不含凭据的安装记录。PATH 如有调整，请重新打开终端。")
}

func readUserPath() (string, error) { return "", nil }
func writeUserPath(string) error    { return nil }
