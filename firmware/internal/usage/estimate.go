package usage

// estimate.go 是 usage 缺失时的估算兜底（方案 §2.3）。
//
// 它存在的意义是「**显式关掉 usage 的客户端不能借此隐身**」，不是精确计费：
// 只有三条路径拿不到上游 usage——客户端显式 include_usage:false（字节保真契约
// 禁止我们改它）、流中途断、上游确实没报。估算恒偏保守也没关系，量级对就够
// 用；每一笔估算都在聚合里打 estimated_requests、在环里带 estimated 标、在
// 管理台图表下方留「含估算」脚注，绝不冒充真值。
//
// 两条硬规则：
//   - **估算值绝不回写进任何响应**（refs/new-api 会把重建的 usage 塞回响应体，
//     我们的字节保真契约禁止这么做）。
//   - **不落内容**：估算只对已经在内存里的请求 map / 已解析的 SSE 增量数
//     **字符**，算完即弃（§15.1）。
//
// 公式：ASCII rune 每 4 个算 1 token（向上取整），非 ASCII rune 每个算 1
// token。比 count_tokens 兜底的纯 /4 对中文准得多——`heuristicInputTokens`
// 的对外契约不动，这里是它的姊妹函数而不是替代品。
//
// 视频/图片**不估算**：那两条路的量是秒数与张数，猜不出来也不该猜。厂商
// usage 缺失或已过 7 天查询窗口时记 0 元 + estimated 标 + 告警（见 settle.go）。

import (
	"encoding/json"
	"unicode/utf8"
)

// TextCounter 累计多段文本的估算 token——SSE 的输出侧是几百上千个增量分片，
// 必须能一段一段地喂进来。
//
// 为什么不是「每片各自 EstimateTokens 再求和」：公式对 ASCII 是 (n+3)/4
// **向上取整**，逐片取整会把一条 2000 片的流估成实际值的数倍（每片各补最多
// 3/4 个 token）。这里分别累计 ASCII 与非 ASCII 的 rune 数，到 Tokens() 才套
// 一次公式——与「把整段文本一次性交给 EstimateTokens」逐字节等价。
//
// 零值可用。不复制、不留存文本本身（§15.1）：Add 只数 rune 就把参数丢掉。
type TextCounter struct {
	ascii int64
	wide  int64
}

// Add 累计一段文本（空串是空操作）。
func (c *TextCounter) Add(s string) {
	for _, r := range s {
		if r < utf8.RuneSelf {
			c.ascii++
		} else {
			c.wide++
		}
	}
}

// AddBytes 与 [TextCounter.Add] 同义，但直接吃字节切片：估算路径手里是
// json.Marshal 的产物，先转 string 会把整段（agent 长上下文可达数十上百 KB）
// 无谓复制一遍。无效 UTF-8 字节与 range string 同口径（按 U+FFFD 计一个宽
// rune）。同样不复制、不留存内容（§15.1）。
func (c *TextCounter) AddBytes(b []byte) {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r < utf8.RuneSelf {
			c.ascii++
		} else {
			c.wide++
		}
		b = b[size:]
	}
}

// Tokens 返回累计至今的估算 token 数。
func (c TextCounter) Tokens() int64 {
	// (ascii + 3) / 4 = 向上取整。
	return (c.ascii+3)/4 + c.wide
}

// EstimateTokens 按上述公式把一段文本折成 token 数。空串得 0。
func EstimateTokens(s string) int64 {
	var c TextCounter
	c.Add(s)
	return c.Tokens()
}

// EstimateInputTokens 估算一次请求的输入 token：把 messages 与 system 两个
// 字段序列化后套 EstimateTokens。有内容时至少记 1（与 heuristicInputTokens
// 的「至少 1」同口径——一次真实调用不该记成 0 token）。
//
// 序列化产物只在栈上活到计数结束（§15.1）。
func EstimateInputTokens(payload map[string]any) int64 {
	return estimateFields(payload, "messages", "system")
}

// EstimateResponsesInput 是两个 Responses 形入口（标准目录面与 Agents 订阅
// 代理共用）的同一件事：那条协议的输入不叫 messages/system，而是 input
// （字符串或 item 数组）与 instructions（系统提示词）。
//
// 单独一个函数而不是给 EstimateInputTokens 加参数：字段名是**协议事实**，
// 让每个入口在调用点各写一遍字面量，等于把这份事实散到三处；由本包各出一个
// 具名函数，改协议时只改这里。
func EstimateResponsesInput(payload map[string]any) int64 {
	return estimateFields(payload, "input", "instructions")
}

// estimateFields 把 payload 里点名的几个字段序列化后套 EstimateTokens 求和。
func estimateFields(payload map[string]any, fields ...string) int64 {
	var n int64
	for _, field := range fields {
		v, ok := payload[field]
		if !ok || v == nil {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil { // payload 源自合法 JSON，理论不可达
			continue
		}
		// 逐字段各套一次公式（与 EstimateTokens(string(b)) 逐字节等价，
		// 免掉一次整段 string 复制）。
		var c TextCounter
		c.AddBytes(b)
		n += c.Tokens()
	}
	if n < 1 {
		return 1
	}
	return n
}
