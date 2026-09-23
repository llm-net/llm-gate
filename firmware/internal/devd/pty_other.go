//go:build !linux

package devd

// 守护进程只交付 Linux；别的平台只保证包能编译（go vet ./... 在 macOS 上也跑得过），
// 终端功能答「不支持」。

import (
	"errors"
	"os"
	"os/exec"
)

type ptyPair struct {
	master *os.File
	slave  *os.File
}

var errNoPTY = errors.New("此平台不支持伪终端")

func openPTY() (*ptyPair, error)                { return nil, errNoPTY }
func setWinsize(*os.File, uint16, uint16) error { return errNoPTY }
func startWithPTY(*exec.Cmd, *ptyPair) error    { return errNoPTY }
