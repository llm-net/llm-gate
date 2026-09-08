package codexauth

// 令牌提供者已上收 internal/agentauth（grok 加入后两条订阅代理共用同一套
// single-flight / 世代去重 / 两类失败两种反应的状态机——那套语义每条都咬过人，
// 只该存在一份）。本文件是 codex 侧的构造包装与别名：既有调用方、以及本包
// token_test.go 那张按迭代 11 决策逐条钉死的回归测试网，全部原地成立——
// 它们经这层包装测的就是 agentauth 的状态机与本包 [Client.RefreshHandle] 的接线。

import (
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

// Provider / ProviderOptions 即 agentauth 的那一套（别名，不是新类型）。
// 语义文档见 agentauth/provider.go；决策编号（1/5）与当年的实测坑一并迁走。
type (
	Provider        = agentauth.Provider
	ProviderOptions = agentauth.ProviderOptions
)

// 五个时序常量随状态机迁入 agentauth，这里留同名常量（值恒等，测试照用）。
const (
	expirySkew          = agentauth.ExpirySkew
	minTokenValidity    = agentauth.MinTokenValidity
	refreshTimeout      = agentauth.RefreshTimeout
	callbackTimeout     = agentauth.CallbackTimeout
	refreshFailCooldown = agentauth.RefreshFailCooldown
)

// 未导出常量在编译期只被测试引用；这行让非测试构建也「用到」它们，
// 免得哪天误当死代码删掉（测试才是它们的消费方）。
var _ = []time.Duration{expirySkew, minTokenValidity, refreshTimeout, callbackTimeout, refreshFailCooldown}

// NewProvider 用一份已在手的 codex 句柄构造提供者（auth 为 nil 会得到一个恒回
// ErrAuthExpired 的 provider）。**typed-nil 在这里挡**：直接把 nil *Auth 塞进
// interface 会得到一个非 nil 的 Handle，agentauth 那侧的「没有句柄」分支就永远
// 走不到。
func NewProvider(c *Client, auth *Auth, opts ProviderOptions) *Provider {
	var h agentauth.Handle
	if auth != nil {
		h = auth
	}
	return agentauth.NewProvider(c, h, opts)
}

// ProviderAccountID 取当前句柄的 OpenAI 账号标识（代理注入 ChatGPT-Account-ID
// 用；空串是允许的状态，见 [idTokenAccountClaim]）。account_id 是 codex 独有的
// 概念，所以它不在 agentauth.Provider 上，而是经 [agentauth.Provider.Snapshot]
// 从句柄里读——句柄不可变，读到的值与取令牌那一刻自洽。
func ProviderAccountID(p *Provider) string {
	if a, ok := p.Snapshot().(*Auth); ok {
		return a.AccountID()
	}
	return ""
}
