// Package agentauth 是 Agent 订阅代理（Codex、Grok Build）共用的**轮换句柄**核心：
// 单飞刷新的 [Provider] 状态机、确定性拒绝的错误分类（[OAuthError] /
// [Deterministic]）、JWT 自述读取。各 provider 的差异（端点、client_id、句柄
// 形态、登录流）留在 internal/codexauth 与 internal/grokauth，它们各自实现
// [Handle] 与 [Refresher] 接进来。
//
// 为什么共用：Provider 状态机里的每一条（single-flight、OnRotate 立刻落库、
// Invalidate 按世代去重、Reset 同代空操作、minTokenValidity 总闸、失败冷却）
// 都咬过人（见 codexauth 的迭代 11 史），第二份手抄迟早漂移。codexauth 的
// 回归测试网原地保留——它们经 codexauth 的适配层测的就是本包的状态机。
//
// # §15.1
//
// 本包不持有 logger——没有出口就漏不出去。access/refresh token 与整份句柄不进
// 任何错误文本：错误只带阶段名、来源方、HTTP 状态与机读错误码。
package agentauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrAuthExpired 是「这份订阅句柄已经回不来了，要管理员重新登录」。
//
// 它只由**确定性拒绝**（[OAuthError.Deterministic]）产生：刷新被上游明确回绝，
// 再试多少次都是同一个答复。网络错不是它——那类失败保留现有令牌、下次再试。
//
// 文案不带 provider 名：这句话经管理面错误体面向管理员，说话的页面/日志行
// 自会指名是哪份订阅（用「凭据」不用「令牌」的理由见 codexauth 包注释）。
var ErrAuthExpired = errors.New("订阅登录已失效，需要管理员重新登录")

// Handle 是一份可轮换的订阅句柄（某 provider 的整份 auth.json 语义）。
// 实现方的值不可变：换代经 [Refresher.RefreshHandle] 返回新实例。
type Handle interface {
	// AccessToken 是代理注入 Authorization 头要用的那一个。
	AccessToken() string
	// JSON 把句柄回写成落库形态（OnRotate 经它拿到要密封的明文）。
	JSON() (string, error)
	// Expiry 是 access_token **自述**的到期时刻（未减提前量；解不出返回零值
	// = 未知）。签发方就本次发放给的 expires_in 权威值走 RefreshHandle 的返回。
	Expiry() time.Time
}

// Refresher 打某 provider 的令牌端点换下一代句柄。
type Refresher interface {
	// RefreshHandle 用 cur 的 refresh token 换一代新句柄。expiresIn 是应答的
	// expires_in（0 = 应答没给，[Provider] 退回 next.Expiry()）。错误经
	// [Deterministic] 分类；cur 与错误文本都不得含令牌明文（§15.1）。
	RefreshHandle(ctx context.Context, cur Handle) (next Handle, expiresIn time.Duration, err error)
	// Now 是本 provider 的时钟（测试注入用；生产恒 time.Now）。
	Now() time.Time
}

// OAuthError 是「签发方答复了，但不是 2xx」。
//
// **不带 error_description**：那是厂商英文长文案，进错误串只会把日志撑长，
// 而 Code 已经足够定位（§15.1 的「错误只含 HTTP 状态与阶段名」按这个尺度落实）。
type OAuthError struct {
	// Phase 是失败发生在哪一步（各 provider 自己的「换取/刷新 XX 订阅凭据」）。
	Phase string
	// Origin 是答复方的名字（"OpenAI" / "xAI"），进错误文案。空串渲染成「上游」。
	Origin string
	// Status 是 HTTP 状态码。
	Status int
	// Code 是机读错误码（RFC 6749 的扁平 {"error":"…"} 或 OpenAI 的嵌套
	// {"error":{"code":…}} 两种形态都收，见 [ErrorCode]）。取不到时为空。
	Code string
}

func (e *OAuthError) Error() string {
	origin := e.Origin
	if origin == "" {
		origin = "上游"
	}
	if e.Code == "" {
		return fmt.Sprintf("%s失败：%s 返回 HTTP %d", e.Phase, origin, e.Status)
	}
	return fmt.Sprintf("%s失败：%s 返回 HTTP %d（%s）", e.Phase, origin, e.Status, e.Code)
}

// Deterministic 报告这次失败是不是**确定性拒绝**——再试一次也是同一个答复，
// 该停下来等管理员重新登录。
//
// 两个条件都要满足：
//
//   - **4xx，且不是 408/429**。判据是状态码而不是具体错误码：错误码词汇由厂商
//     定、会变（OpenAI 实测过期 refresh token 回的是 401 + token_expired，既不是
//     RFC 的 400 也不是 invalid_grant），而「4xx = 你这份凭据/请求不对，5xx =
//     我这边有事」是 HTTP 自己的语义。408 与 429 说的都是「现在不行，等会儿再来」。
//   - **对方确实说了 OAuth**（Code 非空）。这条挡的是**根本不是签发方给的 4xx**：
//     CDN 质询页、配错的 issuer、中间反代都自己造 4xx，它们一个都不是「你的凭据
//     不行」的裁决，而落闩的代价是逼管理员重新走一遍登录——方向必须偏向「宁可
//     多试几次」。代价对称地写明：万一签发方哪天用一个没有错误码的 4xx 表达凭据
//     失效，会一直重试而不是停手，那由刷新失败冷却（provider.go）兜住。
func (e *OAuthError) Deterministic() bool {
	if e.Code == "" {
		return false
	}
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return e.Status >= 400 && e.Status < 500
}

// Deterministic 报告 err 是不是确定性拒绝。非 [OAuthError]（网络错、超时、
// 应答形态不符）一律为假——**不确定就当可重试**，这个方向是安全的：多试一次
// 只是多一个请求，而把一次网络抖动判成「登录失效」会让管理员白重新登录一遍。
func Deterministic(err error) bool {
	var oe *OAuthError
	return errors.As(err, &oe) && oe.Deterministic()
}

// ErrorCode 从错误应答里取机读错误码，两种形态都认：
//
//	{"error":"invalid_grant", …}                     RFC 6749 的扁平形（xAI 用这形）
//	{"error":{"code":"token_expired","type":…}, …}   OpenAI 实际在用的嵌套形
//
// 两个都试是必须的：只按 RFC 解，OpenAI 那条 401 的原因就会静默丢成空串，
// 排障时只剩一个光秃秃的状态码。取不到返回空串（调用方按无码处理）。
func ErrorCode(raw []byte) string {
	var envl struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envl) != nil || len(envl.Error) == 0 {
		return ""
	}
	var flat string
	if json.Unmarshal(envl.Error, &flat) == nil {
		return SafeCode(flat)
	}
	var nested struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(envl.Error, &nested) != nil {
		return ""
	}
	if nested.Code != "" {
		return SafeCode(nested.Code)
	}
	return SafeCode(nested.Type)
}

// SafeCode 把厂商给的错误码收窄成「只可能是协议词汇」的形状：ASCII 字母数字与
// _-. 之外一律丢弃，最长 64 字节。错误码本不是数据，但它来自网络，
// 让它有机会往日志里塞控制字符或整段文本是没必要的风险。
func SafeCode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 64; i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '_', ch == '-', ch == '.':
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// JWTExpiry 取 JWT 的 exp claim（unix 秒）。解不出来返回零值——调用方据此
// 按「到期时刻未知」处理（见 [Provider]）。
func JWTExpiry(token string) time.Time {
	payload, ok := JWTPayload(token)
	if !ok {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0).UTC()
}

// JWTPayload 解出 JWT 的载荷段（第二段，base64url 无填充）。不验签、不看头部
// 算法——本包与两个 provider 包对 JWT 的全部用途都是「读厂商刚发给我们的那份
// 自述」，取错的代价是一次可纠正的 401，不是鉴权失守。
func JWTPayload(token string) ([]byte, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil, false
	}
	// 规范是 RawURLEncoding（无填充），但带填充的实现也见得到，两种都试。
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	return payload, true
}
