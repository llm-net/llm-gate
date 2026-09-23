package studio_test

// 主机上的创作工作空间验收（假工作节点 devhosttest：真跑 SSH 协议、进程内的真守护进程，
// 目录真的在假主机的家目录下）：
//   - 目录经守护进程读写：三个子目录在主机上按需建出来、上传落到主机、缩略图缓存在设备、对账
//     认出主机上直接出现的文件、改名 / 删除落到主机、超限上传不留半成品、ServeFile 透传 Range；
//   - 对话的引擎在节点上起：拿到节点上的程序路径与工作空间目录，经 ExecSink 报的命令 / 改文件
//     落时间线；工具清单里没有 exec；生成结果经守护进程落到主机目录；
//   - 设备上的空间不能对话；
//   - RemoveDir 只收设备上的缓存，主机目录不动。

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost/devhosttest"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/nodeengine"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

const hostPassword = "n0t-a-real-password"

type hostEnv struct {
	*env
	fake    *devhosttest.Host
	hosts   *agenthost.Manager
	dev     *devhost.Manager
	host    *store.AgentHost
	hostDir string
}

// newHostEnv 起一台假工作节点（工具探测报 PATH 上有 codex）、纳管、装守护进程，在上面建一个
// 创作工作空间。
func newHostEnv(t *testing.T) *hostEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, dir: dir, st: st, png: pngBytes(t, 640, 480)}
	e.gen = &fakeGen{st: st, png: pngBytes(t, 1024, 768)}
	e.media = newMedia(st, dir, e.gen)
	fake := devhosttest.New(t, devhosttest.Options{Username: "dev", Password: hostPassword, Git: "git version 2.43.0",
		DevToolsFound: map[string]string{"codex": "/usr/local/bin/codex"}})
	hosts := agenthost.New(agenthost.Options{Store: st, Dial: fake.SSH.Dial})
	dev := devhost.New(devhost.Options{Store: st, Hosts: hosts, Binaries: devhosttest.FakeBinaries{}})
	ctx := context.Background()
	if _, err := hosts.GenerateCertificate(ctx); err != nil {
		t.Fatal(err)
	}
	addr, port := fake.Addr()
	host, err := hosts.Enroll(ctx, agenthost.EnrollRequest{Name: "工作站", Kind: store.AgentHostKindWorker, Address: addr, Port: port, Username: "dev", Password: hostPassword, ConfigureSudo: true})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := dev.Install(ctx, devhost.InstallRequest{ID: host.ID}); err != nil {
		t.Fatalf("install devd: %v", err)
	}
	var mgr *studio.Manager
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mgr.MCPHandler().ServeHTTP(w, r) }))
	t.Cleanup(mcp.Close)
	e.engine = &fakeEngine{base: mcp.URL}
	e.mcpURL = mcp.URL + studio.DefaultToolPath
	e.hosts, e.dev = hosts, dev
	mgr = studio.New(studio.Options{Store: st, Engines: []nodeengine.Engine{e.engine}, Media: e.media, Hosts: hosts, DevHosts: dev, DataDir: dir,
		IdleTimeout: time.Hour})
	e.m = mgr
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	res, err := dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: host.ID, Name: "poster"})
	if err != nil {
		t.Fatalf("CreateWorkspace on host: %v", err)
	}
	e.ws, err = st.CreateWorkspace(ctx, store.NewWorkspace{Kind: store.WorkspaceKindStudio, HostID: host.ID, Name: "poster", Path: res.Path, Template: studio.DefaultTemplate})
	if err != nil {
		t.Fatal(err)
	}
	e.enableModels()
	return &hostEnv{env: e, fake: fake, hosts: hosts, dev: dev, host: host, hostDir: res.Path}
}

func TestHostFilesLifecycle(t *testing.T) {
	h := newHostEnv(t)
	e := h.env
	ctx := context.Background()
	if info, err := os.Stat(h.hostDir); err != nil || !info.IsDir() {
		t.Fatalf("主机上的目录未建: %v", err)
	}
	// 主机上只有根目录：第一次上传把 media/ 建出来。
	if _, err := os.Stat(filepath.Join(h.hostDir, "media")); !os.IsNotExist(err) {
		t.Fatal("假主机的创建脚本不该预建子目录（这里要验按需建目录）")
	}
	f := e.upload("media/ref.png", e.png)
	if f.Width != 640 || f.Height != 480 || f.Bytes != int64(len(e.png)) || !e.m.HasThumb(e.ws, "media/ref.png") {
		t.Fatalf("上传后的附注 = %+v（thumb=%v）", f, e.m.HasThumb(e.ws, "media/ref.png"))
	}
	if data, err := os.ReadFile(filepath.Join(h.hostDir, "media", "ref.png")); err != nil || !bytes.Equal(data, e.png) {
		t.Fatalf("主机上的文件不符: %v", err)
	}
	// 缩略图缓存在设备上，不在主机目录里。
	if _, err := os.Stat(filepath.Join(e.dir, "workspaces", e.ws.ID, ".thumbs", "media", "ref.png.jpg")); err != nil {
		t.Fatalf("设备上没有缩略图缓存: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, ".thumbs")); !os.IsNotExist(err) {
		t.Fatal("缩略图不该写到主机目录里")
	}
	// 列目录把另两个子目录也建出来；主机上直接出现的文件：对账补行；图像补量尺寸与缩略图。
	if err := os.WriteFile(filepath.Join(h.hostDir, "media", "extra.png"), pngBytes(t, 20, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := e.m.ListFiles(ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range studio.Dirs {
		if info, err := os.Stat(filepath.Join(h.hostDir, dir)); err != nil || !info.IsDir() {
			t.Fatalf("主机上的子目录 %s 未建: %v", dir, err)
		}
	}
	if len(files) != 2 || files[1].Name != "media/extra.png" || files[1].Origin != store.StudioFileOriginUnknown || files[1].Width != 20 || !e.m.HasThumb(e.ws, "media/extra.png") {
		t.Fatalf("对账后的清单 = %+v", files)
	}
	if files[0].Name != "media/ref.png" || files[0].Origin != store.StudioFileOriginUpload {
		t.Fatalf("没改过的上传文件对账后应保持原附注: %+v", files[0])
	}
	// 主机上的命令改写了上传文件、大小不变：按修改时刻认出来，附注归零为 unknown。
	e.upload("media/same.bin", []byte("aaaa"))
	same := filepath.Join(h.hostDir, "media", "same.bin")
	if err := os.WriteFile(same, []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(same, later, later); err != nil {
		t.Fatal(err)
	}
	files, err = e.m.ListFiles(ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name == "media/same.bin" && f.Origin != store.StudioFileOriginUnknown {
			t.Fatalf("同大小改写后附注应归零: %+v", f)
		}
	}
	if err := e.m.DeleteFile(ctx, e.ws, "media/same.bin"); err != nil {
		t.Fatalf("归零后的文件可以删: %v", err)
	}
	// 读取、改名、删除都落到主机上；文本在 docs/ 与 agent/ 之间挪动。
	if data, err := e.m.ReadFile(ctx, e.ws, "media/ref.png", studio.MaxUploadBytes); err != nil || !bytes.Equal(data, e.png) {
		t.Fatalf("ReadFile = %d / %v", len(data), err)
	}
	if err := e.m.RenameFile(ctx, e.ws, "media/extra.png", "media/cover.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, "media", "cover.png")); err != nil || !e.m.HasThumb(e.ws, "media/cover.png") {
		t.Fatal("改名未落到主机 / 缩略图未跟着改")
	}
	if err := e.m.RenameFile(ctx, e.ws, "media/cover.png", "media/ref.png"); err == nil {
		t.Fatal("改成已占用的名字应拒绝")
	}
	if err := e.m.DeleteFile(ctx, e.ws, "media/cover.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, "media", "cover.png")); !os.IsNotExist(err) {
		t.Fatal("删除未落到主机")
	}
	if err := e.m.DeleteFile(ctx, e.ws, "media/cover.png"); err == nil {
		t.Fatal("重复删除应报不存在")
	}
	e.upload("docs/brief.md", []byte("# 简报\n"))
	if err := e.m.RenameFile(ctx, e.ws, "docs/brief.md", "agent/brief.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, "agent", "brief.md")); err != nil {
		t.Fatal("挪目录未落到主机")
	}
	if err := e.m.DeleteFile(ctx, e.ws, "agent/brief.md"); err != nil {
		t.Fatal(err)
	}
	// 超限上传：主机上不留半成品。
	if _, err := e.m.SaveFile(ctx, e.ws, "media/big.bin", bytes.NewReader(make([]byte, 100)), 50, studio.FileMeta{}); err == nil {
		t.Fatal("超限上传应报错")
	}
	entries, _ := os.ReadDir(filepath.Join(h.hostDir, "media"))
	for _, en := range entries {
		if strings.Contains(en.Name(), ".llmgate-") || en.Name() == "big.bin" {
			t.Fatalf("超限上传在主机上留下了 %s", en.Name())
		}
	}
	// ServeFile：Range 透传到守护进程。
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Range", "bytes=0-9")
	rec := httptest.NewRecorder()
	if err := e.m.ServeFile(rec, req, e.ws, "media/ref.png", "inline"); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), e.png[:10]) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("ServeFile Range = %d，%d 字节，%q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
	}
	// RemoveDir 只收设备上的缓存。
	if err := e.m.RemoveDir(e.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "workspaces", e.ws.ID)); !os.IsNotExist(err) {
		t.Fatal("设备上的缓存目录仍在")
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, "media", "ref.png")); err != nil {
		t.Fatal("RemoveDir 不该动主机上的目录")
	}
}

func TestHostEngineOnNode(t *testing.T) {
	h := newHostEnv(t)
	e := h.env
	ctx := context.Background()
	e.upload("media/ref.png", e.png)
	opts := e.m.EngineOptions(ctx, e.ws)
	if len(opts) != 1 || !opts[0].Ready || opts[0].Path != "/usr/local/bin/codex" || opts[0].Tool != "codex" {
		t.Fatalf("引擎读数 = %+v", opts)
	}
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if chat.Engine != "fake" {
		t.Fatalf("对话引擎 = %q", chat.Engine)
	}
	row, _ := e.st.GetStudioChat(ctx, chat.ID)
	for _, want := range []string{"工作站", "dev@", h.hostDir, "shell", "`media/ref.png`"} {
		if !strings.Contains(row.Instructions, want) {
			t.Fatalf("开发者指令缺少 %q:\n%s", want, row.Instructions)
		}
	}
	if strings.Contains(row.Instructions, "`exec`") {
		t.Fatalf("开发者指令不该再提 exec:\n%s", row.Instructions)
	}
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		res := mcpCall(t, tools, "tools/list", nil)
		names := map[string]bool{}
		for _, tl := range res["tools"].([]any) {
			names[tl.(map[string]any)["name"].(string)] = true
		}
		if names["exec"] || !names["generate_image"] {
			t.Errorf("工具清单 = %v", names)
		}
		// 引擎在节点上自己跑命令、改文件，经 ExecSink 报给设备落时间线。
		es := sink.(hostagent.ExecSink)
		if err := os.WriteFile(filepath.Join(h.hostDir, "docs", "table.csv"), []byte("a,b\n"), 0o644); err != nil {
			t.Error(err)
		}
		code := 3
		es.Command("ffprobe media/ref.png; exit 3", strings.Repeat("x", 20<<10)+"tail-marker", &code, 1500*time.Millisecond)
		es.FileChange(filepath.Join(h.hostDir, "docs", "table.csv"), "write")
		es.FileChange("/etc/elsewhere", "delete")
		es.ToolUse("Read", filepath.Join(h.hostDir, "media", "ref.png"))
		text, isErr, _ := callTool(t, tools, "list_files", nil)
		if isErr || !strings.Contains(text, "docs/table.csv") {
			t.Errorf("list_files = %q / %v", text, isErr)
		}
		// 看图经守护进程取回；生成结果落到主机的 media/。
		if _, isErr, content := callTool(t, tools, "view_image", map[string]any{"path": "media/ref.png"}); isErr || len(content) != 2 {
			t.Errorf("view_image = %v / %d 块", isErr, len(content))
		}
		text, isErr, content := callTool(t, tools, "generate_image", map[string]any{"prompt": "海报", "name": "hero", "reference_images": []string{"media/ref.png"}})
		if isErr || !strings.Contains(text, "saved media/hero.png") || len(content) != 2 {
			t.Errorf("generate_image = %q / %v / %d 块", text, isErr, len(content))
		}
		sink.Message("done")
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	run, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "整理"})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.waitRun(run.ID); got.Status != store.AgentRunSucceeded {
		t.Fatalf("指令终态 = %+v", got)
	}
	// 会话启动时按那一次探测把节点上的创作工具写进指令：假节点没装 ffmpeg，Agent 不必再试探。
	started := e.startedInstructions(0)
	for _, want := range []string{"## 节点上的创作工具", "FFmpeg：没有安装", "CJK 字体：没有", "「工具配置」页安装「创作工具」"} {
		if !strings.Contains(started, want) {
			t.Fatalf("会话指令缺少 %q:\n%s", want, started)
		}
	}
	if strings.Contains(started, "command -v") {
		t.Fatalf("会话指令不该再让 Agent 用 command -v 试探:\n%s", started)
	}
	e.engine.mu.Lock()
	req := e.engine.sessions[0].req
	e.engine.mu.Unlock()
	if req.HostID != h.host.ID || req.Binary != "/usr/local/bin/codex" || req.Workdir != h.hostDir || req.Tools.URL != studio.DefaultToolPath ||
		req.Tools.Server != studio.ToolServer || req.Tools.Token == "" || req.Chat.KeyID != 1 {
		t.Fatalf("引擎入参 = %+v", req)
	}
	if _, err := os.Stat(filepath.Join(h.hostDir, "media", "hero.png")); err != nil {
		t.Fatal("生成结果未落到主机目录")
	}
	if hero, err := e.st.GetStudioFile(ctx, e.ws.ID, "media/hero.png"); err != nil || hero.Origin != store.StudioFileOriginGenerated || hero.Width != 1024 || !e.m.HasThumb(e.ws, "media/hero.png") {
		t.Fatalf("生成图的附注 = %+v / %v", hero, err)
	}
	// 时间线：command 事件（退出码在 meta 里、输出只留尾段）、file 事件（工作空间里的记相对路径）、tool 事件。
	var command, fileIn, fileOut, read *store.StudioEvent
	for _, ev := range e.events(chat.ID) {
		switch {
		case ev.Kind == store.StudioEventCommand:
			command = &ev
		case ev.Kind == store.StudioEventFile && ev.Title == "docs/table.csv":
			fileIn = &ev
		case ev.Kind == store.StudioEventFile && ev.Title == "/etc/elsewhere":
			fileOut = &ev
		case ev.Kind == store.StudioEventTool && ev.Title == "Read":
			read = &ev
		}
	}
	if command == nil || command.Title != "ffprobe media/ref.png; exit 3" || !strings.Contains(command.Meta, `"exit_code":3`) ||
		!strings.Contains(command.Meta, `"truncated":true`) || !strings.HasSuffix(command.Body, "tail-marker") || len(command.Body) > 17<<10 || command.DurationMs != 1500 {
		t.Fatalf("command 事件 = %+v", command)
	}
	if fileIn == nil || !strings.Contains(fileIn.Meta, `"action":"write"`) || fileOut == nil || !strings.Contains(fileOut.Meta, `"action":"delete"`) {
		t.Fatalf("file 事件 = %+v / %+v", fileIn, fileOut)
	}
	if read == nil || read.Body != "media/ref.png" {
		t.Fatalf("tool 事件 = %+v", read)
	}
	// 生成请求带的参考图是从主机取回的 data URI；任务搬进主机目录后由内核删掉，设备上不留结果。
	calls, _ := e.gen.snapshot()
	if len(calls) != 1 || len(calls[0].Inputs.ReferenceImages) != 1 || !strings.HasPrefix(calls[0].Inputs.ReferenceImages[0], "data:image/png;base64,") {
		t.Fatalf("生成请求 = %s", describeCalls(calls))
	}
	if _, err := e.st.GetMediaJob(ctx, calls[0].JobID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("搬运后任务行应已删除：%v", err)
	}
	waitUntil(t, func() bool {
		left, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName))
		return len(left) == 0
	})
}

// 节点上没有引擎的 CLI：读数说明原因，新建对话被拒。
func TestHostEngineMissing(t *testing.T) {
	h := newHostEnv(t)
	e := h.env
	ctx := context.Background()
	claude := &fakeEngine{base: e.engine.base}
	mgr := studio.New(studio.Options{Store: e.st, Engines: []nodeengine.Engine{&toolEngine{fakeEngine: claude, tool: "claude"}}, Media: e.media,
		Hosts: h.hosts, DevHosts: h.dev, DataDir: e.dir, IdleTimeout: time.Hour})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	opts := mgr.EngineOptions(ctx, e.ws)
	if len(opts) != 1 || opts[0].Ready || !strings.Contains(opts[0].Reason, "工具配置") {
		t.Fatalf("引擎读数 = %+v", opts)
	}
	if _, err := mgr.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1}); !isCode(err, studio.CodeEngineNotReady) {
		t.Fatalf("CreateChat = %v", err)
	}
	if _, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{Engine: "nope", KeyID: 1}); !isCode(err, studio.CodeEngineNotReady) {
		t.Fatalf("不认识的引擎 = %v", err)
	}
}

// toolEngine 是认另一种开发工具的假引擎。
type toolEngine struct {
	*fakeEngine
	tool string
}

func (e *toolEngine) Tool() string { return e.tool }

func isCode(err error, code string) bool {
	var se *studio.Error
	return errors.As(err, &se) && se.Code == code
}

// 设备上的空间：只能管理文件，不能新建对话，存量对话也不能提交。
func TestDeviceWorkspaceCannotChat(t *testing.T) {
	e := newDeviceEnv(t)
	ctx := context.Background()
	if ok, reason := studio.ChatsEnabled(e.ws); ok || !strings.Contains(reason, "工作节点") {
		t.Fatalf("ChatsEnabled = %v %q", ok, reason)
	}
	for _, opt := range e.m.EngineOptions(ctx, e.ws) {
		if opt.Ready || !strings.Contains(opt.Reason, "工作节点") {
			t.Fatalf("引擎读数 = %+v", opt)
		}
	}
	if _, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1}); !isCode(err, studio.CodeEngineNotReady) {
		t.Fatalf("CreateChat = %v", err)
	}
	chat, err := e.st.CreateStudioChat(ctx, store.NewStudioChat{WorkspaceID: e.ws.ID, Engine: "fake", KeyID: 1, Model: "fake-1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "x"}); !isCode(err, studio.CodeEngineNotReady) {
		t.Fatalf("Submit = %v", err)
	}
}
