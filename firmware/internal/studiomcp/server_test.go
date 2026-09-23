package studiomcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
	"github.com/llm-net/llm-gate/firmware/internal/studiomcp"
)

type backend struct {
	ws  *workspace
	err error
}

func (b *backend) Workspace(context.Context) (studiomcp.Workspace, error) { return b.ws, b.err }

type workspace struct {
	calls  int
	name   string
	args   any
	ctx    context.Context
	result mcpserve.Result
}

func (w *workspace) record(ctx context.Context, name string, args any) mcpserve.Result {
	w.calls++
	w.ctx, w.name, w.args = ctx, name, args
	return w.result
}
func (w *workspace) ListFiles(ctx context.Context, a studiomcp.ListFilesArgs) mcpserve.Result {
	return w.record(ctx, "list_files", a)
}
func (w *workspace) ViewImage(ctx context.Context, a studiomcp.ViewImageArgs) mcpserve.Result {
	return w.record(ctx, "view_image", a)
}
func (w *workspace) ReadText(ctx context.Context, a studiomcp.PathArgs) mcpserve.Result {
	return w.record(ctx, "read_text", a)
}
func (w *workspace) WriteText(ctx context.Context, a studiomcp.WriteTextArgs) mcpserve.Result {
	return w.record(ctx, "write_text", a)
}
func (w *workspace) GenerateImage(ctx context.Context, a studiomcp.GenerateArgs) mcpserve.Result {
	return w.record(ctx, "generate_image", a)
}
func (w *workspace) GenerateVideo(ctx context.Context, a studiomcp.GenerateArgs) mcpserve.Result {
	return w.record(ctx, "generate_video", a)
}
func (w *workspace) DeleteFile(ctx context.Context, a studiomcp.PathArgs) mcpserve.Result {
	return w.record(ctx, "delete_file", a)
}
func (w *workspace) RenameFile(ctx context.Context, a studiomcp.RenameFileArgs) mcpserve.Result {
	return w.record(ctx, "rename_file", a)
}

func handler(b *backend) http.Handler {
	return handlerWith(b, studiomcp.ModelDescriptions{Image: "image-capabilities\n\"quoted\"", Video: "video-capabilities"})
}

func handlerWith(b *backend, descriptions studiomcp.ModelDescriptions) http.Handler {
	return studiomcp.NewHandler(func(token string) studiomcp.Session {
		if token == "test-session" {
			return b
		}
		return nil
	}, descriptions)
}

// paramsDescriptions 取 tools/list 里两个生成工具的 params 说明。
func paramsDescriptions(t *testing.T, h http.Handler) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, item := range rpc(t, h, context.Background(), "tools/list", nil)["tools"].([]any) {
		tool := item.(map[string]any)
		if name := tool["name"].(string); name == "generate_image" || name == "generate_video" {
			props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			out[name] = props["params"].(map[string]any)["description"].(string)
		}
	}
	return out
}

func rpc(t *testing.T, h http.Handler, ctx context.Context, method string, params any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/studio-mcp", strings.NewReader(string(body))).WithContext(ctx)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer test-session")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var response struct {
		Result map[string]any `json:"result"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Result == nil {
		t.Fatalf("MCP response = %d %s", w.Code, w.Body.String())
	}
	return response.Result
}

// 从真实 HTTP 入口验证全部工具的参数边界和结果内容块，不依赖数据库、SSH 或模型后端。
func TestDispatch(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want any
	}{
		{"list_files", `{"dir":"media","kind":"video"}`, studiomcp.ListFilesArgs{Dir: "media", Kind: "video"}},
		{"view_image", `{"path":"media/a.png","detail":"high"}`, studiomcp.ViewImageArgs{Path: "media/a.png", Detail: "high"}},
		{"read_text", `{"path":"agent/PROJECT.md"}`, studiomcp.PathArgs{Path: "agent/PROJECT.md"}},
		{"write_text", `{"path":"docs/a.srt","content":"字幕\n正文","append":true}`, studiomcp.WriteTextArgs{Path: "docs/a.srt", Content: "字幕\n正文", Append: true}},
		{"generate_image", `{"prompt":"海报","model":"image-model","name":"poster","reference_images":["media/ref.png"],"params":{"quality":"high"},"count":2}`, studiomcp.GenerateArgs{Prompt: "海报", Model: "image-model", Name: "poster", ReferenceImages: []string{"media/ref.png"}, Params: map[string]any{"quality": "high"}, Count: 2}},
		{"generate_video", `{"prompt":"视频","model":"video-model","name":"clip","operation":"edit","first_frame":"media/first.png","last_frame":"media/last.png","reference_images":["media/ref.png"],"source_video":"media/source.mp4","params":{"duration":5},"count":1}`, studiomcp.GenerateArgs{Prompt: "视频", Model: "video-model", Name: "clip", Operation: "edit", FirstFrame: "media/first.png", LastFrame: "media/last.png", ReferenceImages: []string{"media/ref.png"}, SourceVideo: "media/source.mp4", Params: map[string]any{"duration": float64(5)}, Count: 1}},
		{"delete_file", `{"path":"media/draft.png"}`, studiomcp.PathArgs{Path: "media/draft.png"}},
		{"rename_file", `{"path":"docs/a.md","new_path":"agent/a.md"}`, studiomcp.RenameFileArgs{Path: "docs/a.md", NewPath: "agent/a.md"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := &workspace{result: mcpserve.Result{IsError: true, Content: []mcpserve.Content{mcpserve.Text("detail"), mcpserve.Image([]byte("image"), "image/png")}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			res := rpc(t, handler(&backend{ws: ws}), ctx, "tools/call", map[string]any{"name": tc.name, "arguments": json.RawMessage(tc.raw)})
			if ws.calls != 1 || ws.name != tc.name || !reflect.DeepEqual(ws.args, tc.want) || ws.ctx != ctx {
				t.Fatalf("dispatch = %s %#v, calls=%d, same context=%v", ws.name, ws.args, ws.calls, ws.ctx == ctx)
			}
			cancel()
			if ws.ctx.Err() != context.Canceled {
				t.Fatal("request cancellation was lost")
			}
			want := map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "detail"}, map[string]any{"type": "image", "data": "aW1hZ2U=", "mimeType": "image/png"}}}
			if !reflect.DeepEqual(res, want) {
				t.Fatalf("result changed at MCP boundary: %#v", res)
			}
		})
	}
}

func TestCapabilitiesAndWorkspaceLifecycle(t *testing.T) {
	ws := &workspace{}
	b := &backend{ws: ws}
	h := handler(b)
	ctx := context.Background()
	res := rpc(t, h, ctx, "tools/list", nil)
	for _, item := range res["tools"].([]any) {
		tool := item.(map[string]any)
		if tool["name"] == "exec" {
			t.Fatal("exec is gone: the engine runs on the worker host with its own shell")
		}
		if tool["name"] == "generate_image" || tool["name"] == "generate_video" {
			props := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
			desc := props["params"].(map[string]any)["description"].(string)
			want := "image-capabilities\n\"quoted\""
			if tool["name"] == "generate_video" {
				want = "video-capabilities"
			}
			if strings.Contains(desc, "{{") || !strings.Contains(desc, want) {
				t.Fatalf("model description = %q", desc)
			}
		}
	}
	if len(res["tools"].([]any)) != 8 {
		t.Fatalf("tools/list: %#v", res)
	}
	res = rpc(t, h, ctx, "tools/call", map[string]any{"name": "exec", "arguments": map[string]any{"command": "ls"}})
	if res["isError"] != true || ws.calls != 0 {
		t.Fatalf("exec reached backend: %#v", res)
	}
	// 能力变化：能力表换了，新装配的端点渲染新的说明，已有端点的清单不被改写（目录不共享）；
	// 是否放行由业务层每次调用重新裁决，MCP 层不缓存调用结果。
	fresh := paramsDescriptions(t, handlerWith(b, studiomcp.ModelDescriptions{Image: "image-v2", Video: "video-v2"}))
	if !strings.Contains(fresh["generate_image"], "image-v2") || !strings.Contains(fresh["generate_video"], "video-v2") ||
		strings.Contains(fresh["generate_image"], "image-capabilities") {
		t.Fatalf("changed capabilities = %#v", fresh)
	}
	if old := paramsDescriptions(t, h); !strings.Contains(old["generate_image"], "image-capabilities") || strings.Contains(old["generate_image"], "image-v2") {
		t.Fatalf("existing endpoint's catalog changed: %#v", old)
	}
	ws.result = mcpserve.TextResult("saved media/a.png", false)
	if res := rpc(t, h, ctx, "tools/call", map[string]any{"name": "generate_image", "arguments": map[string]any{"prompt": "x"}}); res["isError"] == true {
		t.Fatalf("allowed generation = %#v", res)
	}
	ws.result = mcpserve.TextResult("the model is not available for this chat's API key", true)
	if res := rpc(t, h, ctx, "tools/call", map[string]any{"name": "generate_image", "arguments": map[string]any{"prompt": "x"}}); res["isError"] != true || ws.calls != 2 {
		t.Fatalf("revoked generation = %#v, calls=%d", res, ws.calls)
	}
	ws.calls = 0
	b.err = errors.New("workspace removed")
	res = rpc(t, h, ctx, "tools/call", map[string]any{"name": "list_files"})
	if res["isError"] != true || ws.calls != 0 {
		t.Fatalf("removed workspace reached backend: %#v", res)
	}
}

func TestInvalidArgumentsDoNotReachBackend(t *testing.T) {
	ws := &workspace{}
	h := handler(&backend{ws: ws})
	for _, tc := range []struct{ name, raw string }{
		{"write_text", `{"append":"yes"}`},
		{"list_files", `{"dir":7}`},
		{"generate_video", `[]`},
		{"read_text", `"media/a.png"`},
		{"not_a_tool", `{}`},
	} {
		res := rpc(t, h, context.Background(), "tools/call", map[string]any{"name": tc.name, "arguments": json.RawMessage(tc.raw)})
		if res["isError"] != true || ws.calls != 0 {
			t.Fatalf("invalid call reached backend: %s %s -> %#v", tc.name, tc.raw, res)
		}
	}
}

func TestEndpointAuthorization(t *testing.T) {
	b := &backend{ws: &workspace{}}
	active := true
	h := studiomcp.NewHandler(func(token string) studiomcp.Session {
		if active && token == "test-session" {
			return b
		}
		return nil
	}, studiomcp.ModelDescriptions{})
	res := rpc(t, h, context.Background(), "initialize", nil)
	if res["serverInfo"].(map[string]any)["name"] != "llmgate-studio" {
		t.Fatalf("server info = %#v", res)
	}
	for _, tc := range []struct {
		remote, token string
		active        bool
		status        int
	}{
		{"10.0.0.1:1234", "test-session", true, http.StatusForbidden},
		{"127.0.0.1:1234", "", true, http.StatusUnauthorized},
		{"127.0.0.1:1234", "another-session", true, http.StatusUnauthorized},
		{"127.0.0.1:1234", "test-session", false, http.StatusUnauthorized},
	} {
		active = tc.active
		r := httptest.NewRequest(http.MethodPost, "/studio-mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		r.RemoteAddr = tc.remote
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("authorization: remote=%s active=%v status=%d, want %d", tc.remote, tc.active, w.Code, tc.status)
		}
	}
}
