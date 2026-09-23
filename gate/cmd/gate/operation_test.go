package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// `gate update` 在 Unix 与正在运行的工具共存（都只取 usage 共享锁），仍与安装 / 升级工具互斥；
// 被挡时 Linux 报出持锁进程。
func TestOperationLeaseSelfUpdateCoexistsWithRunningTool(t *testing.T) {
	root := t.TempDir()
	running, err := acquireOperation(root, commandSharesUsage([]string{"codex"}))
	if err != nil {
		t.Fatal(err)
	}
	defer running.close()
	// 工具启动后放掉操作锁、只留 usage 共享锁（main.go 的启动路径就是这样）。
	running.releaseOperation()

	update, err := acquireOperation(root, commandSharesUsage([]string{"update"}))
	if runtime.GOOS == "windows" {
		if err == nil {
			update.close()
			t.Fatal("Windows 上 gate update 应仍与运行中的工具互斥")
		}
		return
	}
	if err != nil {
		t.Fatalf("Unix 上 gate update 不该被运行中的工具挡住：%v", err)
	}
	update.close()

	// 安装 / 升级工具仍要排它：被运行中的工具挡住，并且（Linux）点名持锁进程。
	_, err = acquireOperation(root, commandSharesUsage([]string{"codex", "update"}))
	if err == nil {
		t.Fatal("运行中的工具应挡住 gate codex update")
	}
	if !strings.Contains(err.Error(), "正在安装、更新或运行工具") {
		t.Fatalf("提示 = %q", err)
	}
	if runtime.GOOS == "linux" {
		if !strings.Contains(err.Error(), "PID "+strconv.Itoa(os.Getpid())+"：") {
			t.Fatalf("Linux 上应点名持锁进程，得到 %q", err)
		}
	}
}

func TestCommandSharesUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"codex"}, true},
		{[]string{"claude", "--model", "x"}, true},
		{[]string{"codex", "install"}, false},
		{[]string{"codex", "update"}, false},
		{[]string{"update"}, runtime.GOOS != "windows"},
		{[]string{"-url", "http://x"}, false},
		{nil, false},
	} {
		if got := commandSharesUsage(c.args); got != c.want {
			t.Errorf("commandSharesUsage(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}
