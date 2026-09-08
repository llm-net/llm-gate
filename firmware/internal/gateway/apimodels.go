package gateway

// apimodels.go 是「API模型」策略在数据面的闸门：单把客户端 Key 经**厂商兼容
// API 面**（/v1/chat/completions、/v1/messages、/v1/responses 与 AIGC 官方路径
// 那一圈）能调哪些目录模型。管理端契约见 internal/admin/apimodels.go。
//
// 缺省不限制——没有配置行或 restricted=0 的 Key 照旧能调全部 API 模型，既有
// 部署升上来行为不变。restricted=1 时不在选择里的模型一律按 model_not_found
// 处置：与「这个名字在目录里不存在」完全同响应，不给探测面（§11.1）。
//
// 订阅与开发工具面（/agents/*、/codex/*、/claude/*）**不受本策略约束**：那些
// 面的可用模型由开发工具策略（devtoolpolicy）逐 Key 裁决，两份策略互不收窄。
// 它们中转到共享目录 handler 时先立 agentSurface 标记，见各自的分流点。
//
// 每请求点查、无缓存层：与 Key 鉴权同一决策，改策略下一个请求即时生效。

import "context"

// keyAPIModelAllowed 判定当前请求的 Key 能否经 API 面调用某个目录模型。
// 返回的 routeStatus 只在拒绝时有意义：策略拒绝 → routeModelNotFound，
// 库故障 → routeStoreError。
func (s *Server) keyAPIModelAllowed(ctx context.Context, modelID int64) (bool, routeStatus) {
	info := infoFrom(ctx)
	// keyID 为 0 = 没有认证身份（链路外直调 handler 的测试兜底）；订阅面的
	// 请求由分流点标记豁免，它们的模型集合另有裁决。
	if info.keyID == 0 || info.agentSurface {
		return true, routeOK
	}
	scope, err := s.store.LookupKeyAPIModelScope(ctx, info.keyID)
	if err != nil {
		if ctx.Err() != nil { // 客户端中途断开不是库故障
			s.log.Info("客户端断开，API模型策略点查已取消", "request_id", info.id)
			return false, routeStoreError
		}
		// store 的错误只含约束/列名，不含请求内容与凭证。
		s.log.Error("查询API模型策略失败", "request_id", info.id, "key_id", info.keyID, "err", err.Error())
		return false, routeStoreError
	}
	if !scope.Allows(modelID) {
		return false, routeModelNotFound
	}
	return true, routeOK
}

// markAgentSurface 声明本请求走的是订阅/开发工具面，随后中转到共享目录
// handler 的选路不再套用 API模型策略。
func markAgentSurface(ctx context.Context) { infoFrom(ctx).agentSurface = true }
