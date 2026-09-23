//go:build linux

package devd

// 伪终端：只用 syscall 打开 /dev/ptmx、解锁、取从端路径、设窗口尺寸——不引第三方
// 库（守护进程要保持纯标准库、CGO_ENABLED=0）。

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// ptyPair 是一对主从端。主端交给数据搬运，从端交给子进程做控制终端。
type ptyPair struct {
	master *os.File
	slave  *os.File
}

func openPTY() (*ptyPair, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	var n uint32
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		master.Close()
		return nil, fmt.Errorf("pty number: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open pty slave: %w", err)
	}
	return &ptyPair{master: master, slave: slave}, nil
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); e != 0 {
		return e
	}
	return nil
}

// setWinsize 设主端的窗口尺寸；内核给前台进程组发 SIGWINCH。
func setWinsize(f *os.File, cols, rows uint16) error {
	ws := struct{ rows, cols, x, y uint16 }{rows: rows, cols: cols}
	return ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// startWithPTY 把 cmd 挂到从端上启动（新会话、从端为控制终端），并关掉父进程
// 这边的从端句柄。
func startWithPTY(cmd *exec.Cmd, p *ptyPair) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.slave, p.slave, p.slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0 // 相对于子进程的 fd 表：stdin 即从端
	if err := cmd.Start(); err != nil {
		return err
	}
	p.slave.Close()
	return nil
}
