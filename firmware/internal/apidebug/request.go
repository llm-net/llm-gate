// Package apidebug builds bounded, single-turn requests. Prompts, files and
// responses stay in memory; it has no persistence or logging dependencies.
package apidebug

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

const BodyLimit = 8 << 20
const FileBytes = 4 << 20
const MaxFiles = 8

type Model struct {
	Name      string   `json:"name"`
	Provider  string   `json:"provider,omitempty"` // empty: shared API; otherwise codex or grok subscription
	Protocols []string `json:"protocols"`
	// FileTypes is the intersection across eligible sources for each protocol.
	FileTypes map[string][]string `json:"file_types"`
}

type File struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data string `json:"data"` // base64 only, never a remote URL
}

type Request struct {
	Model     string `json:"model"`
	Provider  string `json:"provider,omitempty"`
	Protocol  string `json:"protocol"`
	Prompt    string `json:"prompt"`
	Files     []File `json:"files"`
	MaxTokens int    `json:"max_tokens"`
}

// FileTypes only advertises encodings implemented by this page and transport.
// Responses-to-Chat currently handles text input only.
func FileTypes(modalities []string, protocol string, nativeResponses bool) []string {
	out := []string{}
	if protocol == config.ProtocolOpenAIResponses && !nativeResponses {
		return out
	}
	if slices.Contains(modalities, "image") {
		out = append(out, "image/png", "image/jpeg", "image/webp", "image/gif")
	}
	if slices.Contains(modalities, "file") && (protocol != config.ProtocolOpenAIResponses || nativeResponses) {
		out = append(out, "application/pdf")
	}
	if protocol == config.ProtocolOpenAIChat {
		if slices.Contains(modalities, "audio") {
			out = append(out, "audio/wav", "audio/mpeg")
		}
		if slices.Contains(modalities, "video") {
			out = append(out, "video/mp4")
		}
	}
	return out
}

// Build cannot accept history, tools, arbitrary URLs or a different Key identity.
func Build(in Request, models []Model) (string, []byte, error) {
	var selected *Model
	for i := range models {
		if models[i].Name == in.Model && models[i].Provider == in.Provider {
			selected = &models[i]
			break
		}
	}
	if selected == nil || !slices.Contains(selected.Protocols, in.Protocol) {
		return "", nil, errors.New("所选模型或协议面不可用")
	}
	if in.Provider != "" && ((in.Provider != "codex" && in.Provider != "grok") || in.Protocol != config.ProtocolOpenAIResponses) {
		return "", nil, errors.New("所选模型或协议面不可用")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return "", nil, errors.New("请输入本次调测的提示词")
	}
	if len(in.Files) > MaxFiles {
		return "", nil, errors.New("最多上传 8 个文件")
	}
	if in.MaxTokens < 0 || in.MaxTokens > 32768 || (in.Protocol == config.ProtocolAnthropicMessages && in.MaxTokens == 0) {
		return "", nil, errors.New("输出 Token 上限须为 1–32768")
	}
	if in.Provider == "codex" && in.MaxTokens != 0 {
		return "", nil, errors.New("Codex 订阅使用平台默认输出 Token 上限")
	}
	content := []any{}
	textType := "text"
	if in.Protocol == config.ProtocolOpenAIResponses {
		textType = "input_text"
	}
	content = append(content, map[string]any{"type": textType, "text": in.Prompt})
	total := 0
	for _, f := range in.Files {
		if !slices.Contains(selected.FileTypes[in.Protocol], f.Type) {
			return "", nil, errors.New("模型或协议面不支持该文件类型")
		}
		raw, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil || len(raw) == 0 {
			return "", nil, errors.New("文件内容无效")
		}
		total += len(raw)
		if total > FileBytes {
			return "", nil, errors.New("附件总大小不能超过 4 MiB")
		}
		if f.Name == "" || len(f.Name) > 255 {
			return "", nil, errors.New("文件名无效")
		}
		uri := "data:" + f.Type + ";base64," + f.Data
		var part map[string]any
		switch in.Protocol {
		case config.ProtocolAnthropicMessages:
			typ := "image"
			if f.Type == "application/pdf" {
				typ = "document"
			}
			part = map[string]any{"type": typ, "source": map[string]any{"type": "base64", "media_type": f.Type, "data": f.Data}}
		case config.ProtocolOpenAIResponses:
			if f.Type == "application/pdf" {
				part = map[string]any{"type": "input_file", "filename": f.Name, "file_data": uri}
			} else {
				part = map[string]any{"type": "input_image", "image_url": uri}
			}
		case config.ProtocolOpenAIChat:
			switch {
			case f.Type == "application/pdf":
				part = map[string]any{"type": "file", "file": map[string]any{"filename": f.Name, "file_data": uri}}
			case strings.HasPrefix(f.Type, "audio/"):
				format := "wav"
				if f.Type == "audio/mpeg" {
					format = "mp3"
				}
				part = map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": f.Data, "format": format}}
			case f.Type == "video/mp4":
				part = map[string]any{"type": "video_url", "video_url": map[string]any{"url": uri}}
			default:
				part = map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}}
			}
		}
		content = append(content, part)
	}
	payload := map[string]any{"model": in.Model, "stream": false}
	message := map[string]any{"role": "user", "content": content}
	path := ""
	switch in.Protocol {
	case config.ProtocolOpenAIChat:
		path = "/v1/chat/completions"
		payload["messages"] = []any{message}
		if in.MaxTokens > 0 {
			payload["max_tokens"] = in.MaxTokens
		}
	case config.ProtocolOpenAIResponses:
		path = "/v1/responses"
		message["type"] = "message"
		payload["input"] = []any{message}
		if in.MaxTokens > 0 {
			payload["max_output_tokens"] = in.MaxTokens
		}
		payload["store"] = false
		if in.Provider != "" {
			path = "/agents/" + in.Provider + path
		}
		if in.Provider == "codex" {
			// The subscription backend requires streaming and instructions, and
			// does not accept max_output_tokens. The page reads the final SSE result.
			payload["stream"] = true
			payload["instructions"] = "Respond to the user's request."
		}
	case config.ProtocolAnthropicMessages:
		path = "/v1/messages"
		payload["messages"] = []any{message}
		payload["max_tokens"] = in.MaxTokens
	default:
		return "", nil, errors.New("协议面不可用")
	}
	body, err := json.Marshal(payload)
	return path, body, err
}
