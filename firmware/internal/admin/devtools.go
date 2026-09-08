package admin

// 单把 API 密钥的「可用订阅」端点：GET/PUT /admin/v1/keys/{id}/dev-tools。
//
// 它只管开发工具策略里的四个订阅开关（Codex / Grok / Claude / Cursor）；
// 同一份策略里的目录模型选择（CatalogModelIDs，界面上的「开发工具可见」）
// 由「可用模型」端点（api-models）连同调用范围一起整份替换，本端点原样保留。

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const EventKeyDevToolsUpdate = "key.dev_tools_update"

type devToolSubscriptionsJSON struct {
	Codex  bool `json:"codex"`
	Grok   bool `json:"grok"`
	Claude bool `json:"claude"`
	Cursor bool `json:"cursor"`
}

type devToolsResponse struct {
	Revision           int64                        `json:"revision"`
	Subscriptions      devToolSubscriptionsJSON     `json:"subscriptions"`
	SubscriptionStatus []devtoolpolicy.Subscription `json:"subscription_status"`
}

func (s *Server) devToolResolver() *devtoolpolicy.Resolver {
	return &devtoolpolicy.Resolver{
		Store:          s.st,
		AgentModels:    s.AgentSubscriptionModels,
		PlatformModels: s.EffectivePlatformModels,
	}
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
	return devToolsResponse{
		Revision: cfg.Revision,
		Subscriptions: devToolSubscriptionsJSON{
			Codex: cfg.AllowCodexSubscription, Grok: cfg.AllowGrokSubscription,
			Claude: cfg.AllowClaudeSubscription, Cursor: cfg.AllowCursorSubscription,
		},
		SubscriptionStatus: snapshot.Subscriptions,
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
	current, err := s.st.GetDevToolConfig(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	next, changed, err := s.st.ReplaceDevToolConfig(r.Context(), store.DevToolConfig{
		KeyID:                   id,
		AllowCodexSubscription:  req.Subscriptions.Codex,
		AllowGrokSubscription:   req.Subscriptions.Grok,
		AllowClaudeSubscription: req.Subscriptions.Claude,
		AllowCursorSubscription: req.Subscriptions.Cursor,
		CatalogModelIDs:         current.CatalogModelIDs,
	})
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	if changed {
		// detail 是策略全量快照：model_ids 虽不由本端点改，也一并记下当时值。
		s.audit(r.Context(), store.AuditEvent{
			Event: EventKeyDevToolsUpdate, Entity: entityKey(id), RemoteIP: remoteIP(r),
			Detail: fmt.Sprintf("codex=%t grok=%t claude=%t cursor=%t model_ids=%v revision=%d",
				next.AllowCodexSubscription, next.AllowGrokSubscription, next.AllowClaudeSubscription,
				next.AllowCursorSubscription, next.CatalogModelIDs, next.Revision),
		})
	}
	resp, err := s.devToolsResponse(r, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
