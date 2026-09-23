package admin_test

// 创作工作空间端点验收：路由档位、503 未接入、创建（只落在工作节点上）/ 删除、文件上传（裸二进制体）/
// 清单 / 内联读取 / 缩略图 / 改名 / 删除、新建对话可选项（每种引擎在节点上的读数、密钥各带每种引擎的
// 可见模型与生成模型的可用性 media_models）、一条从新建对话、提交（假引擎经真 MCP 端点列目录并写文件）
// 到陪等 / 删对话的完整路径、审计不含正文；存量的设备上的空间只能管理文件。

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/nodeengine"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studio"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// studioFakeEngine 是认节点上 codex 的节点端引擎：每条指令经 MCP 端点列目录并写一份
// agent/PROJECT.md，再回一句。base 是 MCP 端点所在的测试服务器（真引擎经远程转发访问设备）。
type studioFakeEngine struct {
	base   string
	mu     sync.Mutex
	closed int
	last   nodeengine.Request
}

func (e *studioFakeEngine) ID() string        { return "fake" }
func (e *studioFakeEngine) Label() string     { return "Fake" }
func (e *studioFakeEngine) Tool() string      { return "codex" }
func (e *studioFakeEngine) Efforts() []string { return hostagent.CodexEfforts }
func (e *studioFakeEngine) Validate(_ context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	if cfg.KeyID <= 0 {
		return cfg, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: "请为对话选择一把 API 密钥"}
	}
	cfg.KeyDisplay = "sk_fake…0000"
	if cfg.Model == "" {
		cfg.Model = "fake-1"
	}
	return cfg, nil
}
func (e *studioFakeEngine) Start(_ context.Context, req nodeengine.Request) (hostagent.EngineSession, error) {
	e.mu.Lock()
	e.last = req
	e.mu.Unlock()
	tools := req.Tools
	tools.URL = e.base + req.Tools.URL
	return &studioFakeSession{e: e, tools: tools, done: make(chan struct{})}, nil
}

type studioFakeSession struct {
	e     *studioFakeEngine
	tools hostagent.ToolEndpoint
	done  chan struct{}
}

func (s *studioFakeSession) call(name string, args map[string]any) (string, bool) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	req, _ := http.NewRequest(http.MethodPost, s.tools.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.tools.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err.Error(), true
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	text := ""
	if len(out.Result.Content) > 0 {
		text = out.Result.Content[0].Text
	}
	return text, out.Result.IsError
}

func (s *studioFakeSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	list, isErr := s.call("list_files", nil)
	if isErr {
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: list}, nil
	}
	if _, isErr := s.call("write_text", map[string]any{"path": studio.BriefFileName, "content": "# 项目\n" + in.Text}); isErr {
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "write_text failed"}, nil
	}
	sink.Message("目录里有：" + strings.TrimSpace(list))
	return hostagent.Outcome{Status: hostagent.OutcomeCompleted}, nil
}
func (s *studioFakeSession) Interrupt(context.Context) error { return nil }
func (s *studioFakeSession) Done() <-chan struct{}           { return s.done }
func (s *studioFakeSession) Close() error {
	s.e.mu.Lock()
	s.e.closed++
	s.e.mu.Unlock()
	close(s.done)
	return nil
}

// studioHost 是接好的创作工作空间环境：一台假工作节点（纳管、装好 devd，工具探测报 PATH 上有
// codex）+ 创作工作空间管理器（假引擎 + 真 MCP 端点 + 管理面的媒体生成内核）。
type studioHost struct {
	mgr    *studio.Manager
	engine *studioFakeEngine
	fake   *devhosttest.Host
	// id 是工作节点的主机 id（URL 与 JSON 里原样用）。
	id string
}

func withStudio(t *testing.T, e *env, root string) *studioHost {
	t.Helper()
	fake := devhosttest.New(t, devhosttest.Options{Username: "dev", Password: devPassword, Git: "git version 2.43.0",
		DevToolsFound: map[string]string{"codex": "/usr/local/bin/codex", "grok": "/usr/local/bin/grok"}})
	hostMgr := agenthost.New(agenthost.Options{Store: e.st, Dial: fake.SSH.Dial})
	e.srv.SetAgentHosts(hostMgr)
	devMgr := devhost.New(devhost.Options{Store: e.st, Hosts: hostMgr, Binaries: devhosttest.FakeBinaries{}})
	e.srv.SetDevHosts(devMgr)
	var mgr *studio.Manager
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mgr.MCPHandler().ServeHTTP(w, r) }))
	t.Cleanup(mcp.Close)
	engine := &studioFakeEngine{base: mcp.URL}
	mgr = studio.New(studio.Options{Store: e.st, Engines: []nodeengine.Engine{engine}, Media: e.srv.MediaJobs(),
		Hosts: hostMgr, DevHosts: devMgr, DataDir: e.dir})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	e.srv.SetStudio(mgr)
	id := enrollDevHost(t, e, root, fake, "worker", "dev", true)
	resp := e.do("POST", "/admin/v1/agent-hosts/"+id+"/devd/install", root, `{}`)
	wantStatus(t, resp, http.StatusOK)
	return &studioHost{mgr: mgr, engine: engine, fake: fake, id: id}
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type studioFileDTO struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Mime     string `json:"mime"`
	Bytes    int64  `json:"bytes"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Origin   string `json:"origin"`
	Prompt   string `json:"prompt"`
	HasThumb bool   `json:"has_thumb"`
}

func (e *env) uploadStudio(cookie, wsID, path string, data []byte) *http.Response {
	r := httptest.NewRequest(http.MethodPost, "/admin/v1/workspaces/"+wsID+"/files?path="+path, bytes.NewReader(data))
	r.Header.Set("X-LlmGate-CSRF", "1")
	r.Header.Set("Content-Type", "application/octet-stream")
	r.AddCookie(&http.Cookie{Name: admin.SessionCookieName, Value: cookie})
	return e.send(r)
}

func TestStudioExposure(t *testing.T) {
	e := newEnv(t)
	lister, ok := e.h.(tunnelctx.RouteLister)
	if !ok {
		t.Fatal("管理面 handler 不暴露路由表")
	}
	tiers := map[string]tunnelctx.Exposure{}
	for _, r := range lister.TunnelRoutes() {
		tiers[r.Pattern] = r.Exposure
	}
	for pattern, want := range map[string]tunnelctx.Exposure{
		"GET /admin/v1/workspaces/{id}/files":                                  tunnelctx.Admin,
		"POST /admin/v1/workspaces/{id}/files":                                 tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}/files/{dir}/{name}":                     tunnelctx.Admin,
		"GET /admin/v1/workspaces/{id}/files/{dir}/{name}/download":            tunnelctx.Admin,
		"GET /admin/v1/workspaces/{id}/files/{dir}/{name}/thumb":               tunnelctx.Admin,
		"POST /admin/v1/workspaces/{id}/files/{dir}/{name}/thumb":              tunnelctx.LANOnly,
		"PATCH /admin/v1/workspaces/{id}/files/{dir}/{name}":                   tunnelctx.LANOnly,
		"DELETE /admin/v1/workspaces/{id}/files/{dir}/{name}":                  tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}/agent":                                  tunnelctx.Admin,
		"GET /admin/v1/workspaces/{id}/agent/options":                          tunnelctx.Admin,
		"GET /admin/v1/workspaces/{id}/agent/media":                            tunnelctx.Admin,
		"PUT /admin/v1/workspaces/{id}/agent/media":                            tunnelctx.LANOnly,
		"POST /admin/v1/workspaces/{id}/agent/chats":                           tunnelctx.LANOnly,
		"POST /admin/v1/workspaces/{id}/agent/chats/{chat_id}/archive":         tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}/agent/chats/{chat_id}":                  tunnelctx.Admin,
		"DELETE /admin/v1/workspaces/{id}/agent/chats/{chat_id}":               tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}/agent/chats/{chat_id}/wait":             tunnelctx.Admin,
		"POST /admin/v1/workspaces/{id}/agent/chats/{chat_id}/runs":            tunnelctx.LANOnly,
		"DELETE /admin/v1/workspaces/{id}/agent/chats/{chat_id}/runs/{run_id}": tunnelctx.LANOnly,
		"POST /admin/v1/workspaces/{id}/agent/chats/{chat_id}/stop":            tunnelctx.LANOnly,
		"GET /admin/v1/workspaces/{id}/agent/chats/{chat_id}/events":           tunnelctx.Admin,
	} {
		tier, found := tiers[pattern]
		if !found {
			t.Fatalf("路由 %s 没注册", pattern)
		}
		if tier != want {
			t.Fatalf("%s 档位 = %v，期望 %v", pattern, tier, want)
		}
	}
	// 未登录一律 401；未接入管理器时 503。
	wantStatus(t, e.do("GET", "/admin/v1/workspaces/x/files", "", ""), http.StatusUnauthorized)
	root := e.rootSession()
	resp := e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster"}`)
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if code := errCode(t, resp); code != "studio_unavailable" {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"other","name":"poster"}`)
	wantStatus(t, resp, http.StatusBadRequest)
}

func TestStudioWorkspaceLifecycle(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	sh := withStudio(t, e, root)
	engine := sh.engine
	// 生成后端（假的 grok / codex）：可选项里的 media_models 才有可用的模型。
	e.srv.SetMediaGateway(readyMedia(pngResult))
	owner := mediaOwner(t, e)

	// 创建：只落在工作节点上（没带 host_id 400）；同名冲突；不能带仓库。
	resp := e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_workspace" {
		t.Fatalf("设备上新建的错误码 = %q", code)
	}
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	decodeInto(t, resp, &created)
	ws := created.Workspace
	if ws.ID == "" || ws.HostID == 0 || ws.Name != "poster" || ws.Path != sh.fake.Workspaces()["poster"].Path {
		t.Fatalf("创建结果 = %+v", ws)
	}
	// 创作类型缺省 general；agent/PROJECT.md 按类型预置。
	if ws.Template != "general" || ws.TemplateName != "通用" {
		t.Fatalf("创作类型 = %q / %q", ws.Template, ws.TemplateName)
	}
	if data, err := os.ReadFile(ws.Path + "/agent/PROJECT.md"); err != nil || !strings.Contains(string(data), "创作类型：通用") {
		t.Fatalf("预置的 agent/PROJECT.md = %q / %v", data, err)
	}
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusConflict)
	// 指定创作类型：读数带名称，预置对应骨架；不认识的类型 400。
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"drama","template":"short-drama","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var drama struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	decodeInto(t, resp, &drama)
	if drama.Workspace.Template != "short-drama" || drama.Workspace.TemplateName != "1 分钟短剧" {
		t.Fatalf("短剧空间 = %+v", drama.Workspace)
	}
	if data, err := os.ReadFile(drama.Workspace.Path + "/agent/PROJECT.md"); err != nil || !strings.Contains(string(data), "创作类型：1 分钟短剧") {
		t.Fatalf("短剧空间的 agent/PROJECT.md = %q / %v", data, err)
	}
	resp = e.do("DELETE", "/admin/v1/workspaces/"+drama.Workspace.ID, root, `{"remove_dir":true}`)
	wantStatus(t, resp, http.StatusNoContent)
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"bad","template":"nope","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_workspace" {
		t.Fatalf("错误码 = %q", code)
	}
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"x","repo_url":"https://github.com/x/y.git","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusBadRequest)
	var list struct {
		Workspaces []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"workspaces"`
		StudioTemplates []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"studio_templates"`
	}
	resp = e.do("GET", "/admin/v1/workspaces", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &list)
	if len(list.Workspaces) != 1 || list.Workspaces[0].Kind != "studio" {
		t.Fatalf("清单 = %+v", list.Workspaces)
	}
	if len(list.StudioTemplates) != len(studio.Templates()) || list.StudioTemplates[0].ID != "general" || list.StudioTemplates[0].Name != "通用" || list.StudioTemplates[1].Description == "" {
		t.Fatalf("创作类型清单 = %+v", list.StudioTemplates)
	}

	// 上传：裸二进制体，目标是「子目录/文件名」；JSON 体被 CSRF 中间件按 415 拒；路径非法或
	// 目录与种类不配 400。
	pngData := testPNG(t)
	resp = e.uploadStudio(root, ws.ID, "media/ref.png", pngData)
	wantStatus(t, resp, http.StatusCreated)
	var one struct {
		File studioFileDTO `json:"file"`
	}
	decodeInto(t, resp, &one)
	if one.File.Name != "media/ref.png" || one.File.Kind != "image" || one.File.Width != 32 || one.File.Height != 16 || !one.File.HasThumb || one.File.Origin != "upload" {
		t.Fatalf("上传结果 = %+v", one.File)
	}
	if _, err := os.Stat(ws.Path + "/media/ref.png"); err != nil {
		t.Fatalf("上传未落在 media/: %v", err)
	}
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/files?path=docs/x.txt", root, `{}`)
	wantStatus(t, resp, http.StatusUnsupportedMediaType)
	for _, bad := range []string{"media/.secret", "ref.png", "docs/x.png", "media/notes.txt", "other/x.txt"} {
		resp = e.uploadStudio(root, ws.ID, bad, []byte("x"))
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != "studio_file_invalid" {
			t.Fatalf("上传 %q 的错误码 = %q", bad, code)
		}
	}
	resp = e.uploadStudio(root, ws.ID, "docs/notes.txt", []byte("hello"))
	wantStatus(t, resp, http.StatusCreated)
	resp = e.uploadStudio(root, ws.ID, "docs/other.txt", []byte("other"))
	wantStatus(t, resp, http.StatusCreated)

	// 清单、内联读取（Content-Type 按扩展名）、缩略图、下载。
	var files struct {
		Files []studioFileDTO `json:"files"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &files)
	if len(files.Files) != 4 || files.Files[0].Name != "agent/PROJECT.md" || files.Files[1].Name != "media/ref.png" || files.Files[2].Name != "docs/notes.txt" || files.Files[2].HasThumb {
		t.Fatalf("清单 = %+v", files.Files)
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/ref.png", root, "")
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("内联 Content-Type = %q", ct)
	}
	if body := readAll(t, resp); body != string(pngData) {
		t.Fatal("内联读取的字节与上传的不同")
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/ref.png/thumb", root, "")
	wantStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("缩略图 Content-Type = %q", ct)
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/docs/notes.txt/thumb", root, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/docs/notes.txt/download", root, "")
	wantStatus(t, resp, http.StatusOK)
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "notes.txt") || strings.Contains(cd, "docs/") {
		t.Fatalf("下载 Content-Disposition = %q", cd)
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/missing.png", root, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/other/ref.png", root, "")
	wantStatus(t, resp, http.StatusBadRequest)

	// 改名、挪目录与删除。
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/notes.txt", root, `{"path":"docs/brief.txt"}`)
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/brief.txt", root, `{"path":"docs/other.txt"}`)
	wantStatus(t, resp, http.StatusConflict)
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/brief.txt", root, `{"path":"media/brief.txt"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/brief.txt", root, `{"path":"agent/brief.txt"}`)
	wantStatus(t, resp, http.StatusOK)
	var moved struct {
		File studioFileDTO `json:"file"`
	}
	decodeInto(t, resp, &moved)
	if moved.File.Name != "agent/brief.txt" || moved.File.Origin != "upload" {
		t.Fatalf("挪目录后的读数 = %+v", moved.File)
	}
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/files/agent/brief.txt", root, "")
	wantStatus(t, resp, http.StatusNoContent)
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/files/agent/brief.txt", root, "")
	wantStatus(t, resp, http.StatusNotFound)
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/files/docs/other.txt", root, "")
	wantStatus(t, resp, http.StatusNoContent)

	// 媒体生成能力：显式白名单。新空间一个都没启用，读数列出能力表全部模型与缺省说明；
	// 可选项里的 media_models 只列启用了的。
	var media struct {
		Models []struct {
			ID           string `json:"id"`
			Kind         string `json:"kind"`
			Enabled      bool   `json:"enabled"`
			Usage        string `json:"usage"`
			DefaultUsage string `json:"default_usage"`
		} `json:"models"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/media", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &media)
	if len(media.Models) != len(mediagen.Presets()) {
		t.Fatalf("媒体生成能力 = %d 项，期望 %d", len(media.Models), len(mediagen.Presets()))
	}
	enableAll := []map[string]string{}
	for _, m := range media.Models {
		if m.Enabled || m.DefaultUsage == "" || m.Kind == "" {
			t.Fatalf("新空间的媒体生成能力 %+v", m)
		}
		enableAll = append(enableAll, map[string]string{"model": m.ID, "usage": "机密说明-" + m.ID})
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/options", root, "")
	wantStatus(t, resp, http.StatusOK)
	if raw := readAll(t, resp); !strings.Contains(raw, `"media_models":[]`) {
		t.Fatalf("没启用任何模型时 media_models 应为空: %s", raw)
	}
	resp = e.do("PUT", "/admin/v1/workspaces/"+ws.ID+"/agent/media", root, `{"models":[{"model":"no-such-model","usage":""}]}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "agent_invalid_input" {
		t.Fatalf("未知模型的错误码 = %q", code)
	}
	body, _ := json.Marshal(map[string]any{"models": enableAll})
	resp = e.do("PUT", "/admin/v1/workspaces/"+ws.ID+"/agent/media", root, string(body))
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &media)
	for _, m := range media.Models {
		if !m.Enabled || m.Usage != "机密说明-"+m.ID {
			t.Fatalf("保存后的媒体生成能力 %+v", m)
		}
	}

	// 可选项：每种引擎在节点上的读数（现探）、密钥各带每种引擎的可见模型（mediaOwner 钉了 Codex
	// 与 Grok）与生成模型的可用性——与「媒体生成」页同一份裁决（mediagen.Available）。
	var options struct {
		Engines []struct {
			ID      string   `json:"id"`
			Tool    string   `json:"tool"`
			Ready   bool     `json:"ready"`
			Path    string   `json:"path"`
			Efforts []string `json:"efforts"`
		} `json:"engines"`
		Keys []struct {
			ID      int64 `json:"id"`
			Engines map[string]struct {
				Enabled    bool `json:"enabled"`
				Configured bool `json:"configured"`
				Models     []struct {
					Name   string `json:"name"`
					Source string `json:"source"`
				} `json:"models"`
			} `json:"engines"`
			MediaModels []mediagen.Availability `json:"media_models"`
		} `json:"keys"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/options", root, "")
	wantStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	if err := json.Unmarshal([]byte(raw), &options); err != nil {
		t.Fatalf("解析可选项 %q: %v", raw, err)
	}
	if len(options.Engines) != 1 || !options.Engines[0].Ready || options.Engines[0].Tool != "codex" || options.Engines[0].Path != "/usr/local/bin/codex" ||
		len(options.Engines[0].Efforts) == 0 || len(options.Keys) != 1 || options.Keys[0].ID != owner {
		t.Fatalf("可选项 = %+v", options)
	}
	if access := options.Keys[0].Engines["fake"]; !access.Enabled || !access.Configured {
		t.Fatalf("密钥在引擎上的读数 = %+v", options.Keys[0].Engines)
	}
	if strings.Contains(raw, `"backends"`) {
		t.Fatalf("可选项不该再带 backends: %s", raw)
	}
	models := options.Keys[0].MediaModels
	if len(models) != len(mediagen.Presets()) {
		t.Fatalf("media_models = %d 项，期望 %d", len(models), len(mediagen.Presets()))
	}
	kinds := map[string]int{}
	for i, m := range models {
		if m.ID != mediagen.Presets()[i].ID || !m.Available || m.ReasonCode != "" || len(m.Operations) == 0 {
			t.Fatalf("media_models[%d] = %+v，期望可用的 %s", i, m, mediagen.Presets()[i].ID)
		}
		kinds[m.Backend+"/"+m.Kind]++
	}
	if kinds["grok/video"] != 2 || kinds["grok/image"] != 1 || kinds["codex/image"] != 3 {
		t.Fatalf("media_models 的后端 / 种类分布 = %v", kinds)
	}
	// 钉着的 Grok 账号停用：那几项转成不可用并带原因，Codex 不受影响。
	if err := e.st.SetAgentStatus(t.Context(), mustGrokAccount(t, e), store.AgentStatusDisabled); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/options", root, "")
	wantStatus(t, resp, http.StatusOK)
	options.Keys = nil
	decodeInto(t, resp, &options)
	if len(options.Keys) != 1 {
		t.Fatalf("可选项 = %+v", options)
	}
	for _, m := range options.Keys[0].MediaModels {
		wantAvailable := m.Backend == mediagen.BackendCodex
		if m.Available != wantAvailable || (!wantAvailable && (m.ReasonCode != mediagen.ReasonAgentNotConfigured || m.Reason == "")) {
			t.Fatalf("Grok 账号停用后 %s = %+v", m.ID, m)
		}
	}
	if err := e.st.SetAgentStatus(t.Context(), mustGrokAccount(t, e), store.AgentStatusActive); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}

	// 对话：新建 → 进页读数 → 提交 → 陪等到完成（假引擎列目录、写 PROJECT.md）→ 删对话。
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats", root, `{"engine":"nope","key_id":`+jsonNumber(owner)+`}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != hostagent.CodeEngineNotReady {
		t.Fatalf("不认识的引擎的错误码 = %q", code)
	}
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats", root, `{"engine":"fake","key_id":`+jsonNumber(owner)+`,"effort":"high"}`)
	wantStatus(t, resp, http.StatusCreated)
	var chatOne struct {
		Chat struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
			Engine      string `json:"engine"`
			KeyDisplay  string `json:"key_display"`
			Model       string `json:"model"`
			Status      string `json:"status"`
		} `json:"chat"`
	}
	decodeInto(t, resp, &chatOne)
	chatID := chatOne.Chat.ID
	if chatID == "" || chatOne.Chat.WorkspaceID != ws.ID || chatOne.Chat.Engine != "fake" || chatOne.Chat.Model != "fake-1" || chatOne.Chat.Status != "idle" {
		t.Fatalf("新建对话 = %+v", chatOne.Chat)
	}
	var page struct {
		Workspace    workspaceDTO `json:"workspace"`
		ChatsEnabled bool         `json:"chats_enabled"`
		Chats        []struct {
			ID string `json:"id"`
		} `json:"chats"`
		Files []studioFileDTO `json:"files"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &page)
	if page.Workspace.ID != ws.ID || !page.ChatsEnabled || len(page.Chats) != 1 || len(page.Files) != 2 || page.Files[0].Name != "agent/PROJECT.md" || page.Workspace.Template != "general" {
		t.Fatalf("进页读数 = %+v", page)
	}
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID+"/runs", root, `{"text":"看看素材并建立项目说明","images":[]}`)
	wantStatus(t, resp, http.StatusAccepted)
	var submitted struct {
		Run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"run"`
		State struct {
			Revision int64 `json:"revision"`
		} `json:"state"`
	}
	decodeInto(t, resp, &submitted)
	if submitted.Run.ID == "" {
		t.Fatal("提交没有回指令")
	}
	deadline := time.Now().Add(15 * time.Second)
	var seen map[string]int
	for {
		run, err := e.st.GetStudioRun(context.Background(), submitted.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !store.AgentRunActive(run.Status) {
			if run.Status != store.AgentRunSucceeded {
				t.Fatalf("指令终态 = %+v", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("指令未在期限内结束")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 已结束的指令不能再取消：409 agent_run_finished。
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID+"/runs/"+submitted.Run.ID, root, "")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_run_finished" {
		t.Fatalf("取消已结束指令的错误码 = %q", code)
	}
	var waited struct {
		State struct {
			Status string `json:"status"`
		} `json:"state"`
		Events []struct {
			Kind  string `json:"kind"`
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"events"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID+"/wait?revision=0&after=0", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &waited)
	seen = map[string]int{}
	for _, ev := range waited.Events {
		seen[ev.Kind]++
		if ev.Kind == "file" && ev.Title != "agent/PROJECT.md" {
			t.Fatalf("文件事件 = %+v", ev)
		}
	}
	if seen["user"] != 1 || seen["assistant"] != 1 || seen["file"] != 1 || seen["session"] != 1 {
		t.Fatalf("事件计数 = %v", seen)
	}
	if data, err := os.ReadFile(ws.Path + "/agent/PROJECT.md"); err != nil || !strings.Contains(string(data), "看看素材") {
		t.Fatalf("agent/PROJECT.md = %q / %v", data, err)
	}
	// 目录清单里 agent/PROJECT.md 标为智能体写入。
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files", root, "")
	decodeInto(t, resp, &files)
	if len(files.Files) != 2 || files.Files[0].Name != "agent/PROJECT.md" || files.Files[0].Origin != "agent" {
		t.Fatalf("清单 = %+v", files.Files)
	}
	// 不是创作工作空间的 id：409。
	resp = e.do("GET", "/admin/v1/workspaces/nope/files", root, "")
	wantStatus(t, resp, http.StatusNotFound)

	// 审计里没有指令正文与文件内容。
	auditEvents := map[string]bool{}
	for _, a := range e.auditRows() {
		auditEvents[a.Event] = true
		if strings.Contains(a.Detail, "看看素材") || strings.Contains(a.Detail, "# 项目") || strings.Contains(a.Detail, "机密说明") {
			t.Fatalf("审计泄漏正文: %+v", a)
		}
	}
	for _, want := range []string{"workspace.create", "workspace.file_upload", "workspace.file_rename", "workspace.file_delete", "studio.chat_create", "studio.instruction", "studio.media_config"} {
		if !auditEvents[want] {
			t.Fatalf("缺审计事件 %s: %v", want, auditEvents)
		}
	}

	// 归档：会话结束，读数带 archived_at，之后提交 409，时间线仍可读。
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID+"/archive", root, "")
	wantStatus(t, resp, http.StatusOK)
	var archived struct {
		Chat struct {
			ArchivedAt *time.Time `json:"archived_at"`
		} `json:"chat"`
		Events []struct {
			Kind string `json:"kind"`
		} `json:"events"`
	}
	decodeInto(t, resp, &archived)
	if archived.Chat.ArchivedAt == nil {
		t.Fatal("归档响应没有 archived_at")
	}
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID+"/runs", root, `{"text":"再来一条","images":[]}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_chat_archived" {
		t.Fatalf("归档后提交的错误码 = %q", code)
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID, root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &archived)
	if archived.Chat.ArchivedAt == nil || len(archived.Events) == 0 {
		t.Fatalf("归档后的对话读数 = %+v", archived)
	}
	engine.mu.Lock()
	closedByArchive := engine.closed
	engine.mu.Unlock()
	if closedByArchive == 0 {
		t.Fatal("归档应关掉引擎会话")
	}

	// 引擎在节点上起：程序是探测到的路径、工作目录是工作空间、MCP 端点是设备侧路径。
	engine.mu.Lock()
	last := engine.last
	engine.mu.Unlock()
	if last.Binary != "/usr/local/bin/codex" || last.Workdir != ws.Path || last.Tools.URL != studio.DefaultToolPath || last.HostID != ws.HostID {
		t.Fatalf("引擎入参 = %+v", last)
	}

	// 删对话、删工作空间（勾了 remove_dir 连主机目录一起收走，会话关闭）。
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatID, root, "")
	wantStatus(t, resp, http.StatusOK)
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID, root, `{"remove_dir":true}`)
	wantStatus(t, resp, http.StatusNoContent)
	if _, err := os.Stat(ws.Path); !os.IsNotExist(err) {
		t.Fatal("删除工作空间后目录仍在")
	}
	if _, err := e.st.GetWorkspace(context.Background(), ws.ID); err == nil {
		t.Fatal("删除工作空间后行仍在")
	}
	engine.mu.Lock()
	closed := engine.closed
	engine.mu.Unlock()
	if closed == 0 {
		t.Fatal("删对话应关掉引擎会话")
	}
}

// 存量的设备上的创作空间：文件照常管理，进页读数标明不能对话，新建对话与可选项都答明原因。
func TestStudioDeviceWorkspaceLegacy(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	sh := withStudio(t, e, root)
	owner := mediaOwner(t, e)
	id := store.NewULID(time.Now())
	dir := sh.mgr.PathFor(id)
	for _, sub := range studio.Dirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := e.st.CreateWorkspace(context.Background(), store.NewWorkspace{ID: id, Kind: store.WorkspaceKindStudio, Name: "legacy", Path: dir, Template: studio.DefaultTemplate})
	if err != nil {
		t.Fatal(err)
	}
	resp := e.uploadStudio(root, ws.ID, "media/ref.png", testPNG(t))
	wantStatus(t, resp, http.StatusCreated)
	var page struct {
		ChatsEnabled bool            `json:"chats_enabled"`
		ChatsReason  string          `json:"chats_reason"`
		Files        []studioFileDTO `json:"files"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &page)
	if page.ChatsEnabled || !strings.Contains(page.ChatsReason, "工作节点") || len(page.Files) != 1 {
		t.Fatalf("进页读数 = %+v", page)
	}
	var options struct {
		Engines []struct {
			Ready  bool   `json:"ready"`
			Reason string `json:"reason"`
		} `json:"engines"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/agent/options", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &options)
	if len(options.Engines) != 1 || options.Engines[0].Ready || options.Engines[0].Reason == "" {
		t.Fatalf("可选项 = %+v", options)
	}
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats", root, `{"engine":"fake","key_id":`+jsonNumber(owner)+`}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != hostagent.CodeEngineNotReady {
		t.Fatalf("错误码 = %q", code)
	}
	// 删除连设备上的目录一起收走。
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID, root, "")
	wantStatus(t, resp, http.StatusNoContent)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("删除后设备上的目录仍在")
	}
}

// 主机上的创作工作空间：目录经守护进程读写（上传落到假主机的家目录、内联读取带 Range 透传、
// 缩略图缓存在设备上），对话的工具经守护进程落到主机上，删除可选连主机上的目录。
func TestStudioWorkspaceOnHost(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	sh := withStudio(t, e, root)
	fake, id := sh.fake, sh.id
	owner := mediaOwner(t, e)
	var resp *http.Response

	// 创建：目录落在假主机的 ~/workspaces/poster；读数带主机信息、host_ready。
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster","host_id":`+id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	decodeInto(t, resp, &created)
	ws := created.Workspace
	hostDir := fake.Workspaces()["poster"].Path
	if ws.HostID == 0 || ws.Path != hostDir || !ws.HostReady || ws.HostName == "" {
		t.Fatalf("创建结果 = %+v（主机目录 %s）", ws, hostDir)
	}
	if info, err := os.Stat(hostDir); err != nil || !info.IsDir() {
		t.Fatalf("主机上的目录未建: %v", err)
	}
	// agent/PROJECT.md 经守护进程预置到主机目录里。
	if data, err := os.ReadFile(hostDir + "/agent/PROJECT.md"); err != nil || !strings.Contains(string(data), "创作类型：通用") {
		t.Fatalf("主机上预置的 agent/PROJECT.md = %q / %v", data, err)
	}
	// 同主机同名的开发工作空间冲突。
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"name":"poster","host_id":`+id+`}`)
	wantStatus(t, resp, http.StatusConflict)

	// 上传：经守护进程落到主机目录的 media/（子目录按需建出来）；缩略图缓存在设备上；清单带尺寸。
	pngData := testPNG(t)
	resp = e.uploadStudio(root, ws.ID, "media/ref.png", pngData)
	wantStatus(t, resp, http.StatusCreated)
	var one struct {
		File studioFileDTO `json:"file"`
	}
	decodeInto(t, resp, &one)
	if one.File.Name != "media/ref.png" || one.File.Width != 32 || !one.File.HasThumb || one.File.Bytes != int64(len(pngData)) {
		t.Fatalf("上传结果 = %+v", one.File)
	}
	if data, err := os.ReadFile(hostDir + "/media/ref.png"); err != nil || !bytes.Equal(data, pngData) {
		t.Fatalf("主机上的文件 = %d 字节 / %v", len(data), err)
	}
	if _, err := os.Stat(e.dir + "/workspaces/" + ws.ID + "/.thumbs/media/ref.png.jpg"); err != nil {
		t.Fatalf("设备上没有缩略图缓存: %v", err)
	}
	// 内联读取经守护进程透传，Range 也透传。
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/ref.png", root, "")
	wantStatus(t, resp, http.StatusOK)
	if body := readAll(t, resp); body != string(pngData) {
		t.Fatal("透传读取的字节与上传的不同")
	}
	rangeReq := e.req("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/ref.png", root, "")
	rangeReq.Header.Set("Range", "bytes=0-3")
	resp = e.send(rangeReq)
	wantStatus(t, resp, http.StatusPartialContent)
	if body := readAll(t, resp); body != string(pngData[:4]) || !strings.HasPrefix(resp.Header.Get("Content-Range"), "bytes 0-3/") {
		t.Fatalf("Range 透传 = %q / %q", body, resp.Header.Get("Content-Range"))
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files/media/ref.png/thumb", root, "")
	wantStatus(t, resp, http.StatusOK)
	// 主机上直接出现的文件（比如命令生成的）：清单对账后补一行 unknown。
	if err := os.MkdirAll(hostDir+"/docs", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostDir+"/docs/notes.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var files struct {
		Files []studioFileDTO `json:"files"`
	}
	resp = e.do("GET", "/admin/v1/workspaces/"+ws.ID+"/files", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &files)
	if len(files.Files) != 3 || files.Files[0].Name != "agent/PROJECT.md" || files.Files[2].Name != "docs/notes.txt" || files.Files[2].Origin != "unknown" {
		t.Fatalf("清单 = %+v", files.Files)
	}
	// 改名与删除都落到主机上。
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/notes.txt", root, `{"path":"docs/brief.txt"}`)
	wantStatus(t, resp, http.StatusOK)
	if _, err := os.Stat(hostDir + "/docs/brief.txt"); err != nil {
		t.Fatalf("主机上改名未生效: %v", err)
	}
	// 还没对账过就改名的文件（清单没列过它）：改名后补出附注，端点照样回 200 与 unknown 行。
	if err := os.WriteFile(hostDir+"/docs/raw.txt", []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp = e.do("PATCH", "/admin/v1/workspaces/"+ws.ID+"/files/docs/raw.txt", root, `{"path":"agent/renamed.txt"}`)
	wantStatus(t, resp, http.StatusOK)
	var renamed struct {
		File studioFileDTO `json:"file"`
	}
	decodeInto(t, resp, &renamed)
	if renamed.File.Name != "agent/renamed.txt" || renamed.File.Origin != "unknown" || renamed.File.Bytes != 3 {
		t.Fatalf("未对账文件改名后的读数 = %+v", renamed.File)
	}
	if _, err := os.Stat(hostDir + "/agent/renamed.txt"); err != nil {
		t.Fatalf("主机上挪目录未生效: %v", err)
	}
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/files/agent/renamed.txt", root, "")
	wantStatus(t, resp, http.StatusNoContent)
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID+"/files/docs/brief.txt", root, "")
	wantStatus(t, resp, http.StatusNoContent)
	if _, err := os.Stat(hostDir + "/docs/brief.txt"); !os.IsNotExist(err) {
		t.Fatal("主机上删除未生效")
	}

	// 对话：假引擎列目录并写 agent/PROJECT.md（经守护进程落到主机上）。
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats", root, `{"key_id":`+jsonNumber(owner)+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var chatOne struct {
		Chat struct {
			ID string `json:"id"`
		} `json:"chat"`
	}
	decodeInto(t, resp, &chatOne)
	resp = e.do("POST", "/admin/v1/workspaces/"+ws.ID+"/agent/chats/"+chatOne.Chat.ID+"/runs", root, `{"text":"整理素材","images":[]}`)
	wantStatus(t, resp, http.StatusAccepted)
	var submitted struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	decodeInto(t, resp, &submitted)
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := e.st.GetStudioRun(context.Background(), submitted.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !store.AgentRunActive(run.Status) {
			if run.Status != store.AgentRunSucceeded {
				t.Fatalf("指令终态 = %+v", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("指令未在期限内结束")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if data, err := os.ReadFile(hostDir + "/agent/PROJECT.md"); err != nil || !strings.Contains(string(data), "整理素材") {
		t.Fatalf("主机上的 agent/PROJECT.md = %q / %v", data, err)
	}

	// 删除：不勾 remove_dir 只删记录与设备上的缓存，主机目录保留；勾了连主机目录一起删。
	resp = e.do("DELETE", "/admin/v1/workspaces/"+ws.ID, root, `{}`)
	wantStatus(t, resp, http.StatusNoContent)
	if _, err := os.Stat(hostDir); err != nil {
		t.Fatal("不勾 remove_dir 时主机目录不该被删")
	}
	if _, err := os.Stat(e.dir + "/workspaces/" + ws.ID); !os.IsNotExist(err) {
		t.Fatal("设备上的缩略图缓存应已删除")
	}
	resp = e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"poster2","host_id":`+id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	decodeInto(t, resp, &created)
	dir2 := created.Workspace.Path
	resp = e.do("DELETE", "/admin/v1/workspaces/"+created.Workspace.ID, root, `{"remove_dir":true}`)
	wantStatus(t, resp, http.StatusNoContent)
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatal("勾了 remove_dir 后主机目录仍在")
	}
}

// 真引擎注册表驱动新建选项：第三项 Grok、独立的订阅/模型、落库后的引擎与密钥保持不变。
func TestStudioGrokChatOptionsAndCreate(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	sh := withStudio(t, e, root)
	hosts := agenthost.New(agenthost.Options{Store: e.st, Dial: sh.fake.SSH.Dial})
	dev := devhost.New(devhost.Options{Store: e.st, Hosts: hosts, Binaries: devhosttest.FakeBinaries{}})
	mgr := studio.New(studio.Options{Store: e.st, Engines: nodeengine.Engines(nodeengine.Options{Resolve: e.srv.AgentChatConfig}), Hosts: hosts, DevHosts: dev, DataDir: e.dir})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	e.srv.SetStudio(mgr)
	resp := e.do("POST", "/admin/v1/workspaces", root, `{"kind":"studio","name":"grok-studio","host_id":`+sh.id+`}`)
	wantStatus(t, resp, http.StatusCreated)
	var created struct {
		Workspace workspaceDTO `json:"workspace"`
	}
	decodeInto(t, resp, &created)
	base := "/admin/v1/workspaces/" + created.Workspace.ID + "/agent"
	key, _ := e.createKey(root, "Grok 对话")
	denied, _ := e.createKey(root, "未授权")
	_, grokID := mediaAccounts(t, e)
	e.srv.SyncAgentModels(t.Context())
	if _, _, err := e.st.ReplaceDevToolConfig(t.Context(), store.DevToolConfig{KeyID: key.ID, GrokAccountID: grokID}); err != nil {
		t.Fatal(err)
	}
	var options struct {
		Engines []studio.EngineOption `json:"engines"`
		Keys    []struct {
			ID      int64 `json:"id"`
			Engines map[string]struct {
				Enabled      bool   `json:"enabled"`
				Available    bool   `json:"available"`
				DefaultModel string `json:"default_model"`
			} `json:"engines"`
		} `json:"keys"`
	}
	resp = e.do("GET", base+"/options", root, "")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &options)
	if len(options.Engines) != 3 || options.Engines[2].ID != nodeengine.GrokID || options.Engines[2].Label != "Grok" || options.Engines[2].Tool != "grok" || !options.Engines[2].Ready || options.Engines[2].Path != "/usr/local/bin/grok" || options.Engines[1].Ready {
		t.Fatalf("engines: %+v", options.Engines)
	}
	model := ""
	for _, k := range options.Keys {
		access := k.Engines[nodeengine.GrokID]
		if k.ID == key.ID {
			if !access.Enabled || !access.Available || k.Engines[nodeengine.CodexID].Enabled {
				t.Fatalf("access: %+v", k.Engines)
			}
			model = access.DefaultModel
		} else if k.ID == denied.ID && access.Enabled {
			t.Fatal("ungranted key enabled")
		}
	}
	if model == "" {
		t.Fatal("no Grok default model")
	}
	body := `{"engine":"grok_acp","key_id":` + jsonNumber(key.ID) + `,"effort":"high"}`
	resp = e.do("POST", base+"/chats", root, body)
	wantStatus(t, resp, http.StatusCreated)
	var result struct {
		Chat store.StudioChat `json:"chat"`
	}
	decodeInto(t, resp, &result)
	if result.Chat.Engine != nodeengine.GrokID || result.Chat.KeyID != key.ID || result.Chat.Model != model || result.Chat.Effort != "high" {
		t.Fatalf("chat: %+v", result.Chat)
	}
	stored, err := e.st.GetStudioChat(t.Context(), result.Chat.ID)
	if err != nil || stored.Engine != nodeengine.GrokID {
		t.Fatalf("stored: %+v / %v", stored, err)
	}
	resp = e.do("POST", base+"/chats", root, `{"engine":"grok_acp","key_id":`+jsonNumber(denied.ID)+`}`)
	wantStatus(t, resp, http.StatusBadRequest)
}
