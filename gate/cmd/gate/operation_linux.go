//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// lockHolders 从 /proc/locks 找出持有 f 所指文件 flock 的进程，逐个渲染成「PID 1234：gate codex」；
// 读不到（非 Linux 语义、权限、进程已退出）就返回空，调用方退回通用提示。只用于错误提示，不用于判定。
func lockHolders(f *os.File) []string {
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	raw, err := os.Open("/proc/locks")
	if err != nil {
		return nil
	}
	defer raw.Close()
	var out []string
	seen := map[int]bool{}
	sc := bufio.NewScanner(raw)
	for sc.Scan() {
		// 形如：1: FLOCK  ADVISORY  WRITE 1234 08:01:5678 0 EOF
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[1] != "FLOCK" {
			continue
		}
		pid, err := strconv.Atoi(fields[4])
		if err != nil || pid <= 0 || seen[pid] {
			continue
		}
		parts := strings.Split(fields[5], ":")
		if len(parts) != 3 {
			continue
		}
		ino, err := strconv.ParseUint(parts[2], 10, 64)
		if err != nil || ino != uint64(st.Ino) {
			continue
		}
		seen[pid] = true
		out = append(out, fmt.Sprintf("PID %d：%s", pid, processCommand(pid)))
	}
	return out
}

// processCommand 读进程的命令行（最多前四个参数，够认出 `gate codex` / `gate claude update`）；读不到返回「未知进程」。
func processCommand(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(raw) == 0 {
		return "未知进程"
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(args) > 4 {
		args = append(args[:4], "…")
	}
	return strings.Join(args, " ")
}
