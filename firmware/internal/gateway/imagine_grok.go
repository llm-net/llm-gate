// imagine_grok.go 是 Grok Build 订阅接入面的 Grok Imagine 门：这份订阅自带的
// 图片/视频生成，挂在与 Responses 面同一张脸下——
//
//	POST /agents/grok/v1/images/generations     文生图
//	POST /agents/grok/v1/images/edits           图生图（参考图经 image 对象给）
//	POST /agents/grok/v1/videos/generations     提交视频生成，回 request_id
//	GET  /agents/grok/v1/videos/{request_id}    轮询视频任务
//
// 设备只做**鉴权替换的逐字节透传**：路径就是 xAI 官方 Imagine API 在
// https://api.x.ai/v1 之后的那一段，请求体不解析语义、不改 model、不翻译参数，
// 响应（含非 2xx 错误体）原样回给客户端；客户端把 SDK 的 base_url 换成
// `<设备>/agents/grok/v1`、Bearer 换成自己的 sk_ Key 即可。上游用的是同一份
// Grok Build 订阅令牌与 X-XAI-Token-Auth 标记（buildAgentRequest），401 换代重试
// 与 Responses 面同一段（forwardAgent）。产物 URL 是厂商签名地址，设备不代理
// 下载也不合成设备地址。
//
// 计量：画图门 beginEntry 到 usage.EntryImagineImage（张数从响应 data[] 数出，
// 0 元是真值；管理员在目录建同名 image 行录 ark_image_each 时按张记名义金额），
// 视频提交门到 usage.EntryImagineVideo（1 次请求、0 元，没有任务行与清算）。
// 轮询 GET 是辅助调用：不入账、不消耗密钥 RPM，只进访问日志（同 Cursor 的
// 非 Run RPC 口径）。两个门都过子树闸（withDevTool）与订阅勾选闸
// （subscriptionPermitted），未连订阅回 409 agent_not_configured。
//
// §15.1：本文件绝不把请求/响应 body 传入 logger；提示词、参考图与产物 URL 只在
// 转发字节流里，张数是唯一进内存点位的读数。
package gateway

import (
	"net/http"
	"net/url"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// 相对 grokImagineBaseURL 的官方路径。
const (
	grokImagesGenerationsPath = "/images/generations"
	grokImagesEditsPath       = "/images/edits"
	grokVideosGenerationsPath = "/videos/generations"
	grokVideosPath            = "/videos/"
)

// grokImagineRequestIDMax 是轮询路径段的长度上限：官方 request_id 是短 id，
// 超长的段不出网、按未知路径处理。
const grokImagineRequestIDMax = 256

// SetGrokImagineEndpoint 把 Grok Imagine 官方 API 的端点根指向别处（httptest
// 假后端）。**只给开发期与测试用**，生产恒是编译常量；空串恢复内置常量。
// 令牌状态与本设置无关。
func (s *Server) SetGrokImagineEndpoint(base string) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	s.grokImagine = base
}

// grokImagineEndpoint 取端点根（开发期可覆盖）。
func (s *Server) grokImagineEndpoint() string {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.grokImagine != "" {
		return s.grokImagine
	}
	return grokImagineBaseURL
}

func (s *Server) handleGrokImagesGenerations(w http.ResponseWriter, r *http.Request) {
	s.handleGrokImagineSubmit(w, r, grokImagesGenerationsPath, usage.EntryImagineImage)
}

func (s *Server) handleGrokImagesEdits(w http.ResponseWriter, r *http.Request) {
	s.handleGrokImagineSubmit(w, r, grokImagesEditsPath, usage.EntryImagineImage)
}

func (s *Server) handleGrokVideosGenerations(w http.ResponseWriter, r *http.Request) {
	s.handleGrokImagineSubmit(w, r, grokVideosGenerationsPath, usage.EntryImagineVideo)
}

// handleGrokImagineSubmit 是三个 POST 门共用的一段：解码（model 必填）→ 订阅
// 勾选闸 → 入账 → 预算准入 → 名义价旋钮（只画图门、只借 image 行）→ 取订阅
// 凭据 → 转发。顺序与 handleAgentResponsesFor 逐条对齐。
func (s *Server) handleGrokImagineSubmit(w http.ResponseWriter, r *http.Request, path, entry string) {
	// 参考图可能以 data URI 内嵌：与视频/图片入口同一体积闸（超限 413）。
	r.Body = http.MaxBytesReader(w, r.Body, videoSubmitBodyLimit)
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	if _, ok := s.subscriptionPermitted(w, r, store.AgentProviderGrok, false); !ok {
		return
	}
	info := beginEntry(r, entry, model)
	if !s.admit(w, r, openAIErrorStyle) {
		return
	}
	if entry == usage.EntryImagineImage {
		// 视频不借价：视频形态的 Cost 按上游族分流，订阅路径没有上游族，借了
		// 也算不出数；视频提交恒记 1 次、0 元。
		s.applyAgentModelPricingKind(r, info, model, store.ModelKindImage)
	}
	sess, accountID, ok := s.agentCredential(w, r, store.AgentProviderGrok)
	if !ok {
		return
	}
	body, err := encodeJSON(payload)
	if err != nil { // map 源自合法 JSON，理论不可达；防御分支
		s.log.Error("重序列化请求体失败", "request_id", info.id, "err", err.Error())
		openAIErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	rw := respRewrite{model: model, rewrite: keepModelField}
	if entry == usage.EntryImagineImage {
		rw.observe = imagineImageObserver(info)
	}
	s.forwardAgent(w, r, sess, accountID, http.MethodPost, s.grokImagineEndpoint()+path, body, rw)
}

// handleGrokVideoPoll 是 GET /agents/grok/v1/videos/{request_id}：辅助调用，
// 不入账、不准入，只鉴权、过闸、换令牌转发。
func (s *Server) handleGrokVideoPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("request_id")
	if id == "" || len(id) > grokImagineRequestIDMax {
		handleNotFound(w, r)
		return
	}
	if _, ok := s.subscriptionPermitted(w, r, store.AgentProviderGrok, false); !ok {
		return
	}
	sess, accountID, ok := s.agentCredential(w, r, store.AgentProviderGrok)
	if !ok {
		return
	}
	s.forwardAgent(w, r, sess, accountID, http.MethodGet,
		s.grokImagineEndpoint()+grokVideosPath+url.PathEscape(id), nil, respRewrite{rewrite: keepModelField})
}

// keepModelField 是「不回写」的 respRewrite.rewrite：订阅面请求侧本就不改
// model，响应里的 model（若有）原样过。
func keepModelField(map[string]any, string) bool { return false }

// imagineImageObserver 只数张数：响应顶层 data[] 的长度进 reqInfo.imageCount，
// 供记账（usage.TaskUsage.GeneratedImages）。取到就用、取不到当没有，绝不落日志。
func imagineImageObserver(info *reqInfo) func(map[string]any) {
	return func(m map[string]any) {
		if data, ok := m["data"].([]any); ok {
			info.imageCount = len(data)
		}
	}
}
