package admin

// 单把 API 密钥的「可用订阅」端点：GET/PUT /admin/v1/keys/{id}/dev-tools。
//
// 它只管开发工具策略里四种订阅各自钉死的那一个账号（Codex / Grok / Claude /
// Cursor 各一个账号行 id，null = 未授权）；同一份策略里的目录模型选择
// （CatalogModelIDs，界面上的「开发工具可见」）由「可用模型」端点（api-models）
// 连同调用范围一起整份替换，本端点原样保留。
//
//	GET  → {revision, subscriptions:{codex:<id|null>,…}, subscription_status:[…]}
//	PUT  ← {subscriptions:{codex:<id|null>, grok:…, claude:…, cursor:…}}
//
// PUT 是整份替换：缺席的键等于 null（不授权）。账号不存在或与工具种类不符答
// 400 agent_account_invalid，整份不写。

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const EventKeyDevToolsUpdate = "key.dev_tools_update"

// devToolSubscriptionsJSON 是四种订阅各自钉死的账号行 id；null = 未授权。
type devToolSubscriptionsJSON struct {
	Codex  *int64 `json:"codex"`
	Grok   *int64 `json:"grok"`
	Claude *int64 `json:"claude"`
	Cursor *int64 `json:"cursor"`
}

func optionalID(id int64) *int64 {
	if id == 0 {
		return nil
	}
	return &id
}

func derefID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

func subscriptionsJSON(cfg store.DevToolConfig) devToolSubscriptionsJSON {
	return devToolSubscriptionsJSON{
		Codex:  optionalID(cfg.CodexAccountID),
		Grok:   optionalID(cfg.GrokAccountID),
		Claude: optionalID(cfg.ClaudeAccountID),
		Cursor: optionalID(cfg.CursorAccountID),
	}
}

// devToolSubscriptionStatusJSON 是 devtoolpolicy.Subscription 的管理面投影，
// 外加钉死账号的 id 与名称（gate 读到的配置里没有这两项）。
type devToolSubscriptionStatusJSON struct {
	Provider     string `json:"provider"`
	Configured   bool   `json:"configured"`
	Available    bool   `json:"available"`
	DefaultModel string `json:"default_model"`
	AccountID    *int64 `json:"account_id"`
	AccountLabel string `json:"account_label"`
}

type devToolsResponse struct {
	Revision           int64                           `json:"revision"`
	Subscriptions      devToolSubscriptionsJSON        `json:"subscriptions"`
	SubscriptionStatus []devToolSubscriptionStatusJSON `json:"subscription_status"`
}

func (s *Server) devToolResolver() *devtoolpolicy.Resolver {
	return &devtoolpolicy.Resolver{
		Store:          s.st,
		AgentModels:    s.AgentSubscriptionModels,
		PlatformModels: s.EffectivePlatformModels,
	}
}

// devToolsAuditDetail 是策略全量快照的审计 detail（两个写端点共用同一形态）：
// 四种订阅各自钉的账号行 id（0 = 未授权）、模型选择与 revision。
func devToolsAuditDetail(cfg store.DevToolConfig) string {
	return fmt.Sprintf("codex=%d grok=%d claude=%d cursor=%d model_ids=%v revision=%d",
		cfg.CodexAccountID, cfg.GrokAccountID, cfg.ClaudeAccountID, cfg.CursorAccountID,
		cfg.CatalogModelIDs, cfg.Revision)
}

func (s *Server) devToolsResponse(r *http.Request, keyID int64) (devToolsResponse, error) {
	cfg, err := s.st.GetDevToolConfig(r.Context(), keyID)
	if err != nil {
		return devToolsResponse{}, err
	}
	snapshot, err := s.devToolResolver().Snapshot(r.Context(), keyID)
	if err != nil {
		return devToolsResponse{}, err
	}
	status := make([]devToolSubscriptionStatusJSON, 0, len(snapshot.Subscriptions))
	for _, sub := range snapshot.Subscriptions {
		status = append(status, devToolSubscriptionStatusJSON{
			Provider: sub.Provider, Configured: sub.Configured, Available: sub.Available,
			DefaultModel: sub.DefaultModel, AccountID: optionalID(sub.AccountID), AccountLabel: sub.AccountLabel,
		})
	}
	return devToolsResponse{
		Revision:           cfg.Revision,
		Subscriptions:      subscriptionsJSON(cfg),
		SubscriptionStatus: status,
	}, nil
}

func (s *Server) handleGetKeyDevTools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	resp, err := s.devToolsResponse(r, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePutKeyDevTools(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	var req struct {
		Subscriptions devToolSubscriptionsJSON `json:"subscriptions"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	for _, chosen := range []*int64{req.Subscriptions.Codex, req.Subscriptions.Grok, req.Subscriptions.Claude, req.Subscriptions.Cursor} {
		if chosen != nil && *chosen <= 0 {
			writeError(w, http.StatusBadRequest, "agent_account_invalid", "订阅账号 id 须为正整数或 null")
			return
		}
	}
	if _, ok := s.keyForWrite(w, r, id); !ok {
		return
	}
	current, err := s.st.GetDevToolConfig(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	next, changed, err := s.st.ReplaceDevToolConfig(r.Context(), store.DevToolConfig{
		KeyID:           id,
		CodexAccountID:  derefID(req.Subscriptions.Codex),
		GrokAccountID:   derefID(req.Subscriptions.Grok),
		ClaudeAccountID: derefID(req.Subscriptions.Claude),
		CursorAccountID: derefID(req.Subscriptions.Cursor),
		CatalogModelIDs: current.CatalogModelIDs,
	})
	if err != nil {
		if errors.Is(err, store.ErrDevToolAccountInvalid) {
			writeError(w, http.StatusBadRequest, "agent_account_invalid", "订阅账号不存在，或不是该工具的订阅")
			return
		}
		s.writeKeyError(w, r, err)
		return
	}
	if changed {
		// detail 是策略全量快照：model_ids 虽不由本端点改，也一并记下当时值。
		s.audit(r.Context(), store.AuditEvent{
			Event: EventKeyDevToolsUpdate, Entity: entityKey(id), RemoteIP: remoteIP(r),
			Detail: devToolsAuditDetail(next),
		})
	}
	resp, err := s.devToolsResponse(r, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
