package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// reqInfo 由最外层中间件放入 context，内层回填（认证中间件填 key，选路转发
// 填 attempts/upstream），访问日志统一读取。同一请求单 goroutine 读写，
// 无并发问题。
type reqInfo struct {
	id string // request-id，已回写响应头 X-Request-Id
	// keyID 是认证通过后的数值身份（任务面按它判归属、账本按它记密钥维度）；
	// 未认证为 0——0 匹配不到任何行，缺省即收窄。
	keyID int64
	// attempts 是本请求尝试过的上游来源数（含最终成功的那次），0 表示未转发；
	// upstream 是最后尝试的上游账户名——诊断故障切换用，凭证绝不在内（§15.1）。
	attempts int
	upstream string
	// imageUsage/imageCount/imageReqSize 是图片同步入口的账单事实内存记录点位：
	// 厂商 usage 原文 JSON、出图张数（data[] 长度）、请求 size 参数，由
	// images.go 的观察器在 2xx 出图时回填。消费方是 iteration-9 的计量账本
	// （同步调用无任务行，不建新表）；刻意**不进访问日志**——usage 与 size 都
	// 取自请求/响应 body 内容，访问日志对 body 的纪律是只记长度/哈希/类型
	// （§15.1）。
	imageUsage   string
	imageCount   int
	imageReqSize string
	// limits 是鉴权那一次点查随行带回的限额与按量额度快照（store.KeyAuth
	// 整行，iteration-9）：预算准入就地判、不回库。未认证时是零值，而未认证
	// 的请求根本走不到准入点。
	limits store.KeyAuth
	// agentSurface 由订阅/开发工具面的分流点立起（/codex、/claude 的混合面），
	// 声明本请求的可用模型由开发工具策略裁决，中转到共享目录 handler 后不再
	// 套用 API模型策略（apimodels.go）。
	agentSurface bool
	// devTools 是本请求已解析过的开发工具策略快照（devtools.go）：接入面的
	// 子树闸先算一次，下游处理器复用，同一请求不重复点查。
	devTools *devtoolpolicy.Snapshot
	// bill 是本请求的计量累计（iteration-9）：入口/模型维度、usage 四分量、
	// 估算兜底与长流中间结算的状态。请求收尾时由 withAccessLog 交给
	// [Server.recordUsage] 折成一条 usage.Sample。详见 metering.go。
	bill billState
}

type reqInfoKey struct{}

// infoFrom 取出当前请求的 reqInfo；链路外（如测试直接调 handler）返回零值兜底。
func infoFrom(ctx context.Context) *reqInfo {
	if info, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return info
	}
	return &reqInfo{}
}

// withRequestID 生成 request-id、回写响应头 X-Request-Id，并把 reqInfo
// 放入 context。位于链路最外层，保证 401/404/500 等短路响应也带 id。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &reqInfo{id: newRequestID()}
		w.Header().Set("X-Request-Id", info.id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, info)))
	})
}

// newRequestID 返回 8 字节 crypto/rand 的十六进制串（16 字符）。
func newRequestID() string {
	var b [8]byte
	rand.Read(b[:]) // 自 Go 1.24 起 crypto/rand.Read 保证不失败
	return hex.EncodeToString(b[:])
}

// withAccessLog 记录访问日志：方法/路径/状态/时长/两侧 body 元数据。
// §15.1 硬规则：body 只经 BodyMeter 记长度/哈希前缀/content-type，绝不记内容；
// 只记 URL path 不记 query（query 可能携带凭证或业务数据）。
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqMeter := logging.NewBodyMeter()
		// req 侧计的是处理程序实际读取的字节数（未读完的 body 不计入）。
		r.Body = &meteredBody{rc: r.Body, m: reqMeter}
		rec := &responseRecorder{ResponseWriter: w, meter: logging.NewBodyMeter()}

		next.ServeHTTP(rec, r)

		info := infoFrom(r.Context())
		status := rec.status()
		if rec.statusCode == 0 && r.Context().Err() != nil {
			// No response was committed before the client disconnected. The
			// implicit 200 would invent successful usage for pre-upstream exits.
			// Preserve committed stream status and its observed/partial usage.
			status = 499 // Client Closed Request; accounting only, no response write.
		}
		elapsed := time.Since(start)
		durationMS := float64(elapsed.Microseconds()) / 1e3
		attrs := []any{
			"request_id", info.id,
			// 调用方标识用密钥展示串（"前缀…末4位"）：明文从不存在于库中，
			// 更不进日志（§15.1）。未认证的请求这一项为空串。
			"key", info.bill.keyDisplay,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", durationMS,
			slog.Group("req", reqMeter.Attr(r.Header.Get("Content-Type"))),
			slog.Group("resp", rec.meter.Attr(rec.Header().Get("Content-Type"))),
		}
		// 只有真正转发过的请求才带选路字段：attempts > 1 即发生过故障切换，
		// upstream 是最终落到的上游账户名（不含 Key、不含来源侧模型 ID）。
		if info.attempts > 0 {
			attrs = append(attrs, "attempts", info.attempts, "upstream", info.upstream)
		}
		// 经可信 Cloudflare Tunnel 入口到达的请求带上边缘给出的客户端 IP（只作
		// 审计线索，不是身份）；LAN 请求没有这一项。
		if tinfo, ok := tunnelctx.From(r.Context()); ok {
			attrs = append(attrs, "via", "tunnel", "client_ip", tinfo.ClientIP)
		}
		s.log.Info("access", attrs...)

		// 用量记账搭同一班车（iteration-9）：同样要"对客户端生效的最终状态码"，
		// 而这里是每个数据面请求的唯一必经收尾点。
		//
		// 放在访问日志**之后**：本中间件在 withRecovery 之外，这里 panic 就没有
		// 兜底了，记账排在后面能保证访问日志那一行永远先落盘。
		s.recordUsage(info, status, elapsed)
	})
}

// meteredBody 包装请求 body：读取经过时喂给 BodyMeter（只计长度与哈希）。
type meteredBody struct {
	rc io.ReadCloser
	m  *logging.BodyMeter
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.m.Write(p[:n])
	}
	return n, err
}

func (b *meteredBody) Close() error { return b.rc.Close() }

// responseRecorder 包装 ResponseWriter：记录状态码并把写出的字节喂给 BodyMeter。
type responseRecorder struct {
	http.ResponseWriter
	meter      *logging.BodyMeter
	statusCode int // 0 表示尚未写出头
}

func (w *responseRecorder) WriteHeader(code int) {
	if w.statusCode == 0 {
		w.statusCode = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK // net/http 首次 Write 隐式发 200
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.meter.Write(p[:n])
	}
	return n, err
}

// Flush 透传给底层 writer；SSE 逐事件刷出（Phase 4）依赖它。
func (w *responseRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.statusCode == 0 {
			w.statusCode = http.StatusOK // Flush also commits implicit headers.
		}
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能穿透本包装找到底层 writer。
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// status 返回对客户端生效的状态码：handler 未显式写头即返回时 net/http 发 200。
func (w *responseRecorder) status() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

// withRecovery 捕获 handler panic：记脱敏日志（panic 值与栈来自网关自身
// 代码缺陷，不含请求内容——代码中禁止携带 body/凭证 panic），响应按入口风格
// 返回 500。
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && err == http.ErrAbortHandler {
				panic(rec) // net/http 约定的响应中止信号，继续上抛由 server 处理
			}
			info := infoFrom(r.Context())
			s.log.Error("panic recovered",
				"request_id", info.id,
				"method", r.Method,
				"path", r.URL.Path,
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()),
			)
			entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error",
				"The gateway encountered an internal error.")
		}()
		next.ServeHTTP(w, r)
	})
}

// withAuth 校验客户端 API密钥：接受 Authorization: Bearer 与 x-api-key
// 两种头；失败返回 401，错误体风格按路径选择（messages 系入口 Anthropic
// 风格，其余 OpenAI 风格）。校验经 KeyAuthorizer 查 SQLite（未知 Key 与
// 禁用 Key 同一形态拒绝，不区分原因）。通过后把密钥身份填入 reqInfo。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		errStyle := entryErrorStyle(r)
		key := clientKey(r)
		if key == "" {
			errStyle(w, http.StatusUnauthorized, "missing_api_key",
				"Missing API key. Pass it via the Authorization: Bearer header or the x-api-key header.")
			return
		}
		ident, ok := s.keys.Authorize(r.Context(), key)
		if !ok {
			// 失败的 Key 可能是抄错一位的真实 Key，指认一律走 RedactKey。
			s.log.Debug("认证失败",
				"request_id", infoFrom(r.Context()).id,
				"api_key", logging.RedactKey(key),
			)
			errStyle(w, http.StatusUnauthorized, "invalid_api_key",
				"Invalid API key provided.")
			return
		}
		info := infoFrom(r.Context())
		info.keyID = ident.KeyID
		// keyDisplay 是账本里密钥维度的快照（"前缀…末4位"，明文从不存在于
		// 库中）：密钥被删之后历史账仍读得出这笔是谁花的。
		info.bill.keyDisplay = ident.KeyDisplay
		// 限额整行随行留下：预算准入不必再查一次库，改限额也随下一个请求的
		// 这次点查即时生效（数据面无缓存层的既有决策自然覆盖到限额）。
		info.limits = ident.KeyAuth
		next.ServeHTTP(w, r)
	})
}

// clientKey 提取客户端凭证：优先 Authorization 的 Bearer token，
// 其次 x-api-key 头；都没有返回空串。
func clientKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "bearer "
		if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// openAIError 是 OpenAI 风格错误响应体：{"error":{"message","type","code"}}。
type openAIError struct {
	Error openAIErrorBody `json:"error"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeOpenAIError 写出网关自身的错误响应。响应已开始写出时（如流式中途
// panic）不再追加错误体——此时只能靠日志与连接中断向客户端表达失败。
func writeOpenAIError(w http.ResponseWriter, status int, typ, code, message string) {
	if rec, ok := w.(*responseRecorder); ok && rec.statusCode != 0 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(openAIError{Error: openAIErrorBody{
		Message: message,
		Type:    typ,
		Code:    code,
	}})
}

// anthropicAPIError 是 Anthropic 风格错误响应体：
// {"type":"error","error":{"type":"...","message":"..."}}。
type anthropicAPIError struct {
	Type  string                `json:"type"` // 恒为 "error"
	Error anthropicAPIErrorBody `json:"error"`
}

type anthropicAPIErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// writeAnthropicError 写出网关自身的 Anthropic 风格错误响应（messages 系
// 入口用）。与 writeOpenAIError 同规则：响应已开始写出时不再追加错误体。
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	if rec, ok := w.(*responseRecorder); ok && rec.statusCode != 0 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(anthropicAPIError{Type: "error", Error: anthropicAPIErrorBody{
		Type:    errType,
		Message: message,
	}})
}

// errorStyle 按入口协议写网关自身错误，供共享透传核心（proxy.go）与认证
// 中间件对不同入口输出各自形态：OpenAI 风格 code 进 error.code；Anthropic
// 风格错误体无 code 字段，error.type 由 status 映射。
type errorStyle func(w http.ResponseWriter, status int, code, message string)

// openAIErrorStyle 是 chat 侧入口的 errorStyle：error.type 由 status 归类
// （5xx → api_error，429 → rate_limit_error，其余 → invalid_request_error），
// 与既有调用点取值一致。
//
// 429 单列与 anthropicErrorType 同一理由（iteration-9 预算准入）：归进
// invalid_request_error 会让按 type 分支的客户端把「你超额了」读成一个永久性的
// 请求错误，从而**丢掉** Retry-After 指示的重试——而这是唯一一档「等一会儿就
// 能过」的失败。网关自身发出的 429 只有预算准入这一处（上游的 429 走原样透传，
// 不经 errorStyle）。
func openAIErrorStyle(w http.ResponseWriter, status int, code, message string) {
	typ := "invalid_request_error"
	switch {
	case status >= http.StatusInternalServerError:
		typ = "api_error"
	case status == http.StatusTooManyRequests:
		typ = "rate_limit_error"
	}
	writeOpenAIError(w, status, typ, code, message)
}

// anthropicErrorStyle 是 messages 侧入口的 errorStyle：code 不外显，
// error.type 由 status 映射。
func anthropicErrorStyle(w http.ResponseWriter, status int, code, message string) {
	writeAnthropicError(w, status, anthropicErrorType(status), message)
}

// anthropicErrorType 把网关自身错误的 HTTP 状态码映射为 Anthropic 错误体
// 的 error.type（只列网关会发出的状态；其余归 api_error）。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		// 开发工具策略的闸门（devtool_not_allowed / subscription_not_allowed）：
		// Anthropic 契约里「这把 Key 无权」就叫 permission_error。
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		// 预算准入的 429（iteration-9）：Anthropic 契约里这一档叫
		// rate_limit_error，归进 api_error 会让客户端把「你超额了」当成
		// 「服务端坏了」而去重试。
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

// minimaxErrorStyle 是 MiniMax 协议面（/minimax 段）的 errorStyle：网关
// 自产错误按 MiniMax v2 的错误形 {"type":"error","error":{type,message,
// http_code}} 输出（docs-upstream/minimax-h3-video.md「错误处理」），保持「客户端只
// 需要认一种错误形态」的纯转发口径。code 进 error.type——上游自己的错误
// 恒为原样透传，不经这里。
func minimaxErrorStyle(w http.ResponseWriter, status int, code, message string) {
	if rec, ok := w.(*responseRecorder); ok && rec.statusCode != 0 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":      code,
			"message":   message,
			"http_code": status,
		},
	})
}

// connectErrorStyle 是 /agents/cursor/ 子树的 errorStyle：网关自产错误按
// ConnectRPC 的统一错误形 {"code":"...","message":"..."} 输出（cursor-agent 把
// [code] message 原样展示给成员）。code 字段必须取 Connect 的封闭词汇表，所以
// 由 status 映射；调用方传入的机读 code 不外显——同 anthropicErrorStyle 丢弃
// code 的处理。上游自己的错误恒为原样透传，不经这里。
func connectErrorStyle(w http.ResponseWriter, status int, code, message string) {
	if rec, ok := w.(*responseRecorder); ok && rec.statusCode != 0 {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{connectErrorCode(status), message})
}

// connectErrorCode 把网关自身错误的 HTTP 状态码映射为 Connect 错误码（只列
// 网关会发出的状态；其余归 internal）。409 归 failed_precondition：「没连订阅 /
// 需重新连接」是设备状态不满足前置条件，Connect 客户端以响应体里的 code 为准，
// 状态码只是兜底，所以沿用与其余订阅入口一致的 409 不影响 CLI 读出该码。
func connectErrorCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_argument"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "failed_precondition"
	case http.StatusTooManyRequests:
		return "resource_exhausted"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "unavailable"
	default:
		return "internal"
	}
}

// entryErrorStyle 按请求路径选入口错误风格：/v1/messages 及其子路径、
// /agents/claude/ 子树是
// Anthropic 协议入口，/agents/cursor/ 子树是 ConnectRPC 入口（Connect 统一
// 错误形），/minimax/ 系是 MiniMax 协议面，其余（chat、
// models 等）是 OpenAI 风格入口。
func entryErrorStyle(r *http.Request) errorStyle {
	if r.URL.Path == "/v1/messages" || strings.HasPrefix(r.URL.Path, "/v1/messages/") ||
		strings.HasPrefix(r.URL.Path, "/agents/claude/") || strings.HasPrefix(r.URL.Path, "/claude/") {
		return anthropicErrorStyle
	}
	if strings.HasPrefix(r.URL.Path, "/agents/cursor/") {
		return connectErrorStyle
	}
	if strings.HasPrefix(r.URL.Path, "/minimax/") {
		return minimaxErrorStyle
	}
	return openAIErrorStyle
}
