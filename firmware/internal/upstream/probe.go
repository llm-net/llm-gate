package upstream

// probe.go 是管理面「测试来源」的最小连通性探测：对一条来源、一个入口协议
// 发一次网关自造的 1 token 请求（"ping"），验证「地址可达 + 凭证有效 +
// 来源侧模型 ID 存在 + 账户可扣费」这条链路。请求内容是网关自己生成的固定
// 文本，不含任何客户内容。
//
// §15.1 边界：探测结果只带状态码、耗时与**上游错误体里的简短摘要**——摘要
// 流向管理 API 响应（管理员就是要看拒绝原因），但绝不落日志与审计 detail；
// 通用适配只返回安全的状态摘要，成功体校验协议形状；正文不留存。
// 凭证只经 Account.Authorize 注入请求头。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/config"
)

// probeAnthropicVersion 是探测请求携带的 anthropic-version。数据面透传时该头
// 来自客户端；探测请求是网关自造的，取 iteration-2 实测通过的版本值。
const probeAnthropicVersion = "2023-06-01"

// probeReadCap 是探测响应体的读取上限：错误体用于摘要提取，正常远小于此；
// 成功体只为连接复用而排空，超限即弃。
const probeReadCap = 32 << 10

// probeMessageMaxRunes 是返回给管理面的失败摘要长度上限。
const probeMessageMaxRunes = 200

// ProbeResult 是一次 (来源, 入口协议) 探测的结果。
type ProbeResult struct {
	Questions []SystemOneProbeQuestion
	// OK 表示上游返回 2xx；通用适配还要求响应符合所选协议的基本形状。
	OK bool
	// Status 是上游 HTTP 状态码；0 表示未收到 HTTP 响应（连接失败/超时）。
	Status int
	// LatencyMS 是从发出请求到响应体读毕（或失败）的全程毫秒数。
	LatencyMS int64
	// Message 是失败摘要：上游错误体的 error.message，或传输层错误归类；
	// 成功时为空。只面向管理 API 响应，调用方不得写入日志或审计。
	Message string
}

// Probe 对 acct 在 protocol 入口发一次最小探测请求（modelID 为来源侧模型 ID）。
// 超时由 ctx 控制（调用方限定单次探测上限）；client 复用数据面的分层超时配置。
func Probe(ctx context.Context, client *http.Client, acct Account, protocol, modelID string) ProbeResult {
	path := "/chat/completions"
	if protocol == config.ProtocolOpenAIResponses {
		path = "/responses"
	}
	if protocol == config.ProtocolSystemOne {
		path = "/v1/systemone"
	}
	if protocol == config.ProtocolAnthropicMessages {
		path = "/messages"
	}
	url, ok := acct.URL(protocol, path)
	if !ok { // 调用方按 Endpoint 过滤后才调用，防御分支
		return ProbeResult{Message: "该上游不服务此入口协议"}
	}

	req, err := http.NewRequestWithContext(acct.EgressContext(ctx), http.MethodPost, url, bytes.NewReader(probeBody(protocol, modelID)))
	if err != nil {
		return ProbeResult{Message: trimMessage("构造探测请求失败: " + err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	if protocol == config.ProtocolAnthropicMessages {
		req.Header.Set("anthropic-version", probeAnthropicVersion)
	}
	if acct.Type == config.UpstreamOpenCodeGo {
		// 探测是独立的一轮对话，使用自己的客户端标识与随机会话；不借用
		// 管理员 Cookie、设备身份或客户对话，且不在正常转发时代造会话。
		var session [16]byte
		rand.Read(session[:])
		req.Header.Set("User-Agent", "llmgate/"+buildinfo.Version+" source-probe")
		req.Header.Set(openCodeSessionHeader, hex.EncodeToString(session[:]))
	}
	acct.Authorize(req.Header, protocol)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{LatencyMS: sinceMS(start), Message: transportMessage(err)}
	}
	defer resp.Body.Close()
	// 限量读体：通用适配与 System One 校验响应结构，其余非 2xx 提取摘要；
	// 响应正文不落日志或持久化。
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, probeReadCap+1))
	latency := sinceMS(start)
	if acct.Type == config.UpstreamGeneric || protocol == config.ProtocolSystemOne {
		result := ProbeResult{Status: resp.StatusCode, LatencyMS: latency}
		if resp.StatusCode/100 != 2 {
			result.Message = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
			return result
		}
		if readErr != nil || len(body) > probeReadCap {
			result.Message = "响应读取失败或超过测试大小限制"
			return result
		}
		var payload map[string]json.RawMessage
		field := map[string]string{config.ProtocolOpenAIChat: "choices", config.ProtocolOpenAIResponses: "output", config.ProtocolAnthropicMessages: "content", config.ProtocolSystemOne: "answers"}[protocol]
		if json.Unmarshal(body, &payload) != nil || payload[field] == nil || string(payload[field]) == "null" {
			result.Message = "响应格式不符合所选协议面"
			return result
		}
		if protocol == config.ProtocolSystemOne {
			result.Questions, result.OK = systemOneProbeResults(payload[field])
			if !result.OK {
				result.Message = "System One 混合测试未全部通过"
			}
			return result
		} else {
			var items []json.RawMessage
			if json.Unmarshal(payload[field], &items) != nil {
				result.Message = "响应格式不符合所选协议面"
				return result
			}
		}
		result.OK = true
		return result
	}
	if resp.StatusCode/100 == 2 {
		return ProbeResult{OK: true, Status: resp.StatusCode, LatencyMS: latency}
	}
	return ProbeResult{Status: resp.StatusCode, LatencyMS: latency, Message: errorSummary(body)}
}

// probeBody 使用固定测试输入；System One 单次请求包含 Choice、Noul、Score 三题。
func probeBody(protocol, modelID string) []byte {
	payload := map[string]any{
		"model":      modelID,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	}
	if protocol == config.ProtocolOpenAIChat {
		payload["stream"] = false
	}
	switch protocol {
	case config.ProtocolOpenAIResponses:
		payload = map[string]any{"model": modelID, "input": "ping", "max_output_tokens": 16, "stream": false, "store": false}
	case config.ProtocolSystemOne:
		payload = systemOneProbePayload(modelID)
	}
	b, _ := json.Marshal(payload) // 固定形状的 map，序列化不失败
	return b
}

// sinceMS 取自 start 起的毫秒数，至少 1——0ms 在界面上像"没测过"。
func sinceMS(start time.Time) int64 {
	if ms := time.Since(start).Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

// transportMessage 把传输层错误归类为可展示摘要。错误文本里只有地址与网络
// 原因（凭证在请求头里，不进 error 字符串），截断后可直接展示。
func transportMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "探测超时：连接或响应未在时限内完成"
	case errors.Is(err, context.Canceled):
		return "探测已取消"
	}
	return trimMessage("连接上游失败：" + InnerErrText(err))
}

// InnerErrText 取 HTTP 客户端错误的内层文本：剥掉 *url.Error 的
// `Post "<完整 URL>":` 外层，只留拨号/校验/传输原因。两个动机共用一个函数：
// 探测摘要里地址在内层 `dial tcp <addr>` 已出现一次，外层的第二份只会盖住
// 真正的原因；而视频产物取流的 URL 查询串是签名凭证（§15.1），完整 URL
// 一个字符都不进日志。
func InnerErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

// errorSummary 从上游错误体提取简短摘要：OpenAI 风格 {"error":{"message"}}
// 与 Anthropic 风格 {"type":"error","error":{"type","message"}} 都取
// error.message；解析不出时退回截断的原文（多为网关/CDN 的非 JSON 错误页）。
func errorSummary(body []byte) string {
	var eb struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &eb) == nil && eb.Error.Message != "" {
		if eb.Error.Type != "" {
			return trimMessage(fmt.Sprintf("%s: %s", eb.Error.Type, eb.Error.Message))
		}
		return trimMessage(eb.Error.Message)
	}
	if s := trimMessage(string(body)); s != "" {
		return s
	}
	return "上游未返回错误详情"
}

// trimMessage 把探测摘要收敛到展示上限（CleanMessage 的探测面固定档）。
func trimMessage(s string) string { return CleanMessage(s, probeMessageMaxRunes) }

// CleanMessage 把一段面向人的消息收敛为单行合法 UTF-8 并截断到 maxRunes
// （超出以 … 结尾）：无效 UTF-8 替换为 �、控制字符压成空格、连续空白折叠。
// 探测摘要与网关任务面的厂商错误信息共用——脏字节既不该进管理面响应，
// 也不该进数据面错误体与 aigc_tasks.error_message 落库。
func CleanMessage(s string, maxRunes int) string {
	s = string(bytes.ToValidUTF8([]byte(s), []byte("�")))
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}
