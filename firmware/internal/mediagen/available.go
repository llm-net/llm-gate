package mediagen

import (
	"context"
	"errors"
	"sort"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// Availability 是一个模型对一把 Key 的可用性裁决：能力表项 + 能不能用、为什么不能、订阅后端
// 钉死的账号行。哪把 Key 能用哪个后端只在这里判，页面、创作工作空间与 gate media 同一份。
type Availability struct {
	Model
	Available  bool   `json:"available"`
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason" i18n:"text"`
	// AccountID 是订阅后端按这把 Key 的开发工具策略钉死的订阅账号行；按量后端为 0
	// （上游账户在生成时选路、受理后钉在 aigc_tasks 行里）。
	AccountID int64 `json:"-"`
}

// 不可用的原因码（与数据面同名的错误码同一份）。
const (
	ReasonSubscriptionNotAllowed = "subscription_not_allowed"
	ReasonAgentNotConfigured     = "agent_not_configured"
	ReasonModelNotFound          = "model_not_found"
	ReasonBackendUnavailable     = "media_unavailable"
)

// Entitlement 是一把 Key 对一种订阅的授权读数（开发工具策略快照的投影）。
type Entitlement struct {
	Configured bool
	Available  bool
	AccountID  int64
}

// Entitlements 按 Key 解析各订阅的授权（键是 store.AgentProvider*）。生产实现是开发工具策略
// 快照——与数据面 /agents/<tool>/ 同一份判据。
type Entitlements func(ctx context.Context, keyID int64) (map[string]Entitlement, error)

// Catalog 返回能力表：订阅后端的固定预设 + 设备上现有的厂商面模型行（不看任何 Key）。
func (s *Service) Catalog(ctx context.Context) ([]Model, error) {
	out := Presets()
	rows, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if m, _, ok := familyOf(&rows[i]); ok {
			if _, clash := Preset(m.ID); !clash {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// Available 逐模型给出这把 Key 的可用性。订阅后端按这把 Key 的开发工具策略（未钉账号 =
// 未获授权，钉了但账号停用 / 失效 = 当前不可用）；按量后端按这把 Key 的可用模型范围与
// 模型 / 来源 / 上游三层启停——范围外的模型与不存在一样，不进结果。
func (s *Service) Available(ctx context.Context, keyID int64) ([]Availability, error) {
	var ents map[string]Entitlement
	if s.entitlements != nil {
		var err error
		if ents, err = s.entitlements(ctx, keyID); err != nil {
			return nil, err
		}
	}
	out := make([]Availability, 0, len(presets))
	for _, m := range presets {
		a := Availability{Model: m}
		ent := ents[m.Backend]
		switch {
		case s.backends[m.Backend] == nil:
			a.ReasonCode, a.Reason = ReasonBackendUnavailable, "生成服务不可用"
		case !ent.Configured:
			a.ReasonCode, a.Reason = ReasonSubscriptionNotAllowed, "这把 API 密钥未获授权使用该订阅"
		case !ent.Available:
			a.ReasonCode, a.Reason = ReasonAgentNotConfigured, "该订阅当前在设备上不可用"
		default:
			a.Available, a.AccountID = true, ent.AccountID
		}
		out = append(out, a)
	}
	rows, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := s.st.LookupKeyAPIModelScope(ctx, keyID)
	if err != nil {
		return nil, err
	}
	var metered []Availability
	for i := range rows {
		m, usable, ok := familyOf(&rows[i])
		if !ok || !scope.Allows(rows[i].ID) {
			continue
		}
		if _, clash := Preset(m.ID); clash {
			continue
		}
		a := Availability{Model: m}
		switch {
		case s.backends[m.Backend] == nil:
			a.ReasonCode, a.Reason = ReasonBackendUnavailable, "生成服务不可用"
		case rows[i].Disabled || !usable:
			a.ReasonCode, a.Reason = ReasonModelNotFound, "模型已停用或没有可用的来源"
		default:
			a.Available = true
		}
		metered = append(metered, a)
	}
	sort.SliceStable(metered, func(i, j int) bool {
		if metered[i].Kind != metered[j].Kind {
			return metered[i].Kind < metered[j].Kind
		}
		return metered[i].ID < metered[j].ID
	})
	return append(out, metered...), nil
}

// ErrModelNotFound：模型不在这把 Key 看得到的能力表里。
var ErrModelNotFound = errors.New("模型不存在或这把 API 密钥无权使用")

// Resolve 取一个模型对这把 Key 的可用性；不在表里返回 ErrModelNotFound。
func (s *Service) Resolve(ctx context.Context, keyID int64, model string) (Availability, error) {
	all, err := s.Available(ctx, keyID)
	if err != nil {
		return Availability{}, err
	}
	for _, a := range all {
		if a.ID == model {
			return a, nil
		}
	}
	return Availability{}, ErrModelNotFound
}

// familyOf 判一条共享 API 模型行属于哪个厂商面后端：图像看来源的上游是否服务 ark_image，
// 视频看 ark_video / minimax_video（同一模型的全部来源必须同面，取到第一条即可）。usable
// 报告是否至少有一条启用的来源（来源与上游两层都没停）。
func familyOf(row *store.ModelWithSources) (Model, bool, bool) {
	var protocols []string
	switch row.Kind {
	case store.ModelKindImage:
		protocols = []string{config.ProtocolArkImage}
	case store.ModelKindVideo:
		protocols = []string{config.ProtocolArkVideo, config.ProtocolMinimaxVideo}
	default:
		return Model{}, false, false
	}
	backend, usable := "", false
	for _, src := range row.Sources {
		for _, p := range protocols {
			if _, ok := upstream.EndpointFor(src.UpstreamType, p); !ok {
				continue
			}
			if backend == "" {
				backend = p
			}
			if p == backend && !src.Disabled && !src.UpstreamDisabled {
				usable = true
			}
		}
	}
	if backend == "" {
		return Model{}, false, false
	}
	m, ok := FamilyModel(backend, row.Name)
	return m, usable, ok
}
