// Package devtoolpolicy projects one API key's saved choices onto the currently
// usable Codex, Grok Build, Claude Code, Cursor, OpenCode, and MiniMax Code targets.
package devtoolpolicy

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

const SchemaVersion = 1

type AgentModelsFunc func(context.Context) (map[string]string, error)
type PlatformModelsFunc func(context.Context) platformcatalog.Doc

type Resolver struct {
	Store          *store.Store
	AgentModels    AgentModelsFunc
	PlatformModels PlatformModelsFunc
}

// Subscription 是一种订阅对这把 Key 的投影。AccountID 是管理员为这把 Key 钉死的
// 订阅账号行 id（0 = 未授权），AccountLabel 是该账号的管理名称；两者只给数据面与
// 管理面用，不进 gate 读到的 JSON（Key 持有人的配置读数不含账号标识）。
type Subscription struct {
	Provider     string `json:"provider"`
	Configured   bool   `json:"configured"`
	Available    bool   `json:"available"`
	DefaultModel string `json:"default_model"`
	AccountID    int64  `json:"-"`
	AccountLabel string `json:"-"`
}

type Model struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type Tool struct {
	DefaultModel string  `json:"default_model"`
	Models       []Model `json:"models"`
}

type Snapshot struct {
	SchemaVersion int             `json:"schema_version"`
	Revision      int64           `json:"revision"`
	Subscriptions []Subscription  `json:"subscriptions"`
	Tools         map[string]Tool `json:"tools"`
	selected      map[string]map[string]bool
}

type ModelOption struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Selected  bool     `json:"selected"`
	Available bool     `json:"available"`
	Tools     []string `json:"tools"`
	Reason    string   `json:"reason,omitempty" i18n:"text"`
}

func (r *Resolver) doc(ctx context.Context) platformcatalog.Doc {
	if r.PlatformModels != nil {
		return r.PlatformModels(ctx)
	}
	return platformcatalog.Builtin()
}

func (r *Resolver) agentOwners(ctx context.Context) (map[string]string, error) {
	if r.AgentModels == nil {
		return map[string]string{}, nil
	}
	return r.AgentModels(ctx)
}

func (r *Resolver) Snapshot(ctx context.Context, keyID int64) (Snapshot, error) {
	cfg, err := r.Store.GetDevToolConfig(ctx, keyID)
	if err != nil {
		return Snapshot{}, err
	}
	models, err := r.Store.ListModelsWithSources(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	accounts, err := r.Store.ListAgentAccounts(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	owners, err := r.agentOwners(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	doc := r.doc(ctx)

	accountByID := make(map[int64]store.AgentAccount, len(accounts))
	for _, account := range accounts {
		accountByID[account.ID] = account
	}
	selectedIDs := make(map[int64]bool, len(cfg.CatalogModelIDs))
	for _, id := range cfg.CatalogModelIDs {
		selectedIDs[id] = true
	}
	// cursor entries stay empty forever: compatibleTools never yields "cursor"
	// (like grok, the subscription is the only way in; catalog models are never
	// projected onto it). The keys still exist so lookups stay uniform.
	extraByTool := map[string][]Model{"codex": {}, "grok": {}, "claude": {}, "cursor": {}, "opencode": {}, "mcode": {}}
	selectedNames := map[string]map[string]bool{
		"codex": {}, "grok": {}, "claude": {}, "cursor": {}, "opencode": {}, "mcode": {},
	}
	for i := range models {
		m := &models[i]
		if !selectedIDs[m.ID] {
			continue
		}
		tools := compatibleTools(doc, m)
		for _, tool := range tools {
			extraByTool[tool] = append(extraByTool[tool], Model{Name: m.Name, Source: "catalog"})
			selectedNames[tool][m.Name] = true
		}
	}

	snap := Snapshot{
		SchemaVersion: SchemaVersion,
		Revision:      cfg.Revision,
		Subscriptions: make([]Subscription, 0, 4),
		Tools:         make(map[string]Tool, 6),
		selected:      selectedNames,
	}
	for _, provider := range store.AgentProviders {
		// 授权 = 钉死了一个账号；账号被删时策略行的钉已置空，Configured 随之为假。
		// 钉着的行 provider 必须相符（写入时校验过；这里再挡一次手改过的库）。
		accountID := cfg.SubscriptionAccount(provider)
		acct, connected := accountByID[accountID]
		if connected && acct.Provider != provider {
			connected = false
		}
		configured := accountID != 0 && connected
		available := configured && acct.Status == store.AgentStatusActive
		if available && provider == store.AgentProviderClaude {
			_, blob, err := r.Store.GetAgentCredential(ctx, acct.ID)
			if err != nil {
				available = false
			} else {
				cred, err := claudeauth.Parse(blob)
				available = err == nil && cred.HasSetupToken()
			}
		}
		subModels := []Model{}
		// Cursor 是透明订阅面：设备不掌握模型级真值，运行时只给出订阅是否
		// 可用；cursor-agent 经上游协议自行发现并保存选择，因此 tools.cursor
		// 的模型投影与默认模型恒为空。
		if available && provider != store.AgentProviderCursor {
			subModels = subscriptionModels(doc, owners, provider)
			// Claude's account-level default_model is the optional model exposed
			// to members, not merely a preferred first row. Keep the mixed
			// discovery surface aligned with the direct Claude subscription
			// discovery filter in gateway/claude.go. A configured name that is
			// absent upstream yields no subscription row; catalog selections
			// below remain available independently.
			if provider == store.AgentProviderClaude && acct.DefaultModel != "" {
				kept := subModels[:0]
				for _, model := range subModels {
					if model.Name == acct.DefaultModel {
						kept = append(kept, model)
						break
					}
				}
				subModels = kept
			}
		}
		// A selected catalog model wins a same-name collision.
		if len(selectedNames[provider]) > 0 {
			kept := subModels[:0]
			for _, m := range subModels {
				if !selectedNames[provider][m.Name] {
					kept = append(kept, m)
				}
			}
			subModels = kept
		}
		visible := append(subModels, extraByTool[provider]...)
		def := ""
		if available && containsModel(visible, acct.DefaultModel) {
			def = acct.DefaultModel
		} else if len(visible) > 0 {
			def = visible[0].Name
		}
		sub := Subscription{Provider: provider, Configured: configured, Available: available}
		if configured {
			sub.AccountID, sub.AccountLabel = acct.ID, acct.Label
		}
		if available && provider != store.AgentProviderCursor {
			sub.DefaultModel = acct.DefaultModel
		}
		snap.Subscriptions = append(snap.Subscriptions, sub)
		snap.Tools[provider] = Tool{DefaultModel: def, Models: visible}
	}
	openCodeModels := extraByTool["opencode"]
	openCodeDefault := ""
	if len(openCodeModels) > 0 {
		openCodeDefault = openCodeModels[0].Name
	}
	snap.Tools["opencode"] = Tool{DefaultModel: openCodeDefault, Models: openCodeModels}
	snap.Tools["mcode"] = Tool{DefaultModel: openCodeDefault, Models: extraByTool["mcode"]}
	return snap, nil
}

func (r *Resolver) ModelOptions(ctx context.Context, keyID int64) ([]ModelOption, error) {
	cfg, err := r.Store.GetDevToolConfig(ctx, keyID)
	if err != nil {
		return nil, err
	}
	rows, err := r.Store.ListModelsWithSources(ctx)
	if err != nil {
		return nil, err
	}
	selected := make(map[int64]bool, len(cfg.CatalogModelIDs))
	for _, id := range cfg.CatalogModelIDs {
		selected[id] = true
	}
	doc := r.doc(ctx)
	out := make([]ModelOption, 0, len(rows))
	for i := range rows {
		m := &rows[i]
		tools := compatibleTools(doc, m)
		if len(tools) == 0 && !selected[m.ID] {
			continue
		}
		option := ModelOption{ID: m.ID, Name: m.Name, Selected: selected[m.ID], Available: len(tools) > 0, Tools: tools}
		if !option.Available {
			option.Reason = incompatibilityReason(m)
		}
		out = append(out, option)
	}
	return out, nil
}

func (s Snapshot) Subscription(provider string) Subscription {
	for _, sub := range s.Subscriptions {
		if sub.Provider == provider {
			return sub
		}
	}
	return Subscription{Provider: provider}
}

// ToolEnabled 判定某个开发工具对这把 Key 是否开放：给该工具钉了订阅账号（可用
// 订阅），或有目录模型投影到它，二者其一即开放。Grok 与 Cursor 没有目录投影，
// 结论恒等于订阅授权。网关的 /agents/<tool>/ 接入面以它作子树闸。
func (s Snapshot) ToolEnabled(tool string) bool {
	return s.Subscription(tool).Configured || len(s.Tools[tool].Models) > 0
}

func (s Snapshot) IsCatalogModel(tool, name string) bool {
	return s.selected[tool][name]
}

func (s Snapshot) HasModel(tool, name string) bool {
	return containsModel(s.Tools[tool].Models, name)
}

func containsModel(models []Model, name string) bool {
	if name == "" {
		return false
	}
	for _, model := range models {
		if model.Name == name {
			return true
		}
	}
	return false
}

func subscriptionModels(doc platformcatalog.Doc, owners map[string]string, provider string) []Model {
	result := []Model{}
	seen := map[string]bool{}
	if agent, ok := doc.Agent(provider); ok {
		for _, item := range agent.Models {
			if item.Kind != store.ModelKindText || owners[item.Name] != provider || seen[item.Name] {
				continue
			}
			seen[item.Name] = true
			result = append(result, Model{Name: item.Name, Source: "subscription"})
		}
	}
	// Preserve visibility if a newer provider/model exists locally before the
	// data document is refreshed, while keeping document order first.
	var rest []string
	for name, owner := range owners {
		if owner == provider && !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		result = append(result, Model{Name: name, Source: "subscription"})
	}
	return result
}

func compatibleTools(doc platformcatalog.Doc, m *store.ModelWithSources) []string {
	if m.Kind != store.ModelKindText || m.Disabled {
		return nil
	}
	codex, claude, openCode := false, false, false
	for _, src := range m.Sources {
		if src.Disabled || src.UpstreamDisabled {
			continue
		}
		acct := upstream.Account{Type: src.UpstreamType, BaseURL: src.UpstreamBaseURL, ProtocolURLs: src.UpstreamProtocolURLs}
		if m.EntryAnthropic {
			if _, ok := acct.ModelEndpoint(doc, src.UpstreamCatalogID, m.Name, src.UpstreamModelID, config.ProtocolAnthropicMessages); ok {
				claude = true
			}
		}
		if m.EntryResponses && acct.Type == config.UpstreamGeneric {
			if _, ok := acct.Endpoint(config.ProtocolOpenAIResponses); ok {
				codex = true
			}
		}
		if m.EntryOpenAI || m.EntryResponses {
			if _, ok := acct.ModelEndpoint(doc, src.UpstreamCatalogID, m.Name, src.UpstreamModelID, config.ProtocolOpenAIChat); ok {
				openCode = m.EntryOpenAI
				caps, found := doc.ModelCapabilitiesFor(src.UpstreamCatalogID, src.UpstreamType, m.Name, src.UpstreamModelID)
				if m.EntryResponses && found && caps.ResponsesChat.Profile != "" {
					codex = true
				}
			}
		}
	}
	tools := []string{}
	if codex {
		tools = append(tools, "codex")
	}
	if openCode {
		tools = append(tools, "opencode", "mcode")
	}
	if claude {
		tools = append(tools, "claude")
	}
	return tools
}

func incompatibilityReason(m *store.ModelWithSources) string {
	switch {
	case m.Kind != store.ModelKindText:
		return "not_text"
	case m.Disabled:
		return "model_disabled"
	case len(m.Sources) == 0:
		return "no_source"
	default:
		for _, src := range m.Sources {
			if !src.Disabled && !src.UpstreamDisabled {
				return "protocol_incompatible"
			}
		}
		return "source_unavailable"
	}
}

func IsNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

// AllowedSubscriptions 列出策略里钉了账号的订阅（按固定顺序）。
func AllowedSubscriptions(cfg store.DevToolConfig) []string {
	out := []string{}
	for _, provider := range store.AgentProviders {
		if cfg.SubscriptionAccount(provider) != 0 {
			out = append(out, provider)
		}
	}
	return out
}

func NormalizeProvider(provider string) string { return strings.ToLower(strings.TrimSpace(provider)) }
