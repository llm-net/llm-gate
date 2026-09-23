package admin

// Agent远控新建对话时的可选项：哪些 API 密钥能给智能体用、每把密钥在 Codex 面可见的
// 模型、可选的推理档位；以及按对话钉死的密钥 / 模型解析出引擎要用的凭据。
//
//	GET /admin/v1/agent-hosts/{id}/agent/options   引擎状态 + 可选密钥（各带可选模型）+ 档位
//
// 密钥必须：存在、未停用、留有封存明文（旧版本签发的没有），且在 Codex 面开放——即开发
// 工具策略里钉了 Codex 订阅账号，或勾了「开发工具可见」的目录模型（判据与 Key 持有人走
// /agents/codex 同一份 devtoolpolicy 快照，网关按同一份快照分流：目录模型走目录 Responses
// 面，其余名字走订阅代理）。模型只能选那把密钥在 Codex 面可见的模型，两类来源并列给出：
// 订阅自带的（gpt-6-astra、gpt-5.6-sol 等，随钉死账号可用与否出没）与目录模型；留空取
// 缺省（账号缺省模型，没有就是第一个可见模型），新建对话时落成具体名字钉死。明文只在
// 起会话时解封一次交给引擎进程的环境变量，不进响应、日志或审计（§15.1）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// agentModelJSON 是模型选择器的一项；Source 为 subscription（订阅自带）或 catalog（目录模型）。
type agentModelJSON struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// agentKeyJSON 是密钥选择器的一项：只有 codex_enabled（钉了 Codex 订阅或有开发工具可见的
// 目录模型）、未停用且留有封存明文的能选。Models 是它此刻在 Codex 面可见的模型，DefaultModel
// 是留空时会落成的模型。
type agentKeyJSON struct {
	ID                 int64            `json:"id"`
	Label              string           `json:"label"`
	Display            string           `json:"display"`
	Disabled           bool             `json:"disabled"`
	PlaintextAvailable bool             `json:"plaintext_available"`
	CodexConfigured    bool             `json:"codex_configured"`
	CodexAvailable     bool             `json:"codex_available"`
	CodexEnabled       bool             `json:"codex_enabled"`
	Models             []agentModelJSON `json:"models"`
	DefaultModel       string           `json:"default_model"`
}

type agentOptionsJSON struct {
	Engine  hostagent.EngineStatus `json:"engine"`
	Keys    []agentKeyJSON         `json:"keys"`
	Efforts []string               `json:"efforts"`
}

// codexModelsOf 列出一把密钥在 Codex 面可见的模型（与网关 /agents/codex 的判定同源）。
func codexModelsOf(snapshot devtoolpolicy.Snapshot) []agentModelJSON {
	tool := snapshot.Tools["codex"]
	out := make([]agentModelJSON, 0, len(tool.Models))
	for _, m := range tool.Models {
		out = append(out, agentModelJSON{Name: m.Name, Source: m.Source})
	}
	return out
}

// agentToolLabels 是开发工具接入面的界面名（原因文案里用）。
var agentToolLabels = map[string]string{"codex": "Codex", "claude": "Claude Code", "grok": "Grok"}

// resolveAgentModel 把选的模型名落成会话实际用的模型（tool 是开发工具名，也是订阅种类：
// codex / claude / grok）：空取缺省（账号缺省模型，没有就是第一个可见模型）；返回不可用的原因
// （空 = 可用）。判据与网关一致：目录模型只看勾选，订阅模型要钉死的账号此刻可用且名字在
// 可见清单里。
func resolveAgentModel(snapshot devtoolpolicy.Snapshot, tool, model string) (string, string) {
	label := agentToolLabels[tool]
	sub := snapshot.Subscription(tool)
	surface := snapshot.Tools[tool]
	if !sub.Configured && len(surface.Models) == 0 {
		if tool == "grok" {
			return model, "这把 API 密钥未获授权使用 Grok 订阅：先在「API密钥 → 可用订阅」里为它选择一个 Grok 账号"
		}
		return model, fmt.Sprintf("这把 API 密钥未获授权使用 %[1]s 订阅，也没有开发工具可见的目录模型：先在「API密钥 → 可用订阅」里为它钉一个 %[1]s 账号，或在「可用模型」里勾选开发工具可见的模型", label)
	}
	if model == "" {
		model = surface.DefaultModel
	}
	if model == "" {
		if sub.Configured && !sub.Available {
			return model, fmt.Sprintf("该密钥钉死的 %s 订阅账号当前不可用", label)
		}
		return model, fmt.Sprintf("这把密钥在 %[1]s 面没有可见模型：先连接 %[1]s 订阅账号，或在「API密钥 → 可用模型」里勾选开发工具可见的模型", label)
	}
	if snapshot.IsCatalogModel(tool, model) {
		return model, ""
	}
	if sub.Configured && !sub.Available {
		return model, fmt.Sprintf("该密钥钉死的 %s 订阅账号当前不可用", label)
	}
	if !snapshot.HasModel(tool, model) {
		return model, fmt.Sprintf("模型 %s 对这把密钥不可用，请重新选择", model)
	}
	return model, ""
}

// CodexChatConfig 按对话钉死的密钥 / 模型 / 档位在 Codex 面上解析引擎要用的凭据
// （hostagent.CodexOptions.Config，Agent远控的板端引擎）。
func (s *Server) CodexChatConfig(ctx context.Context, in hostagent.ChatConfig) (hostagent.KeyConfig, error) {
	return s.AgentChatConfig(ctx, "codex", in)
}

// AgentChatConfig 按对话钉死的密钥 / 模型 / 档位在 tool（codex / claude / grok）对应的开发工具接入面上
// 解析引擎要用的凭据（nodeengine.Options.Resolve）。不就绪时 Ready=false 并给出原因；只有
// 就绪时才解封明文。
func (s *Server) AgentChatConfig(ctx context.Context, tool string, in hostagent.ChatConfig) (hostagent.KeyConfig, error) {
	cfg := hostagent.KeyConfig{Model: in.Model, Effort: in.Effort, KeyDisplay: in.KeyDisplay}
	if _, known := agentToolLabels[tool]; !known {
		cfg.Reason = "不认识的开发工具：" + tool
		return cfg, nil
	}
	if in.KeyID <= 0 {
		cfg.Reason = "请为对话选择一把 API 密钥"
		return cfg, nil
	}
	k, err := s.st.GetAPIKeyByID(ctx, in.KeyID)
	if errors.Is(err, store.ErrNotFound) {
		cfg.Reason = "这个对话选定的 API 密钥已被删除；请新建对话重新选择"
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	cfg.KeyDisplay = keyDisplayOf(k)
	if k.Archived() {
		cfg.Reason = "这个对话选定的 API 密钥已归档；请新建对话重新选择"
		return cfg, nil
	}
	if k.Disabled {
		cfg.Reason = "这个对话选定的 API 密钥已停用"
		return cfg, nil
	}
	if !k.PlaintextAvailable {
		cfg.Reason = "这把密钥签发于旧版本、没有封存明文，智能体用不了；请新建一把"
		return cfg, nil
	}
	snapshot, err := s.devToolResolver().Snapshot(ctx, k.ID)
	if err != nil {
		return cfg, err
	}
	model, reason := resolveAgentModel(snapshot, tool, in.Model)
	cfg.Model = model
	if reason != "" {
		cfg.Reason = reason
		return cfg, nil
	}
	plaintext, err := s.st.GetAPIKeyPlaintext(ctx, k.ID)
	if err != nil {
		if errors.Is(err, store.ErrKeyPlaintextMissing) || errors.Is(err, store.ErrKeyPlaintextUnreadable) {
			cfg.Reason = "该密钥没有可用的封存明文（签发于旧版本或密文损坏），请新建一把替换"
			return cfg, nil
		}
		return cfg, err
	}
	cfg.KeyPlaintext = plaintext
	cfg.Ready = true
	return cfg, nil
}

func keyDisplayOf(k *store.APIKey) string { return k.DisplayPrefix + "…" + k.DisplayLast4 }

func (s *Server) agentOptions(ctx context.Context) (*agentOptionsJSON, error) {
	out := &agentOptionsJSON{Keys: []agentKeyJSON{}, Efforts: hostagent.CodexEfforts}
	if s.hostAgent != nil {
		out.Engine = s.hostAgent.EngineStatus(ctx)
	} else {
		out.Engine = hostagent.EngineStatus{ID: hostagent.CodexEngineID, Label: "Codex App Server", Reason: "本进程未接入 Agent远控"}
	}
	keys, err := s.st.ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	resolver := s.devToolResolver()
	for i := range keys {
		k := &keys[i]
		snapshot, err := resolver.Snapshot(ctx, k.ID)
		if err != nil {
			return nil, err
		}
		sub := snapshot.Subscription(store.AgentProviderCodex)
		out.Keys = append(out.Keys, agentKeyJSON{
			ID: k.ID, Label: k.Label, Display: keyDisplayOf(k), Disabled: k.Disabled, PlaintextAvailable: k.PlaintextAvailable,
			CodexConfigured: sub.Configured, CodexAvailable: sub.Available, CodexEnabled: snapshot.ToolEnabled("codex"),
			Models: codexModelsOf(snapshot), DefaultModel: snapshot.Tools["codex"].DefaultModel,
		})
	}
	return out, nil
}

func (s *Server) handleAgentOptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentHostRow(w, r); !ok {
		return
	}
	reply, err := s.agentOptions(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}
