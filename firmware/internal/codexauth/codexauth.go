// Package codexauth 是盒子作为 OAuth 客户端与 OpenAI（auth.openai.com）之间的全部
// 往来：把管理员的**一次**浏览器登录换成一份 auth.json 句柄，此后由盒子**独家持有、
// 独家刷新**它（迭代 11 决策 1/4/5）。
//
// 为什么盒子自己当 OAuth 客户端：板上是单个静态二进制，**不装 codex CLI**——
// appliance 不该带一个能 --dangerously-bypass-approvals-and-sandbox 的 CLI。
//
// # 登录形态是「授权码 + PKCE + 粘贴回调地址」，不是设备码流
//
// 设备码流在 auth.openai.com 上**根本不存在**，2026-08-11 实测三项互证：
//
//  1. 发现文档 grant_types_supported = [authorization_code, refresh_token]，
//     且没有 device_authorization_endpoint；
//  2. 令牌端点对 device_code 参数答 400 `Unknown parameter: 'device_code'`；
//  3. /oauth/device/code 等五条候选路径全部落到 Cloudflare 质询页（= 路由不存在），
//     而同样条件下真实存在的 /oauth/token 答的是 JSON。
//
// 这条结论回填迭代 11 决策 4：备选①（PKCE 粘贴回调地址）升为**主路径**，
// 决策 4 的备选②（粘贴 auth.json）仍在，作为浏览器那半段走不通时的兜底。
// [TestCodexLoginProbe]（门控在 CODEX_LOGIN_PROBE=1，缺省 skip——全仓测试必须
// 离线可跑）是这条结论的复验网：哪天 OpenAI 加回设备码流，它会失败。
//
// # 分工：authorize 归浏览器，token 归盒子
//
// **authorize URL 由盒子拼、由管理员的浏览器打开**——那个端点对非浏览器客户端
// 一律 Cloudflare 质询（实测），盒子本来也不该去打它（它要的是人做登录与授权）。
// **盒子只打令牌端点**（POST /oauth/token），实测对普通 HTTP 客户端正常答 JSON，
// 不看 User-Agent。
//
// # §15.1
//
// 本包**不持有 logger**——没有出口就漏不出去，这是最强的那种保证。access/refresh/
// id token 与整份 auth.json 不进任何错误文本：错误只带阶段名、HTTP 状态与机读错误码
// （token_expired 这类固定枚举，是协议词汇不是数据）。[Auth] 自身也自遮蔽（String /
// GoString / LogValue / 未导出字段），照 boardinfo.SIDValue 先例。
package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
)

// OAuth 客户端参数。三项都是 codex CLI 那个**公开客户端**（public client，无
// client_secret）的公开标识，不是密钥物料。
const (
	// ClientID 是 codex CLI 注册的 client_id。
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// RedirectURI 是该 client_id 注册的回调地址，**必须与 authorize 时逐字节一致**
	// （OAuth 规定换码时再报一次做校验）。地址指向 localhost:1455——盒子上没有
	// 任何东西监听它，管理员的浏览器会停在一个打不开的页面上，地址栏里那条整串
	// URL 就是要粘回面板的东西（决策 4 备选①）。
	RedirectURI = "http://localhost:1455/auth/callback"

	// Scope 里 offline_access 是拿到 refresh_token 的前提；没有它，句柄一小时就废。
	Scope = "openid profile email offline_access"
)

// DefaultIssuer 是 OpenAI 的账号签发方。发现文档把令牌端点写成
// /api/accounts/oauth/token，而 codex CLI 用的是 /oauth/token——两条实测都在、
// 答复逐字节相同（互为别名）。这里跟 codex 走短的那条。
const DefaultIssuer = "https://auth.openai.com"

const (
	authorizePath = "/oauth/authorize"
	tokenPath     = "/oauth/token"
)

// 阶段名：进错误文本的就这两个词，用来区分「登录换码」与「后台刷新」两条路
// 各自失败在哪一步。
//
// 用「凭据」不用「令牌」：这两个词会经管理面 API 的错误体到达管理员眼前，
// 而**用户可见文案里 令牌 是禁用词**（AGENTS.md 术语表：SOC AGENT 是产品名，
// 不是待翻译的词）。代码注释、日志、本文档属工程语域，照旧说令牌。
const (
	phaseExchange = "换取 Codex 订阅凭据"
	phaseRefresh  = "刷新 Codex 订阅凭据"
)

// tokenRespCap 是令牌端点响应体的读取上限。应答是几个 JWT，几十 KiB 顶天，
// 256 KiB 已是十倍余量——上限防的是配置错到别的服务上时的无界读。
const tokenRespCap = 256 << 10

// 分层超时。登录换码与刷新都是一来一回的小请求，不该为一个卡住的连接留十分钟
// 两条路都挂在人的动作或用户请求后面，因此使用短超时。
const (
	dialTimeout  = 10 * time.Second
	tokenTimeout = 30 * time.Second
)

// ErrAuthExpired 是「这份订阅句柄已经回不来了，要管理员重新登录」。
//
// 它只由**确定性拒绝**（[OAuthError.Deterministic]）产生：刷新被上游明确回绝，
// 再试多少次都是同一个答复。网络错不是它——那类失败保留现有令牌、下次再试
// （决策 5 的两类失败两种反应）。
//
// 消费方：Phase 3 的 /v1/responses 据此回机读原因 agent_auth_expired，
// store 那侧的持久状态是 AgentStatusAuthExpired。**与 agentauth 是同一个哨兵**
// （grok 那条路也用它），errors.Is 两个名字都成立。
var ErrAuthExpired = agentauth.ErrAuthExpired

// OAuthError / Deterministic 已上收 agentauth（grok 同用一套判据），这里留
// 别名与委托，既有调用方与测试零改动。本包构造它时恒带 Origin="OpenAI"。
type OAuthError = agentauth.OAuthError

// Deterministic 报告 err 是不是确定性拒绝（委托 agentauth，判据见那里）。
func Deterministic(err error) bool { return agentauth.Deterministic(err) }

// Client 是本包与 OpenAI 之间的 HTTP 出口。零值不可用，经 [NewClient] 构造；
// 构造后字段只读，可并发使用。
type Client struct {
	issuer string
	hc     *http.Client
	now    func() time.Time
}

// Option 调整 Client 装配。
type Option func(*Client)

// WithEgress 接入出站策略（internal/egress，agent_auth 分类）：OAuth 登录、换码与刷新
// 按设备级出口选路。nil 恒直连。
func WithEgress(r egress.Router) Option {
	return func(c *Client) { c.hc = newHTTPClient(r) }
}

// NewClient 构造客户端。issuer 空串取 [DefaultIssuer]；非空只用于开发期指向
// 假 OpenAI（测试桩），生产不该配。
func NewClient(issuer string, opts ...Option) *Client {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	c := &Client{
		issuer: strings.TrimRight(issuer, "/"),
		hc:     newHTTPClient(nil),
		now:    time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// newHTTPClient 构造本包专用的 HTTP 客户端。两条决策照 upstream.NewHTTPClient
// **出站代理环境变量显式忽略**（Proxy: nil，代理
// 迭代之前不认 HTTP_PROXY）、**不跟随重定向**（令牌端点回 3xx 就是出事了，
// 跟过去只会把凭据发到别处）。
func newHTTPClient(router egress.Router) *http.Client {
	base := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: tokenTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Timeout: tokenTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: egress.TransportFor(router, egress.ScopeAgentAuth, base),
	}
}

// tokenResponse 是令牌端点的 2xx 应答。刷新时 refresh_token / id_token **可能
// 不回**（OAuth 允许），缺就沿用旧的——见 [Auth.withRefreshed]。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// postToken 打一次令牌端点。请求体是 application/x-www-form-urlencoded
// （RFC 6749 的形态；实测该端点 JSON 与表单两种都收，答复一致，这里跟规范走）。
//
// 错误一律不含请求内容：form 里有 refresh_token / code / code_verifier，
// 它们不进任何返回值（§15.1）。
func (c *Client) postToken(ctx context.Context, form url.Values, phase string) (*tokenResponse, error) {
	form.Set("client_id", ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+tokenPath,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%s失败：构造请求出错", phase)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// 中性 User-Agent：不冒充 codex CLI。实测该端点不看 UA（Go 默认 UA 也照答），
	// 设它只是为了让 OpenAI 侧能认出这类流量的来源。
	req.Header.Set("User-Agent", "llmgate-codexauth/"+buildinfo.Version)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(err, phase)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tokenRespCap))
	if err != nil {
		return nil, fmt.Errorf("%s失败：读取 OpenAI 应答出错", phase)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &OAuthError{Phase: phase, Origin: "OpenAI", Status: resp.StatusCode, Code: parseOAuthError(raw)}
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s失败：OpenAI 应答不是合法 JSON", phase)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%s失败：OpenAI 应答里没有 access_token", phase)
	}
	return &out, nil
}

// parseOAuthError / safeCode 已上收 agentauth（[agentauth.ErrorCode] /
// [agentauth.SafeCode]，扁平与嵌套两种形态的理由也写在那里），这里留委托，
// 本包与测试的既有拼写零改动。
func parseOAuthError(raw []byte) string { return agentauth.ErrorCode(raw) }

func safeCode(s string) string { return agentauth.SafeCode(s) }

// Now 是本客户端的时钟（[agentauth.Refresher] 的一半；测试经 c.now 注入）。
func (c *Client) Now() time.Time { return c.now() }

// RefreshHandle 实现 [agentauth.Refresher]：打 OpenAI 令牌端点给 codex 句柄
// 换一代。表单三件套 grant_type/refresh_token/scope（client_id 由 postToken
// 统一补），与 codex CLI 的刷新请求同形。
func (c *Client) RefreshHandle(ctx context.Context, cur agentauth.Handle) (agentauth.Handle, time.Duration, error) {
	auth, ok := cur.(*Auth)
	if !ok || auth == nil {
		// 不可达的防御分支：Provider 里的句柄只能来自本包的构造。
		return nil, 0, fmt.Errorf("%s失败：句柄形态与 codex 不符", phaseRefresh)
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", auth.refreshToken)
	form.Set("scope", Scope)
	tok, err := c.postToken(ctx, form, phaseRefresh)
	if err != nil {
		return nil, 0, err
	}
	return auth.withRefreshed(tok, c.now()), time.Duration(tok.ExpiresIn) * time.Second, nil
}

// transportError 把传输层失败翻成一句能给管理员看的话。出网到 OpenAI 在受限
// 现场本来就会失败（决策 9），这条路的文案要说清是「连不上」而不是「凭据不对」。
func transportError(err error, phase string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s失败：连接 OpenAI 超时", phase)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s失败：请求已取消", phase)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return fmt.Errorf("%s失败：连接 OpenAI 出错（%s）", phase, ue.Err.Error())
	}
	return fmt.Errorf("%s失败：连接 OpenAI 出错（%s）", phase, err.Error())
}
