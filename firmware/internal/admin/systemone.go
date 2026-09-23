package admin

import (
	"context"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type systemOneModelJSON struct {
	Name         string `json:"name"`
	API          string `json:"api"`
	ProtocolFace string `json:"protocol_face"`
}

func (s *Server) systemOneModels(ctx context.Context, scope store.KeyAPIModelScope) []systemOneModelJSON {
	models, err := s.st.ListModelsWithSources(ctx)
	if err != nil {
		s.log.Warn("读取 System One 模型清单失败", "err", err.Error())
		return nil
	}
	var out []systemOneModelJSON
	for _, m := range models {
		if m.Kind != store.ModelKindSystemOne || m.Disabled || !scope.Allows(m.ID) {
			continue
		}
		for _, src := range m.Sources {
			if !src.Disabled && !src.UpstreamDisabled && len(servableProtocols(m.Kind, src.UpstreamType, src.UpstreamBaseURL, src.UpstreamProtocolURLs)) > 0 {
				out = append(out, systemOneModelJSON{Name: m.Name, API: config.ProtocolSystemOne, ProtocolFace: config.ProtocolFaceTypeSafe})
				break
			}
		}
	}
	return out
}
