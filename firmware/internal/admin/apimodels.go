package admin

// 单把 API 密钥的「可用模型」策略端点：GET/PUT /admin/v1/keys/{id}/api-models。
//
// 它一次回答两个叠加的问题：
//
//  1. 这把 Key 能调用哪些模型（restricted + model_ids）——管的是厂商兼容 API 面：
//     /v1/chat/completions、/v1/messages、/v1/responses 与 AIGC 官方路径那一圈，
//     OpenCode 的 provider 也指着这个面。可选集合恒为**API密钥接入**承载的模型：
//     订阅接入的模型（无来源的订阅文本计价行）没有可路由的上游来源，本来就
//     调不通 API 入口，因此不进候选、也不接受选中。
//  2. 其中哪些要出现在开发工具里（dev_tool_model_ids）——即写进 Codex、
//     Claude Code、OpenCode 模型列表的目录模型选择，落在开发工具策略的
//     CatalogModelIDs 上；订阅四开关仍归 dev-tools 端点（「可用订阅」对话框）。
//
// 缺省是**不限制**：既有密钥与新签发的密钥都能调全部 API 模型，管理员显式打开
// restricted 才按选择收窄（空选择 = 一个都不能调，是合法表达）。收窄时强制
// 「开发工具可见 ⊆ 可用」：不在可用集合里的模型也不得投影给开发工具——Codex 与
// Claude Code 的混合面虽不经 API 闸门，越出可用范围的投影仍会让成员用上管理员
// 已收走的模型，所以在写入口就拒绝，而不是靠数据面再拦一次。

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const EventKeyAPIModelsUpdate = "key.api_models_update"

// apiModelOption 是一个候选模型的对外形态。Servable 是**此刻**能不能调通
// （模型启用且至少一条启用来源挂在启用的上游上），只作展示——选择是策略，
// 不因为一时不可用就不许勾。
type apiModelOption struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Platforms 是承载它的上游平台标签（去重、按来源序），空数组 = 没挂来源。
	Platforms []string `json:"platforms"`
	Selected  bool     `json:"selected"`
	// Selectable=false 的行只因旧选择还挂在清单里（模型已改由订阅接入承载），
	// 不能新勾为可用；已在策略里的旧选择照收（不然取消不掉）。
	Selectable bool   `json:"selectable"`
	Servable   bool   `json:"servable"`
	Reason     string `json:"reason,omitempty" i18n:"text"`
	// DevTools 是该模型可投影到的开发工具子集（codex/opencode/claude），
	// 空数组 = 不是开发工具候选（非文本、协议不兼容等）。
	DevTools        []string `json:"dev_tools"`
	DevToolSelected bool     `json:"dev_tool_selected"`
	// DevToolReason 是已勾选却失去兼容性时的机器原因（devtoolpolicy 的枚举），
	// 供界面翻译展示；兼容行恒为空。
	DevToolReason string `json:"dev_tool_reason,omitempty"`
}

type apiModelsResponse struct {
	Revision        int64            `json:"revision"`
	Restricted      bool             `json:"restricted"`
	ModelIDs        []int64          `json:"model_ids"`
	DevToolModelIDs []int64          `json:"dev_tool_model_ids"`
	Models          []apiModelOption `json:"models"`
}

// apiDomainModel 判定一行模型是否归 API密钥接入 侧，与管理台模型接入页两条
// 标签页的归属判据同源（订阅接入 = 目录 agents 段里那个名字且一条来源都没挂
// 的文本行）。挂了 API 上游的同名文本行归 API 侧。
func apiDomainModel(agents map[string]string, m *store.ModelWithSources) bool {
	return agentSubscriptionProvider(agents, m.Name, m.Kind, len(m.Sources)) == ""
}

// apiModelServable 与数据面选路的「有没有可用来源」同口径：模型启用，且至少
// 一条启用来源挂在启用的上游上。入口开关与协议匹配是逐入口的事，不在这里判。
func apiModelServable(m *store.ModelWithSources) (bool, string) {
	if m.Disabled {
		return false, "模型已停用"
	}
	for _, src := range m.Sources {
		if !src.Disabled && !src.UpstreamDisabled {
			return true, ""
		}
	}
	if len(m.Sources) == 0 {
		return false, "没有挂上游来源"
	}
	return false, "来源或其上游已停用"
}

// apiModelOptions 组装候选清单：API密钥接入 的全部模型，叠加开发工具投影读数
// （兼容工具子集与当前勾选）。已经选中（可用或开发工具可见）但此后离开该侧的
// 模型保留在列表里，否则那一行取消不掉。
func (s *Server) apiModelOptions(r *http.Request, cfg store.KeyAPIModelConfig) ([]apiModelOption, error) {
	rows, err := s.st.ListModelsWithSources(r.Context())
	if err != nil {
		return nil, err
	}
	agents := s.agentCatalogIndex(r.Context())
	doc, _ := s.effectivePlatformModels(r.Context())
	devOptions, err := s.devToolResolver().ModelOptions(r.Context(), cfg.KeyID)
	if err != nil {
		return nil, err
	}
	devByID := make(map[int64]devtoolpolicy.ModelOption, len(devOptions))
	for _, option := range devOptions {
		devByID[option.ID] = option
	}
	selected := make(map[int64]bool, len(cfg.ModelIDs))
	for _, id := range cfg.ModelIDs {
		selected[id] = true
	}
	out := make([]apiModelOption, 0, len(rows))
	for i := range rows {
		m := &rows[i]
		inDomain := apiDomainModel(agents, m)
		dev, hasDev := devByID[m.ID]
		devSelected := hasDev && dev.Selected
		if !inDomain && !selected[m.ID] && !devSelected {
			continue
		}
		platforms := make([]string, 0, len(m.Sources))
		for _, src := range m.Sources {
			label := catalogPlatformLabel(doc, src.UpstreamCatalogID, src.UpstreamType)
			if label != "" && !slices.Contains(platforms, label) {
				platforms = append(platforms, label)
			}
		}
		servable, reason := apiModelServable(m)
		if !inDomain {
			servable, reason = false, "该模型现由开发工具订阅承载，不能经 API 调用"
		}
		option := apiModelOption{
			ID: m.ID, Name: m.Name, Kind: m.Kind, Platforms: platforms,
			Selected: selected[m.ID], Selectable: inDomain || selected[m.ID],
			Servable: servable, Reason: reason,
			DevTools: []string{}, DevToolSelected: devSelected,
		}
		if hasDev {
			option.DevTools = append(option.DevTools, dev.Tools...)
			if dev.Selected && !dev.Available {
				option.DevToolReason = dev.Reason
			}
		}
		out = append(out, option)
	}
	return out, nil
}

func (s *Server) apiModelsResponse(r *http.Request, keyID int64) (apiModelsResponse, error) {
	cfg, err := s.st.GetKeyAPIModelConfig(r.Context(), keyID)
	if err != nil {
		return apiModelsResponse{}, err
	}
	devCfg, err := s.st.GetDevToolConfig(r.Context(), keyID)
	if err != nil {
		return apiModelsResponse{}, err
	}
	options, err := s.apiModelOptions(r, cfg)
	if err != nil {
		return apiModelsResponse{}, err
	}
	return apiModelsResponse{
		Revision:        cfg.Revision,
		Restricted:      cfg.Restricted,
		ModelIDs:        cfg.ModelIDs,
		DevToolModelIDs: devCfg.CatalogModelIDs,
		Models:          options,
	}, nil
}

func (s *Server) handleGetKeyAPIModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	resp, err := s.apiModelsResponse(r, id)
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

func (s *Server) handlePutKeyAPIModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Key 不存在")
		return
	}
	var req struct {
		Restricted      bool    `json:"restricted"`
		ModelIDs        []int64 `json:"model_ids"`
		DevToolModelIDs []int64 `json:"dev_tool_model_ids"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ids := append([]int64(nil), req.ModelIDs...)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > store.MaxKeyAPIModels {
		writeError(w, http.StatusBadRequest, "too_many_models",
			fmt.Sprintf("最多选择 %d 个模型", store.MaxKeyAPIModels))
		return
	}
	devIDs := append([]int64(nil), req.DevToolModelIDs...)
	slices.Sort(devIDs)
	devIDs = slices.Compact(devIDs)
	if len(devIDs) > store.MaxDevToolModels {
		writeError(w, http.StatusBadRequest, "too_many_models",
			fmt.Sprintf("开发工具可见的模型最多选择 %d 个", store.MaxDevToolModels))
		return
	}
	current, err := s.st.GetKeyAPIModelConfig(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	currentDev, err := s.st.GetDevToolConfig(r.Context(), id)
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	old := make(map[int64]bool, len(current.ModelIDs))
	for _, modelID := range current.ModelIDs {
		old[modelID] = true
	}
	// 候选闸门：新勾选的必须是**存在的 API密钥接入 模型**。已经在策略里的旧
	// 选择照收——模型可能在保存后被改成订阅接入承载，那时整份替换不该失败。
	rows, err := s.st.ListModelsWithSources(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	agents := s.agentCatalogIndex(r.Context())
	byID := make(map[int64]*store.ModelWithSources, len(rows))
	for i := range rows {
		byID[rows[i].ID] = &rows[i]
	}
	allowed := make(map[int64]bool, len(ids))
	for _, modelID := range ids {
		if modelID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_model", "模型 ID 必须为正整数")
			return
		}
		model, found := byID[modelID]
		if !found {
			writeError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("模型 %d 不存在", modelID))
			return
		}
		if !old[modelID] && !apiDomainModel(agents, model) {
			writeError(w, http.StatusBadRequest, "subscription_model",
				fmt.Sprintf("模型 %s 由开发工具订阅承载，不能用于 API 调用", model.Name))
			return
		}
		allowed[modelID] = true
	}
	// 开发工具可见集合：候选门与旧 dev-tools 端点同规（新增必须当前兼容），
	// 收窄时还必须落在可用集合内。
	oldDev := make(map[int64]bool, len(currentDev.CatalogModelIDs))
	for _, modelID := range currentDev.CatalogModelIDs {
		oldDev[modelID] = true
	}
	devOptions, err := s.devToolResolver().ModelOptions(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	devByID := make(map[int64]devtoolpolicy.ModelOption, len(devOptions))
	for _, option := range devOptions {
		devByID[option.ID] = option
	}
	for _, modelID := range devIDs {
		if modelID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_model", "模型 ID 必须为正整数")
			return
		}
		option, found := devByID[modelID]
		if !found {
			if _, err := s.st.GetModelByID(r.Context(), modelID); errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("模型 %d 不存在", modelID))
				return
			} else if err != nil {
				s.internalError(w, r, err)
				return
			}
			writeError(w, http.StatusBadRequest, "invalid_model", fmt.Sprintf("模型 %d 不是文本开发工具模型", modelID))
			return
		}
		if !oldDev[modelID] && !option.Available {
			writeError(w, http.StatusBadRequest, "model_incompatible",
				fmt.Sprintf("模型 %s 当前不兼容 Codex、Claude Code 或 OpenCode", option.Name))
			return
		}
		if req.Restricted && !allowed[modelID] {
			writeError(w, http.StatusBadRequest, "dev_tool_outside_scope",
				fmt.Sprintf("模型 %s 未勾选为可用，不能设为开发工具可见", option.Name))
			return
		}
	}

	// 两份配置各自成事务、按序落库：校验都在前面做完，第二笔只剩库故障一种
	// 失败可能；真失败时响应 500，GET 回读即是真相，重试即可收敛。
	next, changed, err := s.st.ReplaceKeyAPIModelConfig(r.Context(), store.KeyAPIModelConfig{
		KeyID: id, Restricted: req.Restricted, ModelIDs: ids,
	})
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	if changed {
		s.audit(r.Context(), store.AuditEvent{
			Event: EventKeyAPIModelsUpdate, Entity: entityKey(id), RemoteIP: remoteIP(r),
			Detail: fmt.Sprintf("restricted=%t model_ids=%v revision=%d",
				next.Restricted, next.ModelIDs, next.Revision),
		})
	}
	nextDev, devChanged, err := s.st.ReplaceDevToolConfig(r.Context(), store.DevToolConfig{
		KeyID:                   id,
		AllowCodexSubscription:  currentDev.AllowCodexSubscription,
		AllowGrokSubscription:   currentDev.AllowGrokSubscription,
		AllowClaudeSubscription: currentDev.AllowClaudeSubscription,
		AllowCursorSubscription: currentDev.AllowCursorSubscription,
		CatalogModelIDs:         devIDs,
	})
	if err != nil {
		s.writeKeyError(w, r, err)
		return
	}
	if devChanged {
		s.audit(r.Context(), store.AuditEvent{
			Event: EventKeyDevToolsUpdate, Entity: entityKey(id), RemoteIP: remoteIP(r),
			Detail: fmt.Sprintf("codex=%t grok=%t claude=%t cursor=%t model_ids=%v revision=%d",
				nextDev.AllowCodexSubscription, nextDev.AllowGrokSubscription, nextDev.AllowClaudeSubscription,
				nextDev.AllowCursorSubscription, nextDev.CatalogModelIDs, nextDev.Revision),
		})
	}
	resp, err := s.apiModelsResponse(r, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
