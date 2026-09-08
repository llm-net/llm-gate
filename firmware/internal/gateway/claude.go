// claude.go 实现 Claude Code 集中订阅代理——/agents/claude/ 接入面的订阅那一半
// （claude_mixed.go 先按 Key 策略把勾选的目录模型分流走，其余名字进这里）：
// 调用者只把自己的客户端 API 密钥作为 LLM Gateway Bearer 交给设备；设备每请求
// 点查并解封 agent_accounts 里的 Claude setup-token，替换认证头后发往 Anthropic。
//
// 它是第三种 agent_accounts provider，但 setup-token 不可刷新，因此不复用
// Codex/Grok 的 agentSession/Provider；也不按自然人绑定、不走模型目录选路或
// 多来源故障切换。出站目的地编译为 api.anthropic.com；唯一覆盖口是测试钩子
// [Server.SetClaudeEndpoint]，生产装配不调用。完整契约见对应单项文档。
//
// §15.1：Authorization / x-api-key、X-LlmGate-API-Key、请求/响应体与查询串均不得
// 进入日志。本文件的错误日志因此只写 request_id、模型名与有限错误分类。
package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const claudeBackendURL = "https://api.anthropic.com"
const claudeOAuthBeta = "oauth-2025-04-20"

// SetClaudeEndpoint 把 Claude 固定上游根指向开发期/测试假端点。生产装配不调用
// 本方法，空值恢复编译常量 https://api.anthropic.com；它不是配置项或管理 API。
func (s *Server) SetClaudeEndpoint(endpoint string) {
	s.claudeMu.Lock()
	s.claudeBackend = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	s.claudeMu.Unlock()
}

func (s *Server) claudeEndpoint() string {
	s.claudeMu.RLock()
	endpoint := s.claudeBackend
	s.claudeMu.RUnlock()
	if endpoint != "" {
		return endpoint
	}
	return claudeBackendURL
}

// withClaudeAuth 复用共享设备鉴权。Claude Code 按官方 LLM Gateway 形态把
// 客户端 API 密钥放进 Authorization Bearer；客户端认证头在 buildClaudeRequest 中
// 删除，绝不发往 Anthropic。传输 TLS 可以终止在客户自己的反向代理上，所以
// 此处不依据 r.TLS 判断客户端链路。
func (s *Server) withClaudeAuth(next http.Handler) http.Handler {
	return s.withAuth(next)
}

func (s *Server) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	body, payload, model, ok := readClaudePayload(w, r)
	if !ok {
		return
	}
	info := beginEntry(r, usage.EntryClaudeCode, model)
	if !s.admit(w, r, anthropicErrorStyle) {
		return
	}
	// 这条窄读只借同名 text 目录行的价签，不借来源、不选路；被准入拒绝的
	// 请求不会为价签多做一次 SQLite 点查。
	s.applyAgentModelPricing(r, info, model)
	info.bill.estimateInput = func() int64 { return usage.EstimateInputTokens(payload) }
	s.forwardClaude(w, r, "/v1/messages", body, model, s.messagesObserver(info), nil)
}

func (s *Server) handleClaudeCountTokens(w http.ResponseWriter, r *http.Request) {
	body, _, model, ok := readClaudePayload(w, r)
	if !ok {
		return
	}
	info := beginEntry(r, usage.EntryClaudeCode, model)
	// count_tokens 记请求数但不做准入、不估 token；同名目录读数只给报表保留
	// modelKnown/priced 事实，不影响这笔 0 元调用。
	s.applyAgentModelPricing(r, info, model)
	s.forwardClaude(w, r, "/v1/messages/count_tokens", body, model, nil, nil)
}

// readClaudePayload 同时保留原始请求字节与解析视图：前者发往 Anthropic，后者
// 只用来取 model 和做 token 估算。这样未知字段、字段顺序、数字字面量和空白都
// 不因设备中转而改变。
func readClaudePayload(w http.ResponseWriter, r *http.Request) ([]byte, map[string]any, string, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body",
			"Failed to read the request body.")
		return nil, nil, "", false
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body",
			"The request body is not valid JSON.")
		return nil, nil, "", false
	}
	model, _ := payload["model"].(string)
	if model == "" {
		anthropicErrorStyle(w, http.StatusBadRequest, "missing_model",
			"The model field is required and must be a string.")
		return nil, nil, "", false
	}
	return body, payload, model, true
}

// claudeRewriteFor 在凭据点查之后按订阅接入构造响应载荷改写器（返回 nil = 不
// 改写）。它必须晚于点查：唯一的改写理由——可见模型——就存在那一行上。
type claudeRewriteFor func(*http.Request, *store.AgentAccount) func(map[string]any, string) bool

// forwardClaude 只用 setup-token 转发一次；OAuth 续期完全独立于调用。
// observe 非 nil 仅用于 messages 成功响应
// 的 usage 旁路观测；响应 model 绝不改写，其余字节只有模型发现收窄这一条例外
// （rewriteFor，见 [Server.claudeVisibleModels]）。
func (s *Server) forwardClaude(w http.ResponseWriter, r *http.Request, upstreamPath string,
	body []byte, model string, observe func(map[string]any), rewriteFor claudeRewriteFor) {

	info := infoFrom(r.Context())
	acct, cred, ok := s.claudeCredential(w, r)
	if !ok {
		return
	}
	token := cred.Token()
	info.attempts = 1
	req, err := s.buildClaudeRequest(r, upstreamPath, body, token)
	if err != nil {
		s.log.Error("构造 Claude 上游请求失败", "request_id", info.id, "model", model)
		anthropicErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，Claude 上游请求已取消", "request_id", info.id, "model", model)
			return
		}
		s.log.Warn("Claude 上游请求失败", "request_id", info.id, "model", model, "error_kind", claudeTransportErrorKind(err))
		anthropicErrorStyle(w, http.StatusBadGateway, "upstream_unreachable", "The gateway failed to reach Anthropic.")
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.markClaudeAuthExpired(r, acct.ID, token)
	}
	if s.agentQuota != nil && !cred.IsOAuth() && (resp.StatusCode < 300 || resp.StatusCode == http.StatusTooManyRequests) {
		s.agentQuota.Observe(acct, agentquota.ClaudeHeaders(resp.Header))
	}

	// 共享记账收尾把 3xx 视为可读响应；但重定向没有模型产出，不能拿输入估算
	// 冒充 token。http.Client 已配置为不跟随重定向，状态与 Location 原样回客户端。
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		info.bill.estimateInput = nil
	}
	var rewrite func(map[string]any, string) bool
	if rewriteFor != nil {
		rewrite = rewriteFor(r, acct)
	}
	// 既不观测也不改写时走纯字节转发：连解析都不做，保真度最高。
	if observe == nil && rewrite == nil {
		s.passthroughResponse(w, r, resp, model)
		return
	}
	if rewrite == nil {
		rewrite = preserveClaudePayload
	}
	s.commitResponse(w, r, resp, respRewrite{
		model:   model,
		rewrite: rewrite,
		observe: observe,
	}, anthropicErrorStyle)
}

func (s *Server) buildClaudeRequest(r *http.Request, upstreamPath string, body []byte, token string) (*http.Request, error) {
	target, err := s.claudeTarget(upstreamPath, r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, reader)
	if err != nil {
		return nil, err
	}
	copyClaudeForwardHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+token)
	ensureAnthropicBeta(req.Header, claudeOAuthBeta)
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (s *Server) claudeTarget(upstreamPath, rawQuery string) (string, error) {
	base, err := url.Parse(s.claudeEndpoint())
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New("invalid Claude endpoint")
	}
	base.Path = strings.TrimRight(base.Path, "/") + upstreamPath
	base.RawPath = ""
	base.RawQuery = rawQuery
	base.Fragment = ""
	return base.String(), nil
}

// copyClaudeForwardHeaders 先复制业务头，再剥掉客户端与设备控制面的全部认证
// 物料。buildClaudeRequest 随后只注入设备解封的 setup-token。
func copyClaudeForwardHeaders(dst, src http.Header) {
	for k, values := range src {
		for _, value := range values {
			dst.Add(k, value)
		}
	}
	stripHopByHop(dst, src)
	for name := range dst {
		if strings.HasPrefix(strings.ToLower(name), "x-llmgate-") {
			dst.Del(name)
		}
	}
	dst.Del("Cookie")
	dst.Del("Authorization")
	dst.Del("Proxy-Authorization")
	dst.Del("x-api-key")
	dst.Del("Content-Length")
	dst.Del("Expect")
	// 让共享 Transport 自己协商并透明解压，commitResponse 才能旁路观察 usage；
	// Claude Code 没有要求某种压缩编码的业务语义。
	dst.Del("Accept-Encoding")
}

// ensureAnthropicBeta keeps every beta emitted by the installed Claude Code
// version and adds the OAuth beta required when the upstream credential is a
// setup-token. Header values are comma-delimited by Anthropic's contract.
func ensureAnthropicBeta(h http.Header, required string) {
	values := h.Values("Anthropic-Beta")
	for _, value := range values {
		for _, beta := range strings.Split(value, ",") {
			if strings.TrimSpace(beta) == required {
				return
			}
		}
	}
	values = append(values, required)
	h.Set("Anthropic-Beta", strings.Join(values, ","))
}

// claudeCredential reads only the inference credential, without consulting OAuth.
func (s *Server) claudeCredential(w http.ResponseWriter, r *http.Request) (*store.AgentAccount, *claudeauth.Credential, bool) {
	info := infoFrom(r.Context())
	if s.store == nil {
		anthropicErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, nil, false
	}
	acct, blob, err := s.store.GetAgentCredential(r.Context(), store.AgentProviderClaude)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeClaudeNotConfigured(w)
		return nil, nil, false
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		s.log.Warn("Claude Code 订阅凭据解不开，请管理员重新连接",
			"request_id", info.id, "provider", store.AgentProviderClaude, "err", err.Error())
		writeClaudeAuthExpired(w)
		return nil, nil, false
	case r.Context().Err() != nil:
		return nil, nil, false
	default:
		s.log.Error("读取 Claude Code 订阅失败", "request_id", info.id, "err", err.Error())
		anthropicErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, nil, false
	}

	// 上游维度在**账号点查成功的这一刻**就钉住，不等凭据解析完：从这里往下的每
	// 一种拒绝（管理员停用、登录失效、凭据形态不合）说的都是这一个订阅账号自己的
	// 事，账要记在它头上，别掉进用量页「按上游账号」那一格没有名字的行里（同
	// responses.go 的 agentCredential）。
	info.upstream = agentUsageUpstreamName(acct)
	if acct.Status == store.AgentStatusAuthExpired {
		writeClaudeAuthExpired(w)
		return nil, nil, false
	}
	if acct.Status != store.AgentStatusActive {
		writeClaudeNotConfigured(w)
		return nil, nil, false
	}
	cred, err := claudeauth.Parse(blob)
	if err != nil {
		s.log.Warn("Claude Code 订阅凭据形态不合，请管理员重新连接",
			"request_id", info.id, "account", acct, "err", err.Error())
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentauth.CallbackTimeout)
		defer cancel()
		_, markErr := s.store.MutateAgentAuth(ctx, acct.ID, store.AgentProviderClaude, false, func(a *store.AgentAccount, current string) (string, error) {
			if current != blob || a.Status != store.AgentStatusActive {
				return "", nil
			}
			a.Status = store.AgentStatusAuthExpired
			return current, nil
		})
		if markErr != nil {
			s.log.Warn("标记损坏的 Claude 凭据失败", "account_row", acct.ID)
		}
		writeClaudeAuthExpired(w)
		return nil, nil, false
	}
	if !cred.HasSetupToken() {
		anthropicErrorStyle(w, http.StatusConflict, "claude_setup_token_required", "Claude Code inference requires setup-token. Ask the administrator to configure it; browser OAuth is only used for quota queries.")
		return nil, nil, false
	}
	return acct, cred, true
}

func (s *Server) markClaudeAuthExpired(r *http.Request, accountID int64, rejectedToken string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentauth.CallbackTimeout)
	defer cancel()
	_, err := s.store.MutateAgentAuth(ctx, accountID, store.AgentProviderClaude, false, func(a *store.AgentAccount, blob string) (string, error) {
		cred, err := claudeauth.Parse(blob)
		if err != nil {
			return "", err
		}
		if cred.Token() != rejectedToken || a.Status != store.AgentStatusActive {
			return "", nil
		}
		a.Status = store.AgentStatusAuthExpired
		return blob, nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("标记 Claude setup-token 失效失败", "request_id", infoFrom(r.Context()).id, "account_row", accountID)
	}
}

func writeClaudeNotConfigured(w http.ResponseWriter) {
	anthropicErrorStyle(w, http.StatusConflict, "agent_not_configured",
		"No active Claude Code subscription is connected on this device. Ask the administrator to connect one.")
}

func writeClaudeAuthExpired(w http.ResponseWriter) {
	anthropicErrorStyle(w, http.StatusConflict, "agent_auth_expired",
		"The Claude Code subscription connected on this device needs a new setup-token. Ask the administrator to reconnect it.")
}

func preserveClaudePayload(map[string]any, string) bool { return false }

// transport error 的 Error() 可能包含完整 URL（包括禁止落日志的查询串），所以
// 只归有限类别，不把 err 文本交给 logger。
func claudeTransportErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "network_error"
	}
}
