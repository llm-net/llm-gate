package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/apidebug"
	"github.com/llm-net/llm-gate/firmware/internal/i18n"
)

type apiDebugModelAccess interface {
	APIDebugModels(context.Context, int64) ([]apidebug.Model, error)
}

func (s *Server) apiDebugModels(r *http.Request) ([]apidebug.Model, error) {
	access, ok := s.keyAccess.(apiDebugModelAccess)
	if !ok {
		return nil, errors.New("API 调测暂不可用")
	}
	return access.APIDebugModels(r.Context(), infoFrom(r.Context()).keyID)
}

func (s *Server) handleAPIDebugModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	models, err := s.apiDebugModels(r)
	if err != nil {
		openAIErrorStyle(w, 503, "unavailable", "API debugging unavailable.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"models": models})
}

// DebugAPI is an in-process admin bridge, never an unauthenticated HTTP route.
// No client plaintext is unsealed. The live Key row provides the normal limits;
// the ordinary data-plane middleware records usage and cancellation exactly once.
func (s *Server) DebugAPI(w http.ResponseWriter, r *http.Request, keyID int64) {
	s.withRequestID(s.withAccessLog(s.withRecovery(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, err := s.store.LookupKeyAuthByID(r.Context(), keyID)
		if err != nil || auth.KeyDisabled {
			openAIErrorStyle(w, 401, "invalid_api_key", "Invalid API key provided.")
			return
		}
		info := infoFrom(r.Context())
		info.keyID, info.limits, info.bill.keyDisplay = auth.KeyID, *auth, auth.KeyDisplay
		s.handleAPIDebug(w, r)
	})))).ServeHTTP(w, r)
}

func (s *Server) handleAPIDebug(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	fail := func(status int, message string) {
		openAIErrorStyle(w, status, "debug_invalid", i18n.T(i18n.Negotiate(r.Header.Get("Accept-Language")), message))
	}
	var in apidebug.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, apidebug.BodyLimit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			fail(413, "请求体过大")
			return
		}
		fail(400, "请求格式不正确")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		fail(400, "请求格式不正确")
		return
	}
	models, err := s.apiDebugModels(r)
	if err != nil {
		fail(503, "API 调测暂不可用")
		return
	}
	path, body, err := apidebug.Build(in, models)
	if err != nil {
		fail(400, err.Error())
		return
	}
	// Only generated protocol bodies reach the production handlers. Forward no
	// admin cookies, CSRF headers, client credentials or user-selected destinations.
	req := r.Clone(r.Context())
	req.URL.Path, req.URL.RawQuery, req.URL.RawPath = path, "", ""
	req.RequestURI = path
	req.Header = http.Header{"Content-Type": []string{"application/json"}, "Anthropic-Version": []string{"2023-06-01"}}
	req.Body, req.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	switch path {
	case "/v1/chat/completions":
		s.handleChatCompletions(w, req)
	case "/v1/responses":
		s.handleResponsesCatalog(w, req)
	case "/v1/messages":
		s.handleMessages(w, req)
	case "/agents/codex/v1/responses", "/agents/grok/v1/responses":
		snapshot, ok := s.subscriptionPermitted(w, req, in.Provider, true)
		if !ok {
			return
		}
		// Recheck the subscription selection on the data plane. A policy change
		// must not redirect a subscription test to a same-name catalog source.
		if !snapshot.HasModel(in.Provider, in.Model) || snapshot.IsCatalogModel(in.Provider, in.Model) {
			openAIErrorStyle(w, 404, "model_not_found", "The requested model is not available to this API key.")
			return
		}
		if in.Provider == "codex" {
			s.handleCodexResponses(w, req)
		} else {
			s.handleGrokResponses(w, req)
		}
	}
}
