// Package fwimage 校验一个文件「是不是本产品的固件二进制」。
//
// 它是升级链路上三处共用的形状闸门（docs/firmware-update.md）：管理台手动
// 上传、官网下载落地、升级引擎安装前的复核。校验不执行目标文件——ELF 头与
// Go build info 都是纯解析（debug/elf + debug/buildinfo），所以 gatewayd 以
// 非特权用户就能做，引擎以 root 复核时也不给未知文件任何执行机会。
//
// 三道闸：ELF 且机器类型与本机 GOARCH 一致（x86-64 主机拦 arm64 包，反之
// 亦然）、Go 主模块路径等于本固件 module（别家的 Go 程序过不来）、以及
// buildinfo.FileMarker 自述标记里的版本（正式构建恒有；dev 裸 go build 没有，
// 由调用方决定要不要容忍）。版本走**文件字节扫描**而不是 debug/buildinfo 的
// 构建设置：-trimpath 构建（本产品恒开）刻意不把 -ldflags 记进 buildinfo，
// 那条路对真实产物读不到版本——Makefile 于是把 lgfw1{版本|}lgfw1
// 整串经 -X 注入，链接器把字面量写进数据段，这里扫出来。这不是密码学签名——
// 它挡的是「拿错文件」，不是恶意构造；签名验证是预留的硬化位
// （docs/firmware-update.md「边界与已知限制」）。
package fwimage

import (
	"bytes"
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
)

// ModulePath 是本固件的 Go module 路径；上传的二进制主模块必须是它。
const ModulePath = "github.com/llm-net/llm-gate/firmware"

// Info 是校验通过后能读出的事实。固件只有一个版本名称，所以这里也只有
// 一个字段。
type Info struct {
	// Version 是构建时 -ldflags 注入的 buildinfo.Version（形如
	// 2608221732-2d81）；裸 `go build` 产物（dev）没有这一项，此时为空串。
	Version string
}

// ErrNotFirmware 表示文件不是本产品、本机架构的固件二进制。
var ErrNotFirmware = errors.New("不是本设备可安装的固件包")

// markerNeedle 返回自述标记的前缀 lgfw1{ ——**运行期拼接**，不写成一个字面量：
// fwimage 自己也编进 llmgate 二进制，一个完整字面量会以自身字节出现在数据段里，
// 扫描器就可能命中自己的针而不是 -X 注入的标记（buildinfo_test 钉着这一幕的
// 反面）。拆两段后，rodata 里只有互不相邻的碎片，唯一完整出现的就是 -X 值。
func markerNeedle() []byte { return []byte("lgf" + "w1{") }

// markerTail 匹配针之后的 `版本|}lgfw1`（长度设上限防扫进乱码）。同针一样
// 运行期拼接，不留完整字面量。竖线后那个捕获组是标记的第二字段，恒发空值：
// 版本名称只有一串，第二字段读出来一律丢弃。
var markerTail = regexp.MustCompile(`^([^|{}\x00]{1,64})\|([^|{}\x00]{0,64})\}lgf` + `w1`)

// markerScanCap 是扫描窗口上限：-X 注入的字面量在数据段里，64 MiB 覆盖整个
// llmgate 二进制（~20 MiB）三倍有余；超过即当没有版本，不无界读。
const markerScanCap = 64 << 20

// Verify 校验 path 处的文件并返回其构建信息。错误面向管理员（zh-CN、
// 不含文件内容），且全部 errors.Is(err, ErrNotFirmware) 可判。
func Verify(path string) (*Info, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w：不是 ELF 可执行文件", ErrNotFirmware)
	}
	machine := f.Machine
	f.Close()

	if want, ok := machineFor(runtime.GOARCH); !ok || machine != want {
		return nil, fmt.Errorf("%w：目标架构与本设备（%s）不符", ErrNotFirmware, runtime.GOARCH)
	}

	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w：读不到 Go 构建信息", ErrNotFirmware)
	}
	if bi.Main.Path != ModulePath {
		return nil, fmt.Errorf("%w：不是 LLM Gate 固件构建", ErrNotFirmware)
	}

	version, err := scanMarker(path)
	if err != nil {
		return nil, fmt.Errorf("%w：读取版本标记失败", ErrNotFirmware)
	}
	return &Info{Version: version}, nil
}

// scanMarker 在文件字节里找版本自述标记；没有标记不是错误（dev 裸构建），
// 返回空版本。
func scanMarker(path string) (version string, err error) {
	return scanMarkerWith(path, markerNeedle(), markerTail)
}

// scanMarkerWith 是 scanMarker 的单标记实现。分块读 + 尾部重叠，内存恒定；
// 针命中但尾不匹配时跳过继续，防字面量碎片凑巧相邻的假阳性。
func scanMarkerWith(path string, needle []byte, tail *regexp.Regexp) (version string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// 标记体上限：版本 ≤64 + '|' + 第二字段 ≤64 + '}lgfw1' < 160。
	const tailMax = 160
	const chunk = 1 << 20
	overlap := len(needle) + tailMax

	buf := make([]byte, 0, chunk+overlap)
	tmp := make([]byte, chunk)

	// scan 消化 buf 里的命中。final=false 时遇到「针命中但标记体还没读全」
	// 会把 buf 截到命中处等下一块；final=true（EOF 终扫）不再等，读到什么
	// 判什么——标记贴着文件尾是常态（测试就把它 append 在尾部）。
	scan := func(final bool) (string, bool) {
		for {
			idx := bytes.Index(buf, needle)
			if idx < 0 {
				return "", false
			}
			rest := buf[idx+len(needle):]
			if len(rest) < tailMax && !final {
				buf = append(buf[:0], buf[idx:]...)
				return "", false
			}
			if m := tail.FindSubmatch(rest); m != nil {
				return string(m[1]), true
			}
			buf = append(buf[:0], buf[idx+len(needle):]...)
		}
	}

	var scanned int64
	for scanned < markerScanCap {
		n, rerr := f.Read(tmp)
		if n > 0 {
			scanned += int64(n)
			buf = append(buf, tmp[:n]...)
			if v, ok := scan(false); ok {
				return v, nil
			}
			// 挂起的部分命中已被 scan 截到 buf 头（长度必小于 overlap），
			// 这里的裁剪只会动「完全无针」的长尾。
			if len(buf) > overlap {
				buf = append(buf[:0], buf[len(buf)-overlap:]...)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if v, ok := scan(true); ok {
					return v, nil
				}
				break
			}
			return "", rerr
		}
	}
	return "", nil
}

// machineFor 把 GOARCH 映到 ELF 机器类型。固件只有两个交付平台：linux/arm64（板子与
// ARM64 主机）与 linux/amd64（x86-64 云主机与电脑）；其余架构不在支持面上，宁可拒绝。
func machineFor(goarch string) (elf.Machine, bool) {
	switch goarch {
	case "arm64":
		return elf.EM_AARCH64, true
	case "amd64":
		return elf.EM_X86_64, true
	}
	return 0, false
}
