// Package elfcheck 校验一个文件是不是「本机架构的 ELF 可执行文件」。
//
// 它是第三方组件（cloudflared）安装链路上两处共用的形状闸门：gatewayd 侧下载/上传
// 落地时，以及升级引擎以 root 装进 slot 之前的复核。校验只解析 ELF 头
// （debug/elf），不执行目标文件；机器类型与本机 GOARCH 一致（x86-64 主机拦
// arm64 制品，反之亦然）。它挡的是「拿错文件」，签名清单与 SHA-256 才是完整性
// 依据。
package elfcheck

import (
	"debug/elf"
	"errors"
	"fmt"
	"runtime"
)

// ErrNotExecutable 表示文件不是本机架构的 ELF 可执行文件。
var ErrNotExecutable = errors.New("不是本设备架构的可执行文件")

// machineFor 是 GOARCH → ELF e_machine 的对照表（只列产品与开发机会遇到的）。
func machineFor(goarch string) (elf.Machine, bool) {
	switch goarch {
	case "arm64":
		return elf.EM_AARCH64, true
	case "amd64":
		return elf.EM_X86_64, true
	case "arm":
		return elf.EM_ARM, true
	case "386":
		return elf.EM_386, true
	case "riscv64":
		return elf.EM_RISCV, true
	}
	return 0, false
}

// Verify 校验 path 处的文件：ELF、64 位类（arm64/amd64 都是）、机器类型与本机
// GOARCH 一致、且是可执行或位置无关可执行类型。错误面向管理员，不含文件内容。
func Verify(path string) error {
	return verifyFor(path, runtime.GOARCH)
}

func verifyFor(path, goarch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%w：不是 ELF 文件", ErrNotExecutable)
	}
	defer f.Close()
	want, ok := machineFor(goarch)
	if !ok {
		return fmt.Errorf("%w：本机架构 %s 不受支持", ErrNotExecutable, goarch)
	}
	if f.Machine != want {
		return fmt.Errorf("%w：文件面向 %s，本机是 %s", ErrNotExecutable, f.Machine, want)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("%w：ELF 类型 %s 不是可执行文件", ErrNotExecutable, f.Type)
	}
	return nil
}

// HostMachine 返回本机 GOARCH 对应的 ELF 机器类型（测试构造假制品用）。
func HostMachine() (elf.Machine, bool) { return machineFor(runtime.GOARCH) }
