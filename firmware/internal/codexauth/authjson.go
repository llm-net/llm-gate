package codexauth

// auth.json —— 一份 Codex 订阅句柄的解析、保真回写与自遮蔽。
//
// 形态就是 codex CLI 写在 ~/.codex/auth.json 的那一份（决策 4 备选②「粘贴
// auth.json」直接吃它，PKCE 登录成功后本包也按同一形状造一份）：
//
//	{
//	  "OPENAI_API_KEY": null,
//	  "tokens": {
//	    "id_token":      "<JWT>",
//	    "access_token":  "<JWT>",
//	    "refresh_token": "<opaque>",
//	    "account_id":    "<uuid>"
//	  },
//	  "last_refresh": "2026-08-11T00:00:00Z"
//	}
//
// **保真回写**：解析时把整份原文留在 raw / rawTokens 两张表里，回写只覆盖自己
// 认识的那几个键。理由与网关透传口径同源——这份 blob 是管理员从别处粘来的，
// 也是刷新后要重新密封落库的那一份；把不认识的键（OPENAI_API_KEY、将来 codex
// 新加的字段）在一次刷新里悄悄抹掉，是设备单方面损毁用户数据。
//
// §15.1：[Auth] 的令牌字段全部未导出，只开 [Auth.AccessToken]（代理注入用）与
// [Auth.AccountID]（明文值，不是密钥物料）两个读口；String / GoString / LogValue
// 自遮蔽，于是 %v、%+v、%#v、slog 与 json.Marshal 五条路都吐不出令牌（照
// boardinfo.SIDValue「keep it that way rather than passing raw strings around」）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

// idTokenAccountClaim 是 OpenAI 塞在 id_token 里的命名空间 claim，
// 里面的 chatgpt_account_id 就是代理要注入的 ChatGPT-Account-ID。
//
// 它在 authorize 时要靠 id_token_add_organizations=true 才会带上（见 login.go），
// 所以这里取不到时**不是错误**：粘贴进来的 auth.json 若已带 tokens.account_id，
// 那条路照样能拿到值。
const idTokenAccountClaim = "https://api.openai.com/auth"

// Auth 是一份 Codex 订阅句柄。经 [ParseAuthJSON] 构造；构造后除
// [Auth.withRefreshed] 造新值外不可变（该方法返回新实例，不改旧的）。
type Auth struct {
	accessToken  string
	refreshToken string
	idToken      string
	// accountID 是 OpenAI 侧账号标识。明文值、不是密钥物料——代理靠它注入
	// ChatGPT-Account-ID，管理台靠它认账号。
	accountID   string
	lastRefresh time.Time

	// raw / rawTokens 是原文里**本包不认识的那些键**，回写时原样带回去。
	raw       map[string]json.RawMessage
	rawTokens map[string]json.RawMessage
}

// ParseAuthJSON 解析一份 auth.json。
//
// 校验刻意只做最小的一项——access_token 与 refresh_token 都在（决策 4）：
// 少了 refresh_token 的句柄活不过一小时，落库只会造出一台「连上了但明天就废」
// 的设备，这种错要在管理员还站在面板前的时候说清楚。至于令牌本身有没有效，
// 那不是形态校验答得了的问题，第一次刷新/代理会给出确定的答复。
//
// 错误文本只说缺什么、该怎么办，**不回显任何原文**（§15.1）——包括不包装
// encoding/json 的错误，那类错误会把出错位置附近的字节带出来。
func ParseAuthJSON(blob string) (*Auth, error) {
	trimmed := strings.TrimSpace(blob)
	if trimmed == "" {
		return nil, errors.New("auth.json 为空")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, errors.New("auth.json 不是合法的 JSON 对象")
	}
	tokensRaw, ok := raw["tokens"]
	if !ok {
		return nil, errors.New("auth.json 缺少 tokens 段：请粘贴 ~/.codex/auth.json 的整份内容")
	}
	var rawTokens map[string]json.RawMessage
	if err := json.Unmarshal(tokensRaw, &rawTokens); err != nil {
		return nil, errors.New("auth.json 的 tokens 段不是 JSON 对象")
	}

	a := &Auth{raw: raw, rawTokens: rawTokens}
	a.accessToken = jsonString(rawTokens["access_token"])
	a.refreshToken = jsonString(rawTokens["refresh_token"])
	a.idToken = jsonString(rawTokens["id_token"])
	if a.accessToken == "" || a.refreshToken == "" {
		return nil, errors.New("auth.json 缺少 access_token 或 refresh_token：" +
			"请在装有 codex 的机器上重新登录后再复制整份文件")
	}
	// account_id 两个来源，显式那份优先：codex 自己就是这么写的，而 id_token
	// 里的 claim 要 authorize 时带 id_token_add_organizations=true 才有。
	a.accountID = jsonString(rawTokens["account_id"])
	if a.accountID == "" {
		a.accountID = accountIDFromIDToken(a.idToken)
	}
	if ts := jsonString(raw["last_refresh"]); ts != "" {
		// 解不出来就当没有：这个值只是给人看的时间戳，不该因为格式不认识
		// 就把一份好句柄拒之门外。
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			a.lastRefresh = t
		}
	}
	return a, nil
}

// newAuthFromTokens 用一次令牌端点应答造一份全新的 auth.json 句柄
// （PKCE 登录成功后走这条；没有可保真的原文，按 codex 的规范形态造）。
func newAuthFromTokens(tok *tokenResponse, now time.Time) *Auth {
	a := &Auth{
		accessToken:  tok.AccessToken,
		refreshToken: tok.RefreshToken,
		idToken:      tok.IDToken,
		lastRefresh:  now,
		raw: map[string]json.RawMessage{
			// codex 自己写的那一份带这个键（值为 null = 没在用 API密钥 模式）。
			// 造出来的 blob 与它同形，管理员把它拷回一台装了 codex 的机器也认。
			"OPENAI_API_KEY": json.RawMessage("null"),
		},
		rawTokens: map[string]json.RawMessage{},
	}
	a.accountID = accountIDFromIDToken(a.idToken)
	return a
}

// withRefreshed 用一次刷新应答造下一代句柄（返回新实例，不改旧的——旧实例可能
// 正被别的 goroutine 读着）。
//
// refresh_token / id_token **缺就沿用旧的**：OAuth 允许刷新应答不重发它们，
// 而把 refresh_token 覆盖成空串等于把句柄丢了。account_id 同理——新 id_token
// 解得出就用新的（换账号重连时它才是对的），解不出就保留旧值。
func (a *Auth) withRefreshed(tok *tokenResponse, now time.Time) *Auth {
	next := &Auth{
		accessToken:  tok.AccessToken,
		refreshToken: firstNonEmpty(tok.RefreshToken, a.refreshToken),
		idToken:      firstNonEmpty(tok.IDToken, a.idToken),
		lastRefresh:  now,
		raw:          cloneRaw(a.raw),
		rawTokens:    cloneRaw(a.rawTokens),
	}
	next.accountID = firstNonEmpty(accountIDFromIDToken(next.idToken), a.accountID)
	return next
}

// AccessToken 是代理注入 Authorization 头要用的那一个。**只该走
// [Provider.Current]**——直接读这里拿到的可能是已经过期的一代。
func (a *Auth) AccessToken() string {
	if a == nil {
		return ""
	}
	return a.accessToken
}

// AccountID 是 OpenAI 侧账号标识（可能为空，见 [idTokenAccountClaim]）。
func (a *Auth) AccountID() string {
	if a == nil {
		return ""
	}
	return a.accountID
}

// LastRefresh 是这份句柄最近一次换代的时刻；零值 = 原文没写或解不出来。
func (a *Auth) LastRefresh() time.Time {
	if a == nil {
		return time.Time{}
	}
	return a.lastRefresh
}

// JSON 把句柄回写成一份 auth.json（落库前要经设备密钥密封，见 store.sealAgent）。
//
// 保真：原文里本包不认识的键原样带回，只覆盖 tokens 的四个键与 last_refresh。
// map 的 marshal 按键排序，所以同一份句柄的输出是确定的（测试据此比对）。
func (a *Auth) JSON() (string, error) {
	if a == nil {
		return "", errors.New("auth.json 为空")
	}
	tokens := cloneRaw(a.rawTokens)
	for key, val := range map[string]string{
		"access_token":  a.accessToken,
		"refresh_token": a.refreshToken,
		"id_token":      a.idToken,
		"account_id":    a.accountID,
	} {
		if val == "" {
			// 空值不写：留一个 "" 在文件里，比没有这个键更容易被下一个读者
			// 当成「有这个东西，只是空的」。
			delete(tokens, key)
			continue
		}
		enc, err := json.Marshal(val)
		if err != nil {
			return "", fmt.Errorf("序列化 auth.json 的 %s 失败", key)
		}
		tokens[key] = enc
	}
	tokensEnc, err := json.Marshal(tokens)
	if err != nil {
		return "", errors.New("序列化 auth.json 的 tokens 段失败")
	}

	top := cloneRaw(a.raw)
	top["tokens"] = tokensEnc
	if !a.lastRefresh.IsZero() {
		enc, err := json.Marshal(a.lastRefresh.UTC().Format(time.RFC3339))
		if err != nil {
			return "", errors.New("序列化 auth.json 的 last_refresh 失败")
		}
		top["last_refresh"] = enc
	}
	out, err := json.Marshal(top)
	if err != nil {
		return "", errors.New("序列化 auth.json 失败")
	}
	return string(out), nil
}

// String 自遮蔽（§15.1）。**值接收者**是有意的：这样 Auth 与 *Auth 两种写法、
// %v 与 %+v 两种动词都走这里，而不是只挡住其中一半。
func (a Auth) String() string {
	id := a.accountID
	if id == "" {
		id = "(未知)"
	}
	return "codexauth.Auth{account_id=" + id + ", 令牌已遮蔽}"
}

// GoString 挡 %#v：那个动词走 fmt.GoStringer 而不是 Stringer，缺了它 %#v 会按
// Go 语法连未导出的令牌字段（含 raw / rawTokens 表里的原文）一起打印（全局硬
// 规则「凭据包装类型三件套」）。
func (a Auth) GoString() string { return a.String() }

// LogValue 让 slog 拿到的也是遮蔽后的值。
func (a Auth) LogValue() slog.Value { return slog.StringValue(a.String()) }

// accountIDFromIDToken 从 id_token 的 claim 里取 chatgpt_account_id。
//
// **只解 claim 不验签**：签名验证要拉 JWKS、要处理轮换，而这里的用途不是鉴权——
// 令牌是 OpenAI 刚发给我们的，真伪由 TLS 与令牌端点保证；取错了顶多是注入一个
// 错的 ChatGPT-Account-ID，下一次代理请求就会 401。解析失败一律返回空串
// （不 panic、不报错），空 account_id 是允许的状态。
func accountIDFromIDToken(idToken string) string {
	payload, ok := jwtPayload(idToken)
	if !ok {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		// 顶层写法作为兜底：命名空间 claim 是当前形态，但这类厂商 claim 换过位置
		// 的先例不少，多认一处的成本是两行。
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return firstNonEmpty(claims.Auth.ChatGPTAccountID, claims.ChatGPTAccountID)
}

// Expiry 实现 [agentauth.Handle]：access_token 自述的到期时刻（exp claim；
// 解不出返回零值 = 未知，Provider 按有效处理）。
func (a *Auth) Expiry() time.Time {
	if a == nil {
		return time.Time{}
	}
	return jwtExpiry(a.accessToken)
}

// jwtExpiry / jwtPayload 已上收 agentauth（grok 同用），这里留委托，
// 本包既有拼写零改动。不验签的理由见 [agentauth.JWTPayload]。
func jwtExpiry(token string) time.Time { return agentauth.JWTExpiry(token) }

func jwtPayload(token string) ([]byte, bool) { return agentauth.JWTPayload(token) }

// jsonString 把一个 JSON 值当字符串取出；不是字符串（含 null、缺席）返回空串。
func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// cloneRaw 复制一张原文表（nil 也返回可写的空表）。复制而不是共享：
// [Auth.withRefreshed] 造出的新一代不该与旧一代共用底层 map。
func cloneRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// firstNonEmpty 返回第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
