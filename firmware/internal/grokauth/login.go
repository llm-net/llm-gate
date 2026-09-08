package grokauth

// 管理员登录：设备码授权（RFC 8628）。两段式，中间那步不在盒子上：
//
//	① 盒子 [Client.StartDeviceLogin]：POST /oauth2/device/code，拿到
//	   user_code / verification_uri / device_code。
//	② 管理员在**任何设备的浏览器**里打开 verification_uri（有
//	   verification_uri_complete 的话 user_code 已带上），登录 xAI 账号并批准。
//	③ 盒子 [Client.CompleteDeviceLogin]：打**一次**令牌端点换句柄。还没批准
//	   时上游答 authorization_pending，盒子原样告知——**没有轮询循环**，
//	   管理员的下一次点击就是下一次尝试（零轮询铁律在登录路上的形态）。
//
// 与 codex 的粘贴回调地址流相比少了「打不开的 localhost 页」那步反直觉交互；
// 代价是多一次「回来点完成」。安全性同源：device_code 只在盒子内存里
// （等同 codex 的 code_verifier——凭它加公开的 client_id 就能在批准后换出令牌），
// 不落库、不回显、不进日志，由管理面的会话单件 + TTL 自毁管着。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

// deviceGrantType 是 RFC 8628 的授权类型标识。
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// deviceLoginFallbackTTL 是设备码应答缺 expires_in 时的会话寿命兜底
// （RFC 要求必给；官方 CLI 的兜底也是 10 分钟，跟它走）。
const deviceLoginFallbackTTL = 10 * time.Minute

// ErrAuthorizationPending / ErrSlowDown 是设备码流的两个「还没到时候」信号，
// 都不是失败：前者是管理员还没在浏览器里批准，后者是上游要求放慢节奏。
// 管理面对两者都回「稍后再点一次」，会话保留。
var (
	ErrAuthorizationPending = errors.New("授权还没有完成")
	ErrSlowDown             = errors.New("上游要求放慢节奏")
)

// DeviceLogin 是一次在飞的设备码登录会话。**deviceCode 是密钥物料**：不落库、
// 不进日志、不进 API 响应，只在管理面进程内存里活到换出句柄或过期。
type DeviceLogin struct {
	// UserCode 是要管理员在授权页上核对/输入的短码（给人看的，可显示）。
	UserCode string
	// VerificationURI 是管理员要打开的授权页地址。
	VerificationURI string
	// VerificationURIComplete 是带上 user_code 的完整地址（上游可以不给，
	// 空则界面只展示 VerificationURI + UserCode 两样）。
	VerificationURIComplete string
	// ExpiresAt 是这次会话的绝对截止时刻（上游 expires_in 换算）。
	ExpiresAt time.Time
	// Interval 是上游建议的轮询间隔秒数（界面提示用；盒子不自动轮询）。
	Interval int
	// CreatedAt 是会话起点。
	CreatedAt time.Time

	// deviceCode 未导出且不进 String——见类型注释。
	deviceCode string
}

// Expired 报告这次登录会话是否已经超时。
func (l *DeviceLogin) Expired(now time.Time) bool {
	return now.After(l.ExpiresAt)
}

// String 自遮蔽：DeviceLogin 带 device_code，%v/%+v 都不该把它带出去（§15.1）。
// 值接收者，理由同 [Auth.String]。
func (l DeviceLogin) String() string {
	return "grokauth.DeviceLogin{user_code=" + l.UserCode + ", device_code 已遮蔽}"
}

// GoString 挡 %#v（走 fmt.GoStringer 不走 Stringer），LogValue 挡 slog——
// 与 [Auth] 同一套三件套（全局硬规则「凭据包装类型三件套」）。
func (l DeviceLogin) GoString() string { return l.String() }

// LogValue 让 slog 拿到的也是遮蔽后的值。
func (l DeviceLogin) LogValue() slog.Value { return slog.StringValue(l.String()) }

// deviceCodeResponse 是设备码端点的 2xx 应答（RFC 8628 §3.2）。
type deviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// StartDeviceLogin 起一次设备码登录：打设备码端点拿 user_code 与授权页地址。
// 这是登录路上盒子的第一次出网（受限现场会失败，错误文案说清是连不上）。
func (c *Client) StartDeviceLogin(ctx context.Context) (*DeviceLogin, error) {
	form := url.Values{}
	form.Set("scope", Scope)
	raw, err := c.postForm(ctx, deviceCodePath, form, phaseDeviceStart)
	if err != nil {
		return nil, err
	}
	var resp deviceCodeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%s失败：xAI 应答不是合法 JSON", phaseDeviceStart)
	}
	if resp.DeviceCode == "" || resp.UserCode == "" || resp.VerificationURI == "" {
		return nil, fmt.Errorf("%s失败：xAI 应答缺少设备码字段", phaseDeviceStart)
	}
	now := c.now()
	ttl := deviceLoginFallbackTTL
	if resp.ExpiresIn > 0 {
		ttl = time.Duration(resp.ExpiresIn) * time.Second
	}
	return &DeviceLogin{
		UserCode:                resp.UserCode,
		VerificationURI:         resp.VerificationURI,
		VerificationURIComplete: resp.VerificationURIComplete,
		ExpiresAt:               now.Add(ttl),
		Interval:                resp.Interval,
		CreatedAt:               now,
		deviceCode:              resp.DeviceCode,
	}, nil
}

// CompleteDeviceLogin 打一次令牌端点，尝试用已批准的设备码换出句柄。
//
// 一次就是一次：authorization_pending / slow_down 翻成本包的两个哨兵原样返回，
// 由管理面告诉管理员「稍后再点」；access_denied / expired_token 这类确定性拒绝
// （[agentauth.Deterministic]）意味着这次会话已经死了，调用方该清掉会话让
// 管理员重来。
func (c *Client) CompleteDeviceLogin(ctx context.Context, login *DeviceLogin) (*Auth, error) {
	if login == nil {
		return nil, errors.New("登录会话不存在或已过期，请重新发起连接")
	}
	if login.Expired(c.now()) {
		return nil, errors.New("登录会话已超时，请重新发起连接")
	}
	form := url.Values{}
	form.Set("grant_type", deviceGrantType)
	form.Set("device_code", login.deviceCode)
	tok, err := c.postToken(ctx, form, phaseDevicePoll)
	if err != nil {
		switch oauthCode(err) {
		case "authorization_pending":
			return nil, ErrAuthorizationPending
		case "slow_down":
			return nil, ErrSlowDown
		}
		return nil, err
	}
	if tok.RefreshToken == "" {
		// 没有 refresh_token 的句柄活不过一个令牌周期，而盒子是长期代理。
		// 这通常意味着 scope 里的 offline_access 没生效——落库前就拦下来，
		// 别造一台明天自己坏掉的设备。
		return nil, errors.New("xAI 没有返回 refresh_token（这份订阅凭据无法长期使用），请重新发起连接")
	}
	return newAuthFromTokens(tok, c.issuer, c.now()), nil
}

// oauthCode 取 err 里 OAuth 错误码（没有则空串）。
func oauthCode(err error) string {
	var oe *agentauth.OAuthError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}
