package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Claude Code gateway discovery only accepts model IDs beginning with
// "claude" or "anthropic". Catalog model names are product data and need not
// use either prefix, so expose a reversible client-only ID while keeping the
// original name as the routing and billing identity inside the gateway.
const claudeCatalogModelPrefix = "anthropic/llmgate/"

func claudeClientModelID(model devtoolpolicy.Model) string {
	if model.Source != "catalog" {
		return model.Name
	}
	return claudeCatalogModelPrefix + base64.RawURLEncoding.EncodeToString([]byte(model.Name))
}

func claudeCatalogModelName(clientID string) (string, bool) {
	if !strings.HasPrefix(clientID, claudeCatalogModelPrefix) {
		return "", false
	}
	encoded := strings.TrimPrefix(clientID, claudeCatalogModelPrefix)
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(b) == 0 || base64.RawURLEncoding.EncodeToString(b) != encoded {
		return "", false
	}
	return string(b), true
}

func (s *Server) claudeMixedBody(w http.ResponseWriter, r *http.Request) ([]byte, string, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body", "Failed to read the request body.")
		return nil, "", false
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body", "The request body is not valid JSON.")
		return nil, "", false
	}
	model, _ := payload["model"].(string)
	if model == "" {
		anthropicErrorStyle(w, http.StatusBadRequest, "missing_model", "The model field is required and must be a string.")
		return nil, "", false
	}
	return body, model, true
}

func (s *Server) handleClaudeMixedMessages(w http.ResponseWriter, r *http.Request) {
	body, model, ok := s.claudeMixedBody(w, r)
	if !ok {
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	body, model, ok = resolveClaudeCatalogModel(w, snapshot, body, model)
	if !ok {
		return
	}
	if snapshot.IsCatalogModel("claude", model) {
		// 勾选项由开发工具策略裁决，中转到目录面前立起订阅面标记
		// （apimodels.go）。
		markAgentSurface(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.handleMessages(w, r)
		return
	}
	sub := snapshot.Subscription("claude")
	if !sub.Configured {
		anthropicErrorStyle(w, http.StatusForbidden, "subscription_not_allowed", "This API key is not allowed to use the Claude Code subscription.")
		return
	}
	if sub.Available && !snapshot.HasModel("claude", model) {
		anthropicErrorStyle(w, http.StatusNotFound, "model_not_found", "The requested model is not available to this API key.")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.handleClaudeMessages(w, r)
}

func (s *Server) handleClaudeMixedCountTokens(w http.ResponseWriter, r *http.Request) {
	body, model, ok := s.claudeMixedBody(w, r)
	if !ok {
		return
	}
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	body, model, ok = resolveClaudeCatalogModel(w, snapshot, body, model)
	if !ok {
		return
	}
	if snapshot.IsCatalogModel("claude", model) {
		markAgentSurface(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.handleCountTokens(w, r)
		return
	}
	sub := snapshot.Subscription("claude")
	if !sub.Configured {
		anthropicErrorStyle(w, http.StatusForbidden, "subscription_not_allowed", "This API key is not allowed to use the Claude Code subscription.")
		return
	}
	if sub.Available && !snapshot.HasModel("claude", model) {
		anthropicErrorStyle(w, http.StatusNotFound, "model_not_found", "The requested model is not available to this API key.")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.handleClaudeCountTokens(w, r)
}

func resolveClaudeCatalogModel(w http.ResponseWriter, snapshot devtoolpolicy.Snapshot, body []byte, model string) ([]byte, string, bool) {
	original, alias := claudeCatalogModelName(model)
	if !alias {
		return body, model, true
	}
	if !snapshot.IsCatalogModel("claude", original) {
		anthropicErrorStyle(w, http.StatusNotFound, "model_not_found", "The requested model is not available to this API key.")
		return nil, "", false
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body", "The request body is not valid JSON.")
		return nil, "", false
	}
	payload["model"] = original
	rewritten, err := json.Marshal(payload)
	if err != nil {
		anthropicErrorStyle(w, http.StatusBadRequest, "invalid_request_body", "The request body is not valid JSON.")
		return nil, "", false
	}
	return rewritten, original, true
}

func (s *Server) handleClaudeMixedModels(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	tool := snapshot.Tools["claude"]
	sub := snapshot.Subscription("claude")
	if sub.Available {
		// Use the official model discovery response as the base, then filter it
		// to the Key-visible subscription set and append catalog entries.
		s.forwardClaude(w, r, "/v1/models", nil, "", nil, func(*http.Request, *store.AgentAccount) func(map[string]any, string) bool {
			return func(payload map[string]any, _ string) bool {
				mergeClaudeModels(payload, tool.Models)
				return true
			}
		})
		return
	}
	writeClaudeModelList(w, tool.Models)
}

func (s *Server) handleClaudeMixedHello(w http.ResponseWriter, r *http.Request) {
	snapshot, ok := s.devToolSnapshot(w, r)
	if !ok {
		return
	}
	if len(snapshot.Tools["claude"].Models) == 0 {
		anthropicErrorStyle(w, http.StatusConflict, "agent_not_configured", "This API key has no currently available Claude Code target.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func mergeClaudeModels(payload map[string]any, models []devtoolpolicy.Model) {
	existing, _ := payload["data"].([]any)
	byID := make(map[string]map[string]any, len(existing)+len(models))
	for _, item := range existing {
		if row, ok := item.(map[string]any); ok {
			if id, _ := row["id"].(string); id != "" {
				byID[id] = row
			}
		}
	}
	data := make([]any, 0, len(models))
	for _, model := range models {
		clientID := claudeClientModelID(model)
		item := byID[model.Name]
		if model.Source == "catalog" {
			item = nil
		}
		if item == nil {
			item = map[string]any{"id": clientID, "type": "model", "display_name": model.Name}
		}
		displayName, _ := item["display_name"].(string)
		displayName = strings.TrimSpace(displayName)
		if displayName == "" {
			displayName = model.Name
		}
		if !strings.HasPrefix(displayName, "LLM Gate · ") {
			displayName = "LLM Gate · " + strings.TrimPrefix(displayName, "SOC AGENT · ")
		}
		item["display_name"] = displayName
		data = append(data, item)
	}
	payload["data"] = data
	payload["has_more"] = false
	if len(models) == 0 {
		payload["first_id"], payload["last_id"] = nil, nil
	} else {
		payload["first_id"], payload["last_id"] = claudeClientModelID(models[0]), claudeClientModelID(models[len(models)-1])
	}
}

func writeClaudeModelList(w http.ResponseWriter, models []devtoolpolicy.Model) {
	payload := map[string]any{"data": []any{}, "has_more": false, "first_id": nil, "last_id": nil}
	mergeClaudeModels(payload, models)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
