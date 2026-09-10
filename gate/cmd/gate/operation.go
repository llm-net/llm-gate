package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

type operationLease struct{ operation, usage *os.File }

func commandLaunches(args []string) bool {
	if len(args) == 0 || !slices.Contains(toolNames, args[0]) {
		return false
	}
	return len(args) == 1 || !slices.Contains([]string{"connect", "disconnect", "install", "update", "status", "uninstall"}, args[1])
}
func commandMutates(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if slices.Contains([]string{"help", "-h", "--help", "version", "--version", "status"}, args[0]) {
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
		f.Close()
		return nil, errors.New("gate 正在安装、更新或运行工具；请关闭对应 gate 进程后重试")
	}
	return f, nil
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
