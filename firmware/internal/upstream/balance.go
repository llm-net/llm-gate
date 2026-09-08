package upstream

// balance.go 是特化平台的余额查询（2026-08-09）：对上游平台的账户余额 API
// 发一次只读请求，把厂商形态归一为 [BalanceResult]。这是「特定平台更特化」
// 的首个平台能力——通用 openai_compat 只知道转发形状，恒不在支持表里；
// 端点按 type 内置（balanceEndpoints），后续平台各自补条目与解析函数。
//
// 纪律（照探测面 §15.1 口径，金额从严）：余额是客户上游账户的财务读数，
// **只进管理 API 响应**——金额与失败摘要都不落日志、不进审计 detail（审计只
// 记状态码）。凭证只经 Account.Authorize 注入请求头；金额字符串原样透传，
// 设备不做任何数值换算（换算即引入口径，余额不是本设备的账）。
//
// 已核对的平台事实：DeepSeek `GET https://api.deepseek.com/user/balance`
// （Bearer 鉴权）返回 {"is_available":bool,"balance_infos":[{"currency",
// "total_balance","granted_balance","topped_up_balance"}]}，金额是十进制
// 字符串。方舟按量/套餐的余额在火山引擎账号层，须 AK/SK 签名（Ark API密钥
// 够不着）；阿里云百炼 Token Plan 同理——套餐余量在阿里云账号/控制台层，
// 百炼 API密钥 够不着；MiniMax 无公开的 Key 鉴权余额端点——三家都不是漏配，
// 是配不了，别拿着上游 Key 去猜端点。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

// balanceAmountMaxRunes 是余额读数单字段的展示上限：厂商金额/币种是短字符串，
// 超长即形态异常，截断保护管理面而不是解读它。
const balanceAmountMaxRunes = 32

// balanceEndpoint 是某平台余额 API 的内置地址：base 是站点根（注意与该平台
// 的协议端点根**不是**同一个值——deepseek 的 chat 根带 /v1，余额路径却挂在
// 裸站点根下），path 是余额资源路径。base_url 非空时以之替换 base
// （dev/测试指向假上游用，产品行经管理 API 建不出带地址的特化平台行）。
type balanceEndpoint struct {
	base string
	path string
}

// balanceEndpoints 是支持余额查询的平台表。新增平台 = 补一条目 + 在
// QueryBalance 里补该家的解析分支；管理面与 UI 经 SupportsBalance 自动跟进。
var balanceEndpoints = map[string]balanceEndpoint{
	config.UpstreamDeepseek: {base: "https://api.deepseek.com", path: "/user/balance"},
}

// SupportsBalance 报告某上游类型是否支持余额查询。管理端点据此拒绝不支持的
// 类型，上游列表据此给 UI 出 balance_supported 事实。
func SupportsBalance(upstreamType string) bool {
	_, ok := balanceEndpoints[upstreamType]
	return ok
}

// BalanceAmount 是一种币种的余额读数。金额保持厂商返回的十进制字符串原样
// （设备不解析数值——余额不是本设备的账，转数值只会引入精度与口径问题）。
type BalanceAmount struct {
	// Currency 是币种代码（如 CNY/USD）。
	Currency string
	// Total 是总余额；Granted 是赠金部分、ToppedUp 是充值部分（平台没有
	// 对应口径时为空串，UI 按空隐藏）。
	Total    string
	Granted  string
	ToppedUp string
}

// BalanceResult 是一次余额查询的结果。失败摘要与金额同受「只进管理 API
// 响应」约束，调用方不得写入日志或审计。
type BalanceResult struct {
	// OK 表示查到了可解析的余额（上游 2xx 且响应形态认识）。
	OK bool
	// Status 是上游 HTTP 状态码；0 表示未收到 HTTP 响应（连接失败/超时）。
	Status int
	// LatencyMS 是全程毫秒数。
	LatencyMS int64
	// Message 是失败摘要（上游错误 message 或传输层归类）；成功为空。
	Message string
	// Available 是平台报告的「余额是否充足可用」（deepseek is_available）。
	Available bool
	// Balances 按厂商返回顺序逐币种列出；成功时至少一条。
	Balances []BalanceAmount
}

// QueryBalance 查询 acct 的平台余额。调用方用 ctx 限定单次查询上限；
// client 复用管理面的上游 HTTP 客户端。类型不在支持表里返回 OK=false 的
// 说明性结果（正常入口先经 SupportsBalance 判定，这里只是防御分支）。
func QueryBalance(ctx context.Context, client *http.Client, acct Account) BalanceResult {
	ep, ok := balanceEndpoints[acct.Type]
	if !ok {
		return BalanceResult{Message: "该上游类型不支持余额查询"}
	}
	base := ep.base
	if acct.BaseURL != "" {
		base = strings.TrimRight(acct.BaseURL, "/")
	}
	req, err := http.NewRequestWithContext(acct.EgressContext(ctx), http.MethodGet, base+ep.path, nil)
	if err != nil {
		return BalanceResult{Message: trimMessage("构造余额请求失败: " + err.Error())}
	}
	req.Header.Set("Accept", "application/json")
	acct.Authorize(req.Header, config.ProtocolOpenAIChat)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return BalanceResult{LatencyMS: sinceMS(start), Message: transportMessage(err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeReadCap))
	latency := sinceMS(start)
	if resp.StatusCode/100 != 2 {
		return BalanceResult{Status: resp.StatusCode, LatencyMS: latency, Message: errorSummary(body)}
	}
	// 解析分支按平台走；当前只有 deepseek 一家在支持表里。
	res := parseDeepseekBalance(body)
	res.Status = resp.StatusCode
	res.LatencyMS = latency
	return res
}

// parseDeepseekBalance 解析 DeepSeek 余额响应形态。2xx 但解析不出（形态
// 变更/网关错误页）如实报失败，不编造读数。
func parseDeepseekBalance(body []byte) BalanceResult {
	var v struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.BalanceInfos) == 0 {
		return BalanceResult{Message: "上游返回了无法解析的余额响应"}
	}
	out := BalanceResult{OK: true, Available: v.IsAvailable}
	for _, b := range v.BalanceInfos {
		out.Balances = append(out.Balances, BalanceAmount{
			Currency: cleanAmount(b.Currency),
			Total:    cleanAmount(b.TotalBalance),
			Granted:  cleanAmount(b.GrantedBalance),
			ToppedUp: cleanAmount(b.ToppedUpBalance),
		})
	}
	return out
}

// cleanAmount 把厂商金额/币种字段收敛为可展示的短单行（不解析数值，只挡
// 脏字节与异常超长）。
func cleanAmount(s string) string { return CleanMessage(s, balanceAmountMaxRunes) }
