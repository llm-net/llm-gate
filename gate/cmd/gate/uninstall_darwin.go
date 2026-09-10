package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func checkProgramUse(p *uninstallPlan) error {
	paths := []string{}
	for _, c := range p.changes {
		if c.label == "删除受管程序" && c.link == "" {
			paths = append(paths, c.path)
		}
	}
	if p.self != nil {
		paths = append(paths, p.self.path)
	}
	for len(paths) > 0 {
		n := min(64, len(paths))
		args := append([]string{"-t", "--"}, paths[:n]...)
		paths = paths[n:]
		cmd := exec.Command("/usr/sbin/lsof", args...)
		cmd.Env = installerEnv()
		out, err := cmd.Output()
		var ee *exec.ExitError
		if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
			return errors.New("无法检查程序占用状态，请检查系统 lsof 后重试")
		}
		for _, raw := range strings.Fields(string(out)) {
			pid, err := strconv.Atoi(raw)
			if err == nil && pid != os.Getpid() {
				return fmt.Errorf("受管程序仍被进程 %d 使用；请关闭后重试", pid)
			}
		}
	}
	return nil
}
