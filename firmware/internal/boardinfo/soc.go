package boardinfo

// SoC 身份源。每个 SoC 厂商把「这颗芯片是谁」放在不同的地方，本文件是那些
// 差异的唯一收口——[profile] 声明自己用哪一种，其余代码只看 [socIdentity]。
//
// 两条路线的共同硬要求（新增第三种平台前先对照）：
//
//   - **全局可读**：官网固件兼容筛选只认型号、不读取 SID；本地诊断读取 SID
//     时仍不要求额外设备节点授权。需要 root 的板卡 EEPROM 只由显式的
//     `sudo llmgate boardinfo` 诊断命令读取。
//   - **每颗芯片唯一且不可改写**：信封的安全论证建立在「克隆一张 SD 卡带不走
//     这颗芯片」上。可改写的身份（EEPROM 里的板卡序列号）是约定而非物理，
//     不能拿来派生密钥。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// socKind 标识一个 SoC 身份源的实现。
type socKind int

const (
	// socSunxi 是全志 **BSP 内核**路线：/sys/class/sunxi_info/sys_info 一份
	// key : value 文本，四项齐全（platform/serial/chiptype/batchno）。
	//
	// **「全志」不等于走这条**：这个节点是 BSP 内核的产物，主线内核上不存在。
	// 同为全志的 h618-x98h 跑 Armbian 主线 sunxi64 内核，因此走的是
	// [socDeviceTree]。选哪条看内核，不看芯片厂商。
	socSunxi socKind = iota
	// socDeviceTree 是设备树路线：serial-number 与 compatible 两个属性
	// （均 0444 全局可读）。**瑞芯微 RK3576、博通 BCM2712（树莓派 5）与
	// 主线内核下的全志 H618 走的是同一条**——三家都只对外发布 OTP/eFuse
	// 派生的 64 位芯片 ID，放的位置、读法、解析逐字节相同，所以这里是一种
	// 机制而不是三种（2026-08-17 接入树莓派时按这个口径把原 socRockchip 改名
	// 为机制名；再来一家同样读设备树的 SoC 直接复用，不必新增常量）。
	//
	// 只有 SID 与平台名两项。chiptype/batchno 由 [profile] 的占位常量补上
	// （2026-08-10 决定：不为此升 schema），但缺的原因两样：瑞芯微与博通
	// **没有这两个概念**，全志 H618 是**概念在、主线内核没导出**——分别记在
	// boardinfo.go 的 profile 注释里。
	socDeviceTree
)

// readSoCBy 按平台读取 SoC 身份。
func (c *Collector) readSoCBy(kind socKind) (socIdentity, error) {
	switch kind {
	case socSunxi:
		return c.readSunxiSoC()
	case socDeviceTree:
		return c.readDTSoC()
	default:
		return socIdentity{}, fmt.Errorf("%w：未知的 SoC 身份源 %d", ErrUnsupportedBoard, kind)
	}
}

// dtPath 返回设备树属性在 sysfs 里的路径。
//
// 刻意走 /sys/firmware/devicetree/base 而不是 /proc/device-tree：后者只是前者
// 的兼容符号链接，走 sysfs 这条能让整包共用一个 sysRoot，夹具注入照 sysinfo
// 的老办法即可。
func (c *Collector) dtPath(prop string) string {
	return filepath.Join(c.sysRoot, "firmware", "devicetree", "base", prop)
}

// readDTString 读一个设备树字符串属性。设备树的字符串以 NUL 结尾，属性里可能
// 并列多个（compatible 就是），这里统一切成切片。
func readDTString(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range strings.Split(string(data), "\x00") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("设备树属性 %s 为空", path)
	}
	return out, nil
}

// readDTSoC 读设备树路线的芯片身份（瑞芯微、树莓派、主线内核下的全志共用，
// 见 [socDeviceTree]）。
//
//	<sysRoot>/firmware/devicetree/base/serial-number  → 16 位 hex 芯片 ID
//	<sysRoot>/firmware/devicetree/base/compatible     → rockchip,rk3576-evb1-v10
//	                                                    rockchip,rk3576
//	                                     树莓派 5 则是 raspberrypi,5-model-b
//	                                                    brcm,bcm2712
//	                                     X98H 则是     vontar,x98h
//	                                                    allwinner,sun50i-h618
//
// 三家的 serial-number 都是固件/内核从 OTP/eFuse 取出的 64 位值、全局可读（0444）、
// 与 /proc/cpuinfo 的 Serial 同源同值，位宽差异的安全口径见 [ValidSIDHexLens]：
//
//   - 瑞芯微：由安全 OTP 里的 128 位 CPUID 派生。**128 位原值取不到**——
//     rockchip-otp0 暴露的那 256 字节窗口里没有它（实测只有 "RK5v" 头部与
//     校准值），内核也没导出具名 cell。
//   - 树莓派：出厂烧进 OTP 的板卡序列号，软件改不了。老机型（Pi 4 及更早）
//     这 16 位 hex 的高半截常是固定前缀、真实熵约 32 位；手上这块 Pi 5 读出来
//     高位不固定，但**只有一块板，不据此断言全系**——安全余量照 64 位那条算。
//   - 全志（主线内核）：U-Boot 由 128 位 eFuse SID 取两个 32 位字拼成，
//     **高半截很可能是芯片型号常量**（手上这块 H618 的高 32 位形如 0x33802000
//     这类全志 chip-id），所以真实熵可能只有 32 位。与前两家不同的是，这块板上
//     128 位原值确实还有一个全局可读的出口（sunxi-sid nvmem）——**但没验明布局
//     之前不改这里**，待办记在 docs-board/h618-x98h/README.md。
func (c *Collector) readDTSoC() (socIdentity, error) {
	serialPath := c.dtPath("serial-number")
	serials, err := readDTString(serialPath)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return socIdentity{}, fmt.Errorf("读取 SoC 序列号 %s: %w", serialPath, err)
		}
		return socIdentity{}, fmt.Errorf("%w：读取 SoC 序列号 %s: %w", ErrUnsupportedBoard, serialPath, err)
	}
	// 错误里带上字段名与形状，但绝不回显读到的值（§15.1）。
	sid, err := ParseSID(serials[0])
	if err != nil {
		return socIdentity{}, fmt.Errorf("SoC 序列号 %s 非法: %w", serialPath, err)
	}

	// 平台名取 compatible 的**末项**：设备树的约定是从最具体排到最泛，
	// 末项恒为 "厂商,芯片型号"（rockchip,rk3576），正是要的那一项。
	// 取首项会拿到板级型号（rockchip,rk3576-evb1-v10），换块同芯片的板子就变了。
	compatPath := c.dtPath("compatible")
	compat, err := readDTString(compatPath)
	if err != nil {
		return socIdentity{}, fmt.Errorf("%w：读取设备树 compatible %s: %w", ErrUnsupportedBoard, compatPath, err)
	}
	last := compat[len(compat)-1]
	_, chip, found := strings.Cut(last, ",")
	if !found || chip == "" {
		return socIdentity{}, fmt.Errorf("%w：设备树 compatible 末项 %q 不是「厂商,型号」形", ErrUnsupportedBoard, last)
	}

	return socIdentity{platform: strings.ToUpper(chip), sid: sid}, nil
}

// readDTPCBVersion 从设备树 model 串尾部取板卡改版号：
//
//	"Raspberry Pi 5 Model B Rev 1.1" → "1.1"
//
// 只给声明了 pcbVersionFromDTModel 的档案用，理由见 [profile]：那类档案的
// compatible 不含改版号，写成常量就会在别的改版上撒谎。取不到即报错——
// 本包「必填源读不到就整体报错」的口径对这一项照旧成立（对比 chiptype/batchno
// 那两项是「本平台没有这个概念」，不是读失败）。
func (c *Collector) readDTPCBVersion() (string, error) {
	path := c.dtPath("model")
	models, err := readDTString(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", fmt.Errorf("读取设备树 model %s: %w", path, err)
		}
		return "", fmt.Errorf("%w：读取设备树 model %s: %w", ErrUnsupportedBoard, path, err)
	}
	// model 串的末两项恒为 "Rev X.Y"。不做更宽松的猜测：认不出就报错，
	// 好过把型号串里随便哪个词当成改版号写进诊断结果。
	fields := strings.Fields(models[0])
	if len(fields) < 2 || !strings.EqualFold(fields[len(fields)-2], "Rev") {
		return "", fmt.Errorf("设备树 model %q 末尾不是「Rev X.Y」形，取不到板卡改版号", models[0])
	}
	return fields[len(fields)-1], nil
}

// matchDTCompatible 报告本机设备树的 compatible 是否含 want。
// 无 EEPROM 的板子靠这条认型号档案。
func (c *Collector) matchDTCompatible(want string) bool {
	compat, err := readDTString(c.dtPath("compatible"))
	if err != nil {
		return false
	}
	for _, s := range compat {
		if s == want {
			return true
		}
	}
	return false
}
