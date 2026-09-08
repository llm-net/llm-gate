package gateway

// routing.go 是数据面选路组装（iteration-5 决策 3/4）：每请求一次
// store.ResolveModelRoute 三表点查（无缓存——启停、优先级、换 Key 都在下一个
// 请求即时生效），把库里的候选来源裁决成本入口可用的有序 [candidate] 列表。
//
// 裁决口径（与 /v1/models 的 ListServableModels 对齐）：
//
//   - 模型的 kind 与入口所需不符（迭代 8 闸门：文本入口只收 text，视频/图片
//     入口同理）→ routeModelNotFound——对客户端与「不存在」完全同响应，
//     不解释内部原因，kind 闸门因此双向（视频模型进不了 chat，反之亦然）；
//   - 模型不存在、模型停用、或没有任何「启用来源且其上游启用」的候选
//     → routeModelNotFound（404 model_not_found）；
//   - 本入口不可服务（模型的调用入口开关关着，或启用候选都不服务本入口
//     协议——如只挂 ark 型来源的模型进 /v1/messages），而另一入口开着且有
//     能服务它的启用候选 → routeProtocolMismatch（404 protocol_mismatch，
//     消息指向另一入口）——这个口径只在文本种类内有意义（chat ↔ messages
//     互指），且从不指向一个同样调不通的入口；
//   - 库故障 → routeStoreError（500，只记错误文本不记参数）。
//
// 客户端可见名就是模型的原始名（iteration-5 反转 §11.1 的对客隐藏决策）：
// 上游账户名、来源侧模型 ID、来源数量一律不进**网关自身**的错误信息与日志之外
// 的响应。上游 4xx/5xx 的错误体仍按透传契约原样回写（AGENTS.md「错误体从不
// 改写」），其中若含上游侧模型 ID 属于上游自己的措辞，不在本层遮蔽范围内。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// candidate 是一条已通过启停与入口协议过滤的候选来源，按
// (priority, 订阅先行, id) 有序——同优先级下订阅套餐型上游（ark_plan/qwen_plan/opencode_go）先于
// 按量付费（排序由 store 的 billingRankSQL 钉死，此处照单全收）。
type candidate struct {
	account upstream.Account
	// catalogID 是数据目录里的具体平台身份，供 Responses/Codex 能力解析；
	// 端点仍由 account 中已快照的 type/base_url 决定。
	catalogID string
	// modelID 是该来源侧的模型 ID（请求体 model 的改写目标）。库中留空时
	// store 的路由视图已解析为模型名。
	modelID string
	// sourceID 只用于诊断日志，不外泄给客户端。
	sourceID int64
}

// routeStatus 是选路裁决结果。
type routeStatus int

const (
	routeOK routeStatus = iota
	routeModelNotFound
	routeProtocolMismatch
	routeStoreError
)

// fetchModelRoute 执行三表点查并统一错误口径（未命中/客户端断开/库故障），
// 候选裁决留给调用方（文本入口 resolveRoute 与视频入口 resolveVideoRoute
// 共用这一段）。
func (s *Server) fetchModelRoute(ctx context.Context, model string) (*store.ModelRoute, routeStatus) {
	route, err := s.store.ResolveModelRoute(ctx, model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, routeModelNotFound
		}
		// 客户端中途断开会让点查返回 context.Canceled：那是客户端行为不是库
		// 故障，记 Info 即可（错误体也写不回一个已断开的连接）。
		if ctx.Err() != nil {
			s.log.Info("客户端断开，模型路由点查已取消", "request_id", infoFrom(ctx).id, "model", model)
			return nil, routeStoreError
		}
		// store 的错误只含约束/列名与解密失败原因，不含凭证与请求内容。
		s.log.Error("解析模型路由失败", "request_id", infoFrom(ctx).id, "model", model, "err", err.Error())
		return nil, routeStoreError
	}
	// 记账维度里两项来自模型行：kind（入口与 kind 多对一，账本按 kind 分解）
	// 与目录价原文——**记账时点价**就是在这里定下来的，此后改价不影响本请求
	// （iteration-9，metering.go）。modelKnown 同时立起来：点查命中就说明这个
	// 名字在目录里真有一行（停用与否都算），账本的维度基数封顶据此放行它。
	info := infoFrom(ctx)
	info.bill.kind, info.bill.pricing = route.Model.Kind, route.Model.Pricing
	info.bill.modelKnown = true
	return route, routeOK
}

// resolveRoute 按客户端请求的模型名、入口所需的模型种类与入口协议取有序候选
// 来源。返回的候选非空当且仅当 status == routeOK。
func (s *Server) resolveRoute(ctx context.Context, model, kind, protocol string) ([]candidate, routeStatus) {
	route, status := s.fetchModelRoute(ctx, model)
	if status != routeOK {
		return nil, status
	}
	// kind 闸门先于一切候选裁决：种类不符的模型对本入口就是不存在，连
	// protocol_mismatch 的提示都不给——那会泄露「这个名字在别的入口活着」。
	if route.Model.Kind != kind {
		return nil, routeModelNotFound
	}
	if route.Model.Disabled {
		return nil, routeModelNotFound
	}
	// API模型策略（单把 Key 的可调模型范围，apimodels.go）：不在范围里的模型
	// 与「不存在」同响应，位置与启停闸门并列——它同样是"这个名字对你不存在"。
	if ok, status := s.keyAPIModelAllowed(ctx, route.Model.ID); !ok {
		return nil, status
	}

	// 文本协议面各自开关，来源能力按实际的上游协议过滤。
	entryOn := kind != store.ModelKindText || textEntryOn(route.Model, protocol)
	doc := s.effectivePlatformModels(ctx)

	var (
		cands             []candidate
		servableElsewhere bool // 另一入口开着且有启用候选服务它 → 口径是 protocol_mismatch
	)
	for _, c := range route.Candidates {
		if c.SourceDisabled || c.Upstream.Disabled {
			continue
		}
		acct := upstream.Account{
			Name:       c.Upstream.Name,
			Type:       c.Upstream.Type,
			APIKey:     c.Upstream.APIKey,
			BaseURL:    c.Upstream.BaseURL,
			EgressMode: c.Upstream.EgressMode,
		}
		if kind == store.ModelKindText && !servableElsewhere {
			for _, other := range []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropicMessages} {
				if other != protocol && textEntryOn(route.Model, other) {
					if _, ok := acct.ModelEndpoint(doc, c.Upstream.CatalogID, model, c.UpstreamModelID, upstream.CatalogWireProtocol(other)); ok {
						servableElsewhere = true
					}
				}
			}
		}

		if !entryOn {
			continue
		}
		if _, ok := acct.ModelEndpoint(doc, c.Upstream.CatalogID, model, c.UpstreamModelID, upstream.CatalogWireProtocol(protocol)); !ok {
			continue
		}
		cands = append(cands, candidate{account: acct, catalogID: c.Upstream.CatalogID, modelID: c.UpstreamModelID, sourceID: c.SourceID})
	}
	if len(cands) > 0 {
		return cands, routeOK
	}
	if servableElsewhere {
		return nil, routeProtocolMismatch
	}
	// 无任何启用来源、本入口开关关着而另一入口也活不了、或仅有的启用来源
	// 两个协议都不服务（如 mock 上游缺 base_url）：对客一律"模型不存在"，
	// 不解释内部原因。
	return nil, routeModelNotFound
}

// textEntryOn 读文本模型的调用入口开关（仅对 ModelKindText 调用；开关对
// 视频/图片种类无语义）。
func textEntryOn(m store.Model, protocol string) bool {
	switch protocol {
	case config.ProtocolOpenAIChat:
		return m.EntryOpenAI
	case config.ProtocolOpenAIResponses:
		return m.EntryResponses
	case config.ProtocolAnthropicMessages:
		return m.EntryAnthropic
	default:
		return false
	}
}

// listServableModels 在存储层启停筛选后应用实际来源侧模型的协议声明，避免
// 原生 Responses-only 或已无可用协议的来源继续出现在 /v1/models。
func (s *Server) listServableModels(ctx context.Context) ([]store.Model, error) {
	models, err := s.store.ListServableModels(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := s.store.ListServableModelSources(ctx)
	if err != nil {
		return nil, err
	}
	doc := s.effectivePlatformModels(ctx)
	available := make(map[int64]bool)
	for _, src := range sources {
		acct := upstream.Account{Type: src.UpstreamType, BaseURL: src.UpstreamBaseURL}
		_, chatOK := acct.ModelEndpoint(doc, src.UpstreamCatalogID, src.ModelName, src.UpstreamModelID, config.ProtocolOpenAIChat)
		_, messagesOK := acct.ModelEndpoint(doc, src.UpstreamCatalogID, src.ModelName, src.UpstreamModelID, config.ProtocolAnthropicMessages)
		if (chatOK && (src.EntryOpenAI || src.EntryResponses)) || (messagesOK && src.EntryAnthropic) {
			available[src.ModelID] = true
		}
	}
	out := models[:0]
	for _, m := range models {
		if available[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

// protocolMismatchMessage 指向可检查的其他协议面，不承诺它们全部可调用。
func protocolMismatchMessage(model, protocol string) string {
	paths := []string{}
	for _, entry := range []struct{ protocol, path string }{
		{config.ProtocolOpenAIChat, "/v1/chat/completions"},
		{config.ProtocolOpenAIResponses, "/v1/responses"},
		{config.ProtocolAnthropicMessages, "/v1/messages"},
	} {
		if entry.protocol != protocol {
			paths = append(paths, "POST "+entry.path)
		}
	}
	return fmt.Sprintf("The model %q is unavailable on this protocol surface. Check its available protocol surfaces: %s.", model, strings.Join(paths, ", "))
}
