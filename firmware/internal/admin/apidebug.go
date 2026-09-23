package admin

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/llm-net/llm-gate/firmware/internal/apidebug"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type APIDebugGateway interface {
	DebugAPI(http.ResponseWriter, *http.Request, int64)
}

func (s *Server) SetAPIDebugGateway(g APIDebugGateway) { s.apiDebug = g }

// APIDebugModels uses the same model visibility as the public endpoints. Upload
// capabilities are intersected across sources so failover cannot lose an input.
func (s *Server) APIDebugModels(ctx context.Context, keyID int64) ([]apidebug.Model, error) {
	scope, err := s.st.LookupKeyAPIModelScope(ctx, keyID)
	if err != nil {
		return nil, err
	}
	rows, err := s.st.ListServableModelSources(ctx)
	if err != nil {
		return nil, err
	}
	doc, _ := s.effectivePlatformModels(ctx)
	out := []apidebug.Model{}
	for _, m := range s.servableModels(ctx, scope) {
		model := apidebug.Model{Name: m.Name, Protocols: m.Protocols, FileTypes: map[string][]string{}}
		for _, row := range rows {
			if row.ModelName != m.Name || !scope.Allows(row.ModelID) {
				continue
			}
			caps, _ := doc.ModelCapabilitiesFor(row.UpstreamCatalogID, row.UpstreamType, row.ModelName, row.UpstreamModelID)
			for _, protocol := range sourceProtocols(doc, store.ModelKindText, row.ModelName, row.UpstreamModelID, row.UpstreamType, row.UpstreamCatalogID, row.UpstreamBaseURL, row.UpstreamProtocolURLs) {
				if !slices.Contains(m.Protocols, protocol) {
					continue
				}
				types := apidebug.FileTypes(caps.InputModalities, protocol, row.UpstreamType == config.UpstreamGeneric)
				if prior, ok := model.FileTypes[protocol]; ok {
					types = slices.DeleteFunc(prior, func(typ string) bool { return !slices.Contains(types, typ) })
				}
				model.FileTypes[protocol] = types
			}
		}
		out = append(out, model)
	}
	snapshot, err := s.devToolResolver().Snapshot(ctx, keyID)
	if err != nil {
		return nil, err
	}
	for _, provider := range []string{store.AgentProviderCodex, store.AgentProviderGrok} {
		if !snapshot.Subscription(provider).Available {
			continue
		}
		for _, model := range snapshot.Tools[provider].Models {
			if model.Source != "subscription" {
				continue
			}
			out = append(out, apidebug.Model{
				Name: model.Name, Provider: provider,
				Protocols: []string{config.ProtocolOpenAIResponses},
				// The subscription catalog does not declare file input capabilities.
				FileTypes: map[string][]string{config.ProtocolOpenAIResponses: {}},
			})
		}
	}
	return out, nil
}

func (s *Server) handleAPIDebugModels(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("key_id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "key_required", "请先选择 API 密钥")
		return
	}
	if _, ok := s.mediaKey(w, r, id); !ok {
		return
	}
	models, err := s.APIDebugModels(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"models": models})
}

func (s *Server) handleAPIDebug(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "key_required", "请先选择 API 密钥")
		return
	}
	if _, ok := s.mediaKey(w, r, id); !ok {
		return
	}
	if s.apiDebug == nil {
		writeError(w, 503, "unavailable", "API 调测暂不可用")
		return
	}
	s.apiDebug.DebugAPI(w, r, id)
}
