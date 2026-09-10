package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Inspect file identities only, never process arguments, environment or session
// contents. This also covers a legacy gate or an owned binary launched directly.
func checkProgramUse(p *uninstallPlan) error {
	var targets []os.FileInfo
	for _, c := range p.changes {
		if c.label == "删除受管程序" && c.link == "" {
			targets = append(targets, c.before)
		}
	}
	if p.self != nil {
		targets = append(targets, p.self.before)
	}
	if len(targets) == 0 {
		return nil
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return errors.New("无法读取程序占用状态，请检查 /proc 后重试")
	}
	for _, proc := range procs {
		pid, err := strconv.Atoi(proc.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		base := filepath.Join("/proc", proc.Name())
		paths := []string{filepath.Join(base, "exe")}
		fds, _ := os.ReadDir(filepath.Join(base, "fd"))
		for _, fd := range fds {
			paths = append(paths, filepath.Join(base, "fd", fd.Name()))
		}
		for _, path := range paths {
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			for _, target := range targets {
				if os.SameFile(st, target) {
					return fmt.Errorf("受管程序仍被进程 %d 使用；请关闭后重试", pid)
				}
			}
		}
	}
	return nil
}
