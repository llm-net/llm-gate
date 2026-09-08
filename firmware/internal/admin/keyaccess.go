package admin

// 凭 API 密钥自证的接入读数：数据面 GET /gate-helper/v1/endpoints 的执行体。
//
// 为什么执行体在管理面：地址读数（网卡枚举、明文端口、公网接入基址）与「客户端
// 现在能调哪些模型」的组装只该有一份（endpoints.go），数据面只经 gateway.KeyAccess
// 注入来取它——与 GET /agents/v1/models 取订阅模型读数是同一种接线（main.go）。
// Key 鉴权、keyID 与 no-store 都归数据面那条路由，本文件只按给定的 keyID 组装。
//
// 与管理员视角的差别只有两点：模型清单按这把 Key 的 API模型范围裁剪（范围外的
// 模型对它就是不存在，与 /v1/models 同一份谓词），以及**不带订阅读数**——按 Key 的
// 订阅可用性由 GET /gate-helper/v1/config 的 subscriptions 给出，两处各说一件事。
// 响应里没有 Key 的任何标识：地址与模型名都是设备事实，不是用户数据。

import (
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
)

// keyAccessJSON 是凭 Key 读到的接入读数：与 endpointsResponse 同源，少 agents。
type keyAccessJSON struct {
	Endpoints  endpointsJSON       `json:"endpoints"`
	Models     []servableModelJSON `json:"models"`
	AIGCModels []aigcModelJSON     `json:"aigc_models,omitempty"`
	// FirmwareVersion / HardwareModel 与管理员视角同一份（登录页铭牌也印它们，
	// 免会话可读）。
	FirmwareVersion string `json:"firmware_version"`
	HardwareModel   string `json:"hardware_model"`
}

// KeyAccessSnapshot 实现 gateway.KeyAccess：按 keyID 的 API模型范围组装接入读数。
// 范围点查失败返回错误（数据面答 5xx）——在这里猜「不限制」会把范围外的模型
// 名泄给那把 Key；地址与模型清单的读失败仍按 endpoints.go 的口径降级。
func (s *Server) KeyAccessSnapshot(r *http.Request, keyID int64) (any, error) {
	scope, err := s.st.LookupKeyAPIModelScope(r.Context(), keyID)
	if err != nil {
		return nil, fmt.Errorf("查询API模型范围: %w", err)
	}
	return keyAccessJSON{
		Endpoints:       s.endpointsSnapshot(r),
		Models:          s.servableModels(r.Context(), scope),
		AIGCModels:      s.aigcModels(r.Context(), scope),
		FirmwareVersion: buildinfo.Version,
		HardwareModel:   s.hardwareModel,
	}, nil
}
