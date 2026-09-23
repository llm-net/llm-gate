package studio_test

// 创作工作空间验收（假引擎 + 假生成后端 + 真媒体生成内核，全程离线）：
//   - 目录：三个子目录（agent / docs / media）与按种类分工的落位规则、上传落盘、缩略图与尺寸、
//     与附注表对账（外来文件补行、消失的文件删行、根目录不进清单）、改名 / 挪目录、删除、路径白名单；
//   - 对话：新建时注入目录清单与可用后端，引擎经 MCP 端点调用工具——列目录、看图（图像内容）、
//     写文本（管理员素材不可覆盖）、生成图像 / 视频（经媒体生成内核落进目录、任务与设备上的
//     结果文件随即由内核删除、附注记提示词）、删 / 改名，每一步都落时间线；令牌校验；
//   - 生成：任务以 origin=studio 落库不进页面列表、同批候选、并发上限、工具调用被放弃后后台
//     接着等、进程重启后的收尾、开发者指令与工具描述出自能力表；
//   - 取消排队中的指令、结束会话、删除工作空间连目录一起收走。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/nodeengine"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

// pngBytes 造一张 w×h 的纯色 PNG。
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisyPNG 造一张压不小的 PNG（每个像素都不同），用来做超过 8 MiB 的图像。
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(2463534242)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGen 是媒体生成内核的假后端（grok 与 codex 两个名字共用这一份状态）：图像回一张 PNG
// data URI，视频回一段假 MP4 data URI；记下每次请求，以及请求那一刻任务行的样子与页面列表
// 的条数。block 非空时 Generate 等它关闭才返回。
type fakeGen struct {
	st    *store.Store
	mu    sync.Mutex
	calls []mediagen.Request
	// jobs 是每次 Generate 那一刻读到的任务行；pageListed 是那一刻页面列表的条数之和。
	jobs       []store.MediaJob
	pageListed int
	png        []byte
	fail       bool
	block      chan struct{}
	entered    chan string
}

func (g *fakeGen) backends() []mediagen.Backend {
	return []mediagen.Backend{fakeBackend{g, mediagen.BackendGrok}, fakeBackend{g, mediagen.BackendCodex}}
}

func (g *fakeGen) snapshot() ([]mediagen.Request, []store.MediaJob) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mediagen.Request(nil), g.calls...), append([]store.MediaJob(nil), g.jobs...)
}

func (g *fakeGen) listedOnPage() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pageListed
}

// describeCalls 把生成请求折成一行摘要（媒体输入只报形态，不把整段 base64 打进失败信息）。
func describeCalls(calls []mediagen.Request) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "[%s/%s op=%s key=%d account=%d inputs=%v params=%v prompt=%q] ", c.Model.Backend, c.Model.ID, c.Operation, c.KeyID, c.AccountID, c.Inputs.Shape(), c.Params, c.Prompt)
	}
	return b.String()
}

type fakeBackend struct {
	g    *fakeGen
	name string
}

func (b fakeBackend) Name() string                    { return b.name }
func (b fakeBackend) Validate(mediagen.Request) error { return nil }

func (b fakeBackend) Generate(ctx context.Context, in mediagen.Request) (mediagen.Result, error) {
	g := b.g
	job, _ := g.st.GetMediaJob(ctx, in.JobID)
	listed, _ := g.st.ListPageMediaJobs(ctx)
	g.mu.Lock()
	g.calls = append(g.calls, in)
	if job != nil {
		g.jobs = append(g.jobs, *job)
	}
	g.pageListed += len(listed)
	fail, block, entered := g.fail, g.block, g.entered
	g.mu.Unlock()
	if entered != nil {
		entered <- in.JobID
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return mediagen.Result{}, ctx.Err()
		}
	}
	if fail {
		return mediagen.Result{Status: store.MediaStatusFailed, Error: "platform said no"}, nil
	}
	res := mediagen.Result{Status: store.MediaStatusSucceeded, AccountID: in.AccountID}
	if in.Model.Kind == store.ModelKindVideo {
		res.MediaURL = "data:video/mp4;base64," + base64.StdEncoding.EncodeToString([]byte("not-really-mp4"))
		res.MediaType = "video"
	} else {
		res.MediaURL = "data:image/png;base64," + base64.StdEncoding.EncodeToString(g.png)
		res.MediaType = "image"
	}
	return res, nil
}

func (b fakeBackend) Refresh(context.Context, store.MediaJob) (mediagen.Result, error) {
	return mediagen.Result{}, nil
}

// 假授权：密钥 1 钉了 Codex（账号 7）与 Grok（账号 8）；keyGrokOnly 只钉 Grok；keyNoMedia
// 什么订阅都没钉。
const (
	keyGrokOnly = 2
	keyNoMedia  = 3
)

func fakeEntitlements(_ context.Context, keyID int64) (map[string]mediagen.Entitlement, error) {
	codex := mediagen.Entitlement{Configured: true, Available: true, AccountID: 7}
	grok := mediagen.Entitlement{Configured: true, Available: true, AccountID: 8}
	switch keyID {
	case keyGrokOnly:
		return map[string]mediagen.Entitlement{store.AgentProviderGrok: grok}, nil
	case keyNoMedia:
		return map[string]mediagen.Entitlement{}, nil
	}
	return map[string]mediagen.Entitlement{store.AgentProviderCodex: codex, store.AgentProviderGrok: grok}, nil
}

// newMedia 建一份真内核，接上假授权与假后端。
func newMedia(st *store.Store, dir string, gen *fakeGen) *mediagen.Service {
	svc := mediagen.New(st, discardLogger(), nil, dir)
	svc.SetEntitlements(fakeEntitlements)
	svc.SetBackends(gen.backends()...)
	return svc
}

// fakeEngine 是可编程的节点端引擎：script 决定一条指令做什么（可经 MCP 端点调用工具）。它认
// 工作节点上的 codex（假主机的工具探测报 PATH 上有 /usr/local/bin/codex）；base 是 MCP 端点所在
// 的测试服务器（真引擎经远程转发访问设备，这里直连）。
type fakeEngine struct {
	mu       sync.Mutex
	base     string
	script   func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, interrupt <-chan struct{}) hostagent.Outcome
	sessions []*fakeSession
}

func (e *fakeEngine) ID() string        { return "fake" }
func (e *fakeEngine) Label() string     { return "Fake Engine" }
func (e *fakeEngine) Tool() string      { return "codex" }
func (e *fakeEngine) Efforts() []string { return hostagent.CodexEfforts }
func (e *fakeEngine) Validate(_ context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	if cfg.KeyID <= 0 {
		return cfg, &hostagent.Error{Code: hostagent.CodeKeyInvalid, Msg: "请为对话选择一把 API 密钥"}
	}
	cfg.KeyDisplay = "sk_fake…0000"
	if cfg.Model == "" {
		cfg.Model = "fake-1"
	}
	return cfg, nil
}
func (e *fakeEngine) Start(_ context.Context, req nodeengine.Request) (hostagent.EngineSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tools := req.Tools
	tools.URL = e.base + req.Tools.URL
	s := &fakeSession{e: e, req: req, tools: tools, instructions: req.Instructions, done: make(chan struct{})}
	e.sessions = append(e.sessions, s)
	return s, nil
}

type fakeSession struct {
	e            *fakeEngine
	req          nodeengine.Request
	tools        hostagent.ToolEndpoint
	instructions string
	mu           sync.Mutex
	interrupt    chan struct{}
	closed       bool
	done         chan struct{}
}

func (s *fakeSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	s.mu.Lock()
	s.interrupt = make(chan struct{})
	ch := s.interrupt
	s.mu.Unlock()
	return s.e.script(ctx, in, sink, s.tools, ch), nil
}
func (s *fakeSession) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.interrupt:
	default:
		close(s.interrupt)
	}
	return nil
}
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

// mcpCall 从测试里像引擎那样调一次工具，回整个 result。
func mcpCall(t *testing.T, tools hostagent.ToolEndpoint, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, tools.URL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tools.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("MCP %s: %v", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("MCP %s 状态 = %d: %s", method, resp.StatusCode, raw)
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解码 MCP 应答: %v", err)
	}
	if out.Error != nil {
		t.Fatalf("MCP %s 错误: %s", method, out.Error.Message)
	}
	return out.Result
}

func callTool(t *testing.T, tools hostagent.ToolEndpoint, name string, args map[string]any) (string, bool, []any) {
	t.Helper()
	res := mcpCall(t, tools, "tools/call", map[string]any{"name": name, "arguments": args})
	content, _ := res["content"].([]any)
	text := ""
	if len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text, _ = m["text"].(string)
		}
	}
	isErr, _ := res["isError"].(bool)
	return text, isErr, content
}

type env struct {
	t      *testing.T
	dir    string
	st     *store.Store
	gen    *fakeGen
	media  *mediagen.Service
	mcpURL string
	engine *fakeEngine
	m      *studio.Manager
	ws     *store.Workspace
	png    []byte
	// hosts / dev 是假工作节点的连接（newEnv）；设备上的空间（newDeviceEnv）为空。
	hosts *agenthost.Manager
	dev   *devhost.Manager
}

// newEnv 是能对话的环境：一个落在假工作节点上的创作工作空间（studio_host_test.go）。
func newEnv(t *testing.T) *env { return newHostEnv(t).env }

// newDeviceEnv 是落在设备数据目录下的创作工作空间（只能管理文件，不能对话）。
func newDeviceEnv(t *testing.T) *env {
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
	var mgr *studio.Manager
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mgr.MCPHandler().ServeHTTP(w, r) }))
	t.Cleanup(mcp.Close)
	e.engine = &fakeEngine{base: mcp.URL}
	e.mcpURL = mcp.URL + studio.DefaultToolPath
	mgr = studio.New(studio.Options{Store: st, Engines: []nodeengine.Engine{e.engine}, Media: e.media, DataDir: dir, IdleTimeout: time.Hour})
	e.m = mgr
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	id := store.NewULID(time.Now())
	// 设备上的空间不再新建：照早先的布局手工落目录，模拟存量空间。
	path := mgr.PathFor(id)
	for _, sub := range studio.Dirs {
		if err := os.MkdirAll(filepath.Join(path, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ws, err = st.CreateWorkspace(context.Background(), store.NewWorkspace{ID: id, Kind: store.WorkspaceKindStudio, Name: "poster", Path: path, Template: studio.DefaultTemplate})
	if err != nil {
		t.Fatal(err)
	}
	// 多数用例要生成：把能力表里的模型全部启用（白名单本身的行为在 TestMediaConfig 里测）。
	e.enableModels()
	return e
}

// mediaFiles 是目录里 media/ 下的文件（新建对话时按创作类型预置的 agent/PROJECT.md 不算）。
func (e *env) mediaFiles() []store.StudioFile {
	files, err := e.m.ListFiles(context.Background(), e.ws)
	if err != nil {
		e.t.Fatal(err)
	}
	out := []store.StudioFile{}
	for _, f := range files {
		if strings.HasPrefix(f.Name, studio.DirMedia+"/") {
			out = append(out, f)
		}
	}
	return out
}

// enableModels 整份替换工作空间启用的生成模型；不给名字即启用能力表里的全部。
func (e *env) enableModels(ids ...string) {
	e.t.Helper()
	if ids == nil {
		catalog, err := e.media.Catalog(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		for _, m := range catalog {
			ids = append(ids, m.ID)
		}
	}
	models := make([]store.StudioMediaModel, 0, len(ids))
	for _, id := range ids {
		models = append(models, store.StudioMediaModel{Model: id})
	}
	if err := e.m.SetMediaConfig(context.Background(), e.ws, models); err != nil {
		e.t.Fatalf("SetMediaConfig: %v", err)
	}
}

// startedInstructions 是第 n 段引擎会话启动时拿到的开发者指令。
func (e *env) startedInstructions(n int) string {
	e.engine.mu.Lock()
	defer e.engine.mu.Unlock()
	if n >= len(e.engine.sessions) {
		e.t.Fatalf("只有 %d 段引擎会话，要第 %d 段", len(e.engine.sessions), n)
	}
	return e.engine.sessions[n].instructions
}

func (e *env) upload(name string, data []byte) *store.StudioFile {
	e.t.Helper()
	f, err := e.m.SaveFile(context.Background(), e.ws, name, bytes.NewReader(data), 0, studio.FileMeta{Origin: store.StudioFileOriginUpload})
	if err != nil {
		e.t.Fatalf("SaveFile(%s): %v", name, err)
	}
	return f
}

// countMediaJobs 从库里数生成任务行（全部来源）。
func (e *env) countMediaJobs() int {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		e.t.Fatalf("打开任务视角连接: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM media_jobs`).Scan(&n); err != nil {
		e.t.Fatalf("数任务行: %v", err)
	}
	return n
}

func (e *env) events(chatID string) []store.StudioEvent {
	e.t.Helper()
	evs, err := e.st.ListStudioEvents(context.Background(), store.StudioEventQuery{ChatID: chatID})
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *env) waitRun(runID string) *store.StudioRun {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		run, err := e.st.GetStudioRun(context.Background(), runID)
		if err != nil {
			e.t.Fatal(err)
		}
		if !store.AgentRunActive(run.Status) {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("指令 %s 未在期限内结束", runID)
	return nil
}

func TestFilesLifecycle(t *testing.T) {
	e := newDeviceEnv(t)
	ctx := context.Background()
	// 建空间即有三个子目录。
	for _, dir := range studio.Dirs {
		if info, err := os.Stat(filepath.Join(e.ws.Path, dir)); err != nil || !info.IsDir() {
			t.Fatalf("子目录 %s 未建: %v", dir, err)
		}
	}
	f := e.upload("media/ref.png", e.png)
	if f.Name != "media/ref.png" || f.Kind != store.StudioFileImage || f.Width != 640 || f.Height != 480 || f.Bytes != int64(len(e.png)) || f.Origin != store.StudioFileOriginUpload || f.Mime != "image/png" {
		t.Fatalf("上传后的附注 = %+v", f)
	}
	if _, err := os.Stat(filepath.Join(e.ws.Path, "media", "ref.png")); err != nil {
		t.Fatalf("文件未落在 media/ 之下: %v", err)
	}
	if !e.m.HasThumb(e.ws, "media/ref.png") {
		t.Fatal("PNG 上传后应有缩略图")
	}
	if th, err := e.m.OpenThumb(e.ws, "media/ref.png"); err != nil || th.ContentType != "image/jpeg" {
		t.Fatalf("OpenThumb = %v / %v", th, err)
	} else {
		th.Close()
	}
	// 落位规则：文本只进 agent/ 或 docs/，媒体只进 media/；路径必须带子目录。
	for _, bad := range []string{"docs/x.png", "agent/x.mp4", "media/notes.txt", "ref.png", "other/ref.png", "media/sub/ref.png"} {
		if _, err := e.m.SaveFile(ctx, e.ws, bad, bytes.NewReader([]byte("x")), 0, studio.FileMeta{}); err == nil {
			t.Fatalf("写 %q 应被拒", bad)
		}
	}
	// 外来文件（不经本包写入）：列目录时补一行 unknown；根目录与别的子目录不进清单。
	if err := os.WriteFile(filepath.Join(e.ws.Path, "docs", "notes.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(e.ws.Path, "stray.txt"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(e.ws.Path, "other"), 0o700)
	_ = os.WriteFile(filepath.Join(e.ws.Path, "other", "x.png"), e.png, 0o600)
	// 表里有但大小对不上（不经本包改过）：附注归零、重新量尺寸。
	small := pngBytes(t, 320, 240)
	if err := os.WriteFile(filepath.Join(e.ws.Path, "media", "ref.png"), small, 0o600); err != nil {
		t.Fatal(err)
	}
	if files, err := e.m.ListFiles(ctx, e.ws); err != nil {
		t.Fatal(err)
	} else if len(files) != 2 || files[0].Name != "media/ref.png" || files[0].Origin != store.StudioFileOriginUnknown || files[0].Bytes != int64(len(small)) || files[0].Width != 320 || files[0].Height != 240 {
		t.Fatalf("外部改写后的附注 = %+v", files)
	}
	// 隐藏文件、再下一层的目录与名字不合 ValidateFileName 的条目不进清单（与主机落点同一条规则）。
	_ = os.WriteFile(filepath.Join(e.ws.Path, "media", ".hidden"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(e.ws.Path, "media", "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(e.ws.Path, "docs", "bad\tname.txt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(e.ws.Path, "docs", " padded.txt"), []byte("x"), 0o600)
	files, err := e.m.ListFiles(ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Name != "docs/notes.txt" || files[1].Origin != store.StudioFileOriginUnknown || files[1].Kind != store.StudioFileText || files[1].Bytes != 5 {
		t.Fatalf("对账后的清单 = %+v", files)
	}
	// 子目录被外部删掉：列目录时建回来，清单照常。
	if err := os.RemoveAll(filepath.Join(e.ws.Path, "agent")); err != nil {
		t.Fatal(err)
	}
	if files, err := e.m.ListFiles(ctx, e.ws); err != nil || len(files) != 2 {
		t.Fatalf("缺子目录时的清单 = %+v / %v", files, err)
	}
	if info, err := os.Stat(filepath.Join(e.ws.Path, "agent")); err != nil || !info.IsDir() {
		t.Fatal("缺失的子目录应被建回来")
	}
	// 目录里的文件被外部删掉：行跟着清。
	_ = os.Remove(filepath.Join(e.ws.Path, "docs", "notes.txt"))
	files, _ = e.m.ListFiles(ctx, e.ws)
	if len(files) != 1 {
		t.Fatalf("外部删除后清单 = %+v", files)
	}
	if _, err := e.st.GetStudioFile(ctx, e.ws.ID, "docs/notes.txt"); err == nil {
		t.Fatal("消失的文件的附注应被删")
	}
	// 改名：目录、缩略图、附注一起换。
	if err := e.m.RenameFile(ctx, e.ws, "media/ref.png", "media/hero.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.ws.Path, "media", "hero.png")); err != nil || !e.m.HasThumb(e.ws, "media/hero.png") || e.m.HasThumb(e.ws, "media/ref.png") {
		t.Fatal("改名后目录 / 缩略图不一致")
	}
	if _, err := e.st.GetStudioFile(ctx, e.ws.ID, "media/hero.png"); err != nil {
		t.Fatalf("改名后附注: %v", err)
	}
	e.upload("media/other.png", e.png)
	if err := e.m.RenameFile(ctx, e.ws, "media/hero.png", "media/other.png"); err == nil || !strings.Contains(err.Error(), "占用") {
		t.Fatalf("改成已占用的名字应拒绝: %v", err)
	}
	// 挪目录：文本可在 agent/ 与 docs/ 之间挪，不能挪进 media/；媒体不能离开 media/。
	e.upload("docs/brief.md", []byte("# 简报\n"))
	if err := e.m.RenameFile(ctx, e.ws, "docs/brief.md", "agent/brief.md"); err != nil {
		t.Fatalf("挪进 agent/ 应放行: %v", err)
	}
	if row, err := e.st.GetStudioFile(ctx, e.ws.ID, "agent/brief.md"); err != nil || row.Origin != store.StudioFileOriginUpload {
		t.Fatalf("挪目录后的附注 = %+v / %v", row, err)
	}
	if err := e.m.RenameFile(ctx, e.ws, "agent/brief.md", "media/brief.md"); err == nil {
		t.Fatal("文本挪进 media/ 应拒绝")
	}
	if err := e.m.RenameFile(ctx, e.ws, "media/hero.png", "docs/hero.png"); err == nil {
		t.Fatal("媒体挪出 media/ 应拒绝")
	}
	// 文本读取与上限。
	e.upload("agent/PROJECT.md", []byte("# 目标\n"))
	if data, err := e.m.ReadFile(ctx, e.ws, studio.BriefFileName, studio.MaxTextBytes); err != nil || string(data) != "# 目标\n" {
		t.Fatalf("ReadFile = %q / %v", data, err)
	}
	if _, err := e.m.ReadFile(ctx, e.ws, "media/other.png", 10); err == nil {
		t.Fatal("超限读取应报错")
	}
	// 删除。
	if err := e.m.DeleteFile(ctx, e.ws, "media/other.png"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.DeleteFile(ctx, e.ws, "media/other.png"); err == nil {
		t.Fatal("重复删除应报不存在")
	}
	// 文件名与路径白名单。
	for _, bad := range []string{"", ".hidden", "a/b.png", "a\\b", " x.png", "x.png ", "bad\x00name", strings.Repeat("a", 130)} {
		if err := studio.ValidateFileName(bad); err == nil {
			t.Fatalf("%q 应被拒", bad)
		}
	}
	for _, good := range []string{"hero.png", "海报 v2.png", "a-b_c (1).mp4", "PROJECT.md"} {
		if err := studio.ValidateFileName(good); err != nil {
			t.Fatalf("%q 应放行: %v", good, err)
		}
	}
	for _, bad := range []string{"", "hero.png", "/media/hero.png", "media/", "media//hero.png", "media/.hidden", "media/a/b.png", "assets/hero.png", "Media/hero.png"} {
		if _, _, err := studio.ValidatePath(bad); err == nil {
			t.Fatalf("路径 %q 应被拒", bad)
		}
	}
	if dir, name, err := studio.ValidatePath("docs/分镜 v2.md"); err != nil || dir != "docs" || name != "分镜 v2.md" {
		t.Fatalf("ValidatePath = %q / %q / %v", dir, name, err)
	}
	// 上传超限：半成品不留。
	if _, err := e.m.SaveFile(ctx, e.ws, "media/big.bin", bytes.NewReader(make([]byte, 100)), 50, studio.FileMeta{}); err == nil {
		t.Fatal("超限上传应报错")
	}
	entries, _ := os.ReadDir(filepath.Join(e.ws.Path, "media"))
	for _, en := range entries {
		if strings.Contains(en.Name(), ".part") || en.Name() == "big.bin" {
			t.Fatalf("超限上传留下了 %s", en.Name())
		}
	}
	// 删工作空间：目录整个收走。
	if err := e.m.RemoveDir(e.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.ws.Path); !os.IsNotExist(err) {
		t.Fatal("RemoveDir 后目录仍在")
	}
	if err := e.m.RemoveDir(&store.Workspace{ID: "x", Path: t.TempDir()}); err == nil {
		t.Fatal("根目录之外的路径应拒绝删除")
	}
}

func TestChatToolsRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.upload("media/ref.png", e.png)
	e.upload("agent/PROJECT.md", []byte("# 海报\n目标：春季促销\n"))

	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1, Effort: "high"})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	row, _ := e.st.GetStudioChat(ctx, chat.ID)
	for _, want := range []string{"poster", "`media/ref.png`", "640×480", "春季促销", "`agent/PROJECT.md`", "`docs/`"} {
		if !strings.Contains(row.Instructions, want) {
			t.Fatalf("开发者指令缺少 %q:\n%s", want, row.Instructions)
		}
	}
	if _, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 0}); err == nil {
		t.Fatal("没选密钥应拒绝")
	}

	var toolsSeen hostagent.ToolEndpoint
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		toolsSeen = tools
		if tools.Server != studio.ToolServer {
			t.Errorf("MCP 服务器名 = %q", tools.Server)
		}
		// 列目录：路径带子目录；可只列一个子目录。
		text, isErr, _ := callTool(t, tools, "list_files", nil)
		if isErr || !strings.Contains(text, "media/ref.png") || !strings.Contains(text, "agent/PROJECT.md") {
			t.Errorf("list_files = %q / %v", text, isErr)
		}
		if text, isErr, _ := callTool(t, tools, "list_files", map[string]any{"dir": "agent"}); isErr || strings.Contains(text, "media/ref.png") || !strings.Contains(text, "agent/PROJECT.md") {
			t.Errorf("list_files dir=agent = %q / %v", text, isErr)
		}
		if _, isErr, _ := callTool(t, tools, "list_files", map[string]any{"dir": "assets"}); !isErr {
			t.Error("未知子目录应报错")
		}
		// 看图：第二块内容是图像。
		_, isErr, content := callTool(t, tools, "view_image", map[string]any{"path": "media/ref.png"})
		if isErr || len(content) != 2 {
			t.Errorf("view_image 内容块 = %d / err=%v", len(content), isErr)
		} else if img, _ := content[1].(map[string]any); img["type"] != "image" || img["mimeType"] != "image/jpeg" || img["data"] == "" {
			t.Errorf("view_image 图像块 = %+v", img)
		}
		if _, isErr, _ := callTool(t, tools, "view_image", map[string]any{"path": "agent/PROJECT.md"}); !isErr {
			t.Error("view_image 对文本文件应报错")
		}
		// 设备解得开的大图（> 8 MiB）照样缩放交出；解不开的格式超过 8 MiB 才拒绝。
		big := noisyPNG(t, 2000, 1400)
		if len(big) <= studio.MaxImageBytes {
			t.Fatalf("测试图像只有 %d 字节，不够大", len(big))
		}
		e.upload("media/big.png", big)
		if _, isErr, content := callTool(t, tools, "view_image", map[string]any{"path": "media/big.png"}); isErr || len(content) != 2 {
			t.Errorf("view_image 大 PNG = %d / err=%v", len(content), isErr)
		}
		e.upload("media/big.webp", bytes.Repeat([]byte("RIFF????WEBP"), studio.MaxImageBytes/12+1))
		if text, isErr, _ := callTool(t, tools, "view_image", map[string]any{"path": "media/big.webp"}); !isErr || !strings.Contains(text, "8 MiB") {
			t.Errorf("view_image 解不开的大文件 = %q / err=%v", text, isErr)
		}
		// 读 / 写文本：管理员上传的不能覆盖，追加可以；新文件可以；只能写进 agent/ 与 docs/。
		text, isErr, _ = callTool(t, tools, "read_text", map[string]any{"path": "agent/PROJECT.md"})
		if isErr || !strings.Contains(text, "春季促销") {
			t.Errorf("read_text = %q / %v", text, isErr)
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "agent/PROJECT.md", "content": "x"}); !isErr {
			t.Error("覆盖管理员上传的文件应被拒")
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "agent/PROJECT.md", "content": "## 进展\n已出图", "append": true}); isErr {
			t.Error("追加应放行")
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "docs/storyboard.md", "content": "1. 开场"}); isErr {
			t.Error("写新文件应放行")
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "agent/notes.md", "content": "备忘"}); isErr {
			t.Error("写 agent/ 应放行")
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "media/x.png", "content": "x"}); !isErr {
			t.Error("write_text 写非文本扩展名应被拒")
		}
		if text, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "media/notes.txt", "content": "x"}); !isErr || !strings.Contains(text, "agent/") {
			t.Errorf("write_text 写进 media/ 应被拒并指明去处: %q / %v", text, isErr)
		}
		if _, isErr, _ := callTool(t, tools, "write_text", map[string]any{"path": "notes.txt", "content": "x"}); !isErr {
			t.Error("不带子目录的路径应被拒")
		}
		// 生成图像：没给模型取第一个可用的（能力表顺序，gpt-image-2）；带参考图；结果落进目录
		// 并回图像内容。
		sink.Activity("…")
		text, isErr, content = callTool(t, tools, "generate_image", map[string]any{"prompt": "春季促销海报", "name": "hero v1", "reference_images": []string{"media/ref.png"}, "params": map[string]any{"aspect_ratio": "16:9"}})
		if isErr || !strings.Contains(text, "saved media/hero-v1.png") || len(content) != 2 {
			t.Errorf("generate_image = %q / %v / %d 块", text, isErr, len(content))
		}
		// 同名再生成：自动加序号；名字里带的目录 / 扩展名被剥掉，结果恒在 media/。
		text, isErr, _ = callTool(t, tools, "generate_image", map[string]any{"prompt": "第二版", "name": "docs/hero v1.jpg", "model": "grok-imagine-image-2.0"})
		if isErr || !strings.Contains(text, "saved media/hero-v1-2.png") {
			t.Errorf("第二次 generate_image = %q / %v", text, isErr)
		}
		// 不在能力表里的模型、表里没有的参数、取值越界、旧形状（provider / 顶层参数）都被拒，
		// 错误里列出可用的模型名。
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "model": "nope"}); !isErr || !strings.Contains(text, "gpt-image-2") {
			t.Errorf("不在清单里的模型应被拒并列出可用模型: %q / %v", text, isErr)
		}
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "params": map[string]any{"duration": 6}}); !isErr || !strings.Contains(text, "duration") {
			t.Errorf("图像模型不认 duration: %q / %v", text, isErr)
		}
		if _, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "params": map[string]any{"duration": 99}}); !isErr {
			t.Error("时长越界应被拒")
		}
		if text, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "model": "gpt-image-2"}); !isErr || !strings.Contains(text, "grok-imagine-video-1.5") {
			t.Errorf("图像模型不能拿来生成视频: %q / %v", text, isErr)
		}
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "model": "grok-imagine-video-1.5"}); !isErr || !strings.Contains(text, "gpt-image-2") {
			t.Errorf("视频模型不能拿来生成图像: %q / %v", text, isErr)
		}
		if _, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "first_frame": "media/ref.png"}); !isErr {
			t.Error("图像模型不接受首帧")
		}
		if _, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "first_frame": "agent/PROJECT.md"}); !isErr {
			t.Error("首帧必须是目录里的图像")
		}
		if _, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "first_frame": "ref.png"}); !isErr {
			t.Error("首帧路径必须带子目录")
		}
		// 文件输入按角色只从 media/ 读：命令放进 agent/ 的图像不收；源视频按字节认 MP4。
		if err := os.WriteFile(filepath.Join(e.ws.Path, "agent", "frame.png"), e.png, 0o600); err != nil {
			t.Errorf("写 agent/frame.png: %v", err)
		}
		if text, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "first_frame": "agent/frame.png"}); !isErr || !strings.Contains(text, "media/") {
			t.Errorf("首帧不在 media/ 应被拒: %q / %v", text, isErr)
		}
		e.upload("media/fake.mp4", []byte("not-really-mp4-bytes"))
		if text, isErr, _ := callTool(t, tools, "generate_video", map[string]any{"prompt": "x", "operation": "edit", "source_video": "media/fake.mp4"}); !isErr || !strings.Contains(text, "ftyp") {
			t.Errorf("源视频不是 MP4 应被拒: %q / %v", text, isErr)
		}
		if err := e.m.DeleteFile(ctx, e.ws, "media/fake.mp4"); err != nil {
			t.Errorf("删 media/fake.mp4: %v", err)
		}
		_ = os.Remove(filepath.Join(e.ws.Path, "agent", "frame.png"))
		// 生成视频（首帧取目录里的图；没给模型取第一个可用的视频模型）。
		text, isErr, _ = callTool(t, tools, "generate_video", map[string]any{"prompt": "镜头推进", "first_frame": "media/hero-v1.png", "params": map[string]any{"duration": 6}})
		if isErr || !strings.Contains(text, "saved media/video-") || !strings.Contains(text, ".mp4") {
			t.Errorf("generate_video = %q / %v", text, isErr)
		}
		// 删 / 改名：上传的素材不能删，生成的可以；媒体不能挪出 media/。
		if _, isErr, _ := callTool(t, tools, "delete_file", map[string]any{"path": "media/ref.png"}); !isErr {
			t.Error("删管理员上传的素材应被拒")
		}
		if _, isErr, _ := callTool(t, tools, "rename_file", map[string]any{"path": "media/hero-v1-2.png", "new_path": "media/hero-v2.png"}); isErr {
			t.Error("改名应放行")
		}
		if _, isErr, _ := callTool(t, tools, "rename_file", map[string]any{"path": "media/hero-v2.png", "new_path": "docs/hero-v2.png"}); !isErr {
			t.Error("媒体挪出 media/ 应被拒")
		}
		if _, isErr, _ := callTool(t, tools, "rename_file", map[string]any{"path": "docs/storyboard.md", "new_path": "agent/storyboard-draft.md"}); isErr {
			t.Error("文本在 docs/ 与 agent/ 之间挪动应放行")
		}
		if _, isErr, _ := callTool(t, tools, "delete_file", map[string]any{"path": "media/hero-v2.png"}); isErr {
			t.Error("删生成的文件应放行")
		}
		if _, isErr, _ := callTool(t, tools, "nope", nil); !isErr {
			t.Error("未知工具应报错")
		}
		sink.Message("已完成海报初稿")
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	run, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "做一张春季促销海报"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if got := e.waitRun(run.ID); got.Status != store.AgentRunSucceeded {
		t.Fatalf("指令终态 = %+v", got)
	}
	// 目录：两张生成图（一张已删）与一段视频；附注带提示词与来源。
	files, _ := e.m.ListFiles(ctx, e.ws)
	names := map[string]store.StudioFile{}
	for _, f := range files {
		names[f.Name] = f
	}
	hero, ok := names["media/hero-v1.png"]
	if !ok || hero.Origin != store.StudioFileOriginGenerated || hero.Prompt != "春季促销海报" || hero.Provider != "codex" || hero.Model != "gpt-image-2" || hero.Width != 1024 || !strings.Contains(hero.Params, "16:9") || hero.ChatID != chat.ID || hero.RunID != run.ID {
		t.Fatalf("生成图的附注 = %+v（ok=%v）", hero, ok)
	}
	if !e.m.HasThumb(e.ws, "media/hero-v1.png") {
		t.Fatal("生成图应有缩略图")
	}
	if _, ok := names["media/hero-v2.png"]; ok {
		t.Fatal("已删的生成图仍在清单里")
	}
	var video *store.StudioFile
	for name, f := range names {
		if strings.HasSuffix(name, ".mp4") {
			v := f
			video = &v
		}
	}
	if video == nil || video.Kind != store.StudioFileVideo || video.Provider != "grok" || video.Model != "grok-imagine-video-1.5" || !strings.Contains(video.Params, `"duration":6`) || video.Prompt != "镜头推进" {
		t.Fatalf("视频附注 = %+v", video)
	}
	if sb, ok := names["agent/storyboard-draft.md"]; !ok || sb.Origin != store.StudioFileOriginAgent {
		t.Fatalf("智能体写的文件附注 = %+v（ok=%v）", sb, ok)
	}
	if _, ok := names["docs/storyboard.md"]; ok {
		t.Fatal("挪走的文件仍在原处")
	}
	if data, _ := e.m.ReadFile(ctx, e.ws, "agent/PROJECT.md", studio.MaxTextBytes); !strings.Contains(string(data), "春季促销") || !strings.Contains(string(data), "已出图") {
		t.Fatalf("追加后的 PROJECT.md = %q", data)
	}
	// 生成请求以对话钉死的密钥与后端账号名义提交，参考图 / 首帧已读成 data URI。
	calls, jobs := e.gen.snapshot()
	if len(calls) != 3 || calls[0].KeyID != 1 || calls[0].KeyDisplay != "sk_fake…0000" || calls[0].AccountID != 7 || calls[0].Model.Backend != "codex" ||
		len(calls[0].Inputs.ReferenceImages) != 1 || !strings.HasPrefix(calls[0].Inputs.ReferenceImages[0], "data:image/png;base64,") ||
		calls[0].Params.String("aspect_ratio") != "16:9" || calls[1].AccountID != 8 || calls[1].Model.ID != "grok-imagine-image-2.0" ||
		calls[2].Model.Kind != store.ModelKindVideo || !strings.HasPrefix(calls[2].Inputs.FirstFrame, "data:image/png;base64,") {
		t.Fatalf("生成请求 = %s", describeCalls(calls))
	}
	if d, ok := calls[2].Params.Int("duration"); !ok || d != 6 {
		t.Fatalf("视频请求的时长 = %v / %v", d, ok)
	}
	// 生成期间：任务以 origin=studio 落库，owner 记着工作空间 / 对话 / 指令与目标文件名，
	// 不出现在页面列表里。
	if listed := e.gen.listedOnPage(); len(jobs) != 3 || listed != 0 {
		t.Fatalf("生成期间读到 %d 条任务行、页面列表累计 %d 条，期望 3 / 0", len(jobs), listed)
	}
	for i, j := range jobs {
		var owner struct {
			WorkspaceID string `json:"workspace_id"`
			ChatID      string `json:"chat_id"`
			RunID       string `json:"run_id"`
			Name        string `json:"name"`
		}
		if err := json.Unmarshal([]byte(j.Owner), &owner); err != nil {
			t.Fatalf("任务 %d 的 owner = %q: %v", i, j.Owner, err)
		}
		if j.Origin != store.MediaOriginStudio || j.Status != store.MediaStatusRunning || j.KeyID != 1 || j.BatchID != "" ||
			owner.WorkspaceID != e.ws.ID || owner.ChatID != chat.ID || owner.RunID != run.ID || owner.Name == "" {
			t.Fatalf("生成期间的任务行 %d = %+v（owner %+v）", i, j, owner)
		}
		if i < 2 && owner.Name != "hero-v1" {
			t.Fatalf("任务 %d 的目标文件名 = %q", i, owner.Name)
		}
	}
	// 搬运完成：任务行与 <data_dir>/preview/ 下的结果文件都由内核删掉，结果只留工作空间目录一份。
	for _, j := range jobs {
		if _, err := e.st.GetMediaJob(ctx, j.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("任务 %s 应已删除：%v", j.ID, err)
		}
	}
	if n := e.countMediaJobs(); n != 0 {
		t.Fatalf("生成任务应已全部删除，剩 %d 条", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName)); len(entries) != 0 {
		t.Fatalf("设备上的结果目录应已清空，剩 %d 个文件", len(entries))
	}
	if listed, _ := e.st.ListPageMediaJobs(ctx); len(listed) != 0 {
		t.Fatalf("页面列表 = %+v", listed)
	}
	// 时间线：指令、工具调用、文件变更、生成记录、回复。
	kinds := map[string]int{}
	var generated []store.StudioEvent
	for _, ev := range e.events(chat.ID) {
		kinds[ev.Kind]++
		if ev.Kind == store.StudioEventGenerate {
			generated = append(generated, ev)
		}
	}
	if kinds[store.StudioEventUser] != 1 || kinds[store.StudioEventAssistant] != 1 || kinds[store.StudioEventSession] != 1 ||
		kinds[store.StudioEventTool] < 2 || kinds[store.StudioEventFile] < 5 || kinds[store.StudioEventGenerate] != 3 {
		t.Fatalf("事件种类计数 = %v", kinds)
	}
	if generated[0].Title != "media/hero-v1.png" || generated[0].Body != "春季促销海报" || !strings.Contains(generated[0].Meta, `"status":"succeeded"`) ||
		!strings.Contains(generated[0].Meta, `"provider":"codex"`) || !strings.Contains(generated[0].Meta, `"model":"gpt-image-2"`) || generated[0].RunID != run.ID {
		t.Fatalf("生成事件 = %+v", generated[0])
	}
	// 令牌守卫：令牌错误 401、没令牌 401；会话关闭后令牌作废。
	for _, token := range []string{"", "nope"} {
		req, _ := http.NewRequest(http.MethodPost, toolsSeen.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("令牌 %q 状态 = %v / %v", token, resp, err)
		}
		resp.Body.Close()
	}
	if err := e.m.Stop(ctx, e.ws.ID, chat.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return e.m.Snapshot(chat.ID).EngineStartedAt == nil })
	req, _ := http.NewRequest(http.MethodPost, toolsSeen.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+toolsSeen.Token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("会话结束后令牌仍可用: %v / %v", resp, err)
	}
	// 陪等：revision 变化即回帧。
	st := e.m.Snapshot(chat.ID)
	got, err := e.m.Wait(ctx, e.ws.ID, chat.ID, st.Revision-1)
	if err != nil || got.Revision != st.Revision {
		t.Fatalf("Wait = %+v / %v", got, err)
	}
	// 删对话：会话已结束，行与事件一起删。
	if err := e.m.DeleteChat(ctx, e.ws.ID, chat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.GetChat(ctx, e.ws.ID, chat.ID); err == nil {
		t.Fatal("删后仍能取到对话")
	}
}

func TestGenerationFailureAndCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.gen.fail = true
	e.engine.script = func(ctx context.Context, in hostagent.Input, sink hostagent.Sink, tools hostagent.ToolEndpoint, interrupt <-chan struct{}) hostagent.Outcome {
		if in.Text == "block" {
			select {
			case <-interrupt:
			case <-ctx.Done():
			}
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}
		}
		text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x"})
		if !isErr || !strings.Contains(text, "platform said no") {
			t.Errorf("失败的生成 = %q / %v", text, isErr)
		}
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	run, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "画"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitRun(run.ID)
	var failed *store.StudioEvent
	for _, ev := range e.events(chat.ID) {
		if ev.Kind == store.StudioEventGenerate {
			f := ev
			failed = &f
		}
	}
	if failed == nil || failed.Title != "" || !strings.Contains(failed.Meta, `"status":"failed"`) || !strings.Contains(failed.Meta, "platform said no") {
		t.Fatalf("失败的生成事件 = %+v", failed)
	}
	if n := e.countMediaJobs(); n != 0 {
		t.Fatalf("失败的生成任务应已删除，剩 %d 条", n)
	}
	if files := e.mediaFiles(); len(files) != 0 {
		t.Fatalf("失败的生成不该留文件: %+v", files)
	}
	// 一条阻塞、一条排队：取消排队中的，再中止执行中的。
	blocking, _ := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "block"})
	queued, _ := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "后面"})
	waitUntil(t, func() bool { return e.m.Snapshot(chat.ID).Status == hostagent.StatusRunning })
	if err := e.m.Cancel(ctx, e.ws.ID, chat.ID, queued.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.st.GetStudioRun(ctx, queued.ID); got.Status != store.AgentRunCancelled {
		t.Fatalf("排队中的取消后 = %+v", got)
	}
	if err := e.m.Cancel(ctx, e.ws.ID, chat.ID, blocking.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.waitRun(blocking.ID); got.Status != store.AgentRunCancelled {
		t.Fatalf("执行中的中止后 = %+v", got)
	}
	if err := e.m.Cancel(ctx, e.ws.ID, chat.ID, blocking.ID); err == nil {
		t.Fatal("已结束的指令再取消应报错")
	}
	// 删除工作空间：会话收走。
	e.m.WorkspaceRemoved(ctx, e.ws.ID)
	if st := e.m.Snapshot(chat.ID); st.Status != hostagent.StatusIdle || st.EngineStartedAt != nil {
		t.Fatalf("WorkspaceRemoved 后读数 = %+v", st)
	}
}

// runScript 让假引擎按 script 执行一条指令并等它结束。
func (e *env) runScript(chatID string, script func(tools hostagent.ToolEndpoint)) *store.StudioRun {
	e.t.Helper()
	e.engine.mu.Lock()
	e.engine.script = func(_ context.Context, _ hostagent.Input, _ hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
		script(tools)
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
	}
	e.engine.mu.Unlock()
	run, err := e.m.Submit(context.Background(), e.ws, chatID, hostagent.Input{Text: "go"})
	if err != nil {
		e.t.Fatalf("Submit: %v", err)
	}
	return e.waitRun(run.ID)
}

func (e *env) generateEvents(chatID string) []store.StudioEvent {
	e.t.Helper()
	var out []store.StudioEvent
	for _, ev := range e.events(chatID) {
		if ev.Kind == store.StudioEventGenerate {
			out = append(out, ev)
		}
	}
	return out
}

// 一次出多个候选：同批任务各自落成一个文件（第二个带序号），各留一条 generate 事件，
// 工具结果里每个候选一段文字加一张图；任务全部由内核删除。
func TestGenerateCandidates(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.runScript(chat.ID, func(tools hostagent.ToolEndpoint) {
		text, isErr, content := callTool(t, tools, "generate_image", map[string]any{"prompt": "两个候选", "name": "cand", "count": 2})
		if isErr || !strings.Contains(text, "saved media/cand.png") || len(content) != 4 {
			t.Errorf("count=2 的 generate_image = %q / %v / %d 块", text, isErr, len(content))
			return
		}
		if second, _ := content[2].(map[string]any); !strings.Contains(fmt.Sprint(second["text"]), "saved media/cand-2.png") {
			t.Errorf("第二个候选 = %+v", second)
		}
		if _, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "count": 4}); !isErr {
			t.Error("候选数超过上限应被拒")
		}
	})
	files := e.mediaFiles()
	var got []string
	for _, f := range files {
		got = append(got, f.Name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "media/cand-2.png,media/cand.png" {
		t.Fatalf("目录 = %v，期望 media/cand.png 与 media/cand-2.png", got)
	}
	for _, f := range files {
		if f.Origin != store.StudioFileOriginGenerated || f.Prompt != "两个候选" || f.Model != "gpt-image-2" || f.Width != 1024 || !e.m.HasThumb(e.ws, f.Name) {
			t.Fatalf("候选的附注 = %+v", f)
		}
	}
	events := e.generateEvents(chat.ID)
	if len(events) != 2 || events[0].Title != "media/cand.png" || events[1].Title != "media/cand-2.png" {
		t.Fatalf("generate 事件 = %+v", events)
	}
	// 同批两条任务共用 batch_id，生成期间同样不进页面列表；搬完即删。
	calls, jobs := e.gen.snapshot()
	if len(calls) != 2 || len(jobs) != 2 || jobs[0].BatchID == "" || jobs[0].BatchID != jobs[1].BatchID || jobs[0].ID == jobs[1].ID ||
		jobs[0].Origin != store.MediaOriginStudio || e.gen.listedOnPage() != 0 {
		t.Fatalf("同批任务 = %s / %+v", describeCalls(calls), jobs)
	}
	if n := e.countMediaJobs(); n != 0 {
		t.Fatalf("候选任务应已全部删除，剩 %d 条", n)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName)); len(entries) != 0 {
		t.Fatalf("设备上的结果目录应已清空，剩 %d 个文件", len(entries))
	}
}

// 并发上限：对话钉死的那把密钥未到终态的任务（全部来源合计）加上本次候选数超过
// mediagen.RunningPerKey 时整次拒绝，不建任务、不打平台。
func TestGenerateRespectsRunningLimit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var seeded []string
	for i, origin := range []string{store.MediaOriginPage, store.MediaOriginCLI, store.MediaOriginStudio} {
		id := store.NewULID(now.Add(time.Duration(i) * time.Millisecond))
		if err := e.st.CreateMediaJob(ctx, store.MediaJob{ID: id, Origin: origin, Backend: "grok", Provider: "grok", KeyID: 1, Kind: store.ModelKindImage,
			Model: "grok-imagine-image-2.0", Operation: store.MediaOpGenerate, Prompt: "p", Status: store.MediaStatusRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, id)
	}
	e.runScript(chat.ID, func(tools hostagent.ToolEndpoint) {
		text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x"})
		if !isErr || !strings.Contains(text, "already has 3 generations running") {
			t.Errorf("名额占满时 = %q / %v", text, isErr)
		}
		// 腾出一个名额：count=2 仍整批拒绝，count=1 受理。
		if err := e.st.DeleteMediaJob(ctx, seeded[0]); err != nil {
			t.Errorf("DeleteMediaJob: %v", err)
		}
		text, isErr, _ = callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "count": 2})
		if !isErr || !strings.Contains(text, "limit 3") {
			t.Errorf("名额只剩 1 时的 count=2 = %q / %v", text, isErr)
		}
		if calls, _ := e.gen.snapshot(); len(calls) != 0 || e.countMediaJobs() != 2 {
			t.Errorf("被拒的生成不该建任务 / 打平台：%d 次调用、%d 条任务", len(calls), e.countMediaJobs())
		}
		text, isErr, _ = callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "name": "ok"})
		if isErr || !strings.Contains(text, "saved media/ok.png") {
			t.Errorf("名额够时 = %q / %v", text, isErr)
		}
	})
	if n := e.countMediaJobs(); n != 2 {
		t.Fatalf("只应剩预置的两条任务，得到 %d 条", n)
	}
}

// 工具调用被引擎放弃（连接断开）时生成不白做：后台接着等，落成后搬进目录、留 generate 事件、
// 任务删除。
func TestGenerateAdoptedAfterAbandonedCall(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.gen.block = make(chan struct{})
	e.gen.entered = make(chan string, 4)
	var jobID string
	run := e.runScript(chat.ID, func(tools hostagent.ToolEndpoint) {
		callCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "generate_image", "arguments": map[string]any{"prompt": "慢慢画", "name": "later"}}})
			req, _ := http.NewRequestWithContext(callCtx, http.MethodPost, tools.URL, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tools.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
			done <- err
		}()
		select {
		case jobID = <-e.gen.entered:
		case <-time.After(10 * time.Second):
			t.Error("平台请求没有发起")
			return
		}
		cancel() // 引擎放弃这次工具调用。
		if err := <-done; err == nil {
			t.Error("放弃的工具调用不该拿到应答")
		}
	})
	if jobID == "" {
		t.Fatal("没有拿到任务 id")
	}
	// 平台还没回：任务还在跑，目录里还没有结果。
	if job, err := e.st.GetMediaJob(ctx, jobID); err != nil || job.Status != store.MediaStatusRunning {
		t.Fatalf("放弃之后的任务 = %+v / %v，期望仍在 running", job, err)
	}
	if files := e.mediaFiles(); len(files) != 0 {
		t.Fatalf("平台未返回时目录 = %+v", files)
	}
	close(e.gen.block)
	waitUntil(t, func() bool { return len(e.generateEvents(chat.ID)) == 1 })
	f, err := e.st.GetStudioFile(ctx, e.ws.ID, "media/later.png")
	if err != nil || f.Origin != store.StudioFileOriginGenerated || f.Prompt != "慢慢画" || f.ChatID != chat.ID || f.RunID != run.ID {
		t.Fatalf("后台搬进目录的文件 = %+v / %v", f, err)
	}
	if ev := e.generateEvents(chat.ID)[0]; ev.Title != "media/later.png" || ev.RunID != run.ID || !strings.Contains(ev.Meta, `"status":"succeeded"`) {
		t.Fatalf("后台留下的 generate 事件 = %+v", ev)
	}
	waitUntil(t, func() bool { return e.countMediaJobs() == 0 })
	if entries, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName)); len(entries) != 0 {
		t.Fatalf("设备上的结果目录应已清空，剩 %d 个文件", len(entries))
	}
}

// 重启收尾：上次进程在「任务完成」与「搬进目录」之间退出时，库里留着 origin=studio 的终态
// 任务。新进程给内核注册收尾器（Manager.FinishJob）后——成功的搬进工作空间目录并留 generate
// 事件，失败的留失败事件，工作空间已不存在 / 归属读不出来的直接删；页面任务不受影响。
func TestFinishJobAfterRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	owner := func(wsID, name string) string {
		b, _ := json.Marshal(map[string]string{"workspace_id": wsID, "chat_id": chat.ID, "name": name})
		return string(b)
	}
	now := time.Now().Add(-time.Minute)
	seq := 0
	seed := func(j store.MediaJob) store.MediaJob {
		seq++
		j.ID = store.NewULID(now.Add(time.Duration(seq) * time.Millisecond))
		j.KeyID, j.KeyDisplay, j.Operation = 1, "sk_fake…0000", store.MediaOpGenerate
		j.CreatedAt = now.Add(time.Duration(seq) * time.Millisecond)
		j.UpdatedAt = j.CreatedAt
		if j.MediaFile != "" {
			j.MediaFile = j.ID + j.MediaFile
		}
		if err := e.st.CreateMediaJob(ctx, j); err != nil {
			t.Fatalf("CreateMediaJob: %v", err)
		}
		return j
	}
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(e.gen.png)
	image := seed(store.MediaJob{Origin: store.MediaOriginStudio, Owner: owner(e.ws.ID, "restart-hero"), Backend: "codex", Provider: "codex", AccountID: 7,
		Kind: store.ModelKindImage, Model: "gpt-image-2", Prompt: "重启前画好的图", Status: store.MediaStatusSucceeded, MediaURL: dataURI, MediaType: "image",
		Params: map[string]any{"aspect_ratio": "16:9"}})
	// 结果已落在设备结果目录里的那一种（media_file）。
	video := seed(store.MediaJob{Origin: store.MediaOriginStudio, Owner: owner(e.ws.ID, "restart-clip"), Backend: "grok", Provider: "grok", AccountID: 8,
		Kind: store.ModelKindVideo, Model: "grok-imagine-video-1.5", Prompt: "重启前出好的视频", Status: store.MediaStatusSucceeded, MediaType: "video", MediaFile: ".mp4"})
	if err := os.MkdirAll(filepath.Join(e.dir, mediagen.MediaDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, mediagen.MediaDirName, video.MediaFile), []byte("not-really-mp4"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := seed(store.MediaJob{Origin: store.MediaOriginStudio, Owner: owner(e.ws.ID, "restart-bad"), Backend: "grok", Provider: "grok",
		Kind: store.ModelKindImage, Model: "grok-imagine-image-2.0", Prompt: "重启前失败的", Status: store.MediaStatusFailed, Error: "platform said no"})
	gone := seed(store.MediaJob{Origin: store.MediaOriginStudio, Owner: owner("01GONE00000000000000000000", "orphan"), Backend: "codex", Provider: "codex",
		Kind: store.ModelKindImage, Model: "gpt-image-2", Prompt: "空间已删", Status: store.MediaStatusSucceeded, MediaURL: dataURI, MediaType: "image"})
	ownerless := seed(store.MediaJob{Origin: store.MediaOriginStudio, Owner: "", Backend: "codex", Provider: "codex",
		Kind: store.ModelKindImage, Model: "gpt-image-2", Prompt: "归属读不出来", Status: store.MediaStatusFailed, Error: "x"})
	page := seed(store.MediaJob{Origin: store.MediaOriginPage, Backend: "codex", Provider: "codex",
		Kind: store.ModelKindImage, Model: "gpt-image-2", Prompt: "页面的失败任务", Status: store.MediaStatusFailed, Error: "x"})

	// 「重启」：同一个库上再建一份内核与 Manager，装配顺序同 main.go。
	svc := newMedia(e.st, e.dir, e.gen)
	mgr := studio.New(studio.Options{Store: e.st, Engines: []nodeengine.Engine{e.engine}, Media: svc, DataDir: e.dir, Hosts: e.hosts, DevHosts: e.dev, IdleTimeout: time.Hour})
	t.Cleanup(func() { mgr.Shutdown(context.Background()) })
	svc.SetFinisher(store.MediaOriginStudio, mgr.FinishJob)

	// 事件在任务删除之后才落时间线：两样都等到。
	waitUntil(t, func() bool { return e.countMediaJobs() == 1 && len(e.generateEvents(chat.ID)) == 3 })
	for _, j := range []store.MediaJob{image, video, failed, gone, ownerless} {
		if _, err := e.st.GetMediaJob(ctx, j.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("任务 %q 应已收尾删除：%v", j.Prompt, err)
		}
	}
	if got, err := e.st.GetMediaJob(ctx, page.ID); err != nil || got.Origin != store.MediaOriginPage {
		t.Fatalf("收尾器不该动页面任务：%+v / %v", got, err)
	}
	files := e.mediaFiles()
	names := map[string]store.StudioFile{}
	for _, f := range files {
		names[f.Name] = f
	}
	hero, ok := names["media/restart-hero.png"]
	if len(files) != 2 || !ok || hero.Origin != store.StudioFileOriginGenerated || hero.Prompt != "重启前画好的图" || hero.Provider != "codex" ||
		hero.Model != "gpt-image-2" || !strings.Contains(hero.Params, "16:9") || hero.ChatID != chat.ID || hero.Width != 1024 || !e.m.HasThumb(e.ws, "media/restart-hero.png") {
		t.Fatalf("收尾搬进目录的图 = %+v（ok=%v，共 %d 个文件）", hero, ok, len(files))
	}
	clip, ok := names["media/restart-clip.mp4"]
	if !ok || clip.Kind != store.StudioFileVideo || clip.Provider != "grok" || clip.Prompt != "重启前出好的视频" {
		t.Fatalf("收尾搬进目录的视频 = %+v（ok=%v）", clip, ok)
	}
	if data, err := e.m.ReadFile(ctx, e.ws, "media/restart-clip.mp4", studio.MaxUploadBytes); err != nil || string(data) != "not-really-mp4" {
		t.Fatalf("视频内容 = %q / %v", data, err)
	}
	// 时间线：两条成功、一条失败；空间已删 / 没有归属的不留事件。
	events := e.generateEvents(chat.ID)
	titles := map[string]string{}
	for _, ev := range events {
		titles[ev.Body] = ev.Title + "|" + ev.Meta
	}
	if len(events) != 3 || !strings.HasPrefix(titles["重启前画好的图"], "media/restart-hero.png|") || !strings.Contains(titles["重启前画好的图"], `"status":"succeeded"`) ||
		!strings.HasPrefix(titles["重启前出好的视频"], "media/restart-clip.mp4|") ||
		!strings.HasPrefix(titles["重启前失败的"], "|") || !strings.Contains(titles["重启前失败的"], `"status":"failed"`) || !strings.Contains(titles["重启前失败的"], "platform said no") {
		t.Fatalf("收尾留下的 generate 事件 = %+v", events)
	}
	// 设备上的结果文件随任务删除（启动时的补齐与收尾并发，等清扫落定）。
	waitUntil(t, func() bool {
		entries, _ := os.ReadDir(filepath.Join(e.dir, mediagen.MediaDirName))
		return len(entries) == 0
	})
}

// 开发者指令里的模型说明出自能力表与这把密钥的可用性：可用模型的名字与参数名都在，不可用的
// 模型不出现；一个可用模型都没有时换成说明。
func TestInstructionsListUsableModels(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	noop := func(hostagent.ToolEndpoint) {}
	started := 0
	for _, keyID := range []int64{1, keyGrokOnly} {
		chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: keyID})
		if err != nil {
			t.Fatalf("CreateChat(key %d): %v", keyID, err)
		}
		// 模型说明不存进对话行：引擎会话启动时按当前配置渲染。
		if row, _ := e.st.GetStudioChat(ctx, chat.ID); strings.Contains(row.Instructions, "生成模型（") || strings.Contains(row.Instructions, "`gpt-image-2`") {
			t.Fatalf("对话行里不该存模型说明:\n%s", row.Instructions)
		}
		e.runScript(chat.ID, noop)
		instructions := e.startedInstructions(started)
		started++
		models, err := e.media.Available(ctx, keyID)
		if err != nil {
			t.Fatal(err)
		}
		usable := 0
		for _, a := range models {
			quoted := "`" + a.ID + "`（"
			if !a.Available {
				if strings.Contains(instructions, quoted) {
					t.Fatalf("密钥 %d 的开发者指令不该出现不可用的模型 %s:\n%s", keyID, a.ID, instructions)
				}
				continue
			}
			usable++
			if !strings.Contains(instructions, quoted) {
				t.Fatalf("密钥 %d 的开发者指令缺少模型 %s:\n%s", keyID, a.ID, instructions)
			}
			for _, op := range a.Operations {
				for _, p := range op.Params {
					if !strings.Contains(instructions, p.Name+"（"+p.Label) {
						t.Fatalf("密钥 %d 的开发者指令缺少 %s 的参数 %s", keyID, a.ID, p.Name)
					}
				}
			}
		}
		if usable == 0 || strings.Contains(instructions, "当前没有可用的图像 / 视频生成模型") || !strings.Contains(instructions, "## 可用的生成模型") {
			t.Fatalf("密钥 %d（%d 个可用模型）的开发者指令:\n%s", keyID, usable, instructions)
		}
		if keyID == keyGrokOnly && (usable != 3 || strings.Contains(instructions, "output_format")) {
			t.Fatalf("只钉 Grok 的密钥：可用 %d 个，指令里不该有 Codex 的参数:\n%s", usable, instructions)
		}
	}
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: keyNoMedia})
	if err != nil {
		t.Fatal(err)
	}
	// 这把密钥调生成工具：失败并说明原因，不建任务。
	e.runScript(chat.ID, func(tools hostagent.ToolEndpoint) {
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x"}); !isErr || !strings.Contains(text, "no usable image model") {
			t.Errorf("没有可用模型时的 generate_image = %q / %v", text, isErr)
		}
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "model": "gpt-image-2"}); !isErr || !strings.Contains(text, "not available") {
			t.Errorf("未获授权的模型 = %q / %v", text, isErr)
		}
	})
	instructions := e.startedInstructions(started)
	if !strings.Contains(instructions, "当前没有可用的图像 / 视频生成模型") || strings.Contains(instructions, "## 可用的生成模型") {
		t.Fatalf("没有可用模型的开发者指令:\n%s", instructions)
	}
	for _, m := range mediagen.Presets() {
		if strings.Contains(instructions, "`"+m.ID+"`（") {
			t.Fatalf("没有可用模型时指令里不该出现 %s", m.ID)
		}
	}
	if n := e.countMediaJobs(); n != 0 {
		t.Fatalf("被拒的生成不该建任务：%d 条", n)
	}
}

// 媒体生成能力是工作空间的显式白名单：没启用的模型不能用、不进开发者指令；配置变了，开着的
// 引擎会话在下一条指令前收到新的一节，新起的会话直接带上；说明文字进指令。
func TestMediaConfig(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.m.SetMediaConfig(ctx, e.ws, nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := e.m.MediaConfig(ctx, e.ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg) != len(mediagen.Presets()) {
		t.Fatalf("配置读数应列出能力表全部 %d 个模型，得到 %d", len(mediagen.Presets()), len(cfg))
	}
	for _, m := range cfg {
		if m.Enabled || m.DefaultUsage == "" {
			t.Fatalf("未配置的空间：%s enabled=%v default=%q", m.ID, m.Enabled, m.DefaultUsage)
		}
	}
	for _, bad := range [][]store.StudioMediaModel{
		{{Model: "no-such-model"}},
		{{Model: "gpt-image-2"}, {Model: "gpt-image-2"}},
		{{Model: "gpt-image-2", Usage: strings.Repeat("长", studio.MaxUsageRunes+1)}},
	} {
		var se *studio.Error
		if err := e.m.SetMediaConfig(ctx, e.ws, bad); !errors.As(err, &se) || se.Code != studio.CodeInvalidInput {
			t.Fatalf("SetMediaConfig(%v) = %v，应为 invalid input", bad[0].Model, err)
		}
	}

	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	run := func(script func(tools hostagent.ToolEndpoint)) {
		t.Helper()
		e.engine.mu.Lock()
		e.engine.script = func(_ context.Context, in hostagent.Input, _ hostagent.Sink, tools hostagent.ToolEndpoint, _ <-chan struct{}) hostagent.Outcome {
			inputs = append(inputs, in.Text)
			script(tools)
			return hostagent.Outcome{Status: hostagent.OutcomeCompleted}
		}
		e.engine.mu.Unlock()
		r, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "go"})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		e.waitRun(r.ID)
	}
	// 一个都没启用：指令里说明没有模型，生成被拒、不建任务。
	run(func(tools hostagent.ToolEndpoint) {
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x"}); !isErr || !strings.Contains(text, "no image model is enabled") {
			t.Errorf("未启用任何模型时的 generate_image = %q / %v", text, isErr)
		}
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "model": "gpt-image-2"}); !isErr || !strings.Contains(text, "not enabled in this workspace") {
			t.Errorf("点名未启用的模型 = %q / %v", text, isErr)
		}
	})
	if first := e.startedInstructions(0); !strings.Contains(first, "当前没有可用的图像 / 视频生成模型") {
		t.Fatalf("未启用任何模型的开发者指令:\n%s", first)
	}
	if inputs[0] != "go" || e.countMediaJobs() != 0 {
		t.Fatalf("首条指令应原文交给引擎且不建任务：%q，任务 %d 条", inputs[0], e.countMediaJobs())
	}

	// 会话还开着时启用一个模型并写上说明：下一条指令前收到设备通知，时间线留事件，模型立即可用。
	if err := e.m.SetMediaConfig(ctx, e.ws, []store.StudioMediaModel{{Model: "grok-imagine-image-2.0", Usage: "  只用来出\n草图  "}}); err != nil {
		t.Fatal(err)
	}
	run(func(tools hostagent.ToolEndpoint) {
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "a cat"}); isErr {
			t.Errorf("启用后缺省模型应可生成：%q", text)
		}
		if text, isErr, _ := callTool(t, tools, "generate_image", map[string]any{"prompt": "x", "model": "gpt-image-2"}); !isErr || !strings.Contains(text, "grok-imagine-image-2.0") {
			t.Errorf("点名未启用的模型应被拒并列出可用的：%q / %v", text, isErr)
		}
	})
	notice := inputs[1]
	for _, want := range []string{"设备通知", "`grok-imagine-image-2.0`（", "什么时候用：只用来出 草图", "\ngo"} {
		if !strings.Contains(notice, want) {
			t.Fatalf("配置变化后的指令缺少 %q:\n%s", want, notice)
		}
	}
	if strings.Contains(notice, "`gpt-image-2`（") {
		t.Fatalf("通知里不该出现未启用的模型:\n%s", notice)
	}
	updated := 0
	for _, ev := range e.events(chat.ID) {
		if ev.Kind == store.StudioEventSession && ev.Title == "media_updated" {
			updated++
		}
	}
	if updated != 1 {
		t.Fatalf("应留 1 条 media_updated 事件，得到 %d", updated)
	}
	// 配置没再变：指令原文。指令正文落库的也是原文。
	run(func(hostagent.ToolEndpoint) {})
	if inputs[2] != "go" {
		t.Fatalf("配置未变时应原文交给引擎：%q", inputs[2])
	}
	for _, ev := range e.events(chat.ID) {
		if ev.Kind == store.StudioEventUser && ev.Body != "go" {
			t.Fatalf("时间线里的指令应是原文：%q", ev.Body)
		}
	}
	// 会话重启：新会话的开发者指令直接带当前配置，不再另发通知。
	if err := e.m.Stop(ctx, e.ws.ID, chat.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { return e.m.Snapshot(chat.ID).EngineStartedAt == nil })
	run(func(hostagent.ToolEndpoint) {})
	if second := e.startedInstructions(1); !strings.Contains(second, "什么时候用：只用来出 草图") || inputs[3] != "go" {
		t.Fatalf("重启后的会话：指令 %q\n%s", inputs[3], second)
	}
	// 读数：启用状态与说明。
	cfg, _ = e.m.MediaConfig(ctx, e.ws)
	for _, m := range cfg {
		if on := m.ID == "grok-imagine-image-2.0"; m.Enabled != on || (on && m.Usage != "只用来出\n草图") {
			t.Fatalf("配置读数 %s: enabled=%v usage=%q", m.ID, m.Enabled, m.Usage)
		}
	}
}

// 归档：会话结束，之后只能查看——提交被拒，时间线与读数照旧。
func TestArchiveChat(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.runScript(chat.ID, func(hostagent.ToolEndpoint) {})
	archived, err := e.m.ArchiveChat(ctx, e.ws.ID, chat.ID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("ArchiveChat = %+v, %v", archived, err)
	}
	if st := e.m.Snapshot(chat.ID); st.EngineStartedAt != nil || st.Status != hostagent.StatusIdle {
		t.Fatalf("归档后会话应已结束：%+v", st)
	}
	var se *studio.Error
	if _, err := e.m.Submit(ctx, e.ws, chat.ID, hostagent.Input{Text: "again"}); !errors.As(err, &se) || se.Code != studio.CodeChatArchived {
		t.Fatalf("归档后提交 = %v，应为 %s", err, studio.CodeChatArchived)
	}
	again, err := e.m.ArchiveChat(ctx, e.ws.ID, chat.ID)
	if err != nil || !again.ArchivedAt.Equal(*archived.ArchivedAt) {
		t.Fatalf("重复归档应保持原时刻：%v / %v", again, err)
	}
	chats, _ := e.m.ListChats(ctx, e.ws.ID)
	if len(chats) != 1 || chats[0].ArchivedAt == nil || len(e.events(chat.ID)) == 0 {
		t.Fatalf("归档的对话仍在列表里、时间线保留：%+v", chats)
	}
	if _, err := e.m.ArchiveChat(ctx, e.ws.ID, "nope"); !errors.As(err, &se) || se.Code != studio.CodeChatNotFound {
		t.Fatalf("归档不存在的对话 = %v", err)
	}
}

// 绊线：生成工具的参数描述与受理校验同源。tools/list 里 generate_image / generate_video 的
// inputSchema 是合法 JSON、params.description 不留 {{ 占位，且包含能力表里对应种类每个预设模型
// 的 id、每个参数名（含厂商面后端的基线参数）；另一种类的模型不混进来。
func TestToolSchemasRenderedFromCapabilities(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, err := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	var listed map[string]any
	e.runScript(chat.ID, func(tools hostagent.ToolEndpoint) { listed = mcpCall(t, tools, "tools/list", nil) })
	raw, err := json.Marshal(listed)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("tools/list 不是合法 JSON: %v", err)
	}
	if strings.Contains(string(raw), "{{") {
		t.Fatalf("tools/list 里还留着占位: %s", raw)
	}
	descriptions := map[string]string{}
	for _, item := range listed["tools"].([]any) {
		tool := item.(map[string]any)
		name, _ := tool["name"].(string)
		if name != "generate_image" && name != "generate_video" {
			continue
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s 的 inputSchema = %+v", name, tool["inputSchema"])
		}
		props, _ := schema["properties"].(map[string]any)
		for _, want := range []string{"prompt", "model", "name", "reference_images", "params", "count"} {
			if _, ok := props[want]; !ok {
				t.Fatalf("%s 的入参缺少 %s", name, want)
			}
		}
		if _, ok := props["provider"]; ok {
			t.Fatalf("%s 的入参不该再有 provider", name)
		}
		params, _ := props["params"].(map[string]any)
		desc, _ := params["description"].(string)
		if params["type"] != "object" || desc == "" {
			t.Fatalf("%s 的 params = %+v", name, params)
		}
		descriptions[name] = desc
	}
	if len(descriptions) != 2 {
		t.Fatalf("tools/list 里的生成工具 = %d 个", len(descriptions))
	}
	for tool, kind := range map[string]string{"generate_image": store.ModelKindImage, "generate_video": store.ModelKindVideo} {
		desc := descriptions[tool]
		models := mediagen.Presets()
		for _, family := range mediagen.Families() {
			if m, ok := mediagen.FamilyModel(family, "<"+family+" 模型>"); ok {
				models = append(models, m)
			}
		}
		seen := 0
		for _, m := range models {
			quoted := "`" + m.ID + "`"
			if m.Kind != kind {
				if strings.Contains(desc, quoted) {
					t.Fatalf("%s 的参数描述混进了另一种类的模型 %s", tool, m.ID)
				}
				continue
			}
			seen++
			if !strings.Contains(desc, quoted) {
				t.Fatalf("%s 的参数描述缺少模型 %s:\n%s", tool, m.ID, desc)
			}
			for _, op := range m.Operations {
				if !strings.Contains(desc, "operation `"+op.Name+"`") {
					t.Fatalf("%s 的参数描述缺少 %s 的操作 %s", tool, m.ID, op.Name)
				}
				for _, in := range op.Inputs {
					if !strings.Contains(desc, in.Role+"（") {
						t.Fatalf("%s 的参数描述缺少 %s 的输入角色 %s", tool, m.ID, in.Role)
					}
				}
				for _, p := range op.Params {
					if !strings.Contains(desc, p.Name+"（"+p.Label) {
						t.Fatalf("%s 的参数描述缺少 %s 的参数 %s:\n%s", tool, m.ID, p.Name, desc)
					}
					for _, v := range p.Values {
						if !strings.Contains(desc, v) {
							t.Fatalf("%s 的参数描述缺少 %s.%s 的取值 %s", tool, m.ID, p.Name, v)
						}
					}
				}
			}
		}
		if seen == 0 {
			t.Fatalf("能力表里没有 %s 模型", kind)
		}
	}
}

func TestRecover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	chat, _ := e.m.CreateChat(ctx, e.ws, studio.NewChat{KeyID: 1})
	run, err := e.st.CreateStudioRun(ctx, store.NewStudioRun{WorkspaceID: e.ws.ID, ChatID: chat.ID, Text: "残留"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.Recover(ctx)
	if got, _ := e.st.GetStudioRun(ctx, run.ID); got.Status != store.AgentRunFailed {
		t.Fatalf("Recover 后 = %+v", got)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

// 创作类型：内嵌模板齐全、缺省项存在；建空间后 EnsureBrief 按类型预置 agent/PROJECT.md（来源 agent，
// 已有即不动、智能体可覆盖）；开发者指令带这一类的名称与规程；不认识的类型退到缺省。
func TestTemplatesAndBrief(t *testing.T) {
	all := studio.Templates()
	if len(all) < 2 || all[0].ID != studio.DefaultTemplate {
		t.Fatalf("创作类型清单 = %+v", all)
	}
	seen := map[string]bool{}
	for _, tpl := range all {
		if tpl.ID == "" || tpl.Name == "" || tpl.Description == "" || tpl.Guide == "" || tpl.Brief == "" || seen[tpl.ID] {
			t.Fatalf("创作类型不完整或重复: %+v", tpl)
		}
		seen[tpl.ID] = true
		if _, err := store.NormalizeWorkspaceTemplate(tpl.ID); err != nil {
			t.Fatalf("创作类型 ID %q 不合 store 的形状: %v", tpl.ID, err)
		}
		if !strings.Contains(tpl.Brief, "创作类型："+tpl.Name) || !strings.HasPrefix(tpl.Brief, "# 项目说明") {
			t.Fatalf("创作类型 %s 的 PROJECT.md 骨架头部不对:\n%s", tpl.ID, tpl.Brief)
		}
	}
	for _, id := range []string{"short-drama", "music-video", "product-visual", "storyboard"} {
		if _, ok := studio.TemplateByID(id); !ok {
			t.Fatalf("创作类型 %s 缺失", id)
		}
	}
	if _, ok := studio.TemplateByID("nope"); ok {
		t.Fatal("不认识的创作类型不该找到")
	}
	if tpl, ok := studio.TemplateByID(""); !ok || tpl.ID != studio.DefaultTemplate {
		t.Fatalf("空 ID 应取缺省，得到 %+v / %v", tpl, ok)
	}

	h := newHostEnv(t)
	e := h.env
	ctx := context.Background()
	newWorkspace := func(name, template string) *store.Workspace {
		res, err := h.dev.CreateWorkspace(ctx, devhost.WorkspaceRequest{HostID: h.host.ID, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		ws, err := e.st.CreateWorkspace(ctx, store.NewWorkspace{Kind: store.WorkspaceKindStudio, HostID: h.host.ID, Name: name, Path: res.Path, Template: template})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	ws := newWorkspace("drama", "short-drama")
	drama, _ := studio.TemplateByID("short-drama")
	if err := e.m.EnsureBrief(ctx, ws); err != nil {
		t.Fatalf("EnsureBrief: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws.Path, "agent", "PROJECT.md"))
	if err != nil || string(data) != drama.Brief {
		t.Fatalf("预置的 PROJECT.md = %q / %v", data, err)
	}
	row, err := e.st.GetStudioFile(ctx, ws.ID, studio.BriefFileName)
	if err != nil || row.Origin != store.StudioFileOriginAgent || row.Kind != store.StudioFileText {
		t.Fatalf("附注 = %+v / %v", row, err)
	}
	// 已有就不动：管理员上传替换过的内容保留。
	if _, err := e.m.SaveFile(ctx, ws, studio.BriefFileName, strings.NewReader("# 我的说明\n"), 0, studio.FileMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.EnsureBrief(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(ws.Path, "agent", "PROJECT.md")); string(data) != "# 我的说明\n" {
		t.Fatalf("EnsureBrief 覆盖了已有的项目说明: %q", data)
	}
	// 开发者指令带创作类型一节与规程正文、当前项目说明。
	chat, err := e.m.CreateChat(ctx, ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	chatRow, _ := e.st.GetStudioChat(ctx, chat.ID)
	for _, want := range []string{"## 创作类型：1 分钟短剧", drama.Guide, "# 我的说明", "## 当前项目说明（agent/PROJECT.md）"} {
		if !strings.Contains(chatRow.Instructions, want) {
			t.Fatalf("开发者指令缺 %q:\n%s", want, chatRow.Instructions)
		}
	}
	if i, j := strings.Index(chatRow.Instructions, "## 工作方式"), strings.Index(chatRow.Instructions, "## 创作类型："); i < 0 || j < i {
		t.Fatalf("创作类型一节应在「工作方式」之后:\n%s", chatRow.Instructions)
	}
	// 行里的类型不认识时退到缺省，对话仍能开；没有 PROJECT.md 的空间在新建对话时补上缺省骨架。
	ws = newWorkspace("retired", "retired-kind")
	chat, err = e.m.CreateChat(ctx, ws, studio.NewChat{KeyID: 1})
	if err != nil {
		t.Fatalf("CreateChat(未知类型): %v", err)
	}
	general, _ := studio.TemplateByID(studio.DefaultTemplate)
	chatRow, _ = e.st.GetStudioChat(ctx, chat.ID)
	if !strings.Contains(chatRow.Instructions, "## 创作类型："+general.Name) || !strings.Contains(chatRow.Instructions, "创作类型：通用") {
		t.Fatalf("未知类型应退到缺省并补骨架:\n%s", chatRow.Instructions)
	}
}
