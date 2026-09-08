// chat.go 实现 POST /v1/chat/completions 的选路透传（非流式 + SSE）。
//
// 透传语义（iteration-1 的核心承诺，iteration-5 起走动态选路）：
//   - 请求体解析为通用 map，未知 JSON 字段原样保留；只重写 model 为所选来源的
//     upstream_model_id（每次切换来源都按新来源重建），流式时按需注入
//     stream_options.include_usage。
//   - 响应侧唯一的语义改写是把 model 字段改回客户端请求的模型名（见
//     proxy.go）：状态码/响应头（除逐跳头）/其余字段原样回写，上游 4xx/5xx
//     错误体整体不改写；未知响应字段与未知 finish_reason（如 pause_turn）保留。
//   - 选路、故障切换与回写走 proxy.go 的共享核心（SSE 逐块转发与客户端断开
//     取消上游请求都在其中）。
//
// 日志遵守 §15.1：本文件绝不把请求/响应 body 或凭证传入 logger，
// body 元数据由访问日志中间件的 BodyMeter 统一记录。
package gateway

import (
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// handleChatCompletions 是 POST /v1/chat/completions 的入口（已过认证中间件）。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// 读体 + 通用 map 解析（未知字段保留、UseNumber 保数字字面量）+ model
	// 校验：三入口共享的 decodeEntryPayload（entry.go）。
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	// 自此本请求要入账（iteration-9）：模型名已知，收尾时无论成败都记一笔。
	info := beginEntry(r, usage.EntryChat, model)
	// 输入侧估算器（上游不报 usage 时的兜底，懒算一次）：只读 messages/system，
	// 不受逐来源改写 model 的影响。
	info.bill.estimateInput = func() int64 { return usage.EstimateInputTokens(payload) }
	// 预算准入（iteration-9）：鉴权之后、选路之前——被拒的请求要带着模型维度
	// 进账本，但不该消耗一次选路点查，更不该碰上游。
	if !s.admit(w, r, openAIErrorStyle) {
		return
	}

	// 每请求点查三表选路（决策 3）：启停、优先级、换 Key 即时生效。
	// 文本入口只收 kind=text（迭代 8 闸门）：视频/图片模型在这里与不存在同响应。
	cands, status := s.resolveRoute(r.Context(), model, store.ModelKindText, config.ProtocolOpenAIChat)
	switch status {
	case routeModelNotFound:
		// 只回显客户端提交的模型名，不提示任何上游信息。
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("The model %q does not exist or you do not have access to it.", model))
		return
	case routeProtocolMismatch:
		// 该模型的启用来源都不服务 openai 协议（如只挂 ark 型来源）：明确
		// 拒绝并注明所属入口，不做跨协议转换（MVP §6）。
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "protocol_mismatch",
			protocolMismatchMessage(model, config.ProtocolOpenAIChat))
		return
	case routeStoreError:
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			internalErrorMessage)
		return
	}
	// 流式请求未带 include_usage 时注入，保证最终 usage chunk 到达客户端
	// （MVP §4 P0）；与来源无关，只需在选路后做一次。
	if stream, _ := payload["stream"].(bool); stream {
		injectIncludeUsage(payload)
	}

	// chat 入口固定走上游的 openai 协议端点；每个候选来源重建一次请求体
	// （model 改为该来源的 upstream_model_id）。
	s.forward(w, r, cands, forwardSpec{
		protocol: config.ProtocolOpenAIChat,
		path:     "/chat/completions",
		model:    model,
		body: func(upstreamModelID string) ([]byte, error) {
			payload["model"] = upstreamModelID
			return encodeJSON(payload)
		},
		errStyle: openAIErrorStyle,
		observe:  s.chatObserver(info),
	})
}

// injectIncludeUsage 在流式请求缺失 stream_options.include_usage 时注入 true。
// 客户端显式给过 include_usage（含 false）时不改动；stream_options 存在但
// 不是对象时原样透传，让上游按它的规则报错。
func injectIncludeUsage(payload map[string]any) {
	so, ok := payload["stream_options"].(map[string]any)
	switch {
	case ok:
		if _, has := so["include_usage"]; !has {
			so["include_usage"] = true
		}
	case payload["stream_options"] == nil: // 字段缺失（或显式 null）
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
}
