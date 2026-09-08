// cursor.go 实现 Cursor 集中订阅代理（/agents/cursor/ 子树）：成员的
// cursor-agent 把 CURSOR_API_ENDPOINT 指到设备、CURSOR_API_KEY 填自己的客户端
// API 密钥；设备逐请求鉴权后把 aiserver.v1 与 agent.v1 整面按字节转发到
// api2.cursor.sh，并把 authorization 换成设备侧换发的上游 accessToken。
//
// 两段面，判然有别：
//
//	exchange  POST /agents/cursor/auth/exchange_user_api_key 先用设备封存的
//	          Dashboard API Key 验证上游订阅，再签发只在本设备有效的 JWT。
//	          JWT 的 sub 沿用上游身份主体、另带客户端 Key 摘要；客户端 Key 与
//	          上游 accessToken 都不出设备。exchange 不计量（接入探针语义）。
//	RPC       POST /agents/cursor/{aiserver.v1,agent.v1}.<Service>/<Method>
//	          是透明转发：服务/方法名不是公开契约，按命名空间正则整面放行
//	          （挑方法白名单会在 CLI 加一个方法的那天悄悄坏掉）；不匹配的
//	          路径本地 404、不出网——/auth/poll 浏览器登录、repo42 代码索引
//	          与 api3 遥测都不在代理面内。字节原样转发，旁路只读
//	          请求模型与回合最终用量。
//
// 凭据形态与 Claude 同族（静态凭据、每请求点查、不进 agentSessions）；不同的
// 是出站换发：库里那把 Dashboard API Key 经上游 /auth/exchange_user_api_key
// 换出 accessToken 才能打 RPC。换发结果只活在内存（cursorSession，恒不落盘），
// 单飞（换发在持锁期间完成，并发请求复用刚换好的那一个），上游 401 作废后
// 单次重试（只看状态码，responses.go 的纪律）；exchange 被上游 401/403 确定性
// 拒绝时按 claude.go 的闩模式落 auth_expired。出站目的地编译为
// api2.cursor.sh，唯一覆盖口是测试钩子 [Server.SetCursorEndpoints]，生产
// 装配不调用。
//
// §15.1：客户端 Key、板上 API Key、换发的上游 token、请求/响应体（含 proto
// 帧）都不得进入日志；本文件的日志只写 request_id、RPC 名（访问日志本就记
// 路径）与有限错误分类。
package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorwire"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// cursorBackendURL 是 Cursor 上游根（cursor-agent 的 HTTP/1.1 endpoint）：
// exchange、aiserver.v1 与 agent.v1/RunSSE 同根。编译常量、不是配置项。
const cursorBackendURL = "https://api2.cursor.sh"

// cursorExchangePath 是上游换发端点（真机抓包钉死：头 Authorization: Bearer
// <API Key>、体 {}，成功响应是同时含 accessToken 与 refreshToken 的 JSON）。
const cursorExchangePath = "/auth/exchange_user_api_key"

// cursorExchangeTimeout 是一次换发的整体超时：共享 client 的响应头超时是给
// 长流转发用的（分钟级），一次短 POST 挂住不该把排队的 RPC 一起吊死。
const cursorExchangeTimeout = 15 * time.Second

// cursorExchangeMaxBytes 是换发响应体的读取上限（防御性：正常应答不足 1 KiB）。
const cursorExchangeMaxBytes = 1 << 20

// cursorClientTokenIssuer 区分设备签发的 Cursor 客户端 JWT 与任何上游 JWT。
const cursorClientTokenIssuer = "llmgate-cursor"

const cursorClientTokenMaxBytes = 16 << 10

// cursorClientTokenKeySetting 存设备专属的本地 JWT 签名根。值以 device-key
// 密封；实际 HMAC key 再混入当前 Cursor Dashboard API Key，所以设备或订阅
// 任一侧变化都会作废旧 JWT。
const cursorClientTokenKeySetting = "cursor_client_token_signing_key_v1"

// cursorRPCRe 是 RPC 面唯一放行的路径形。服务与方法名按 proto 标识符形收
// （首字符字母，后续字母/数字/下划线），命名空间只放 aiserver.v1 与
// agent.v1；后者承载对话流（受管 HTTP/1.1 当前使用 RunSSE）。
var cursorRPCRe = regexp.MustCompile(`^/agents/cursor/((?:aiserver|agent)\.v1\.[A-Za-z][A-Za-z0-9_]*/[A-Za-z][A-Za-z0-9_]*)$`)

// cursorSession 是当前 Cursor 凭据世代在**本进程内**的换发状态。库是凭据的
// 权威（每请求点查），这里只缓存换发结果；世代身份由 rowID/updatedAt/apiKey
// 三元共同界定，管理与 agentSession 同一套「只进不退」（见 cursorSessionFor）。
type cursorSession struct {
	rowID     int64
	updatedAt time.Time
	// apiKey 是该世代解封的 Dashboard API Key，构建后不变（换 Key 即换会话）。
	apiKey string

	// mu 既护 token 也做单飞：换发在持锁期间完成，等锁的并发请求醒来复用
	// 刚换好的那一个（同 agentauth.Provider 的手法，不为此引 x/sync）。
	mu sync.Mutex
	// token 是最近一次换发得到的上游 accessToken；空 = 尚未换发或已作废。
	// **恒不落盘**：库里只有 API Key，token 是每次进程内换发的产物。
	token string
}

// invalidate 报告「我拿着的这个 token 被上游拒了」。参数是去重键（同
// [agentauth.Provider.Invalidate] 的理由）：N 个在飞请求同时 401 时，只有第一个
// 真正清掉缓存，其余一看「你说的那个早已换掉」就不再触发重复换发。
func (sess *cursorSession) invalidate(rejected string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if rejected != "" && sess.token == rejected {
		sess.token = ""
	}
}

// SetCursorEndpoints 把 Cursor 固定上游根指向开发期/测试假端点。生产装配不
// 调用本方法，空值恢复编译常量；它不是配置项或管理 API。调用会丢掉已换发
// 的 token，下一个请求按新根重换。
func (s *Server) SetCursorEndpoints(endpoint string) {
	s.cursorMu.Lock()
	s.cursorBackend = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	s.cursorSess = nil
	s.cursorMu.Unlock()
}

func (s *Server) cursorEndpoint() string {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	if s.cursorBackend != "" {
		return s.cursorBackend
	}
	return cursorBackendURL
}

// handleCursorExchange 处理 CLI 的换发请求（已过客户端 Key 认证与
// withSubscription("cursor", requireAvailable=true)）。当前 cursor-agent 会把
// accessToken 当 JWT 解析 sub，因此这里先真换发并读取上游身份主体，再签发
// 仅本设备接受的 JWT。JWT 只含主体与客户端 Key 摘要，不含两侧凭证明文；
// refreshToken 是占位串。exchange 不计量。
func (s *Server) handleCursorExchange(w http.ResponseWriter, r *http.Request) {
	acct, apiKey, ok := s.cursorCredential(w, r)
	if !ok {
		return
	}
	upstreamToken, err := s.cursorToken(r.Context(), s.cursorSessionFor(acct, apiKey))
	if err != nil {
		s.writeCursorTokenError(w, r, acct.ID, err)
		return
	}
	subject, ok := cursorJWTSubject(upstreamToken)
	if !ok {
		s.log.Warn("Cursor 上游换发响应缺少可用的 JWT 身份主体",
			"request_id", infoFrom(r.Context()).id)
		connectErrorStyle(w, http.StatusBadGateway, "upstream_invalid_response",
			"Cursor returned an invalid subscription credential. Ask the administrator to check the connected account.")
		return
	}
	deviceKey, err := s.cursorClientSigningKey()
	if err != nil {
		s.log.Error("装载 Cursor 本地客户端令牌签名密钥失败",
			"request_id", infoFrom(r.Context()).id, "err", err.Error())
		connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	accessToken, err := cursorClientToken(subject, keyDigest(clientKey(r)),
		cursorClientMACKey(deviceKey, apiKey))
	if err != nil {
		s.log.Error("签发 Cursor 本地客户端令牌失败", "request_id", infoFrom(r.Context()).id)
		connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}{accessToken, "llmgate"})
}

type cursorClientClaims struct {
	Issuer    string `json:"llmgate_issuer"`
	Subject   string `json:"sub"`
	KeyDigest string `json:"llmgate_key_digest"`
	Version   int    `json:"llmgate_version"`
}

// cursorJWTSubject 只从上游 JWT 的公开 payload 取 CLI 必需的 sub；签名由
// Cursor 上游自己负责，设备已通过 HTTPS 从固定上游取得它。其余公开声明与
// 上游签名都不复制给客户端，保持最小披露。
func cursorJWTSubject(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > cursorClientTokenMaxBytes {
		return "", false
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", false
	}
	subject := strings.TrimSpace(claims.Subject)
	return subject, subject != "" && len(subject) <= 1024
}

func (s *Server) cursorClientSigningKey() ([]byte, error) {
	s.cursorClientKeyOnce.Do(func() {
		if s.store == nil {
			s.cursorClientKeyErr = errors.New("Cursor 本地令牌签名存储未装配")
			return
		}
		encoded, err := s.store.GetSealedSetting(context.Background(), cursorClientTokenKeySetting)
		if err != nil {
			s.cursorClientKeyErr = err
			return
		}
		if encoded != "" {
			key, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil || len(key) != sha256.Size {
				s.cursorClientKeyErr = errors.New("Cursor 本地令牌签名密钥形态无效")
				return
			}
			s.cursorClientKey = key
			return
		}
		key := make([]byte, sha256.Size)
		if _, err := rand.Read(key); err != nil {
			s.cursorClientKeyErr = fmt.Errorf("生成 Cursor 本地令牌签名密钥: %w", err)
			return
		}
		encoded = base64.RawURLEncoding.EncodeToString(key)
		if err := s.store.SetSealedSetting(context.Background(), cursorClientTokenKeySetting, encoded); err != nil {
			s.cursorClientKeyErr = err
			return
		}
		s.cursorClientKey = key
	})
	return s.cursorClientKey, s.cursorClientKeyErr
}

func cursorClientMACKey(deviceKey []byte, cursorAPIKey string) []byte {
	mac := hmac.New(sha256.New, deviceKey)
	_, _ = mac.Write([]byte(cursorClientTokenIssuer))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(cursorAPIKey))
	return mac.Sum(nil)
}

func cursorClientToken(subject, digest string, signingKey []byte) (string, error) {
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{"HS256", "JWT"})
	if err != nil {
		return "", err
	}
	claims := cursorClientClaims{
		Issuer:    cursorClientTokenIssuer,
		Subject:   subject,
		KeyDigest: digest,
		Version:   1,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, signingKey)
	_, _ = mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseCursorClientToken(token string) (cursorClientClaims, string, []byte, bool) {
	var claims cursorClientClaims
	if len(token) == 0 || len(token) > cursorClientTokenMaxBytes {
		return claims, "", nil, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, "", nil, false
	}
	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, "", nil, false
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if json.Unmarshal(headerRaw, &header) != nil || header.Algorithm != "HS256" || header.Type != "JWT" {
		return claims, "", nil, false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payloadRaw, &claims) != nil ||
		claims.Issuer != cursorClientTokenIssuer || claims.Version != 1 ||
		strings.TrimSpace(claims.Subject) == "" || len(claims.KeyDigest) != sha256.Size*2 {
		return cursorClientClaims{}, "", nil, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != sha256.Size {
		return cursorClientClaims{}, "", nil, false
	}
	return claims, parts[0] + "." + parts[1], signature, true
}

func verifyCursorClientToken(input string, signature, signingKey []byte) bool {
	mac := hmac.New(sha256.New, signingKey)
	_, _ = mac.Write([]byte(input))
	return hmac.Equal(signature, mac.Sum(nil))
}

// withCursorAuth 同时接受安装命令注入的原客户端 Key 与 exchange 签发的本地
// JWT。JWT 先按其受签名保护的 Key 摘要回查原条目，再用当前 Cursor Dashboard
// API Key 验签；因此客户端 Key 被禁用/删除或订阅 Key 被轮换都会即时拒绝。
func (s *Server) withCursorAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := clientKey(r)
		if token == "" {
			connectErrorStyle(w, http.StatusUnauthorized, "missing_api_key",
				"Missing API key. Pass it via the Authorization: Bearer header or the x-api-key header.")
			return
		}
		ident, ok := s.keys.Authorize(r.Context(), token)
		if !ok {
			claims, input, signature, parsed := parseCursorClientToken(token)
			if parsed {
				if candidate, found := s.keys.AuthorizeDigest(r.Context(), claims.KeyDigest); found {
					if _, blob, err := s.store.GetAgentCredential(r.Context(), store.AgentProviderCursor); err == nil {
						if credential, err := cursorauth.Parse(blob); err == nil &&
							credential.Token() != "" {
							if deviceKey, err := s.cursorClientSigningKey(); err == nil &&
								verifyCursorClientToken(input, signature,
									cursorClientMACKey(deviceKey, credential.Token())) {
								ident, ok = candidate, true
							}
						}
					}
				}
			}
		}
		if !ok {
			connectErrorStyle(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key provided.")
			return
		}
		info := infoFrom(r.Context())
		info.keyID = ident.KeyID
		info.bill.keyDisplay = ident.KeyDisplay
		info.limits = ident.KeyAuth
		next.ServeHTTP(w, r)
	})
}

// handleCursorRPC 是 /agents/cursor/ 子树的整面入口（已过认证中间件与
// withSubscription("cursor", false)）。路径不匹配 RPC 形（含非 POST）一律
// 本地 404，与 /agents/ 兜底同纪律、Connect 风格。
func (s *Server) handleCursorRPC(w http.ResponseWriter, r *http.Request) {
	m := cursorRPCRe.FindStringSubmatch(r.URL.Path)
	if m == nil || r.Method != http.MethodPost {
		handleNotFound(w, r)
		return
	}
	rpc := m[1]
	// 只有 Run/RunSSE 真正开始一轮模型对话：它们各记 1 次请求、
	// 旁路提取模型与最终 token 用量，并过密钥级预算/RPM 准入。BidiAppend、模型发现、
	// 配置查询和 Analytics 等辅助 RPC 是一轮对话内的协议流量：照常转发
	// 与记访问日志，但不冒充模型消费，也不消耗 RPM 配额。
	if usage.IsCursorAgentRunRPC(rpc) {
		beginEntry(r, usage.EntryCursorAgent, usage.CursorAgentModelDimension)
		s.observeCursorCall(r, rpc)
		defer s.finishCursorCall(r)
		if !s.admit(w, r, connectErrorStyle) {
			return
		}
	}
	acct, apiKey, ok := s.cursorCredential(w, r)
	if !ok {
		return
	}
	if strings.HasPrefix(rpc, "agent.v1.") {
		// agent.v1 是对话流：HTTP/2 使用 Run 双向流，受管 HTTP/1.1 当前使用
		// RunSSE；两者都不能按普通 RPC 先 io.ReadAll 再发。gate 通过
		// useHttp1ForAgent 把它留在 CURSOR_API_ENDPOINT 本入口；这里以设备
		// 缓存的真实 Cursor token 替换本地 JWT 后流式转到 api2。
		s.forwardCursorAgentRPC(w, r, s.cursorSessionFor(acct, apiKey), rpc)
		return
	}
	// 请求体整读在手：401 之后要原样重发，而 r.Body 只能读一遍。proto 帧是
	// 二进制，不解析、不改写（§15.1：内容也不进日志）。整读预设了客户端
	// 半双工——真机钉死的 cursor-agent（connect-es/undici）走纯 HTTP/1.1，
	// 请求侧必然先发完再收；若将来某个客户端真以 h2 全双工打 bidi RPC，要在
	// 「流式转发」与「401 重试」之间重新取舍。上限口径同文本入口：刻意不预裁
	// （entry.go 的 videoSubmitBodyLimit 注释——只有视频/图片面挂体积闸）。
	body, err := io.ReadAll(r.Body)
	if err != nil {
		connectErrorStyle(w, http.StatusBadRequest, "invalid_request_body",
			"Failed to read the request body.")
		return
	}
	s.observeCursorAppend(r, rpc, body)
	s.forwardCursorRPC(w, r, s.cursorSessionFor(acct, apiKey), acct.ID, rpc, body)
}

// forwardCursorAgentRPC 转发 agent.v1 Connect 双向流。与 aiserver.v1 的
// 可回放 RPC 不同，这里不能在收到 401 后重放已经流出的请求帧；拒绝时只作废
// 当前上游 token，让客户端下一次请求触发重新换发。请求/响应两侧都逐块转发，
// 计量观察器只临时缓存有界的单帧，不把对话内容落盘。
func (s *Server) forwardCursorAgentRPC(w http.ResponseWriter, r *http.Request,
	sess *cursorSession, rpc string) {

	info := infoFrom(r.Context())
	token, err := s.cursorToken(r.Context(), sess)
	if err != nil {
		// cursorCredential 已经拿到行；这里的 accountID 只在确定性换发拒绝时
		// 用于落闩，而 sess.rowID 就是该行的稳定身份。
		s.writeCursorTokenError(w, r, sess.rowID, err)
		return
	}
	info.attempts = 1

	// Go 的 HTTP/1 server 缺省会在写响应前耗尽请求体；Connect bidi 必须显式
	// 打开全双工。HTTP/2 天然全双工，ResponseController 在该情形同样安全。
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil && r.ProtoMajor == 1 {
		s.log.Warn("Cursor 双向流无法启用全双工", "request_id", info.id, "rpc", rpc)
		connectErrorStyle(w, http.StatusBadGateway, "full_duplex_unavailable",
			"The gateway cannot establish a full-duplex Cursor agent stream.")
		return
	}

	attemptCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := s.buildCursorRequest(attemptCtx, r, rpc, r.Body, token)
	if err != nil {
		s.log.Error("构造 Cursor Agent 上游请求失败", "request_id", info.id, "rpc", rpc)
		connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，Cursor Agent 上游请求已取消", "request_id", info.id, "rpc", rpc)
			return
		}
		s.log.Warn("Cursor Agent 上游请求失败", "request_id", info.id, "rpc", rpc,
			"error_kind", claudeTransportErrorKind(err))
		connectErrorStyle(w, http.StatusBadGateway, "upstream_unreachable",
			"The gateway failed to reach the Cursor agent service.")
		return
	}
	if resp.StatusCode == http.StatusUnauthorized {
		sess.invalidate(token)
	}
	s.passthroughCursorResponse(w, r, resp, rpc)
}

// cursorCredential 取「已连接且可用的 Cursor 订阅」的解封 API Key：每请求点查
// agent_accounts（同 Key 鉴权的无缓存口径），按行状态分岔出机读原因。返回
// ok=false 时错误响应已写出。整体仿 claudeCredential。
func (s *Server) cursorCredential(w http.ResponseWriter, r *http.Request) (*store.AgentAccount, string, bool) {
	info := infoFrom(r.Context())
	if s.store == nil {
		connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, "", false
	}
	acct, blob, err := s.store.GetAgentCredential(r.Context(), store.AgentProviderCursor)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeCursorNotConfigured(w)
		return nil, "", false
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		s.log.Warn("Cursor 订阅凭据解不开，请管理员重新连接",
			"request_id", info.id, "provider", store.AgentProviderCursor, "err", err.Error())
		writeCursorAuthExpired(w)
		return nil, "", false
	case r.Context().Err() != nil:
		return nil, "", false
	default:
		s.log.Error("读取 Cursor 订阅失败", "request_id", info.id, "err", err.Error())
		connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return nil, "", false
	}

	// 上游维度在**账号点查成功的这一刻**就钉住，不等凭据解析完：从这里往下的每
	// 一种拒绝（管理员停用、登录失效、凭据形态不合）说的都是这一个订阅账号自己的
	// 事，账要记在它头上，别掉进用量页「按上游账号」那一格没有名字的行里（同
	// responses.go 的 agentCredential）。
	info.upstream = agentUsageUpstreamName(acct)
	if acct.Status == store.AgentStatusAuthExpired {
		writeCursorAuthExpired(w)
		return nil, "", false
	}
	if acct.Status != store.AgentStatusActive {
		writeCursorNotConfigured(w)
		return nil, "", false
	}
	cred, err := cursorauth.Parse(blob)
	if err != nil {
		s.log.Warn("Cursor 订阅凭据形态不合，请管理员重新连接",
			"request_id", info.id, "account", acct, "err", err.Error())
		s.markCursorAuthExpired(r.Context(), acct.ID)
		writeCursorAuthExpired(w)
		return nil, "", false
	}
	return acct, cred.Token(), true
}

// cursorSessionFor 取（必要时建/换代）当前凭据世代的换发缓存。**只进不退**：
// 点查发生在 cursorMu 之外，两个并发请求的快照可以乱序到达——迟到的旧快照
// 解出的旧 Key 不得把内存里的新世代倒回去（倒回去的代价没有 codex 那么致命
// ——exchange 幂等、不烧世代——但会造成两把 Key 交替重换发的抖动）。同一行
// 同一把 Key 时只跟进世代号，已换发的 token 继续用。
func (s *Server) cursorSessionFor(acct *store.AgentAccount, apiKey string) *cursorSession {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	sess := s.cursorSess
	if sess != nil && sess.rowID == acct.ID {
		if sess.apiKey == apiKey {
			if acct.UpdatedAt.After(sess.updatedAt) {
				sess.updatedAt = acct.UpdatedAt
			}
			return sess
		}
		if !acct.UpdatedAt.After(sess.updatedAt) {
			return sess // 迟到的旧快照：只进不退
		}
	}
	sess = &cursorSession{rowID: acct.ID, updatedAt: acct.UpdatedAt, apiKey: apiKey}
	s.cursorSess = sess
	return sess
}

// cursorToken 返回一个可用的上游 accessToken，必要时先换发（单飞：换发在持锁
// 期间完成）。绝大多数请求是一次纯内存读。
func (s *Server) cursorToken(ctx context.Context, sess *cursorSession) (string, error) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	// 等锁期间调用方可能已经走了（客户端断开）：别为没人要的应答再打一次上游。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if sess.token != "" {
		return sess.token, nil
	}
	token, err := s.cursorExchangeUpstream(ctx, sess.apiKey)
	if err != nil {
		return "", err
	}
	sess.token = token
	return token, nil
}

// cursorExchangeUpstream 用板上封存的 API Key 打上游换发端点。
//
// 三种结局：2xx 且体含非空 accessToken → 成功；401/403 → **确定性拒绝**
// （包 agentauth.ErrAuthExpired，调用方据此落闩）；其余（网络错/5xx/形态不合）
// → 可重试失败。响应里的 refreshToken 有意不留存——上游无 refresh 协议要求，
// 「刷新」恒等于重走本函数，少存一份秘密。错误文本只含阶段与状态码；换发
// URL 无查询串，传输层错误文本可安全落日志。
func (s *Server) cursorExchangeUpstream(ctx context.Context, apiKey string) (string, error) {
	target, err := s.cursorTarget(cursorExchangePath, "")
	if err != nil {
		return "", fmt.Errorf("换发 Cursor 上游令牌: %w", err)
	}
	rctx, cancel := context.WithTimeout(ctx, cursorExchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		return "", fmt.Errorf("换发 Cursor 上游令牌: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("换发 Cursor 上游令牌: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 确定性拒绝：API Key 被上游明确回绝，再试多少次都是同一个答复。
		// 双 %w 同 agentauth.refreshLocked：ErrAuthExpired 给分支判断，
		// OAuthError 留状态码给排障，消息仍是一行。
		return "", fmt.Errorf("%w（%w）", agentauth.ErrAuthExpired,
			&agentauth.OAuthError{Phase: "换发 Cursor 上游令牌", Origin: "Cursor", Status: resp.StatusCode})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("换发 Cursor 上游令牌: 上游状态 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cursorExchangeMaxBytes))
	if err != nil {
		return "", fmt.Errorf("换发 Cursor 上游令牌: 读取响应失败")
	}
	payload, ok := decodeJSONObject(raw)
	if !ok {
		return "", errors.New("换发 Cursor 上游令牌: 响应不是可解析的 JSON 对象")
	}
	token := jsonString(payload["accessToken"])
	if token == "" {
		return "", errors.New("换发 Cursor 上游令牌: 响应缺 accessToken")
	}
	return token, nil
}

// forwardCursorRPC 把一个 RPC 转给上游并原样回写。至多两次尝试，且第二次只在
// **上游拒绝了当前 token**（401）时发生：作废那一代、重新换发、重发。判据
// **只看状态码**（responses.go 的纪律）。第二次仍 401 说明新换发的 token 也
// 被拒——订阅本身已不可用，落 auth_expired 闩，响应仍原样透传（上游错误体
// 不包装）。其余 4xx/5xx 一律原样透传：这里没有下一个来源。
func (s *Server) forwardCursorRPC(w http.ResponseWriter, r *http.Request,
	sess *cursorSession, accountID int64, rpc string, body []byte) {

	info := infoFrom(r.Context())
	for attempt := 1; attempt <= 2; attempt++ {
		token, err := s.cursorToken(r.Context(), sess)
		if err != nil {
			s.writeCursorTokenError(w, r, accountID, err)
			return
		}
		info.attempts = attempt

		attemptCtx, cancel := context.WithCancel(r.Context())
		req, err := s.buildCursorRequest(attemptCtx, r, rpc, bytes.NewReader(body), token)
		if err != nil {
			cancel()
			s.log.Error("构造 Cursor 上游请求失败", "request_id", info.id, "rpc", rpc)
			connectErrorStyle(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
			return
		}
		resp, err := s.client.Do(req)
		if err != nil {
			cancel()
			if r.Context().Err() != nil {
				s.log.Info("客户端断开，Cursor 上游请求已取消", "request_id", info.id, "rpc", rpc)
				return
			}
			// transport error 的文本可能含完整 URL（客户端查询串禁止落日志），
			// 只归有限类别（claude.go 同款）。
			s.log.Warn("Cursor 上游请求失败", "request_id", info.id, "rpc", rpc,
				"attempt", attempt, "error_kind", claudeTransportErrorKind(err))
			connectErrorStyle(w, http.StatusBadGateway, "upstream_unreachable",
				"The gateway failed to reach Cursor.")
			return
		}
		if resp.StatusCode == http.StatusUnauthorized {
			sess.invalidate(token)
			if attempt == 1 {
				// 有界丢弃这份 401 体（内容一概不进日志），换一代 token 重发。
				discardBody(resp, cancel)
				s.log.Info("Cursor 上游拒绝当前凭据，重新换发后重试一次", "request_id", info.id)
				continue
			}
			// 刚换发的 token 也被拒：订阅失效，落闩后仍原样透传上游应答。
			s.markCursorAuthExpired(r.Context(), accountID)
		}
		// 提交：此后一律原样透传（取消推迟到回写完成之后，提前取消会掐断
		// 正在转发的流式响应）。
		defer cancel()
		s.passthroughCursorResponse(w, r, resp, rpc)
		return
	}
}

// buildCursorRequest 装配发往上游的 RPC 请求：客户端业务头照转（连接头/
// Cookie/双向凭证/设备控制头剥掉），再注入换发的上游 token。设备前缀
// /agents/cursor 剥掉，上游看到的路径就是 /aiserver.v1.<Service>/<Method>。
func (s *Server) buildCursorRequest(ctx context.Context, r *http.Request,
	rpc string, body io.Reader, token string) (*http.Request, error) {

	target, err := s.cursorTarget("/"+rpc, r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, err
	}
	copyCursorForwardHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (s *Server) cursorTarget(upstreamPath, rawQuery string) (string, error) {
	base, err := url.Parse(s.cursorEndpoint())
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", errors.New("invalid Cursor endpoint")
	}
	base.Path = strings.TrimRight(base.Path, "/") + upstreamPath
	base.RawPath = ""
	base.RawQuery = rawQuery
	base.Fragment = ""
	return base.String(), nil
}

// copyCursorForwardHeaders 先复制业务头（x-cursor-*、x-ghost-mode、
// x-request-id、connect-protocol-version、content-type……CLI 加新头也照常过），
// 再剥掉客户端与设备控制面的认证物料。与 claude 的头清洗唯一的差别是保留
// Accept-Encoding：本入口不解析响应，客户端自己协商的编码字节原样到达它。
func copyCursorForwardHeaders(dst, src http.Header) {
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
}

// passthroughCursorResponse 把上游响应原样回写：状态码/响应头（除逐跳头）、
// 响应体逐块即时 flush（Connect 流式响应的增量帧不因设备中转攒批）、HTTP
// trailer 跟随转发。旁路只读最终用量，不改写响应字节。
func (s *Server) passthroughCursorResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, rpc string) {
	if call := infoFrom(r.Context()).bill.cursor; call != nil && resp.StatusCode < 400 {
		encoding := resp.Header.Get("Connect-Content-Encoding")
		if encoding == "" {
			encoding = resp.Header.Get("Content-Encoding")
		}
		observer := cursorwire.NewObserver(resp.Header.Get("Content-Type"), encoding, call.response)
		observer.EndStreamError = call.fail
		resp.Body = cursorwire.ObserveReader(resp.Body, observer)
	}
	defer resp.Body.Close()
	copyBackHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	_, copyErr := io.Copy(&flushWriter{w: w}, resp.Body)
	// resp.Trailer 在 body 读尽后由 Transport 填充；经 TrailerPrefix 写出，
	// net/http 以 chunked trailer 发给客户端（Content-Length 定长响应没有
	// trailer 可言，循环为空即空操作）。
	for name, values := range resp.Trailer {
		for _, value := range values {
			w.Header().Add(http.TrailerPrefix+name, value)
		}
	}
	if copyErr != nil {
		if call := infoFrom(r.Context()).bill.cursor; call != nil {
			call.fail()
		}
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，Cursor 响应回写终止", "request_id", infoFrom(r.Context()).id, "rpc", rpc)
			return
		}
		s.log.Warn("Cursor 上游响应中途断流",
			"request_id", infoFrom(r.Context()).id, "rpc", rpc, "err", copyErr.Error())
	}
}

// writeCursorTokenError 把「换发不出可用 token」翻成对客户端的应答。两类失败
// 两种反应（同 writeAgentTokenError）：确定性拒绝是**状态**，落闩并指向管理员
// 重新连接；出网失败是**故障**，回 502 并说清是连不上。
func (s *Server) writeCursorTokenError(w http.ResponseWriter, r *http.Request, accountID int64, err error) {
	info := infoFrom(r.Context())
	switch {
	case r.Context().Err() != nil:
		s.log.Info("客户端断开，Cursor 换发已取消", "request_id", info.id)
	case errors.Is(err, agentauth.ErrAuthExpired):
		s.log.Warn("Cursor 订阅被上游拒绝，需管理员重新连接",
			"request_id", info.id, "err", err.Error())
		s.markCursorAuthExpired(r.Context(), accountID)
		writeCursorAuthExpired(w)
	default:
		// 换发错误只含阶段名与 HTTP 状态（§15.1），可整句落日志。
		s.log.Warn("Cursor 换发失败", "request_id", info.id, "err", err.Error())
		connectErrorStyle(w, http.StatusBadGateway, "upstream_unreachable",
			"The gateway failed to reach Cursor to exchange the subscription credential. "+
				"Check the device's outbound connectivity and retry.")
	}
}

// markCursorAuthExpired 把库里那一行标成 auth_expired（claude.go 的闩模式）：
// 管理台据此显「需重新连接」，后续请求据此不再出网。
func (s *Server) markCursorAuthExpired(ctx context.Context, accountID int64) {
	if err := s.store.SetAgentStatus(context.WithoutCancel(ctx), accountID, store.AgentStatusAuthExpired); err != nil {
		s.log.Warn("标记 Cursor 订阅登录失效失败",
			"request_id", infoFrom(ctx).id, "account_row", accountID, "err", err.Error())
	}
}

// refreshCursorAgent 是管理面「自检」在 cursor 侧的落点（RefreshAgent 分流至
// 此）：用库里那把 API Key 强制重走一次 exchange——管理员要的答复就是「这份
// 订阅现在还能不能换出 token」，只有真去换一次才答得了。成功盖 last_refresh_at
// 并刷新缓存；被上游确定性拒绝落 auth_expired（错误按 agentauth.ErrAuthExpired
// 一族返回，管理面据此显「需重新连接」）。
func (s *Server) refreshCursorAgent(ctx context.Context) error {
	acct, blob, err := s.store.GetAgentCredential(ctx, store.AgentProviderCursor)
	if err != nil {
		return err
	}
	cred, err := cursorauth.Parse(blob)
	if err != nil {
		// 同数据面处置（cursorCredential）：形态不合只有重新连接一条路，报成
		// 「登录已失效」，别让管理面把它归进「连不上上游」（RefreshAgent 同款）。
		return fmt.Errorf("%w（订阅凭据形态不合：%v）", agentauth.ErrAuthExpired, err)
	}
	sess := s.cursorSessionFor(acct, cred.Token())
	sess.mu.Lock()
	defer sess.mu.Unlock()
	token, err := s.cursorExchangeUpstream(ctx, sess.apiKey)
	if err != nil {
		if errors.Is(err, agentauth.ErrAuthExpired) {
			sess.token = ""
			s.markCursorAuthExpired(ctx, acct.ID)
		}
		return err
	}
	sess.token = token
	// 盖「最近刷新」：重封同一份规范明文（SetAgentAuthJSON 顺带盖
	// last_refresh_at；Key 本身不轮换，重封无副作用）。落库失败不推翻自检
	// 结论——exchange 刚刚真的成功了——只记一条日志。
	if canonical, jerr := cred.JSON(); jerr == nil {
		if serr := s.store.SetAgentAuthJSON(ctx, acct.ID, store.AgentProviderCursor, canonical); serr != nil {
			s.log.Warn("Cursor 自检成功但盖 last_refresh_at 失败",
				"account_row", acct.ID, "err", serr.Error())
		}
	}
	return nil
}

// writeCursorNotConfigured / writeCursorAuthExpired 是这条路仅有的两个机读
// 原因，与其余订阅入口同用 409（设备**状态**，不是故障也不是客户端 Key 的
// 问题）；Connect 侧成形为 failed_precondition（connectErrorCode）。
func writeCursorNotConfigured(w http.ResponseWriter) {
	connectErrorStyle(w, http.StatusConflict, "agent_not_configured",
		"No Cursor subscription is connected on this device. "+
			"Ask the administrator to connect one on the Agents accounts page.")
}

func writeCursorAuthExpired(w http.ResponseWriter) {
	connectErrorStyle(w, http.StatusConflict, "agent_auth_expired",
		"The Cursor subscription connected on this device was rejected upstream. "+
			"Ask the administrator to reconnect it on the Agents accounts page.")
}
