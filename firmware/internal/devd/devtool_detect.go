package devd

// 会话里在跑哪个开发工具：界面要在终端标签上标出来（周期性重取会话清单时一起带回）。
// gate 拉起工具后自己留在前台等子进程，tmux 的 #{pane_current_command} 只看得到 gate，
// 所以这里读一次 /proc 的进程表，从每个 pane 的 shell 往下走子进程树：命中 `gate <tool>`
// 最可靠；使用者绕开 gate 直接敲 `claude` / `codex` 之类也认（按可执行名或脚本名）。
// 只读 ppid 与 argv，不读环境、不读文件描述符；非 Linux 没有 /proc 就是什么都不标。

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procInfo 是 /proc 里一个进程的最小读数。
type procInfo struct {
	ppid int
	argv []string
}

// procTable 是一次 /proc 快照：pid → 读数，另按父进程索引子进程。
type procTable struct {
	procs    map[int]procInfo
	children map[int][]int
}

// snapshotProcs 读一遍 /proc。单个进程读不到（正好退出、无权限）就跳过。
func snapshotProcs() *procTable {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	t := &procTable{procs: map[int]procInfo{}, children: map[int][]int{}}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// stat：`pid (comm) state ppid …`——comm 里可能有空格与括号，从最后一个 `)` 之后切。
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(stat[i+1:]))
		if len(f) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		var argv []string
		for _, a := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
			argv = append(argv, string(a))
		}
		t.procs[pid] = procInfo{ppid: ppid, argv: argv}
		t.children[ppid] = append(t.children[ppid], pid)
	}
	return t
}

// toolOf 报告 root 及其全部后代里在跑的开发工具（DevTools 之一）；没有即空串。
// 经 gate 启动的优先，其次才是直接运行的工具名——同一棵树里两者并存时是同一个工具，
// 只是 gate 的读数更准。
func (t *procTable) toolOf(root int) string {
	if t == nil {
		return ""
	}
	direct := ""
	queue := []int{root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := t.procs[pid]; ok {
			tool, viaGate := devToolOf(p.argv)
			if viaGate {
				return tool
			}
			if direct == "" {
				direct = tool
			}
		}
		queue = append(queue, t.children[pid]...)
	}
	return direct
}

// toolBinaries 是各工具可执行文件 / 启动脚本名 → 工具名。
var toolBinaries = map[string]string{
	"codex":        "codex",
	"claude":       "claude",
	"grok":         "grok",
	"cursor-agent": "cursor",
	"opencode":     "opencode",
	"mcode":        "mcode",
}

// devToolOf 从一条 argv 认工具：`gate <tool>` 答 (tool, true)；可执行名（或解释器
// 后面的脚本名：`sh …/gate codex`、`node …/claude`）命中表里的答 (tool, false)。
func devToolOf(argv []string) (tool string, viaGate bool) {
	for i := 0; i < 2 && i < len(argv); i++ {
		base := filepath.Base(argv[i])
		if base == "gate" {
			if i+1 < len(argv) && ValidTool(argv[i+1]) == nil {
				return argv[i+1], true
			}
			return "", false
		}
		if tool, ok := toolBinaries[base]; ok {
			return tool, false
		}
	}
	return "", false
}
