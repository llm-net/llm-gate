// devtools.go 是开发工具策略在数据面的三道门，外加 gate 与设备界面凭 Key 自证的
// 两条只读端点（handleGateConfig / handleGateEndpoints）：
//
//	devToolSnapshot      把这把 Key 的策略快照算出来（每请求一次、同请求复用）；
//	withDevTool          /agents/<tool>/ 接入面的子树闸——工具对这把 Key 不开放
//	                     时整棵子树 403 devtool_not_allowed；
//	withSubscription     单条路由的订阅闸——该工具的订阅没勾 403 subscription_not_allowed，
//	                     要求可用而当前不可用 409 agent_not_configured。
//
// 「工具对这把 Key 开放」的判据是 devtoolpolicy.Snapshot.ToolEnabled：勾了该工具的
// 订阅，或有目录模型投影到它。Grok 与 Cursor 没有目录投影，闸门恒等于「可用订阅」
// 勾选；Codex 与 Claude Code 允许只勾目录模型不勾订阅，子树照样开放，只是订阅模型
// 在各自处理器里再按 subscription_not_allowed 拒绝。错误体风格按路径（entryErrorStyle）。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// errDevToolsUnwired 只出现在没接策略投影器的窄测试进程里。
var errDevToolsUnwired = errors.New("开发工具策略未接线")

// policySnapshot 取本请求的策略快照（每请求一次、同请求复用），不写响应。
// 给不持有 ResponseWriter 的辅助路径用；HTTP 处理器走 devToolSnapshot。
func (s *Server) policySnapshot(r *http.Request) (devtoolpolicy.Snapshot, error) {
	info := infoFrom(r.Context())
	if info.devTools != nil {
		return *info.devTools, nil
	}
	if s.devTools == nil {
		return devtoolpolicy.Snapshot{}, errDevToolsUnwired
	}
	snapshot, err := s.devTools.Snapshot(r.Context(), info.keyID)
	if err != nil {
		return devtoolpolicy.Snapshot{}, err
	}
	info.devTools = &snapshot
	return snapshot, nil
}

func (s *Server) devToolSnapshot(w http.ResponseWriter, r *http.Request) (devtoolpolicy.Snapshot, bool) {
	snapshot, err := s.policySnapshot(r)
	if err != nil {
		if r.Context().Err() == nil && !errors.Is(err, errDevToolsUnwired) {
			s.log.Error("生成开发工具配置失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		}
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return devtoolpolicy.Snapshot{}, false
	}
	return snapshot, true
}

// pinnedAgentCredential 解封某把 Key 为某种订阅钉死的账号凭据：策略未接线、
// 未钉账号、行不存在或行不是这种订阅，一律按 store.ErrNotFound 处理——对
// 调用方都是「这把 Key 没有可用的这份订阅」。返回的明文不得进日志（§15.1）。
func (s *Server) pinnedAgentCredential(ctx context.Context, keyID int64, provider string) (*store.AgentAccount, string, *devtoolpolicy.Snapshot, error) {
	if s.devTools == nil {
		return nil, "", nil, store.ErrNotFound
	}
	snapshot, err := s.devTools.Snapshot(ctx, keyID)
	if err != nil {
		return nil, "", nil, err
	}
	accountID := snapshot.Subscription(provider).AccountID
	if accountID == 0 {
		return nil, "", &snapshot, store.ErrNotFound
	}
	acct, blob, err := s.store.GetAgentCredential(ctx, accountID)
	if err != nil {
		return nil, "", &snapshot, err
	}
	if acct.Provider != provider {
		return nil, "", &snapshot, store.ErrNotFound
	}
	return acct, blob, &snapshot, nil
}

func (s *Server) handleGateConfig(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

// handleGateEndpoints 回这把 Key 的接入读数（GET /gate-helper/v1/endpoints）：地址、
// 按 Key 的 API模型范围裁剪的模型清单与铭牌，由注入的 KeyAccess 组装。与 config
// 同一纪律：身份只来自 Bearer 鉴权、不接受任何入参、恒 no-store、正文不进日志。
// 未接线（keyAccess 为 nil）答 503——这是装配缺陷，不能拿空地址冒充读数。
func (s *Server) handleGateEndpoints(w http.ResponseWriter, r *http.Request) {
	info := infoFrom(r.Context())
	if s.keyAccess == nil {
		entryErrorStyle(r)(w, http.StatusServiceUnavailable, "endpoints_unavailable",
			"Access information is not available on this device.")
		return
	}
	snapshot, err := s.keyAccess.KeyAccessSnapshot(r, info.keyID)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error("组装接入读数失败", "request_id", info.id, "err", err.Error())
		}
		entryErrorStyle(r)(w, http.StatusInternalServerError, "internal_error", internalErrorMessage)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(snapshot)
}

// devToolPermitted 是 /agents/<tool>/ 接入面的子树闸（见文件头）。
func (s *Server) devToolPermitted(w http.ResponseWriter, r *http.Request, tool string) (devtoolpolicy.Snapshot, bool) {
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return devtoolpolicy.Snapshot{}, false
	}
	if !snapshot.ToolEnabled(tool) {
		entryErrorStyle(r)(w, http.StatusForbidden, "devtool_not_allowed",
			"This API key is not allowed to use "+agentDisplayName(tool)+" on this device.")
		return devtoolpolicy.Snapshot{}, false
	}
	return snapshot, true
}

func (s *Server) withDevTool(tool string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.devToolPermitted(w, r, tool); !ok {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) subscriptionPermitted(w http.ResponseWriter, r *http.Request, provider string, requireAvailable bool) (devtoolpolicy.Snapshot, bool) {
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return devtoolpolicy.Snapshot{}, false
	}
	sub := snapshot.Subscription(provider)
	if !sub.Configured {
		entryErrorStyle(r)(w, http.StatusForbidden, "subscription_not_allowed",
			"This API key is not allowed to use the "+agentDisplayName(provider)+" subscription.")
		return devtoolpolicy.Snapshot{}, false
	}
	if requireAvailable && !sub.Available {
		entryErrorStyle(r)(w, http.StatusConflict, "agent_not_configured",
			"The requested development-tool subscription is not currently available on this device.")
		return devtoolpolicy.Snapshot{}, false
	}
	return snapshot, true
}

func (s *Server) withSubscription(provider string, requireAvailable bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.subscriptionPermitted(w, r, provider, requireAvailable); !ok {
			return
		}
		next.ServeHTTP(w, r)
	})
}
