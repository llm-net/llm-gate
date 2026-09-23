package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// 两把锁都在配置根目录下：
//   - .gate-operation.lock 排它：同一时刻只跑一条会改本机 gate 状态的命令（安装 / 升级 / 关联 / 卸载 / 自升级）。
//   - .gate-usage.lock 共享给正在运行的 `gate <工具>`，排它给安装 / 升级 / 卸载工具：工具运行期间不能被换掉。
//
// `gate update` 只替换 gate 自己的可执行文件（Unix 上同目录暂存、原子 rename，正在运行的 gate 进程
// 继续用旧映像，工具子进程不受影响），因此在 Unix 上对 usage 锁只取共享——与正在运行的工具不互斥，
// 仍与安装 / 升级工具互斥。Windows 没法替换被打开的可执行文件，仍取排它。
type operationLease struct{ operation, usage *os.File }

func commandLaunches(args []string) bool {
	if len(args) == 0 || !slices.Contains(toolNames, args[0]) {
		return false
	}
	return len(args) == 1 || !slices.Contains([]string{"connect", "disconnect", "install", "update", "status", "uninstall"}, args[1])
}

// commandSharesUsage 报告这条命令对 usage 锁只取共享：正在运行的工具本身，以及 Unix 上的 `gate update`。
func commandSharesUsage(args []string) bool {
	if commandLaunches(args) {
		return true
	}
	return len(args) > 0 && args[0] == "update" && runtime.GOOS != "windows"
}

func commandMutates(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if slices.Contains([]string{"help", "-h", "--help", "version", "--version", "status"}, args[0]) {
		return false
	}
	// gate media 只读配置、不改本机 gate 状态，陪等期间不能挡住其他 gate 命令。
	if args[0] == "media" {
		return false
	}
	return !(len(args) == 2 && args[1] == "status")
}

// The downloaded gate holds the same locks while an installer commits the
// executable, bootstrap configuration, receipt and PATH edits. Only its two
// internal child commands inherit this lease; regular commands cannot bypass it.
func installerChild(root string, args []string) bool {
	return os.Getenv("GATE_INSTALL_LOCK_HELD") == root && len(args) > 0 && (args[0] == "bootstrap" || args[0] == "__record-install")
}

func runInstaller(root string, args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("安装器缺少提交命令")
	}
	l, err := acquireOperation(root, false)
	if err != nil {
		return err
	}
	defer l.close()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = envWith(os.Environ(), "GATE_INSTALL_LOCK_HELD", root)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errOut
	return runTool(cmd)
}

func openOperationLock(root, name string, shared, create bool) (*os.File, error) {
	path := filepath.Join(root, name)
	if err := noLinkPath(path); err != nil {
		return nil, err
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(path, flags, 0600)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, shared); err != nil {
		busy := lockBusyError(f)
		f.Close()
		return nil, busy
	}
	return f, nil
}

// lockBusyError 组装锁被占用的提示；能查到持锁进程（Linux 读 /proc/locks）就把 PID 与命令列出来，
// 让人知道该退出哪个工具。
func lockBusyError(f *os.File) error {
	msg := "gate 正在安装、更新或运行工具；请关闭对应 gate 进程后重试"
	if holders := lockHolders(f); len(holders) > 0 {
		msg = "gate 正在安装、更新或运行工具（" + strings.Join(holders, "；") + "）；请退出对应工具或结束该进程后重试"
	}
	return errors.New(msg)
}
func acquireOperation(root string, launch bool) (*operationLease, error) {
	if err := noLinkPath(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	l := &operationLease{}
	var err error
	l.operation, err = openOperationLock(root, ".gate-operation.lock", false, true)
	if err != nil {
		return nil, err
	}
	l.usage, err = openOperationLock(root, ".gate-usage.lock", launch, true)
	if err != nil {
		l.close()
		return nil, err
	}
	return l, nil
}
func probeOperation(root string) error {
	op, err := openOperationLock(root, ".gate-operation.lock", false, false)
	if err != nil {
		return err
	}
	if op != nil {
		defer op.Close()
	}
	use, err := openOperationLock(root, ".gate-usage.lock", false, false)
	if use != nil {
		defer use.Close()
	}
	return err
}
func (l *operationLease) releaseOperation() {
	if l != nil && l.operation != nil {
		l.operation.Close()
		l.operation = nil
	}
}
func (l *operationLease) close() {
	if l == nil {
		return
	}
	l.releaseOperation()
	if l.usage != nil {
		l.usage.Close()
		l.usage = nil
	}
}
