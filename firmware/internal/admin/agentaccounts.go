package admin

// Agents 订阅账号的管理面（迭代 11 Phase 4）：
//
//	GET    /admin/v1/agent-accounts                 列表（管理视图，无凭据）
//	POST   /admin/v1/agent-accounts/login/start     发起授权码+PKCE 登录
//	POST   /admin/v1/agent-accounts/login/callback  收管理员粘回的整条回调地址
//	POST   /admin/v1/agent-accounts/import          粘贴 auth.json 兜底
//	POST   /admin/v1/agent-accounts/claude/setup-token  封存 Claude setup-token
//	POST   /admin/v1/agent-accounts/cursor/api-key  封存 Cursor API Key
//	PATCH  /admin/v1/agent-accounts/{id}            改名称/默认模型/启停
//	DELETE /admin/v1/agent-accounts/{id}            删除
//	POST   /admin/v1/agent-accounts/{id}/refresh    自检（强制换一代令牌）
//
// 全部 admin-only——不在 isSelfServicePath 名单里就是管理端点（withSession
// 默认拒绝），本文件因此一个角色判断都不写。
//
// # 登录是两步一次 POST，没有状态机（两个 provider 各自的形态）
//
// codex：设备码流在 auth.openai.com 上根本不存在（迭代 11 决策 4 的实测结论，
// 证据在 codexauth 的包注释里），走**授权码 + PKCE + 粘贴回调地址**：
//
//	start     盒子生成 code_verifier/state，回一条 authorize URL
//	（人）    管理员在浏览器里登录授权，最后停在一个**打不开的**
//	          localhost:1455 页面上——那是预期结果，要的就是地址栏那条 URL
//	callback  管理员把整条地址粘回来；盒子核 state、换码、密封落库
//
// grok：auth.x.ai **有**设备码流（docs/firmware-agents-grok.md），走 RFC 8628：
//
//	start     盒子打一次设备码端点，回 user_code + 授权页地址
//	（人）    管理员在任意浏览器打开授权页、登录 xAI、批准那串代码
//	callback  盒子打**一次**令牌端点问「批准了没」；authorization_pending 回
//	          409 agent_login_pending，管理员批准后**再点一次**就是下一次尝试
//
// 换码与落库全在 callback 这一次 POST 里同步走完：**没有后台协程、没有读态
// GET、没有轮询**（零轮询铁律——grok 的「轮询」由管理员的点击驱动）。也因此
// 这条路是变更方法，天然受 X-LlmGate-CSRF 覆盖。
//
// 在飞的登录会话按 provider 各持**单件**：同 provider 重复 start 顶掉旧的，
// 两个 provider 互不相干。登录会话重来无代价（codex 是一次本地随机数，grok 是
// 一次设备码请求），与网络变更那种「重来要付出代价、所以 409 挡住」的场景
// 不同——那里挡的是把设备改到一半，这里顶掉的只是一串还没用过的随机数。
//
// # 自检不自己刷新
//
// POST {id}/refresh 把刷新**委托给数据面**（[AgentTokens]，生产实现是
// gateway.Server）。本包若自己造一个 codexauth.Provider，盒子就有了第二个
// 消费者去自轮换同一份 refresh token——那正是决策 1 整条安全论证要消灭的形态，
// 发生在进程内与发生在用户 PC 上同样致命（见 gateway.Server.RefreshAgent）。
//
// # §15.1
//
// auth.json、access/refresh/id token、code_verifier 一律不进任何响应、日志与
// 审计 detail。落地方式是**根本不持有它们**：句柄经 codexauth 解析后立刻密封
// 落库（store.UpsertAgentAccount），本包不留副本；code_verifier 只活在
// codexauth.Login 里（未导出字段、自遮蔽 String）。审计只记 provider、名称、
// account_id 的**末段**与状态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/codexauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/grokauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Agents 账号的审计事件（entity 形如 agent:1）。detail 只含 provider/名称/
// account_id 末段/状态——凭据物料一个字节都不进（§15.1）。
const (
	EventAgentConnect = "agent.connect"
	EventAgentUpdate  = "agent.update"
	EventAgentDelete  = "agent.delete"
	EventAgentRefresh = "agent.refresh"
)

// agentLabelMaxRunes 是订阅名称的长度上限（展示用，与 Key 标签同量级）。
const agentLabelMaxRunes = 128

// AgentTokens 是 Agents 令牌状态的持有方（生产实现是 *gateway.Server，
// 由 cmd/llmgate 在装配期经 [Server.SetAgentTokens] 注入）。
//
// 接口定义在本包而不是 import 数据面：与 [UsageReader] 同一条
// 装配纪律——管理面 handler 先于 gateway 构造，倒过来持有会成环。
type AgentTokens interface {
	// RefreshAgent 强制给该 provider 的订阅句柄换一代令牌，走的是数据面
	// 那**同一个** Provider（单刷新者不变量）。刷新成功后的落库与失效标记
	// 由数据面的回调完成，本包只看返回的错误分类。
	RefreshAgent(ctx context.Context, provider string) error
}

// agentLoginState 是「模型接入」页订阅接入标签页的进程内状态。零值可用。
//
// 每个 provider 各一件在飞登录会话（codex 的含 code_verifier、grok 的含
// device_code，都是密钥物料，绝不落库）、各一个 OAuth 客户端（惰性构造），
// 以及共用的令牌状态持有方。两个 provider 的会话互不相干：连 grok 不该顶掉
// 正在连的 codex。
type agentLoginState struct {
	mu     sync.Mutex
	login  *codexauth.Login
	client *codexauth.Client
	// issuer 非空只用于开发期指向假 OpenAI（测试桩），生产恒为空 =
	// codexauth.DefaultIssuer（决策 9：本迭代零新增必填 env）。
	issuer string
	// grok 侧（设备码流，docs/firmware-agents-grok.md）。
	grokLogin  *grokauth.DeviceLogin
	grokClient *grokauth.Client
	grokIssuer string
	tokens     AgentTokens
	// egress 是 OAuth 客户端的出站策略（agent_auth 分类），装配期由 New 写入。
	egress egress.Router
}

// SetAgentTokens 注入 Agents 令牌状态的持有方（数据面）。装配期调用一次；
// 不注入时自检端点答 503——「这台设备上没有能替你刷新的那一半」是实话，
// 假装刷过更糟。
func (s *Server) SetAgentTokens(t AgentTokens) {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	s.agents.tokens = t
}

// SetAgentIssuer 把 codex 的登录与刷新指向别的账号签发方。**只给开发期与测试
// 用**（同 gateway.Server.SetAgentEndpoints），生产恒走内置常量。调用会丢掉
// 在飞的登录会话与已构造的客户端。
func (s *Server) SetAgentIssuer(issuer string) {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	s.agents.issuer, s.agents.client, s.agents.login = issuer, nil, nil
}

// SetGrokIssuer 同上，作用于 grok 那一侧（auth.x.ai 的替身）。
func (s *Server) SetGrokIssuer(issuer string) {
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	s.agents.grokIssuer, s.agents.grokClient, s.agents.grokLogin = issuer, nil, nil
}

// oauthClient 取（必要时建）与 OpenAI 之间的 OAuth 客户端。
func (a *agentLoginState) oauthClient() *codexauth.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil {
		a.client = codexauth.NewClient(a.issuer, codexauth.WithEgress(a.egress))
	}
	return a.client
}

// startLogin 记下新的在飞会话，顶掉旧的（见文件头「单件」一段），并**给它上一个
// 到点自毁的闹钟**。
//
// 闹钟不是洁癖：会话里的 code_verifier 是密钥物料，而最常见的收场恰恰是管理员
// 点了「开始连接」就走开了——没有它，那串东西会在内存里躺到进程结束，而
// codexauth 明写的契约是「只活到换码或过期」。清除按身份比对，所以一个迟到的
// 闹钟不会误伤后来那次登录。
func (a *agentLoginState) startLogin(login *codexauth.Login) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.login = login
	time.AfterFunc(codexauth.LoginTTL, func() { a.expire(login) })
}

// expire 到点丢弃一次会话（仍是当前那一份才丢）。
func (a *agentLoginState) expire(login *codexauth.Login) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.login == login {
		a.login = nil
	}
}

// currentLogin 取在飞会话；nil = 还没 start 过（或已被消费）。
//
// **过期的那一份照样交出来**，由 handler 去分辨——「超时了，请重新发起」与
// 「你还没发起过」是两句不同的话，在这里就地丢掉会把前者压成后者。真正的清除
// 归 [agentLoginState.startLogin] 上的闹钟，它到点就响，不依赖有没有人来读。
func (a *agentLoginState) currentLogin() *codexauth.Login {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.login
}

// finishLogin 消费掉一次会话。**按身份比对**：并发的另一次 start 可能已经换上
// 了新会话，那一份不该被这里清掉。
func (a *agentLoginState) finishLogin(login *codexauth.Login) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.login == login {
		a.login = nil
	}
}

// tokenHolder 取令牌状态持有方；nil = 未接线。
func (a *agentLoginState) tokenHolder() AgentTokens {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokens
}

// grokOAuthClient 取（必要时建）与 xAI 之间的 OAuth 客户端。
func (a *agentLoginState) grokOAuthClient() *grokauth.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grokClient == nil {
		a.grokClient = grokauth.NewClient(a.grokIssuer, grokauth.WithEgress(a.egress))
	}
	return a.grokClient
}

// startGrokLogin / expireGrok / currentGrokLogin / finishGrokLogin 与 codex
// 那组同一套单件 + 到点自毁纪律（device_code 是密钥物料，不该在内存里躺过
// 会话有效期）；TTL 取上游给的 expires_in（DeviceLogin.ExpiresAt）。
func (a *agentLoginState) startGrokLogin(login *grokauth.DeviceLogin) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grokLogin = login
	time.AfterFunc(time.Until(login.ExpiresAt), func() { a.expireGrok(login) })
}

func (a *agentLoginState) expireGrok(login *grokauth.DeviceLogin) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grokLogin == login {
		a.grokLogin = nil
	}
}

func (a *agentLoginState) currentGrokLogin() *grokauth.DeviceLogin {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grokLogin
}

func (a *agentLoginState) finishGrokLogin(login *grokauth.DeviceLogin) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grokLogin == login {
		a.grokLogin = nil
	}
}

// normalizeAgentProvider 校验请求里的 provider（空 = codex，兼容 grok 加入前
// 的调用形态）。词汇表在这里把守——`agent_accounts.provider` 列刻意无 CHECK。
func normalizeAgentProvider(w http.ResponseWriter, provider string) (string, bool) {
	switch provider {
	case "", store.AgentProviderCodex:
		return store.AgentProviderCodex, true
	case store.AgentProviderGrok:
		return store.AgentProviderGrok, true
	default:
		writeError(w, http.StatusBadRequest, "agent_provider_invalid",
			"不支持的 Agent 类型（目前支持 codex 与 grok）")
		return "", false
	}
}

// agentProviderLabel 是文案里的 provider 名。
func agentProviderLabel(provider string) string {
	switch provider {
	case store.AgentProviderGrok:
		return "Grok Build"
	case store.AgentProviderClaude:
		return "Claude Code"
	case store.AgentProviderCursor:
		return "Cursor"
	default:
		return "Codex"
	}
}

// decodeJSONOptional 同 decodeJSON，但**空 body 是合法的**（全取默认值）。
// 给 login/start 用：grok 加入前它就是一条无 body 的 POST，前端旧形态与
// 既有测试都按这个契约写。
func decodeJSONOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	writeError(w, http.StatusBadRequest, "bad_request", "请求体不是合法的 JSON 或含未知字段")
	return false
}

func entityAgent(id int64) string { return fmt.Sprintf("agent:%d", id) }

// agentAccountJSON 是订阅账号在管理 API 里的对外形态。
//
// **没有凭据字段，也没有可以放凭据的地方**：store.AgentAccount 的密文列在管理
// 视图里压根不 SELECT，这里再逐字段抄一遍，于是即便将来那条查询改了，凭据也
// 出不了这个结构（同 upstreamJSON 之于 api_key 的分法）。
//
// AccountID 是 OpenAI 侧账号标识，**明文列、不是密钥物料**（决策 3）：管理台
// 靠它认「连的是哪个账号」。审计只记末段，那是另一回事——审计行会被长期保存
// 且给非当事人看。
type agentAccountJSON struct {
	ID           int64  `json:"id"`
	Provider     string `json:"provider"`
	Label        string `json:"label"`
	AccountID    string `json:"account_id"`
	DefaultModel string `json:"default_model"`
	// Status 三值：active / disabled / auth_expired（后者界面显「需重新登录」）。
	Status string `json:"status"`
	// LastRefreshAt 为 null 表示从未刷新过（刚连上就是这个状态）。
	LastRefreshAt        *time.Time           `json:"last_refresh_at"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	Quota                *agentquota.Snapshot `json:"quota,omitempty"`
	CredentialKind       string               `json:"credential_kind,omitempty"`
	SetupTokenConfigured bool                 `json:"setup_token_configured,omitempty"`
	QuotaOAuthConfigured bool                 `json:"quota_oauth_configured,omitempty"`
	QuotaOAuthExpired    bool                 `json:"quota_oauth_expired,omitempty"`
}

func toAgentAccountJSON(a *store.AgentAccount) agentAccountJSON {
	out := agentAccountJSON{
		ID:           a.ID,
		Provider:     a.Provider,
		Label:        a.Label,
		AccountID:    a.AccountID,
		DefaultModel: a.DefaultModel,
		Status:       a.Status,
		CreatedAt:    a.CreatedAt,
		UpdatedAt:    a.UpdatedAt,
	}
	if !a.LastRefreshAt.IsZero() {
		t := a.LastRefreshAt
		out.LastRefreshAt = &t
	}
	return out
}

// agentAccountResponse 包装单账号响应。
type agentAccountResponse struct {
	Account agentAccountJSON `json:"account"`
}

// handleListAgentAccounts 列出全部订阅账号（当前最多一行——单账户语义）。
func (s *Server) handleListAgentAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := s.st.ListAgentAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := struct {
		Accounts []agentAccountJSON `json:"accounts"`
	}{Accounts: make([]agentAccountJSON, 0, len(accts))}
	for i := range accts {
		item := s.agentAccountJSON(r.Context(), &accts[i])
		if s.agentQuota != nil {
			q := s.agentQuota.Read(&accts[i])
			item.Quota = &q
		}
		out.Accounts = append(out.Accounts, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// agentLoginStartResponse 是发起登录的应答，形态随 provider 分两种：
//
//   - codex（授权码+PKCE）：一条**要给人打开的地址**（authorize_url，里面是
//     公开参数与一个 code_challenge——verifier 的 SHA-256，可以显示、可以复制；
//     verifier 与 state 留在盒内）+ redirect_uri（浏览器最后停在的那个
//     **打不开**的地址，文案要指名道姓）。
//   - grok（设备码流）：授权页地址 + user_code（管理员在授权页上核对/输入；
//     verification_uri_complete 在上游给了时一并回）。device_code 留在盒内。
type agentLoginStartResponse struct {
	Provider string `json:"provider"`
	// ExpiresIn 是这次会话还剩多少秒（界面据此提示「超时请重新发起」）。
	ExpiresIn int `json:"expires_in"`

	// codex 侧两键（grok 时缺席）。
	AuthorizeURL string `json:"authorize_url,omitempty"`
	RedirectURI  string `json:"redirect_uri,omitempty"`

	// grok 侧三键（codex 时缺席）。
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
}

// handleAgentLoginStart 发起一次登录。codex 是纯本地动作（拼 authorize URL，
// 出网发生在管理员的浏览器里）；grok 要打一次设备码端点（这条路的第一次出网，
// 受限现场会失败，错误说清是连不上）。
func (s *Server) handleAgentLoginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
	}
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	provider, ok := normalizeAgentProvider(w, req.Provider)
	if !ok {
		return
	}
	if provider == store.AgentProviderGrok {
		login, err := s.agents.grokOAuthClient().StartDeviceLogin(r.Context())
		if err != nil {
			s.writeGrokLoginStartError(w, r, err)
			return
		}
		s.agents.startGrokLogin(login)
		writeJSON(w, http.StatusOK, agentLoginStartResponse{
			Provider:                provider,
			ExpiresIn:               int(time.Until(login.ExpiresAt).Seconds()),
			UserCode:                login.UserCode,
			VerificationURI:         login.VerificationURI,
			VerificationURIComplete: login.VerificationURIComplete,
		})
		return
	}

	login, err := s.agents.oauthClient().NewLogin()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.agents.startLogin(login)
	writeJSON(w, http.StatusOK, agentLoginStartResponse{
		Provider:     provider,
		AuthorizeURL: login.AuthorizeURL,
		ExpiresIn:    int(codexauth.LoginTTL.Seconds()),
		RedirectURI:  codexauth.RedirectURI,
	})
}

// writeGrokLoginStartError 把设备码端点的失败翻成应答：xAI 明确拒绝（4xx）回
// 400（管理员重试即可），连不上回 502 并指名 auth.x.ai（决策 9 的口径：受限
// 现场给明确报错，不静默挂死）。
func (s *Server) writeGrokLoginStartError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("Grok Build 设备码授权发起失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
	var oe *agentauth.OAuthError
	if errors.As(err, &oe) {
		writeError(w, http.StatusBadRequest, "agent_login_failed", err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, "agent_upstream_unreachable",
		err.Error()+"。若现场限制了出网，请检查设备到 auth.x.ai 的连通性后重试。")
}

// handleAgentLoginCallback 完成一次登录。codex：收管理员粘回来的整条回调
// 地址，核 state、换码；grok：不收任何码，打**一次**令牌端点问「批准了没」。
//
// 一次 POST 走完：换码/问询 → 密封落库 → 审计。失败**不消费会话**（管理员改
// 一处再试、或批准后再点一次即可）；只有真的换出句柄才把会话清掉（授权码 /
// 设备码单次消费）。grok 的 authorization_pending 既不是成功也不是失败——
// 回 409 agent_login_pending，管理员在浏览器里批准后**再点一次**就是下一次
// 尝试（盒子没有轮询循环，零轮询铁律）。
func (s *Server) handleAgentLoginCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider     string `json:"provider"`
		CallbackURL  string `json:"callback_url"`
		Label        string `json:"label"`
		DefaultModel string `json:"default_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	provider, ok := normalizeAgentProvider(w, req.Provider)
	if !ok {
		return
	}
	label, model, ok := s.readAgentProfile(w, req.Label, req.DefaultModel)
	if !ok {
		return
	}
	if provider == store.AgentProviderGrok {
		s.finishGrokLogin(w, r, label, model)
		return
	}
	login := s.agents.currentLogin()
	if login == nil {
		writeError(w, http.StatusConflict, "agent_login_not_started",
			"尚未发起登录，或盒子重启过。请先点「开始连接」拿到授权链接。")
		return
	}
	if login.Expired(time.Now()) {
		s.agents.finishLogin(login)
		writeError(w, http.StatusConflict, "agent_login_expired",
			fmt.Sprintf("登录会话已超过 %d 分钟，请重新发起登录。", int(codexauth.LoginTTL.Minutes())))
		return
	}
	auth, err := s.agents.oauthClient().ExchangeAuthCode(r.Context(), login, req.CallbackURL)
	if err != nil {
		s.writeAgentLoginError(w, r, err)
		return
	}
	s.agents.finishLogin(login)
	blob, err := auth.JSON()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.connectAgentAccount(w, r, store.AgentProviderCodex, auth.AccountID(), blob, label, model, "login")
}

// finishGrokLogin 是 grok 设备码流的收尾半步：一次令牌端点问询 + 三类答复。
func (s *Server) finishGrokLogin(w http.ResponseWriter, r *http.Request, label, model string) {
	login := s.agents.currentGrokLogin()
	if login == nil {
		writeError(w, http.StatusConflict, "agent_login_not_started",
			"尚未发起连接，或盒子重启过。请先点「开始连接」拿到授权页地址。")
		return
	}
	if login.Expired(time.Now()) {
		s.agents.finishGrokLogin(login)
		writeError(w, http.StatusConflict, "agent_login_expired", "登录会话已超时，请重新发起连接。")
		return
	}
	auth, err := s.agents.grokOAuthClient().CompleteDeviceLogin(r.Context(), login)
	switch {
	case err == nil:
	case errors.Is(err, grokauth.ErrAuthorizationPending):
		// 会话保留：管理员在浏览器里批准之后，再点一次就是下一次尝试。
		writeError(w, http.StatusConflict, "agent_login_pending",
			"还没有检测到授权完成。请先在浏览器里登录 xAI 账号并批准这串代码，然后回来再点一次「完成连接」。")
		return
	case errors.Is(err, grokauth.ErrSlowDown):
		writeError(w, http.StatusConflict, "agent_login_pending",
			"xAI 要求放慢节奏，请稍等几秒再点一次「完成连接」。")
		return
	case agentauth.Deterministic(err):
		// access_denied / expired_token：这次会话已经死了，清掉、明说重来。
		s.agents.finishGrokLogin(login)
		s.log.Warn("Grok Build 设备码授权被拒绝", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		writeError(w, http.StatusBadRequest, "agent_login_rejected",
			err.Error()+"。请重新发起连接。")
		return
	default:
		// 网络错等可重试失败：会话保留，说清是这一次没连上。
		s.log.Warn("Grok Build 设备码换码失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		writeError(w, http.StatusBadRequest, "agent_login_failed", err.Error())
		return
	}
	s.agents.finishGrokLogin(login)
	blob, err := auth.JSON()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.connectAgentAccount(w, r, store.AgentProviderGrok, auth.Account(), blob, label, model, "login")
}

// handleImportAgentAccount 是「粘贴 auth.json」兜底入口（决策 4 备选②）：
// 管理员在任意装了 codex / grok 的机器上登录一次，把 ~/.codex/auth.json 或
// ~/.grok/auth.json 整份贴进来。
//
// 与登录那条路共用同一份解析（codexauth/grokauth.ParseAuthJSON）与同一条落库
// 路径——兜底不该有第二套语义，否则两条路会慢慢长歪。
func (s *Server) handleImportAgentAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider     string `json:"provider"`
		AuthJSON     string `json:"auth_json"`
		Label        string `json:"label"`
		DefaultModel string `json:"default_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	provider, ok := normalizeAgentProvider(w, req.Provider)
	if !ok {
		return
	}
	label, model, ok := s.readAgentProfile(w, req.Label, req.DefaultModel)
	if !ok {
		return
	}
	// 形态错误只说缺什么、该怎么办，不回显原文（§15.1，两个解析器同一纪律）。
	var accountID, blob string
	if provider == store.AgentProviderGrok {
		auth, err := grokauth.ParseAuthJSON(req.AuthJSON)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_auth_json", err.Error())
			return
		}
		if blob, err = auth.JSON(); err != nil {
			s.internalError(w, r, err)
			return
		}
		accountID = auth.Account()
	} else {
		auth, err := codexauth.ParseAuthJSON(req.AuthJSON)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_auth_json", err.Error())
			return
		}
		if blob, err = auth.JSON(); err != nil {
			s.internalError(w, r, err)
			return
		}
		accountID = auth.AccountID()
	}
	s.connectAgentAccount(w, r, provider, accountID, blob, label, model, "import")
}

// handleConnectClaudeSetupToken accepts the official CLI's static credential.
// Browser OAuth login uses handleClaudeLoginCallback; both seal immediately.
func (s *Server) handleConnectClaudeSetupToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SetupToken   string `json:"setup_token"`
		Label        string `json:"label"`
		DefaultModel string `json:"default_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	label, model, ok := s.readAgentProfile(w, req.Label, req.DefaultModel)
	if !ok {
		return
	}
	cred, err := claudeauth.FromSetupToken(req.SetupToken)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_setup_token", err.Error())
		return
	}
	blob, err := cred.JSON()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.connectAgentAccount(w, r, store.AgentProviderClaude, "", blob, label, model, "setup_token")
}

// handleConnectCursorAPIKey 是 Cursor 订阅唯一的连接写入路：管理员把
// cursor.com/dashboard 签发的 API Key 粘贴进来，设备校验形态后立刻走设备密钥
// 密封落库（同 setup-token 纪律：**离线封存、不出网验证**——「这把 Key 还换
// 不换得出 token」由自检与数据面的 exchange 回答）。account_id 恒空：
// Dashboard Key 不携带账号标识，管理台靠名称认账号。
func (s *Server) handleConnectCursorAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey       string `json:"api_key"`
		Label        string `json:"label"`
		DefaultModel string `json:"default_model"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.DefaultModel) != "" {
		writeError(w, http.StatusBadRequest, "invalid_default_model",
			"Cursor 模型由 cursor-agent 经订阅面自行发现，设备不设置默认模型")
		return
	}
	label, _, ok := s.readAgentProfile(w, req.Label, "")
	if !ok {
		return
	}
	// 形态错误只说缺什么，不回显原文（§15.1，cursorauth 的错误恒不含明文）。
	cred, err := cursorauth.FromAPIKey(req.APIKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_api_key", err.Error())
		return
	}
	blob, err := cred.JSON()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.connectAgentAccount(w, r, store.AgentProviderCursor, "", blob, label, "", "api_key")
}

// connectAgentAccount 是两条连接路径（登录 / 粘贴导入）的共同收尾：密封落库、
// 记审计、回管理视图。
//
// authBlob 是 [codexauth.Auth.JSON] / [grokauth.Auth.JSON] 的输出而不是管理员
// 粘来的原文：它保真带回了原文里所有不认识的键，同时保证落进去的东西一定
// 解析得回来。
func (s *Server) connectAgentAccount(w http.ResponseWriter, r *http.Request,
	provider, accountID, authBlob, label, defaultModel, source string) {
	if provider == store.AgentProviderClaude {
		s.claudeLogin.mu.Lock()
		defer s.claudeLogin.mu.Unlock()
	}
	s.saveAgentAccount(w, r, provider, accountID, authBlob, label, defaultModel, source)
}

func (s *Server) saveAgentAccount(w http.ResponseWriter, r *http.Request,
	provider, accountID, authBlob, label, defaultModel, source string) {
	in := store.NewAgentAccount{
		Provider:     provider,
		Label:        label,
		AccountID:    accountID,
		DefaultModel: defaultModel,
		AuthJSON:     authBlob,
	}
	var acct *store.AgentAccount
	var err error
	if provider == store.AgentProviderClaude {
		acct, err = s.saveClaudeCredential(r.Context(), in)
	} else {
		acct, err = s.st.UpsertAgentAccount(r.Context(), in)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventAgentConnect,
		Entity:   entityAgent(acct.ID),
		Detail:   agentDetail(acct) + " source=" + source,
		RemoteIP: remoteIP(r),
	})
	s.log.Info("Agents 订阅已连接", "provider", acct.Provider, "account_row", acct.ID, "source", source)
	// 连上订阅 = 这份订阅的模型该出现在目录里了（2026-08-15，agentmodels.go）。
	// 在写响应之前收敛：管理台连接成功后紧接着的那次刷新，读到的就是带模型的
	// 列表，不必等下一次动作。收敛失败不影响本次连接的结果（已经连上了）。
	s.syncAgentModelsAfter(r, "agent_connect")
	writeJSON(w, http.StatusOK, agentAccountResponse{Account: s.agentAccountJSON(r.Context(), acct)})
}

// handlePatchAgentAccount 改名称 / 默认模型 / 启停。
//
// 名称与默认模型是**三态**（照 limits.go 的 json.RawMessage 先例）：缺省 =
// 不变、null 或空串 = 清空、有值 = 设置。这两项走 store.UpdateAgentAccount
// 而不是 Upsert——后者的「空即保持」语义下，清空根本表达不出来。
//
// 校验全部先于写入（同 handlePatchUpstream）：两个子动作各自落库、各记一条
// 审计，中途失败留下已生效的前一步，重试整个 PATCH 即可收敛（都是幂等赋值）。
func (s *Server) handlePatchAgentAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
		return
	}
	var req struct {
		Label        json.RawMessage `json:"label"`
		DefaultModel json.RawMessage `json:"default_model"`
		Disabled     *bool           `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}

	label, labelSet, err := parseAgentText(req.Label, "名称")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_label", err.Error())
		return
	}
	if labelSet {
		if err := validateAgentLabel(label); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_label", "名称不合法："+err.Error())
			return
		}
	} else {
		label = current.Label
	}
	model, modelSet, err := parseAgentText(req.DefaultModel, "默认模型")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_default_model", err.Error())
		return
	}
	if modelSet {
		if current.Provider == store.AgentProviderCursor && model != "" {
			writeError(w, http.StatusBadRequest, "invalid_default_model",
				"Cursor 模型由 cursor-agent 经订阅面自行发现，设备不设置默认模型")
			return
		}
		if err := validateAgentModel(model); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_default_model", "默认模型不合法："+err.Error())
			return
		}
	} else {
		model = current.DefaultModel
	}
	// 启停的目标状态。**auth_expired 不许经启停改写**：否则先停用再启用
	// 就能把真实的失效状态抹成 active，管理台显示正常、每个请求却照旧 409。
	// 恢复的两条路都在同一个页面上：重新连接，或点「自检」——自检真换出一代
	// 新令牌时会把状态带回 active（那时它是实话）。
	status := current.Status
	if req.Disabled != nil {
		switch {
		case current.Status == store.AgentStatusAuthExpired:
			if current.Provider == store.AgentProviderClaude {
				writeError(w, http.StatusConflict, "agent_auth_expired",
					"Claude setup-token 已失效，请重新运行 claude setup-token 并在本页更新模型调用凭据。额度授权自检不会恢复 setup-token。")
				return
			}
			if current.Provider == store.AgentProviderCursor {
				// Cursor 没有登录一说：恢复路是重新粘贴 API Key，或自检重走一次
				// exchange（真换出 token 时状态自己回 active）。
				writeError(w, http.StatusConflict, "agent_auth_expired",
					"这份 Cursor 订阅的 API Key 已被上游拒绝，启停不会让它可用。请重新粘贴有效的 API Key 覆盖连接，或先点「自检」确认它还能换发。")
				return
			}
			writeError(w, http.StatusConflict, "agent_auth_expired",
				"这份订阅的登录已失效，启停不会让它可用。请重新连接该订阅，或先点「自检」确认凭据还能刷新。")
			return
		case *req.Disabled:
			status = store.AgentStatusDisabled
		default:
			status = store.AgentStatusActive
		}
	}

	if label != current.Label || model != current.DefaultModel {
		if err := s.st.UpdateAgentAccount(r.Context(), id, label, model); err != nil {
			s.writeAgentError(w, r, err)
			return
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:    EventAgentUpdate,
			Entity:   entityAgent(id),
			Detail:   fmt.Sprintf("provider=%s label=%s default_model=%s", current.Provider, label, model),
			RemoteIP: remoteIP(r),
		})
	}
	if status != current.Status {
		if err := s.st.SetAgentStatus(r.Context(), id, status); err != nil {
			s.writeAgentError(w, r, err)
			return
		}
		s.audit(r.Context(), store.AuditEvent{
			Event:    EventAgentUpdate,
			Entity:   entityAgent(id),
			Detail:   fmt.Sprintf("provider=%s status=%s", current.Provider, status),
			RemoteIP: remoteIP(r),
		})
	}

	updated, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentAccountResponse{Account: s.agentAccountJSON(r.Context(), updated)})
}

// handleDeleteAgentAccount 删掉一份订阅（连同密文）。
//
// 删除后 /v1/responses 立刻回 agent_not_configured——那条路是每请求点查
// agent_accounts，没有缓存要失效。
func (s *Server) handleDeleteAgentAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
		return
	}
	current, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	if current.Provider == store.AgentProviderClaude {
		s.claudeLogin.mu.Lock()
		defer s.claudeLogin.mu.Unlock()
		s.claudeLogin.clearLocked()
	}
	if err := s.st.DeleteAgentAccount(r.Context(), id); err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event:  EventAgentDelete,
		Entity: entityAgent(id), Detail: agentDetail(current), RemoteIP: remoteIP(r),
	})
	// 撤掉订阅 = 它带的那些模型行该回收了（2026-08-15，agentmodels.go）：
	// 「只要连接了订阅就读出来显示」的另一半，撤了就不显示。
	s.syncAgentModelsAfter(r, "agent_delete")
	w.WriteHeader(http.StatusNoContent)
}

// handleRefreshAgentAccount 是「自检」：强制换一代令牌，回状态**不回令牌**。
//
// 刷新本身委托给数据面（见文件头「自检不自己刷新」）。成功后的落库与失效标记
// 都发生在那一侧的回调里，本处只补一件事——**auth_expired 回 active**：那个
// 状态是「自动重试到此为止」的闩，而管理员刚刚亲手证明了它能换出新令牌。
// disabled 不动：停用是管理员自己的决定，一次自检不该把它推翻。
func (s *Server) handleRefreshAgentAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
		return
	}
	current, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	if current.Provider == store.AgentProviderClaude {
		_, blob, readErr := s.st.GetAgentCredential(r.Context(), current.Provider)
		if readErr != nil {
			s.writeAgentError(w, r, readErr)
			return
		}
		cred, parseErr := claudeauth.Parse(blob)
		if parseErr != nil {
			s.writeAgentRefreshError(w, r, current.Provider, agentauth.ErrAuthExpired)
			return
		}
		if cred.OAuthExpired() {
			s.writeAgentRefreshError(w, r, current.Provider, agentauth.ErrAuthExpired)
			return
		}
		if !cred.IsOAuth() {
			writeError(w, http.StatusConflict, "agent_refresh_unsupported",
				"Claude setup-token 没有可由设备调用的刷新接口；请重新执行 claude setup-token 后覆盖连接")
			return
		}
	}
	tokens := s.agents.tokenHolder()
	if tokens == nil {
		// 只可能出现在没接线的测试进程里。说实话比假装刷过好。
		writeError(w, http.StatusServiceUnavailable, "agent_refresh_unavailable",
			"本进程未接入 Agents 令牌管理，无法自检")
		return
	}

	err = tokens.RefreshAgent(r.Context(), current.Provider)
	s.audit(r.Context(), store.AuditEvent{
		Event:    EventAgentRefresh,
		Entity:   entityAgent(id),
		Detail:   fmt.Sprintf("provider=%s result=%s", current.Provider, refreshResult(err)),
		RemoteIP: remoteIP(r),
	})
	if err != nil {
		s.writeAgentRefreshError(w, r, current.Provider, err)
		return
	}
	if current.Status == store.AgentStatusAuthExpired && current.Provider != store.AgentProviderClaude {
		if err := s.st.SetAgentStatus(r.Context(), id, store.AgentStatusActive); err != nil {
			s.writeAgentError(w, r, err)
			return
		}
	}
	updated, err := s.st.GetAgentAccount(r.Context(), id)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentAccountResponse{Account: s.agentAccountJSON(r.Context(), updated)})
}

// refreshResult 把一次自检的结论压成审计里的一个词（机读，非文案）。
func refreshResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, agentauth.ErrAuthExpired):
		return "auth_expired"
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		return "unreadable"
	default:
		return "unreachable"
	}
}

// writeAgentRefreshError 把自检失败翻成应答（两类失败两种反应，决策 5 在管理面
// 这一侧的落点）：确定性拒绝是**状态**，回 4xx 并指向重新连接；出网失败是
// **故障**，回 502 并说清是连不上，管理员重试即可（决策 9）。
func (s *Server) writeAgentRefreshError(w http.ResponseWriter, r *http.Request, provider string, err error) {
	name := agentProviderLabel(provider)
	switch {
	case errors.Is(err, agentauth.ErrAuthExpired):
		if provider == store.AgentProviderClaude {
			writeError(w, http.StatusConflict, "claude_quota_auth_expired", "Claude 额度授权已失效，请重新进行浏览器授权。模型调用仍使用独立的 setup-token。")
			return
		}
		// Cursor 没有登录可失效：被拒的是 API Key 本身，恢复 = 重新粘贴（与
		// handlePatchAgentAccount 的启停拒绝同一口径）。
		if provider == store.AgentProviderCursor {
			writeError(w, http.StatusConflict, "agent_auth_expired",
				"这份 Cursor 订阅的 API Key 已被上游拒绝，自动换发到此为止。请在本页重新粘贴有效的 API Key 覆盖连接。")
			return
		}
		writeError(w, http.StatusConflict, "agent_auth_expired",
			"这份订阅的登录已失效，自动刷新到此为止。请在本页重新连接该 "+name+" 订阅。")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
	case errors.Is(err, store.ErrAgentAuthUnreadable):
		writeError(w, http.StatusConflict, "agent_auth_unreadable",
			"这份订阅的凭据在本设备上解不开（设备密钥被替换或密文损坏），请重新连接该 "+name+" 订阅。")
	default:
		// 兜底一支。绝大多数落到这里的是出网失败（决策 9 明写受限现场会失败），
		// 但也可能是一次瞬时的库读失败——所以**文案不把原因说死**：先原样给出
		// 那句错误，再把"如果是网络"作为建议提出来。凭据形态不合那种早在数据面
		// 就被归成 ErrAuthExpired 了，不会走到这里（见 gateway.RefreshAgent）。
		// codexauth/grokauth 的错误只含阶段名与 HTTP 状态/错误码，不含令牌（§15.1）。
		issuerHost := "auth.openai.com"
		switch provider {
		case store.AgentProviderClaude:
			issuerHost = "platform.claude.com"
		case store.AgentProviderGrok:
			issuerHost = "auth.x.ai"
		case store.AgentProviderCursor:
			// cursor 的自检 = 数据面重走一次 exchange，出网目标是 API 面本身。
			issuerHost = "api2.cursor.sh"
		}
		s.log.Warn("Agents 自检失败", "request_id", infoFrom(r.Context()).id,
			"provider", provider, "err", err.Error())
		writeError(w, http.StatusBadGateway, "agent_upstream_unreachable",
			"自检没有完成："+err.Error()+"。若现场限制了出网，请检查设备到 "+issuerHost+" 的连通性后重试。")
	}
}

// writeAgentLoginError 把换码失败翻成应答。
//
// 一律 4xx：这条路上每一种失败都要管理员**再做一次动作**（改一处重粘、或重新
// 发起登录），没有一种是「设备坏了」。两个机读码分开是因为下一步不同——
// OpenAI 明确拒绝了这次换码（授权码过期/已用过）只能重新登录，而其余的（state
// 对不上、粘的不是回调地址、这一次没连上 OpenAI）改一处或重试就能过。
func (s *Server) writeAgentLoginError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("Agents 登录换码失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
	var oe *codexauth.OAuthError
	if errors.As(err, &oe) {
		writeError(w, http.StatusBadRequest, "agent_login_rejected",
			err.Error()+"。授权码只能用一次、有效期也很短，请重新发起登录。")
		return
	}
	writeError(w, http.StatusBadRequest, "agent_login_failed", err.Error())
}

// writeAgentError 把 store 的哨兵错误映射为统一错误体。
func (s *Server) writeAgentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Agents 账号不存在")
		return
	}
	s.internalError(w, r, err)
}

// agentDetail 组装审计 detail。
//
// **account_id 只记末段**：审计行长期保存、给非当事人看，而完整的账号标识在
// 这里没有用途——要认账号看管理台那一列就够了（那是一次当场的读数）。
func agentDetail(a *store.AgentAccount) string {
	return fmt.Sprintf("provider=%s label=%s account=%s status=%s",
		a.Provider, a.Label, accountTail(a.AccountID), a.Status)
}

// accountTail 取 account_id 的末 8 位（不足 8 位原样给——那不可能是真实的
// 账号标识，多半是导入的占位值，遮蔽它反而让排障看不出问题在哪）。
func accountTail(id string) string {
	if id == "" {
		return "(无)"
	}
	rs := []rune(id)
	if len(rs) <= 8 {
		return id
	}
	return "…" + string(rs[len(rs)-8:])
}

// readAgentProfile 校验连接时可选带上的名称与默认模型。空 = 不设（对
// UpsertAgentAccount 而言就是「保持既有值」，重新登录不会抹掉管理员的配置）。
func (s *Server) readAgentProfile(w http.ResponseWriter, label, model string) (string, string, bool) {
	label = strings.TrimSpace(label)
	if err := validateAgentLabel(label); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_label", "名称不合法："+err.Error())
		return "", "", false
	}
	model = strings.TrimSpace(model)
	if err := validateAgentModel(model); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_default_model", "默认模型不合法："+err.Error())
		return "", "", false
	}
	return label, model, true
}

// parseAgentText 解析一个可清空的文本入参（三态，照 parseLimit 的先例）：
//
//	present=false            字段缺席，本次不改
//	present=true, value==""  显式 null 或空串，清空
//	present=true, value!=""  设成该值（首尾空白已去掉）
//
// null 与空串同义是有意的：这两项都是纯展示/配置值，"设成空字符串"与"清空"
// 在产品上不是两件事，让界面为它们纠结毫无意义（金额那边区分 0 与 null 是
// 因为零额度与不限确实是两件事）。
func parseAgentText(raw json.RawMessage, name string) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", false, fmt.Errorf("%s 须为字符串或 null（表示清空）", name)
	}
	if decoded == nil {
		return "", true, nil
	}
	str, ok := decoded.(string)
	if !ok {
		return "", false, fmt.Errorf("%s 须为字符串或 null（表示清空）", name)
	}
	return strings.TrimSpace(str), true, nil
}

// validateAgentLabel 校验订阅名称（可空；纯展示值，允许空格，禁控制字符）。
func validateAgentLabel(label string) error {
	if utf8.RuneCountInString(label) > agentLabelMaxRunes {
		return fmt.Errorf("长度须不超过 %d 个字符", agentLabelMaxRunes)
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return errors.New("不得包含控制字符")
		}
	}
	return nil
}

// validateAgentModel 校验默认模型名（可空）。
//
// **收成白名单，比模型目录那套还严**，理由不在这台设备上：这个值会被抄进
// 启动器命令，落到用户电脑的 ~/.codex/config.toml 或 ~/.grok/config.toml
// （`model = "<值>"`）里，而那条命令是一段 shell。一个引号或换行让用户的 TOML 解析失败、codex 直接
// 起不来，现象离病因十万八千里；`$` 或反引号则是往别人机器上执行的命令里塞
// 东西——这是设备唯一一处「管理员填的字符串会到另一台机器上被执行」的地方，
// 边界收在写入侧，而不是指望三处渲染各自记得转义。
//
// 白名单容得下所有见过的厂商模型 ID（gpt-5-codex、doubao-seedance-2.0、
// deepseek-chat、带命名空间的 vendor/model）。真出现装不下的那天，改这里、
// 并同时回头看启动器那段命令的引用方式。
func validateAgentModel(model string) error {
	if model == "" {
		return nil
	}
	if err := validateCatalogName(model); err != nil {
		return err
	}
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':', r == '/':
		default:
			return errors.New("只能包含字母、数字与 . - _ : /（它要写进你电脑上的 codex 配置文件）")
		}
	}
	return nil
}
