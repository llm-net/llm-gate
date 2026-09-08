package grokauth

// grok 订阅句柄 —— ~/.grok/auth.json 那条 OAuth 记录的解析、保真回写与自遮蔽。
//
// 官方 auth.json 是一张 **scope → 记录**的表（多种登录方式并存）：
//
//	{
//	  "https://auth.x.ai::<client_id>": {          ← OAuth 登录（要的就是它）
//	    "key":           "<access token，字段名就叫 key>",
//	    "refresh_token": "<opaque>",
//	    "expires_at":    "2026-08-12T03:04:05Z",
//	    "auth_mode":     "oidc",
//	    "user_id":       "…", "email": "…",
//	    "oidc_issuer":   "https://auth.x.ai",
//	    "oidc_client_id": "<client_id>", …
//	  },
//	  "xai::api_key": { "key": "xai-…", "auth_mode": "api_key", … }   ← 不收
//	}
//
// 粘贴导入吃整份文件但**只收 OAuth 那一条记录**：`xai::api_key` 是用户自己的
// 按量 API密钥，与订阅无关，顺手收进来就是多存一份不需要的凭据。记录内本包
// 不认识的键照 codexauth 的保真纪律原样带回（回写只覆盖 key / refresh_token /
// expires_at / create_time 四个受管键）。
//
// §15.1：[Auth] 的令牌字段全部未导出，只开 [Auth.AccessToken]（代理注入用）与
// [Auth.Account]（明文账号标识，不是密钥物料）两个读口；String / GoString /
// LogValue 自遮蔽，%v、%+v、%#v、slog 与 json.Marshal 五条路都吐不出令牌。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

// 两个特殊的记录键（官方 CLI 的既有拼写）。
const (
	// legacyScopeKey 是旧版 relay 登录的记录键（官方代码里的 LEGACY_SCOPE，
	// devbox 迁移遗留）。它也是一条 OAuth 记录，照收。
	legacyScopeKey = "https://accounts.x.ai/sign-in"
	// apiKeyScopeKey 是纯 API密钥 登录的记录键。**永远不收**（见文件头）。
	apiKeyScopeKey = "xai::api_key"
)

// Auth 是一份 grok 订阅句柄（auth.json 里那条 OAuth 记录 + 它的记录键）。
// 经 [ParseAuthJSON] 或登录构造；构造后除 [Auth.withRefreshed] 造新值外不可变。
type Auth struct {
	// scopeKey 是这条记录在 auth.json 表里的键（回写时原样用它）。
	scopeKey     string
	accessToken  string
	refreshToken string
	// account 是 xAI 侧账号标识：email 优先（人认得出），退 user_id，登录路径
	// 还能退 id_token 的 email/sub claim。明文值、不是密钥物料——管理台靠它
	// 认「连的是哪个账号」（对位 codex 的 account_id）。
	account string
	// expiresAt 是记录里持久化的 access token 到期时刻（expires_at）；
	// 零值 = 未知。
	expiresAt time.Time
	// lastRefresh 是这份句柄最近一次换代/建立的时刻（create_time）。
	lastRefresh time.Time

	// raw 是记录原文里**本包不认识的那些键**（含 auth_mode / user_id / email /
	// oidc_* 等只读键），回写时原样带回去。
	raw map[string]json.RawMessage
}

// ParseAuthJSON 解析一份 ~/.grok/auth.json（或从中单拎出来的一条 OAuth 记录）。
//
// 校验刻意只做最小的一项——access token（key）与 refresh_token 都在：少了
// refresh_token 的句柄活不过一个令牌周期，落库只会造出一台「连上了但明天就废」
// 的设备。令牌本身有没有效，第一次刷新/代理会给出确定的答复。
//
// 错误文本只说缺什么、该怎么办，**不回显任何原文**（§15.1）——包括不包装
// encoding/json 的错误（那类错误会把出错位置附近的字节带出来）。
func ParseAuthJSON(blob string) (*Auth, error) {
	trimmed := strings.TrimSpace(blob)
	if trimmed == "" {
		return nil, errors.New("auth.json 为空")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil {
		return nil, errors.New("auth.json 不是合法的 JSON 对象")
	}
	// 宽容一种常见的人工形态：直接粘了记录本身（顶层就有 key/refresh_token）。
	if _, hasKey := top["key"]; hasKey {
		return parseEntry(scopeKeyFor(DefaultIssuer), top)
	}

	// 表形态：挑 OAuth 记录。键形如 "<issuer>::<client_id>"（首选 auth.x.ai 的
	// 那条），旧版 relay 键其次；xai::api_key 与 auth_mode=api_key 的记录一概
	// 不是候选。挑选必须**确定性**：map 迭代序随机，同一份文件两次解析绝不能
	// 连出两个账号——官方 CLI 自己的规范键最优，其余同档按键字典序裁决平手。
	if raw, ok := top[scopeKeyFor(DefaultIssuer)]; ok {
		return parseEntry(scopeKeyFor(DefaultIssuer), mustEntry(raw))
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		if k != apiKeyScopeKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var fallback string
	for _, k := range keys {
		if strings.Contains(k, "::") && strings.HasPrefix(k, DefaultIssuer) {
			return parseEntry(k, mustEntry(top[k]))
		}
		// 记住一条备选（stub issuer 的表、或旧版 relay 键），扫完没有正主再用；
		// "<issuer>::<client_id>" 形的键比旧版键新，优先。
		switch {
		case strings.Contains(k, "::"):
			if fallback == "" || fallback == legacyScopeKey {
				fallback = k
			}
		case k == legacyScopeKey:
			if fallback == "" {
				fallback = k
			}
		}
	}
	if fallback != "" {
		return parseEntry(fallback, mustEntry(top[fallback]))
	}
	if _, onlyAPIKey := top[apiKeyScopeKey]; onlyAPIKey {
		return nil, errors.New("这份 auth.json 里只有 API密钥 登录，没有订阅登录：" +
			"请先在装有 grok 的机器上用浏览器或设备码方式 grok login，再复制整份文件")
	}
	return nil, errors.New("auth.json 里没有找到订阅登录记录：请粘贴 ~/.grok/auth.json 的整份内容")
}

// mustEntry 把一条记录的原文解成表；解不开返回 nil（parseEntry 会给出统一的
// 形态错误）。
func mustEntry(raw json.RawMessage) map[string]json.RawMessage {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return nil
	}
	return entry
}

// parseEntry 从一条记录构造句柄。
func parseEntry(scopeKey string, entry map[string]json.RawMessage) (*Auth, error) {
	if entry == nil {
		return nil, errors.New("auth.json 的订阅登录记录不是 JSON 对象")
	}
	if jsonString(entry["auth_mode"]) == "api_key" {
		return nil, errors.New("这条 auth.json 记录是 API密钥 登录，不是订阅登录：" +
			"请先在装有 grok 的机器上用浏览器或设备码方式 grok login，再复制整份文件")
	}
	a := &Auth{scopeKey: scopeKey, raw: entry}
	a.accessToken = jsonString(entry["key"])
	a.refreshToken = jsonString(entry["refresh_token"])
	if a.accessToken == "" || a.refreshToken == "" {
		return nil, errors.New("auth.json 缺少 key（access token）或 refresh_token：" +
			"请在装有 grok 的机器上重新登录后再复制整份文件")
	}
	a.account = firstNonEmpty(jsonString(entry["email"]), jsonString(entry["user_id"]))
	if ts := jsonString(entry["expires_at"]); ts != "" {
		// 解不出来就当没有：到期时刻未知按有效处理（agentauth 的口径），
		// 不该因为时间格式不认识就把一份好句柄拒之门外。
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			a.expiresAt = t
		}
	}
	if ts := jsonString(entry["create_time"]); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			a.lastRefresh = t
		}
	}
	return a, nil
}

// scopeKeyFor 拼 auth.json 表里 OAuth 记录的规范键（官方 base_auth_scope 同形：
// issuer 去尾斜杠 + "::" + client_id）。
func scopeKeyFor(issuer string) string {
	return strings.TrimRight(issuer, "/") + "::" + ClientID
}

// newAuthFromTokens 用一次令牌端点应答造一份全新的句柄（设备码登录成功后走
// 这条；没有可保真的原文，按官方记录的规范形态造——管理员把它拷回一台装了
// grok 的机器也认）。
func newAuthFromTokens(tok *tokenResponse, issuer string, now time.Time) *Auth {
	sub, email := idTokenIdentity(tok.IDToken)
	raw := map[string]json.RawMessage{
		"auth_mode":      mustJSON("oidc"),
		"oidc_issuer":    mustJSON(strings.TrimRight(issuer, "/")),
		"oidc_client_id": mustJSON(ClientID),
	}
	if sub != "" {
		raw["user_id"] = mustJSON(sub)
	}
	if email != "" {
		raw["email"] = mustJSON(email)
	}
	a := &Auth{
		scopeKey:     scopeKeyFor(issuer),
		accessToken:  tok.AccessToken,
		refreshToken: tok.RefreshToken,
		account:      firstNonEmpty(email, sub),
		lastRefresh:  now,
		raw:          raw,
	}
	if tok.ExpiresIn > 0 {
		a.expiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return a
}

// withRefreshed 用一次刷新应答造下一代句柄（返回新实例，不改旧的——旧实例
// 可能正被别的 goroutine 读着）。
//
// refresh_token **缺就沿用旧的**：xAI 的刷新应答有时轮换有时不轮换（官方客户端
// 两种都处理），把 refresh_token 覆盖成空串等于把句柄丢了。账号身份不动——
// 刷新不换账号，换账号走重新登录。
func (a *Auth) withRefreshed(tok *tokenResponse, now time.Time) *Auth {
	next := &Auth{
		scopeKey:     a.scopeKey,
		accessToken:  tok.AccessToken,
		refreshToken: firstNonEmpty(tok.RefreshToken, a.refreshToken),
		account:      a.account,
		lastRefresh:  now,
		raw:          cloneRaw(a.raw),
	}
	if tok.ExpiresIn > 0 {
		next.expiresAt = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return next
}

// AccessToken 是代理注入 Authorization 头要用的那一个。**只该走
// [agentauth.Provider.Current]**——直接读这里拿到的可能是已经过期的一代。
func (a *Auth) AccessToken() string {
	if a == nil {
		return ""
	}
	return a.accessToken
}

// Account 是 xAI 侧账号标识（可能为空；email 优先，退 user_id）。
func (a *Auth) Account() string {
	if a == nil {
		return ""
	}
	return a.account
}

// LastRefresh 是这份句柄最近一次换代的时刻；零值 = 原文没写或解不出来。
func (a *Auth) LastRefresh() time.Time {
	if a == nil {
		return time.Time{}
	}
	return a.lastRefresh
}

// Expiry 实现 [agentauth.Handle]：记录里持久化的 expires_at 优先（那是签发方
// 上次就这份令牌给出的权威值），没有再看 access token 自己的 exp claim；
// 都没有返回零值 = 未知，Provider 按有效处理。
func (a *Auth) Expiry() time.Time {
	if a == nil {
		return time.Time{}
	}
	if !a.expiresAt.IsZero() {
		return a.expiresAt
	}
	return agentauth.JWTExpiry(a.accessToken)
}

// JSON 把句柄回写成一份单记录的 auth.json 表（落库前经设备密钥密封）。
//
// 保真：记录里本包不认识的键原样带回，只覆盖 key / refresh_token / expires_at /
// create_time 四个受管键。map 的 marshal 按键排序，同一份句柄输出确定。
func (a *Auth) JSON() (string, error) {
	if a == nil {
		return "", errors.New("auth.json 为空")
	}
	entry := cloneRaw(a.raw)
	for key, val := range map[string]string{
		"key":           a.accessToken,
		"refresh_token": a.refreshToken,
	} {
		enc, err := json.Marshal(val)
		if err != nil {
			return "", fmt.Errorf("序列化 auth.json 的 %s 失败", key)
		}
		entry[key] = enc
	}
	// 两个时间戳：空值不写（留一个 "" 比没有这个键更误导，同 codexauth 口径）。
	if a.expiresAt.IsZero() {
		delete(entry, "expires_at")
	} else {
		entry["expires_at"] = mustJSON(a.expiresAt.UTC().Format(time.RFC3339))
	}
	if a.lastRefresh.IsZero() {
		delete(entry, "create_time")
	} else {
		entry["create_time"] = mustJSON(a.lastRefresh.UTC().Format(time.RFC3339))
	}
	entryEnc, err := json.Marshal(entry)
	if err != nil {
		return "", errors.New("序列化 auth.json 的订阅记录失败")
	}
	out, err := json.Marshal(map[string]json.RawMessage{a.scopeKey: entryEnc})
	if err != nil {
		return "", errors.New("序列化 auth.json 失败")
	}
	return string(out), nil
}

// String 自遮蔽（§15.1）。值接收者是有意的：Auth 与 *Auth、%v 与 %+v 四条路
// 都走这里。
func (a Auth) String() string {
	id := a.account
	if id == "" {
		id = "(未知)"
	}
	return "grokauth.Auth{account=" + id + ", 令牌已遮蔽}"
}

// GoString 挡 %#v：那个动词走 fmt.GoStringer 而不是 Stringer，缺了它 %#v 会按
// Go 语法连未导出的令牌字段（含 raw 表里的记录原文）一起打印（全局硬规则
// 「凭据包装类型三件套」）。
func (a Auth) GoString() string { return a.String() }

// LogValue 让 slog 拿到的也是遮蔽后的值。
func (a Auth) LogValue() slog.Value { return slog.StringValue(a.String()) }

// idTokenIdentity 从 id_token 的标准 claim 里取 sub 与 email。
//
// **只解 claim 不验签**：令牌是 xAI 刚发给我们的，真伪由 TLS 与令牌端点保证；
// 这里的用途是给管理台一个「连的是哪个账号」的读数，取错的代价是显示错，
// 不是鉴权失守。解析失败一律返回空串（空账号标识是允许的状态）。
func idTokenIdentity(idToken string) (sub, email string) {
	payload, ok := agentauth.JWTPayload(idToken)
	if !ok {
		return "", ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Sub, claims.Email
}

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

// mustJSON 序列化一个必然可序列化的字符串（string 的 Marshal 不会失败）。
func mustJSON(s string) json.RawMessage {
	enc, _ := json.Marshal(s)
	return enc
}

// cloneRaw 复制一张原文表（nil 也返回可写的空表）：新一代不该与旧一代共用
// 底层 map。
func cloneRaw(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+2)
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
