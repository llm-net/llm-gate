package boardinfo

// SoC eFuse 序列号（SID）的防泄类型。
//
// §15.1 纪律的纵深防御一层：SID 是不可变硬件标识，不应进入日志、审计或 API
// 响应。把它包成自带打码行为的类型，而不是裸 string——误把整份
// [BoardInfo] 传进 logger、错误文本或 HTTP 响应时，出来的仍是打码值。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// SIDHexLen 是 A733 的 SID 十六进制长度：eFuse 序列号 128 位 = 32 位小写 hex。
// 保留这个名字是因为它已被两侧代码与文档引用；**新代码判长度一律用
// [ValidSIDHexLens]**，别再拿它当唯一合法值。
const SIDHexLen = 32

// ValidSIDHexLens 是各平台 SID 的合法十六进制长度。长度不在集合内即拒绝。
//
// 为什么是集合而不是常量：SID 的位宽随 SoC 厂商不同。
//
//   - 32 位 hex（128 位）：全志 A733（**BSP 内核**），`/sys/class/sunxi_info/sys_info`
//     的 sunxi_serial；
//   - 16 位 hex（64 位）：瑞芯微 RK3576、博通 BCM2712、以及主线内核下的全志 H618，
//     `/proc/device-tree/serial-number`。瑞芯微只对外发布 OTP 派生的 64 位芯片 ID，
//     128 位 CPUID 在安全 OTP 区读不到（实测 rockchip-otp0 那 256 字节窗口里
//     没有它，也没有 cells/）；全志 H618 是同一颗芯片在主线内核下只导出了这 64 位
//     出口（见 [socDeviceTree]）。
var ValidSIDHexLens = []int{16, 32}

// validSIDHexLen 报告 n 是否是合法的 SID hex 长度。
func validSIDHexLen(n int) bool {
	for _, l := range ValidSIDHexLens {
		if n == l {
			return true
		}
	}
	return false
}

// SIDValue 承载一枚 SID。零值表示「未读到」（[SIDValue.Valid] 为 false）。
//
// 所有输出路径——[SIDValue.String]、[SIDValue.LogValue] 与 json.Marshal——
// 恒输出打码值。本包不提供完整 SID 的导出方法。
type SIDValue struct {
	sid string
}

// ParseSID 校验并规范化一个 SID 十六进制串（大小写不敏感，统一收成小写）。
// 错误文本只说形状，不回显任何输入片段。
func ParseSID(s string) (SIDValue, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !validSIDHexLen(len(s)) {
		return SIDValue{}, fmt.Errorf("SoC 序列号长度应为 %v 位十六进制之一，实为 %d 位", ValidSIDHexLens, len(s))
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return SIDValue{}, fmt.Errorf("SoC 序列号第 %d 位不是十六进制字符", i+1)
		}
	}
	return SIDValue{sid: s}, nil
}

// Valid 报告本值是否承载了一枚 SID。
func (v SIDValue) Valid() bool { return v.sid != "" }

// String 恒返回打码值：显示与日志两条路永远不吐明文。
func (v SIDValue) String() string { return v.mask() }

// LogValue 实现 slog.LogValuer：SID 进任何 logger 都只剩打码值。
func (v SIDValue) LogValue() slog.Value { return slog.StringValue(v.mask()) }

// MarshalJSON 输出打码值。任何人把 BoardInfo 直接 json.Marshal 进响应体或文件，
// 都泄不出 SID。
func (v SIDValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.mask())
}

// mask 返回可安全显示/落日志的打码值：只保留 4 位前缀。
//
// 刻意不带 logging.RedactKey 那样的 SHA-256 摘要后缀——SID 是结构化的
// （批次/晶圆/坐标），可枚举空间远小于随机生成的 Key，附一个可验证的摘要
// 等于给猜测者一台离线校验器。前缀足够人工比对两块板是不是同一枚。
func (v SIDValue) mask() string {
	switch {
	case v.sid == "":
		return "(empty)"
	case len(v.sid) <= 4:
		return "…"
	default:
		return v.sid[:4] + "…"
	}
}
