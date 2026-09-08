package grokauth

import (
	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
)

// NewProvider 用一份已在手的 grok 句柄构造 agentauth 提供者（状态机语义见
// agentauth/provider.go）。**typed-nil 在这里挡**：直接把 nil *Auth 塞进
// interface 会得到一个非 nil 的 Handle，agentauth 那侧的「没有句柄」分支就
// 永远走不到（同 codexauth.NewProvider 的理由）。
func NewProvider(c *Client, auth *Auth, opts agentauth.ProviderOptions) *agentauth.Provider {
	var h agentauth.Handle
	if auth != nil {
		h = auth
	}
	return agentauth.NewProvider(c, h, opts)
}

// ProviderAccount 取当前句柄的 xAI 账号标识（管理台读数用；空串是允许的状态）。
// 对位 codexauth.ProviderAccountID——账号标识是 provider 侧概念，不在
// agentauth.Provider 上。
func ProviderAccount(p *agentauth.Provider) string {
	if a, ok := p.Snapshot().(*Auth); ok {
		return a.Account()
	}
	return ""
}
