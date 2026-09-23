package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const systemOneBodyLimit = 2 << 20

// System One 自产错误沿用 detail 形状；不回显验证输入、凭据或问题内容。
func systemOneErrorStyle(w http.ResponseWriter, status int, _ string, message string) {
	w.Header().Set("Content-Type", "application/json")
	if w.Header().Get("X-Typesafe-Request-Id") == "" {
		w.Header().Set("X-Typesafe-Request-Id", w.Header().Get("X-Request-Id"))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": message})
}

// systemOneModelSpan 只读取顶层 model 的字节范围。原文保留问题、选项的顺序和
// 数字字面量；Choice 平局取输入顺序靠前项，不能经过 map 重编码排序。
func systemOneModelSpan(body []byte) (model string, start, end int, ok bool) {
	if !json.Valid(body) {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return
	}
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", 0, 0, false
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return "", 0, 0, false
		}
		if key == "model" {
			if found || json.Unmarshal(raw, &model) != nil || model == "" {
				return "", 0, 0, false
			}
			found = true
			end = int(dec.InputOffset())
			start = end - len(raw)
		}
	}
	return model, start, end, found
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, systemOneBodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			systemOneErrorStyle(w, http.StatusRequestEntityTooLarge, "", "The request body must not exceed 2 MiB.")
		} else {
			systemOneErrorStyle(w, http.StatusUnprocessableEntity, "", "Failed to read the request body.")
		}
		return
	}
	model, start, end, ok := systemOneModelSpan(body)
	if !ok {
		systemOneErrorStyle(w, http.StatusUnprocessableEntity, "", "Expected a JSON object with one nonempty string model field.")
		return
	}
	info := beginEntry(r, usage.EntrySystemOne, model)
	if !s.admit(w, r, systemOneErrorStyle) {
		return
	}
	cands, status := s.resolveRoute(r.Context(), model, store.ModelKindSystemOne, config.ProtocolSystemOne)
	switch status {
	case routeModelNotFound, routeProtocolMismatch:
		systemOneErrorStyle(w, http.StatusNotFound, "", "The model does not exist or you do not have access to it.")
		return
	case routeStoreError:
		systemOneErrorStyle(w, http.StatusInternalServerError, "", internalErrorMessage)
		return
	}
	s.forward(w, r, cands, forwardSpec{
		protocol: config.ProtocolSystemOne, path: "/v1/systemone", model: model,
		contentType: "application/json", errStyle: systemOneErrorStyle,
		body: func(upstreamModel string) ([]byte, error) {
			if upstreamModel == model {
				return body, nil
			}
			name, err := json.Marshal(upstreamModel)
			if err != nil {
				return nil, err
			}
			out := make([]byte, 0, len(body)+len(name))
			out = append(out, body[:start]...)
			out = append(out, name...)
			return append(out, body[end:]...), nil
		},
		commit: func(w http.ResponseWriter, r *http.Request, resp *http.Response) {
			// 上游请求 ID 优先；避免 Add 把本地兜底 ID 留在首值。
			if resp.Header.Get("X-Typesafe-Request-Id") != "" {
				w.Header().Del("X-Typesafe-Request-Id")
			}
			s.commitResponse(w, r, resp, respRewrite{model: model, rewrite: keepModelField,
				observe: func(payload map[string]any) {
					u, ok := payload["usage"].(map[string]any)
					if !ok {
						return
					}
					input, inputOK := systemOneTokenCount(u["input_tokens"])
					output, outputOK := systemOneTokenCount(u["output_tokens"])
					if inputOK && outputOK {
						info.bill.tokens = usage.Tokens{Prompt: input, Completion: output}
						info.bill.usageSeen = true
					}
				},
			}, systemOneErrorStyle)
		},
	})
}

func systemOneTokenCount(value any) (int64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	count, err := n.Int64()
	return count, err == nil && count >= 0
}

// 模型发现由设备目录与 Key 授权决定，不把某条上游的全量别名泄露给持有人。
// release_date 没有可靠的目录来源时留空，不冒充后端发布时间。
func (s *Server) handleSystemOneModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.store.ListModels(r.Context())
	if err != nil {
		systemOneErrorStyle(w, http.StatusInternalServerError, "", internalErrorMessage)
		return
	}
	type modelInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ReleaseDate string `json:"release_date"`
	}
	out := struct {
		Models []modelInfo `json:"models"`
	}{Models: []modelInfo{}}
	for _, m := range models {
		if m.Kind != store.ModelKindSystemOne || m.Disabled {
			continue
		}
		_, status := s.resolveRoute(r.Context(), m.Name, store.ModelKindSystemOne, config.ProtocolSystemOne)
		if status == routeStoreError {
			systemOneErrorStyle(w, http.StatusInternalServerError, "", internalErrorMessage)
			return
		}
		if status == routeOK {
			out.Models = append(out.Models, modelInfo{Name: m.Name, Description: "System One"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
