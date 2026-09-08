// responses_codex.go 是 Codex 的接入面 /agents/codex/v1/*（gate 把 Codex 的
// base_url 指到这里）：同一个 provider 配置里，管理员为这把 Key 勾选的普通目录
// 模型走目录 Responses 面（responses_catalog.go），其余名字走 Codex 订阅代理
// （responses.go，provider 钉死为 codex）。同一 base_url 还提供两类模型的按 Key
// 并集，gate 将对应能力元数据写进独立 model_catalog_json，适配不读取自定义
// provider /models 的 CLI。模型选择只来自服务端 Key 策略；旧 llmgate_catalog
// 参数非空即拒绝。/codex/v1/* 是同一组处理器的兼容别名（server.go）。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

const codexCatalogQuery = "llmgate_catalog"

// Client supplied model selections are no longer accepted. The authenticated
// API key's saved device policy is the sole selection source.
func rejectCodexCatalogQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get(codexCatalogQuery) != "" {
		openAIErrorStyle(w, http.StatusBadRequest, "invalid_catalog_models",
			"The llmgate_catalog query parameter is not accepted; update the gate client and use the API key policy configured on the device.")
		return false
	}
	return true
}

// handleCodexResponses 按 Key 策略分流。勾选项优先走目录面；未勾选
// 的名字保持订阅入口原有行为（含上游新模型尚未来得及进入设备目录的情形）。
func (s *Server) handleCodexResponses(w http.ResponseWriter, r *http.Request) {
	if !rejectCodexCatalogQuery(w, r) {
		return
	}
	payload, model, ok := decodeEntryPayload(w, r, openAIErrorStyle)
	if !ok {
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	if snapshot.IsCatalogModel("codex", model) {
		// 勾选项来自开发工具策略，可用模型已在那里裁决过：中转到目录面之前
		// 立起订阅面标记，避免再被 API模型策略收窄一次（apimodels.go）。
		markAgentSurface(r.Context())
		s.handleResponsesCatalogPayload(w, r, payload, model)
		return
	}
	sub := snapshot.Subscription("codex")
	if !sub.Configured {
		openAIErrorStyle(w, http.StatusForbidden, "subscription_not_allowed",
			"This API key is not allowed to use the Codex subscription.")
		return
	}
	if sub.Available && !snapshot.HasModel("codex", model) {
		openAIErrorStyle(w, http.StatusNotFound, "model_not_found", "The requested model is not available to this API key.")
		return
	}
	// 同一会话先前由目录面作答过的话，Codex 回放的历史里带着目录面自造的
	// reasoning item；它们对订阅后端没有意义且会被整请求拒绝，转发前摘掉。
	// 没有命中时 payload 一字不动（见 dropCatalogReasoningItems）。
	if n := dropCatalogReasoningItems(payload); n > 0 {
		s.log.Debug("摘掉目录面合成的 reasoning item 后转发 Codex 订阅", "request_id", infoFrom(r.Context()).id, "dropped", n)
	}
	s.handleAgentResponsesFor(w, r, store.AgentProviderCodex, payload, model)
}

// dropCatalogReasoningItems 从 Responses 请求的 input 里摘掉**设备目录面自己
// 合成**的 reasoning item：type 为 reasoning、id 带 catalogReasoningIDPrefix、
// 且没有 encrypted_content。这是同一会话从目录模型切到订阅模型时唯一会让
// OpenAI 整请求 404 的东西（"Item with id 'rs_resp_…' not found. Items are not
// persisted when store is set to false"）——目录模型的推理摘要本来就无法在别家
// 后端回放，丢掉不损失可用上下文。
//
// 刻意收得很窄：只认设备自己的命名空间，别的 reasoning item（含 OpenAI 自己
// 带 encrypted_content 的）与其他类型条目一个都不碰；input 不是数组、条目不是
// 对象也原样留下。**一个都没命中就不替换 input**，未受影响的请求转发字节与
// 以前完全一致。返回摘掉的条数，只用于日志计数（§15.1：不记内容）。
func dropCatalogReasoningItems(payload map[string]any) int {
	items, ok := payload["input"].([]any)
	if !ok {
		return 0
	}
	kept := make([]any, 0, len(items))
	dropped := 0
	for _, raw := range items {
		if isCatalogReasoningItem(raw) {
			dropped++
			continue
		}
		kept = append(kept, raw)
	}
	if dropped > 0 {
		payload["input"] = kept
	}
	return dropped
}

func isCatalogReasoningItem(raw any) bool {
	item, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	if typ, _ := item["type"].(string); typ != "reasoning" {
		return false
	}
	if id, _ := item["id"].(string); !strings.HasPrefix(id, catalogReasoningIDPrefix) {
		return false
	}
	if enc, _ := item["encrypted_content"].(string); enc != "" {
		return false
	}
	return true
}

// handleCodexModels 返回「订阅模型 + 该 Key 勾选且仍可从 OpenAI 入口调用
// 的普通模型」。会读取 provider 模型端点的客户端直接使用它；gate 另行
// 生成本地 model_catalog_json 供 Codex /model 使用。普通模型状态变化即时生效；
// 失效项从列表消失，重跑安装命令则可更新勾选集合。
func (s *Server) handleCodexModels(w http.ResponseWriter, r *http.Request) {
	if !rejectCodexCatalogQuery(w, r) {
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	list := modelList{Object: "list", Data: []modelObject{}}
	for _, model := range snapshot.Tools["codex"].Models {
		owned := store.AgentProviderCodex
		if model.Source == "catalog" {
			owned = "llmgate"
		}
		list.Data = append(list.Data, modelObject{ID: model.Name, Object: "model", OwnedBy: owned})
	}
	sort.Slice(list.Data, func(i, j int) bool { return list.Data[i].ID < list.Data[j].ID })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// selectedCodexCatalogModels 从目录读数中圈出仍有 OpenAI Responses 协议面的勾选项。
// 两次小表查询与管理面的 endpoints 快照同量级；不解封凭据、不读请求正文。
func (s *Server) selectedCodexCatalogModels(ctx context.Context, selected map[string]struct{}) ([]modelObject, error) {
	models, err := s.store.ListServableModels(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := s.store.ListServableModelSources(ctx)
	if err != nil {
		return nil, err
	}
	openAI := make(map[string]bool, len(selected))
	doc := s.effectivePlatformModels(ctx)
	for _, row := range sources {
		if _, wanted := selected[row.ModelName]; !wanted || !row.EntryResponses {
			continue
		}
		acct := upstream.Account{Type: row.UpstreamType, BaseURL: row.UpstreamBaseURL}
		if _, ok := acct.ModelEndpoint(doc, row.UpstreamCatalogID, row.ModelName, row.UpstreamModelID, config.ProtocolOpenAIChat); ok {
			openAI[row.ModelName] = true
		}
	}
	out := make([]modelObject, 0, len(selected))
	for _, m := range models {
		if !openAI[m.Name] {
			continue
		}
		out = append(out, modelObject{
			ID:      m.Name,
			Object:  "model",
			Created: m.CreatedAt.Unix(),
			OwnedBy: "llmgate",
		})
	}
	return out, nil
}

type codexLocalCatalog struct {
	Models []codexLocalModel `json:"models"`
	// SubscriptionOverlay 是给订阅模型条目的修正片（见类型注释）。旧版 gate
	// 只解 models，这一键对它们透明。
	SubscriptionOverlay codexSubscriptionOverlay `json:"subscription_overlay"`
}

// codexSubscriptionOverlay 是设备对**订阅模型**目录条目的修正片。订阅模型的
// 元数据来自 Codex CLI 自带的 bundled 目录，那份目录按 OpenAI 自家后端写：
// 有些字段对经设备转发的请求并不成立，有些字段旧内核必填而新版 CLI 不再输出。
// 「这台设备的订阅面能承受什么」只有固件自己知道，所以修正片由固件代码给出，
// 不走数据升级。gate 把它套到每一条从 bundled 取来的订阅模型条目上，绝不碰
// models 里的目录模型条目（那些本来就是设备生成的）：
//
//	set       逐键覆盖（无论原值）
//	default   仅在条目没有该键时补上
//	experimental_supported_tools_add   追加到 experimental_supported_tools，去重
//
// 当前值：
//
//	use_responses_lite=false     设备只提供标准 /responses 一种 wire；bundled 里
//	                             gpt-5.6-sol/luna 等条目硬编码 true，对任何非
//	                             ChatGPT 后端的 provider 都会 400（openai/codex#31882）。
//	supports_reasoning_summaries / supports_parallel_tool_calls 缺省 false
//	                             旧内核（如 ChatGPT 桌面版内置的 0.144.x）必填，
//	                             新版 CLI 的 bundled 不再输出；只补缺省，不改已有值。
//	experimental_supported_tools += image_gen
//	                             条目声明内置画图工具可用，对应设备画图门
//	                             （images_codex.go）。内核是否真的挂出 image_gen
//	                             不看这一键，看 provider 的 http_headers 里有没有
//	                             非空 x-openai-actor-authorization（gate 写入，
//	                             设备转发上游前剥掉，见 responses.go）。
type codexSubscriptionOverlay struct {
	Set      map[string]any `json:"set"`
	Default  map[string]any `json:"default"`
	ToolsAdd []string       `json:"experimental_supported_tools_add"`
}

// codexSubscriptionOverlayFor 是本固件当前给出的修正片（每次调用新建，调用方
// 可自由编码）。
func codexSubscriptionOverlayFor() codexSubscriptionOverlay {
	return codexSubscriptionOverlay{
		Set: map[string]any{"use_responses_lite": false},
		Default: map[string]any{
			"supports_reasoning_summaries": false,
			"supports_parallel_tool_calls": false,
		},
		ToolsAdd: []string{"image_gen"},
	}
}

type codexLocalModel struct {
	Slug                     string                                `json:"slug"`
	DisplayName              string                                `json:"display_name"`
	Description              string                                `json:"description"`
	DefaultReasoningLevel    string                                `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels []platformcatalog.CodexReasoningLevel `json:"supported_reasoning_levels"`
	ShellType                string                                `json:"shell_type"`
	Visibility               string                                `json:"visibility"`
	SupportedInAPI           bool                                  `json:"supported_in_api"`
	Priority                 int                                   `json:"priority"`
	BaseInstructions         string                                `json:"base_instructions"`
	SupportVerbosity         bool                                  `json:"support_verbosity"`
	TruncationPolicy         struct {
		Mode  string `json:"mode"`
		Limit int    `json:"limit"`
	} `json:"truncation_policy"`
	ExperimentalSupportedTools []string `json:"experimental_supported_tools"`
	InputModalities            []string `json:"input_modalities"`
}

// handleCodexModelCatalog 把设备当前生效的数据能力目录渲染成 Codex CLI 可直接
// 合并的本地模型条目。helper 不再按模型名猜 effort；数据升级后的能力在重跑接入
// 命令时进入 model_catalog_json。
func (s *Server) handleCodexModelCatalog(w http.ResponseWriter, r *http.Request) {
	if !rejectCodexCatalogQuery(w, r) {
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	selected := make(map[string]struct{})
	for _, model := range snapshot.Tools["codex"].Models {
		if model.Source == "catalog" {
			selected[model.Name] = struct{}{}
		}
	}
	models, err := s.selectedCodexLocalCatalog(r.Context(), selected)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Warn("生成 Codex 本地模型目录失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		}
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error", internalErrorMessage)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(codexLocalCatalog{Models: models, SubscriptionOverlay: codexSubscriptionOverlayFor()})
}

func (s *Server) selectedCodexLocalCatalog(ctx context.Context, selected map[string]struct{}) ([]codexLocalModel, error) {
	models, err := s.store.ListServableModels(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := s.store.ListServableModelSources(ctx)
	if err != nil {
		return nil, err
	}
	doc := s.effectivePlatformModels(ctx)
	openAI := make(map[string]bool, len(selected))
	capabilities := make(map[string]platformcatalog.CodexCapabilities, len(selected))
	for _, row := range sources {
		if _, wanted := selected[row.ModelName]; !wanted || !row.EntryResponses {
			continue
		}
		acct := upstream.Account{Type: row.UpstreamType, BaseURL: row.UpstreamBaseURL}
		if _, ok := acct.ModelEndpoint(doc, row.UpstreamCatalogID, row.ModelName, row.UpstreamModelID, config.ProtocolOpenAIChat); !ok {
			continue
		}
		openAI[row.ModelName] = true
		if _, set := capabilities[row.ModelName]; set {
			continue
		}
		caps, _ := doc.ModelCapabilitiesFor(row.UpstreamCatalogID, row.UpstreamType, row.ModelName, row.UpstreamModelID)
		capabilities[row.ModelName] = caps.Codex
	}
	out := make([]codexLocalModel, 0, len(selected))
	for _, m := range models {
		if !openAI[m.Name] {
			continue
		}
		cap := capabilities[m.Name]
		row := codexLocalModel{
			Slug: m.Name, DisplayName: m.Name, Description: "LLM Gate catalog model",
			DefaultReasoningLevel:    cap.DefaultReasoningLevel,
			SupportedReasoningLevels: cap.SupportedReasoningLevels,
			ShellType:                "shell_command", Visibility: "list", SupportedInAPI: true, Priority: 100,
			BaseInstructions:           "You are Codex, a coding agent. Follow the user instructions and use the available tools to work in the current repository.",
			ExperimentalSupportedTools: []string{}, InputModalities: []string{"text"},
		}
		if row.SupportedReasoningLevels == nil {
			row.SupportedReasoningLevels = []platformcatalog.CodexReasoningLevel{}
		}
		row.TruncationPolicy.Mode = "bytes"
		row.TruncationPolicy.Limit = 10000
		out = append(out, row)
	}
	return out, nil
}
