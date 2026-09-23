package apidebug

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildSingleTurnMultimodal(t *testing.T) {
	for _, protocol := range []string{"openai_chat", "openai_responses", "anthropic_messages"} {
		t.Run(protocol, func(t *testing.T) {
			models := []Model{{Name: "model", Protocols: []string{protocol}, FileTypes: map[string][]string{protocol: FileTypes([]string{"image", "file", "audio", "video"}, protocol, true)}}}
			files := []File{{Name: "image.png", Type: "image/png", Data: "YWJj"}, {Name: "document.pdf", Type: "application/pdf", Data: "ZGVm"}}
			if protocol == "openai_chat" {
				files = append(files, File{Name: "audio.mp3", Type: "audio/mpeg", Data: "Z2hp"}, File{Name: "video.mp4", Type: "video/mp4", Data: "amts"})
			}
			path, body, err := Build(Request{Model: "model", Protocol: protocol, Prompt: "only this turn", Files: files, MaxTokens: 1234}, models)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			field := "messages"
			tokenField := "max_tokens"
			if protocol == "openai_responses" {
				field = "input"
				tokenField = "max_output_tokens"
				if payload["store"] != false {
					t.Fatal("Responses must not request storage")
				}
			}
			messages := payload[field].([]any)
			if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" || payload[tokenField] != float64(1234) || payload["stream"] != false {
				t.Fatalf("not single turn: %s", body)
			}
			parts := messages[0].(map[string]any)["content"].([]any)
			if len(parts) != len(files)+1 {
				t.Fatalf("lost attachment: %s", body)
			}
			switch protocol {
			case "anthropic_messages":
				if path != "/v1/messages" || parts[1].(map[string]any)["source"].(map[string]any)["data"] != "YWJj" || parts[2].(map[string]any)["type"] != "document" {
					t.Fatalf("wrong wire: %s", body)
				}
			case "openai_responses":
				if path != "/v1/responses" || parts[1].(map[string]any)["image_url"] != "data:image/png;base64,YWJj" || parts[2].(map[string]any)["file_data"] != "data:application/pdf;base64,ZGVm" {
					t.Fatalf("wrong wire: %s", body)
				}
			case "openai_chat":
				if path != "/v1/chat/completions" || parts[3].(map[string]any)["input_audio"].(map[string]any)["format"] != "mp3" || parts[4].(map[string]any)["video_url"].(map[string]any)["url"] != "data:video/mp4;base64,amts" {
					t.Fatalf("wrong wire: %s", body)
				}
			}
		})
	}
}

func TestRejectInvalidInputs(t *testing.T) {
	models := []Model{{Name: "model", Protocols: []string{"openai_chat"}, FileTypes: map[string][]string{"openai_chat": {"image/png"}}}}
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"model", func(r *Request) { r.Model = "hidden" }},
		{"protocol", func(r *Request) { r.Protocol = "anthropic_messages" }},
		{"empty", func(r *Request) { r.Prompt = " " }},
		{"limit", func(r *Request) { r.MaxTokens = -1 }},
		{"unsupported", func(r *Request) { r.Files = []File{{Name: "x.pdf", Type: "application/pdf", Data: "YWJj"}} }},
		{"remote", func(r *Request) {
			r.Files = []File{{Name: "x.png", Type: "image/png", Data: "https://example.invalid/private"}}
		}},
		{"empty file", func(r *Request) { r.Files = []File{{Name: "x.png", Type: "image/png"}} }},
		{"too many", func(r *Request) { r.Files = make([]File, 9) }},
		{"total bytes", func(r *Request) {
			r.Files = []File{{Name: "x.png", Type: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", FileBytes/2+1)))}, {Name: "y.png", Type: "image/png", Data: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", FileBytes/2+1)))}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Request{Model: "model", Protocol: "openai_chat", Prompt: "hello", MaxTokens: 1}
			tc.change(&r)
			if _, _, err := Build(r, models); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
	if got := FileTypes([]string{"image", "file"}, "openai_responses", false); len(got) != 0 {
		t.Fatal("unsupported Responses conversion")
	}
	if got := FileTypes(nil, "openai_chat", false); len(got) != 0 {
		t.Fatal("guessed unknown capability")
	}
}

func TestBuildSubscriptions(t *testing.T) {
	models := []Model{}
	for _, provider := range []string{"", "codex", "grok"} {
		models = append(models, Model{Name: "same-model", Provider: provider, Protocols: []string{"openai_responses"}})
	}
	for _, provider := range []string{"", "codex", "grok"} {
		t.Run(provider, func(t *testing.T) {
			in := Request{Model: "same-model", Provider: provider, Protocol: "openai_responses", Prompt: "one turn"}
			path, body, err := Build(in, models)
			wantPath := "/v1/responses"
			if provider != "" {
				wantPath = "/agents/" + provider + wantPath
			}
			if err != nil || path != wantPath {
				t.Fatalf("path=%q err=%v", path, err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["store"] != false || payload["stream"] != (provider == "codex") || payload["model"] != in.Model || payload["provider"] != nil {
				t.Fatalf("wrong subscription payload: %s", body)
			}
			messages := payload["input"].([]any)
			if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" || messages[0].(map[string]any)["type"] != "message" {
				t.Fatalf("not single turn: %s", body)
			}
			if provider == "codex" && payload["instructions"] == nil {
				t.Fatal("missing Codex instructions")
			}
			in.MaxTokens = 123
			_, body, err = Build(in, models)
			if provider == "codex" {
				if err == nil {
					t.Fatal("silently accepted unsupported Codex token limit")
				}
			} else if err != nil || !strings.Contains(string(body), `"max_output_tokens":123`) {
				t.Fatalf("lost output limit: %s err=%v", body, err)
			}
		})
	}
	for _, provider := range []string{"codex", "grok", "claude", "../v1"} {
		in := Request{Model: "same-model", Provider: provider, Protocol: "openai_responses", Prompt: "one turn"}
		if _, _, err := Build(in, models[:1]); err == nil {
			t.Fatal("shared model granted subscription access")
		}
		invalid := []Model{{Name: in.Model, Provider: provider, Protocols: []string{"openai_chat"}}}
		in.Protocol = "openai_chat"
		if _, _, err := Build(in, invalid); err == nil {
			t.Fatal("accepted unsupported subscription protocol or provider")
		}
	}
}
