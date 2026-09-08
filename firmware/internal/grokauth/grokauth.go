// Package grokauth 是盒子作为 OAuth 客户端与 xAI（auth.x.ai）之间的全部往来：
// 把管理员的**一次**设备码授权换成一份订阅句柄（~/.grok/auth.json 里那条 OAuth
// 记录的同形物），此后由盒子**独家持有、独家刷新**它。安全论证与 codex 完全同源
// （internal/agentauth 与 docs/firmware-agents-codex.md 决策 1/5）；本包只写
// grok 侧的事实，设计取舍见 docs/firmware-agents-grok.md。
//
// # 登录形态是设备码流（RFC 8628），不是粘贴回调地址
//
// 与 OpenAI 相反，auth.x.ai **有**设备码授权，2026-08-12 三项互证：
//
//  1. 发现文档（/.well-known/openid-configuration）明列
//     device_authorization_endpoint 与 device_code 授权类型；
//  2. 官方 CLI 自带 `grok login --device-code`（开源仓库 xai-org/grok-build 的
//     auth/device_code.rs，RFC 8628 标准两段式）；
//  3. 第三方 agent（Hermes 等）就是用它接 SuperGrok / X Premium+ 订阅的。
//
// 所以这条路不需要 codex 那个「停在打不开的 localhost 页、粘贴整条 URL」的
// 反直觉步骤：管理员在任何设备的浏览器上打开 verification_uri、核对 user_code、
// 批准，回面板点「完成连接」。**盒子不做后台轮询**（零轮询铁律）——管理员的
// 每次点击就是一次令牌端点尝试，authorization_pending 原样告知「还没批准」。
// [TestGrokDeviceFlowProbe]（门控在 GROK_LOGIN_PROBE=1，缺省 skip）是这条结论
// 的复验网：哪天 xAI 撤掉设备码流，它会失败。
//
// # §15.1
//
// 本包**不持有 logger**。access/refresh token、device_code 与整份句柄不进任何
// 错误文本：错误只带阶段名、来源方（xAI）、HTTP 状态与机读错误码。[Auth] 与
// [DeviceLogin] 自遮蔽（String / GoString / LogValue / 未导出字段）。
package grokauth

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

// OAuth 客户端参数。都是官方 `grok` CLI 那个**公开桌面客户端**（public client，
// 无 client_secret）的公开标识，不是密钥物料（源码里 obfstr 混淆只是防爬，
// 二进制里原样还原）。
const (
	// ClientID 是官方 grok CLI 注册的 client_id（xai-grok-shell/src/auth/config.rs
	// 的生产缺省值）。
	ClientID = "b1a00492-073a-47ea-816f-4c329264a828"

	// Scope 取官方十项中的最小六项：grok-cli:access 是「令牌可用于 API 代理
	// 请求」的授权位（官方源码注释原话），api:access 与之同组；offline_access
	// 换 refresh_token（少了它句柄活不过一个令牌周期）；前三项换 id_token 身份
	// （账号标识的来源之一）。刻意不要 conversations:* / workspaces:*——那是
	// grok.com 会话/工作区同步的权限，盒子只代理 API（最小权限，
	// docs/firmware-agents-grok.md）。
	Scope = "openid profile email offline_access grok-cli:access api:access"
)

// DefaultIssuer 是 xAI 的账号签发方（OIDC issuer，发现文档 issuer 字段同值）。
const DefaultIssuer = "https://auth.x.ai"

const (
	deviceCodePath = "/oauth2/device/code"
	tokenPath      = "/oauth2/token"
)

// 阶段名：进错误文本的就这三个词。用「凭据」不用「令牌」，理由同 codexauth
// （用户可见文案里 令牌 是禁用词）。
const (
	phaseDeviceStart = "发起 Grok Build 订阅授权"
	phaseDevicePoll  = "换取 Grok Build 订阅凭据"
	phaseRefresh     = "刷新 Grok Build 订阅凭据"
)

// tokenRespCap 是令牌/设备码端点响应体的读取上限（理由同 codexauth：上限防的
// 是配置错到别的服务上时的无界读）。
const tokenRespCap = 256 << 10

// 分层超时，口径同 codexauth（这两条路都挂在人的动作或用户请求后面）。
const (
	dialTimeout  = 10 * time.Second
	tokenTimeout = 30 * time.Second
)

// ErrAuthExpired 即 agentauth 的同名哨兵（codex 与 grok 共用一套分类）。
var ErrAuthExpired = agentauth.ErrAuthExpired

// Client 是本包与 xAI 之间的 HTTP 出口。零值不可用，经 [NewClient] 构造；
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
// 假 xAI（测试桩），生产不该配。
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

// newHTTPClient 构造本包专用的 HTTP 客户端。两条决策照 codexauth：**出站代理
// 环境变量显式忽略**（Proxy: nil）、**不跟随重定向**（令牌端点回 3xx 就是出事
// 了，跟过去只会把凭据发到别处）。
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
// 不回**（xAI 实测有时轮换有时不轮换——官方客户端两种都处理），缺就沿用旧的，
// 见 [Auth.withRefreshed]。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// postForm 打一次 path 指定的端点（令牌端点与设备码端点同一套请求纪律）。
// 请求体 application/x-www-form-urlencoded；client_id 在这里统一补上。
//
// 错误一律不含请求内容：form 里有 refresh_token / device_code，
// 它们不进任何返回值（§15.1）。
func (c *Client) postForm(ctx context.Context, path string, form url.Values, phase string) ([]byte, error) {
	form.Set("client_id", ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%s失败：构造请求出错", phase)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// 中性 User-Agent：不冒充官方 grok CLI，只让 xAI 侧认得出这类流量的来源
	// （同 codexauth 的口径）。
	req.Header.Set("User-Agent", "llmgate-grokauth/"+buildinfo.Version)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, transportError(err, phase)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tokenRespCap))
	if err != nil {
		return nil, fmt.Errorf("%s失败：读取 xAI 应答出错", phase)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &agentauth.OAuthError{
			Phase: phase, Origin: "xAI", Status: resp.StatusCode, Code: agentauth.ErrorCode(raw),
		}
	}
	return raw, nil
}

// postToken 打一次令牌端点并解出令牌应答。
func (c *Client) postToken(ctx context.Context, form url.Values, phase string) (*tokenResponse, error) {
	raw, err := c.postForm(ctx, tokenPath, form, phase)
	if err != nil {
		return nil, err
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s失败：xAI 应答不是合法 JSON", phase)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%s失败：xAI 应答里没有 access_token", phase)
	}
	return &out, nil
}

// Now 是本客户端的时钟（[agentauth.Refresher] 的一半；测试经 c.now 注入）。
func (c *Client) Now() time.Time { return c.now() }

// RefreshHandle 实现 [agentauth.Refresher]：打 xAI 令牌端点给 grok 句柄换一代。
// 表单两件套 grant_type/refresh_token（client_id 由 postForm 统一补）——与官方
// CLI 的刷新请求同形（refresh_tokens_once；团队主体的 principal_* 两个可选参
// 不带，盒子只支持个人订阅）。
func (c *Client) RefreshHandle(ctx context.Context, cur agentauth.Handle) (agentauth.Handle, time.Duration, error) {
	auth, ok := cur.(*Auth)
	if !ok || auth == nil {
		// 不可达的防御分支：Provider 里的句柄只能来自本包的构造。
		return nil, 0, fmt.Errorf("%s失败：句柄形态与 grok 不符", phaseRefresh)
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", auth.refreshToken)
	tok, err := c.postToken(ctx, form, phaseRefresh)
	if err != nil {
		return nil, 0, err
	}
	return auth.withRefreshed(tok, c.now()), time.Duration(tok.ExpiresIn) * time.Second, nil
}

// transportError 把传输层失败翻成一句能给管理员看的话。出网到 xAI 在受限现场
// 本来就会失败，这条路的文案要说清是「连不上」而不是「凭据不对」。
func transportError(err error, phase string) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s失败：连接 xAI 超时", phase)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s失败：请求已取消", phase)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return fmt.Errorf("%s失败：连接 xAI 出错（%s）", phase, ue.Err.Error())
	}
	return fmt.Errorf("%s失败：连接 xAI 出错（%s）", phase, err.Error())
}
