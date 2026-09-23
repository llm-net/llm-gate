// Package studio 是「智能体 → 工作空间」里**创作工作空间**（kind = studio）的本体：一个目录
// ——落在一台工作节点上（~/workspaces/<name>，经守护进程 devd 读写，storage.go；早先的空间也
// 可能落在设备数据目录下 <data_dir>/workspaces/<id>，那种只能查看与管理文件、不能对话），固定三个
// 子目录 agent/（智能体文档）、docs/（创作文档）、media/（媒体，files.go）——管理员往 media/
// 上传素材，在对话里逐条给智能体下指令；智能体就跑在那台工作节点上（引擎的 cwd 是工作空间目录），
// 用自己的 shell 与读写文件工具处理素材，用设备暴露的工具看素材、生成图像 / 视频、整理文件——生成
// 的结果落回 media/，前一轮的成果就是下一轮的素材。
//
// 结构照 Agent远控（internal/hostagent）：
//   - 对话（studio_chats）：新建时选定引擎（Codex / Claude Code / Grok）、API 密钥、模型、推理档位并
//     注入当时的目录清单与项目说明（开发者指令），之后不改；可归档（只能查看、不再接受指令），
//     可删，删了只带走对话内容，目录里的文件不动。
//   - 媒体生成能力（studio_media_models）属于工作空间：管理员显式启用哪些生成模型、各自
//     什么时候用；改完对进行中的对话立即生效（media.go）。
//   - 每个对话一个会话（session.go）：一条指令队列 + 至多一个正在执行的指令 + 一段引擎会话
//     （空闲一段时间自动关掉，下次提交再起——起的时候附上本对话先前的记录）。不插队。
//   - 引擎是节点端引擎（internal/nodeengine）：工作节点上 gate 关联的 Codex CLI（app-server）、
//     Claude Code（stream-json）或 Grok（ACP），经 SSH 拉起、经远程转发回到设备的开发工具接入面与 MCP 端点。
//     引擎在节点上直接跑命令、改文件（经 hostagent.ExecSink 落时间线）；设备侧的规则（落位、
//     上传保护、附注）与生成经 MCP 工具（mcp.go / tools.go）回到设备执行。生成走媒体生成内核
//     （internal/mediagen）：以对话钉死的密钥名义调用订阅平台，用量记在那把密钥名下，结果经内核
//     Adopt 搬进工作空间目录、任务随即删除（generate.go）。
//   - 读数零轮询：页面按会话 revision 陪等（Wait）。
//
// §15.1：本包日志只记工作空间 id、对话 id、指令 id、文件名、状态与耗时；指令文本、图片、
// 回复与提示词都不进日志（它们进数据库与目录，那是产品功能）。
package studio

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/nodeengine"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Error 与错误码沿用 Agent远控的形态（管理面按同一张表映射 HTTP 状态）。
type Error = hostagent.Error

const (
	CodeEngineNotReady = hostagent.CodeEngineNotReady
	CodeInvalidInput   = hostagent.CodeInvalidInput
	CodeKeyInvalid     = hostagent.CodeKeyInvalid
	CodeChatNotFound   = hostagent.CodeChatNotFound
	CodeRunNotFound    = hostagent.CodeRunNotFound
	CodeRunFinished    = hostagent.CodeRunFinished
	CodeUnavailable    = hostagent.CodeUnavailable
	// CodeFileInvalid：文件名不合法 / 内容超限 / 种类不对（400）。
	CodeFileInvalid = "studio_file_invalid"
	// CodeFileNotFound：目录里没有这个文件（404）。
	CodeFileNotFound = "studio_file_not_found"
	// CodeFileExists：目标文件名已被占用（409）。
	CodeFileExists = "studio_file_exists"
	// CodeFileTooLarge：上传超过上限（413）。
	CodeFileTooLarge = "studio_file_too_large"
)

// 输入上限沿用 Agent远控。
const (
	MaxImages     = hostagent.MaxImages
	MaxImageBytes = hostagent.MaxImageBytes
	MaxTextRunes  = hostagent.MaxTextRunes
	MaxTitleRunes = hostagent.MaxTitleRunes
)

// WaitFor 是一次陪等的上限。
const WaitFor = hostagent.WaitFor

// RunTimeout 是一条指令的整体上限：一条指令里可能串几次生成（视频一次十来分钟）。
const RunTimeout = 90 * time.Minute

// ToolServer 是写进引擎配置的 MCP 服务器名（模型看到的工具命名空间）。
const ToolServer = "studio"

// DefaultToolPath 是设备 MCP 工具端点的路径。
const DefaultToolPath = "/studio-mcp"

// deviceChatReason 是设备上的创作空间不能对话的原因。
const deviceChatReason = "设备上的创作空间不再支持对话：智能体只在工作节点上运行，请在一台工作节点上新建创作空间（素材可下载后上传过去）"

// Options 装配 Manager。
type Options struct {
	Store *store.Store
	// Engines 是可选的节点端引擎（nodeengine.Engines）；对话新建时按 ID 选一个钉死。
	Engines []nodeengine.Engine
	// Media 是媒体生成的任务内核（全进程共用的那一份）；nil 即不能生成（工具答明原因）。
	Media *mediagen.Service
	// Hosts 提供到工作节点的 SSH 连接；DevHosts 提供守护进程透传（主机上的目录）与工具探测
	// （节点上的 CLI 在哪）。两者为 nil 时主机上的创作工作空间不可用。
	Hosts    *agenthost.Manager
	DevHosts *devhost.Manager
	Logger   *slog.Logger
	Now      func() time.Time
	// IdleTimeout 是引擎会话空闲多久后关闭（零值 15 分钟）。
	IdleTimeout time.Duration
	// MaxParallel 是全设备同时执行的指令数上限（零值 2）。
	MaxParallel int
	// ToolPath 是设备 MCP 工具端点的路径（零值 DefaultToolPath）：节点上的引擎经远程转发访问它。
	ToolPath string
	// DataDir 是设备数据目录；工作空间目录在它的 workspaces/ 之下。
	DataDir string
}

// Manager 是本包入口。并发安全。
type Manager struct {
	st       *store.Store
	engines  []nodeengine.Engine
	media    *mediagen.Service
	hosts    *agenthost.Manager
	devHosts *devhost.Manager
	log      *slog.Logger
	now      func() time.Time
	idle     time.Duration
	sem      chan struct{}
	tool     string
	root     string

	mu sync.Mutex
	// sessions 按对话 id；tokens 按引擎会话令牌。
	sessions map[string]*session
	tokens   map[string]*session
	// fsMu 按工作空间串行化目录写操作（上传 / 生成落盘 / 删除 / 改名 / 对账）。
	fsMu map[string]*sync.Mutex
}

// New 装配 Manager；Recover 由调用方在装配后调一次。
func New(o Options) *Manager {
	m := &Manager{st: o.Store, engines: o.Engines, media: o.Media, hosts: o.Hosts, devHosts: o.DevHosts, log: o.Logger, now: o.Now,
		idle: o.IdleTimeout, tool: o.ToolPath, sessions: map[string]*session{}, tokens: map[string]*session{}, fsMu: map[string]*sync.Mutex{}}
	if m.tool == "" {
		m.tool = DefaultToolPath
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.idle <= 0 {
		m.idle = 15 * time.Minute
	}
	n := o.MaxParallel
	if n <= 0 {
		n = 2
	}
	m.sem = make(chan struct{}, n)
	m.root = workspacesRoot(o.DataDir)
	return m
}

// Recover 把进程重启前残留的未完成指令判失败（引擎会话随进程一起没了，不可恢复）。
func (m *Manager) Recover(ctx context.Context) {
	runs, err := m.st.ListActiveStudioRuns(ctx)
	if err != nil {
		m.log.Error("读取未完成指令失败", "error", err.Error())
		return
	}
	for _, r := range runs {
		if _, err := m.st.SetStudioRunStatus(ctx, r.ID, store.AgentRunFailed, interruptedReason, m.now()); err != nil {
			m.log.Error("判定中断指令失败", "run_id", r.ID, "error", err.Error())
			continue
		}
		if _, err := m.st.AppendStudioEvent(ctx, store.StudioEvent{WorkspaceID: r.WorkspaceID, ChatID: r.ChatID, RunID: r.ID, At: m.now(),
			Kind: store.StudioEventRun, Title: store.AgentRunFailed, Body: interruptedReason}); err != nil {
			m.log.Error("写中断事件失败", "run_id", r.ID, "error", err.Error())
		}
	}
	if len(runs) > 0 {
		m.log.Info("已把进程重启前的未完成指令判为失败", "count", len(runs))
	}
}

const interruptedReason = "设备进程已重启，指令被中断，请重新提交"

// engine 按标识取引擎；不认识即 nil。
func (m *Manager) engine(id string) nodeengine.Engine {
	for _, e := range m.engines {
		if e.ID() == id {
			return e
		}
	}
	return nil
}

// EngineOption 是新建对话时一种引擎的读数：工作节点上有没有它的 CLI（gate 关联的，或 PATH 上的
// 同名程序）。
type EngineOption struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Tool    string   `json:"tool"`
	Efforts []string `json:"efforts"`
	Ready   bool     `json:"ready"`
	// Reason 是不能用的原因（能用时为空）。
	Reason string `json:"reason,omitempty" i18n:"text"`
	// Path / Version 是节点上找到的程序与 gate 记下的版本（Version 可能为空）。
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// EngineOptions 给出这个工作空间可选的引擎：探一次工作节点上的开发工具（一条 SSH 连接）。
// 设备上的空间与探测失败都不报错，而是每种引擎各带原因。
func (m *Manager) EngineOptions(ctx context.Context, ws *store.Workspace) []EngineOption {
	out, _ := m.engineOptions(ctx, ws)
	return out
}

// engineOptions 同 EngineOptions，另回这次探测的读数（探测失败或设备上的空间为 nil）。
func (m *Manager) engineOptions(ctx context.Context, ws *store.Workspace) ([]EngineOption, *devhost.Tools) {
	out := make([]EngineOption, 0, len(m.engines))
	for _, e := range m.engines {
		out = append(out, EngineOption{ID: e.ID(), Label: e.Label(), Tool: e.Tool(), Efforts: e.Efforts()})
	}
	var tools *devhost.Tools
	reason := ""
	switch {
	case !ws.OnHost():
		reason = deviceChatReason
	case m.devHosts == nil:
		reason = "本进程未接入工作节点"
	default:
		var err error
		if tools, err = m.devHosts.Tools(ctx, ws.HostID); err != nil {
			reason = "探测工作节点上的开发工具失败：" + err.Error()
		}
	}
	for i := range out {
		if reason != "" {
			out[i].Reason = reason
			continue
		}
		path, version := locateTool(tools, out[i].Tool)
		if path == "" {
			out[i].Reason = fmt.Sprintf("工作节点上没有 %s：到这台节点的「工具配置」页用 gate 安装或关联", out[i].Label)
			continue
		}
		out[i].Ready, out[i].Path, out[i].Version = true, path, version
	}
	return out, tools
}

// locateTool 在工具配置的读数里找一个开发工具的程序：gate 的关联优先，其次 PATH 上的同名程序。
func locateTool(tools *devhost.Tools, name string) (path, version string) {
	if tools == nil {
		return "", ""
	}
	for _, d := range tools.DevTools {
		if d.Name != name {
			continue
		}
		if d.Linked && d.Path != "" {
			return d.Path, d.Version
		}
		return d.Found, ""
	}
	return "", ""
}

// chatEngine 取一个工作空间里一种引擎、并找到节点上的程序；不能用时返回可展示的 *Error。
// 同一次探测的创作工具读数一并交回（引擎会话启动时写进开发者指令）。
func (m *Manager) chatEngine(ctx context.Context, ws *store.Workspace, id string) (nodeengine.Engine, string, *devhost.StudioTools, error) {
	if !ws.OnHost() {
		return nil, "", nil, &Error{Code: CodeEngineNotReady, Msg: deviceChatReason}
	}
	eng := m.engine(id)
	if eng == nil {
		return nil, "", nil, &Error{Code: CodeEngineNotReady, Msg: fmt.Sprintf("不认识的引擎 %q", id)}
	}
	opts, tools := m.engineOptions(ctx, ws)
	for _, opt := range opts {
		if opt.ID != id {
			continue
		}
		if !opt.Ready {
			return nil, "", nil, &Error{Code: CodeEngineNotReady, Msg: opt.Reason}
		}
		var studio *devhost.StudioTools
		if tools != nil {
			studio = &tools.Studio
		}
		return eng, opt.Path, studio, nil
	}
	return nil, "", nil, &Error{Code: CodeEngineNotReady, Msg: fmt.Sprintf("不认识的引擎 %q", id)}
}

// ChatsEnabled 报告这个工作空间能不能对话；不能时给出原因（设备上的空间）。
func ChatsEnabled(ws *store.Workspace) (bool, string) {
	if !ws.OnHost() {
		return false, deviceChatReason
	}
	return true, ""
}

// KeyModels 给出一把密钥在这个工作空间里的生成模型可用性：只列空间启用了的模型，可用与否是
// 内核的同一份裁决；没接内核时为空。
func (m *Manager) KeyModels(ctx context.Context, workspaceID string, keyID int64) ([]mediagen.Availability, error) {
	out := []mediagen.Availability{}
	if m.media == nil {
		return out, nil
	}
	rows, err := m.st.ListStudioMediaModels(ctx, workspaceID)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	enabled := make(map[string]bool, len(rows))
	for _, r := range rows {
		enabled[r.Model] = true
	}
	avail, err := m.media.Available(ctx, keyID)
	if err != nil {
		return nil, err
	}
	for _, a := range avail {
		if enabled[a.ID] {
			out = append(out, a)
		}
	}
	return out, nil
}

// NewChat 是新建对话的入参。Engine 为空取第一种引擎。
type NewChat struct {
	Title  string
	Engine string
	KeyID  int64
	Model  string
	Effort string
}

// CreateChat 新建一个对话：校验引擎（节点上有它的 CLI）与密钥 / 模型 / 档位，把当时的目录清单
// 与项目说明渲染成开发者指令一起存下来（可用的生成模型随工作空间配置变，会话启动时另渲染，
// media.go）。
func (m *Manager) CreateChat(ctx context.Context, ws *store.Workspace, in NewChat) (*store.StudioChat, error) {
	in.Title = strings.TrimSpace(in.Title)
	if len([]rune(in.Title)) > MaxTitleRunes {
		return nil, &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("对话标题最多 %d 个字符", MaxTitleRunes)}
	}
	if in.Engine == "" && len(m.engines) > 0 {
		in.Engine = m.engines[0].ID()
	}
	eng, _, _, err := m.chatEngine(ctx, ws, in.Engine)
	if err != nil {
		return nil, err
	}
	cfg, err := eng.Validate(ctx, hostagent.ChatConfig{KeyID: in.KeyID, Model: in.Model, Effort: in.Effort})
	if err != nil {
		return nil, err
	}
	var host *store.AgentHost
	if ws.OnHost() {
		if host, err = m.st.GetAgentHost(ctx, ws.HostID); err != nil {
			return nil, err
		}
	}
	if err := m.EnsureBrief(ctx, ws); err != nil {
		m.log.Warn("未能预置项目说明", "workspace_id", ws.ID, "error", err.Error())
	}
	files, err := m.ListFiles(ctx, ws)
	if err != nil {
		return nil, err
	}
	brief := m.readBrief(ctx, ws)
	chat, err := m.st.CreateStudioChat(ctx, store.NewStudioChat{WorkspaceID: ws.ID, Title: in.Title, Engine: eng.ID(),
		KeyID: cfg.KeyID, KeyDisplay: cfg.KeyDisplay, Model: cfg.Model, Effort: cfg.Effort,
		Instructions: buildInstructions(*ws, host, brief, files), CreatedAt: m.now()})
	if err != nil {
		return nil, err
	}
	m.log.Info("已新建对话", "workspace_id", ws.ID, "chat_id", chat.ID, "engine", eng.ID())
	return chat, nil
}

// Chat 是对话读数：行 + 会话此刻的状态。
type Chat struct {
	store.StudioChat
	// Status 同 State.Status；没有会话对象时是 idle。
	Status string `json:"status"`
	// Busy 表示有指令在执行或排队。
	Busy bool `json:"busy"`
}

// ListChats 列出一个工作空间的对话（最近活动的在前）。
func (m *Manager) ListChats(ctx context.Context, workspaceID string) ([]Chat, error) {
	rows, err := m.st.ListStudioChats(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]Chat, 0, len(rows))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		c := Chat{StudioChat: row, Status: hostagent.StatusIdle}
		if s, ok := m.sessions[row.ID]; ok {
			st := s.snapshot()
			c.Status = st.Status
			c.Busy = st.Current != nil || len(st.Queue) > 0
		}
		out = append(out, c)
	}
	return out, nil
}

// GetChat 取一个对话，并确认它属于这个工作空间。
func (m *Manager) GetChat(ctx context.Context, workspaceID, chatID string) (*store.StudioChat, error) {
	chat, err := m.st.GetStudioChat(ctx, chatID)
	if isNotFound(err) || (err == nil && chat.WorkspaceID != workspaceID) {
		return nil, &Error{Code: CodeChatNotFound, Msg: "对话不存在"}
	}
	if err != nil {
		return nil, err
	}
	return chat, nil
}

// DeleteChat 删一个对话：结束它的会话（中止执行中的、取消排队的、关引擎），再删行。
func (m *Manager) DeleteChat(ctx context.Context, workspaceID, chatID string) error {
	chat, err := m.GetChat(ctx, workspaceID, chatID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	s, ok := m.sessions[chat.ID]
	delete(m.sessions, chat.ID)
	m.mu.Unlock()
	if ok {
		_ = s.stop(ctx)
		s.waitIdle(5 * time.Second)
	}
	if err := m.st.DeleteStudioChat(ctx, chat.ID); err != nil {
		if isNotFound(err) {
			return &Error{Code: CodeChatNotFound, Msg: "对话不存在"}
		}
		return err
	}
	m.log.Info("已删除对话", "workspace_id", workspaceID, "chat_id", chat.ID)
	return nil
}

// session 取（或建）一个对话的会话对象。
func (m *Manager) session(chat *store.StudioChat) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[chat.ID]
	if !ok {
		s = newSession(m, chat.ID, chat.WorkspaceID)
		m.sessions[chat.ID] = s
	}
	return s
}

func (m *Manager) peekSession(chatID string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[chatID]
}

// Submit 向一个对话提交一条指令：校验输入、引擎 / 密钥状态，落库为 queued，追加 user 事件，
// 排进队列。
func (m *Manager) Submit(ctx context.Context, ws *store.Workspace, chatID string, in hostagent.Input) (*store.StudioRun, error) {
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	chat, err := m.GetChat(ctx, ws.ID, chatID)
	if err != nil {
		return nil, err
	}
	if chat.ArchivedAt != nil {
		return nil, &Error{Code: CodeChatArchived, Msg: "对话已归档，只能查看，不能再下指令"}
	}
	if !ws.OnHost() {
		return nil, &Error{Code: CodeEngineNotReady, Msg: deviceChatReason}
	}
	eng := m.engine(chat.Engine)
	if eng == nil {
		return nil, &Error{Code: CodeEngineNotReady, Msg: fmt.Sprintf("这个对话的引擎 %q 已不可用，请新建对话", chat.Engine)}
	}
	if _, err := eng.Validate(ctx, chatConfigOf(chat)); err != nil {
		return nil, err
	}
	return m.session(chat).submit(ctx, chat, in, eng.ID())
}

func chatConfigOf(c *store.StudioChat) hostagent.ChatConfig {
	return hostagent.ChatConfig{KeyID: c.KeyID, KeyDisplay: c.KeyDisplay, Model: c.Model, Effort: c.Effort}
}

// validateInput 收窄输入：正文或图片至少一样；图片是 image/* 的 data URI 且不超上限。
func validateInput(in *hostagent.Input) error {
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" && len(in.Images) == 0 {
		return &Error{Code: CodeInvalidInput, Msg: "请输入指令内容"}
	}
	if len([]rune(in.Text)) > MaxTextRunes {
		return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("指令最多 %d 个字符", MaxTextRunes)}
	}
	if len(in.Images) > MaxImages {
		return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("一条指令最多附 %d 张图片", MaxImages)}
	}
	for _, img := range in.Images {
		if !strings.HasPrefix(img, "data:image/") || !strings.Contains(img, ";base64,") {
			return &Error{Code: CodeInvalidInput, Msg: "图片须是 image/* 的 data URI"}
		}
		if len(img) > MaxImageBytes*4/3+64 {
			return &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("单张图片不能超过 %d MiB", MaxImageBytes>>20)}
		}
	}
	return nil
}

// Cancel 取消一条指令：排队中的直接取消；正在执行的请引擎中止。
func (m *Manager) Cancel(ctx context.Context, workspaceID, chatID, runID string) error {
	chat, err := m.GetChat(ctx, workspaceID, chatID)
	if err != nil {
		return err
	}
	return m.session(chat).cancel(ctx, runID)
}

// Stop 结束一个对话的会话：取消队列里全部指令、中止正在执行的那条、关掉引擎。
func (m *Manager) Stop(ctx context.Context, workspaceID, chatID string) error {
	chat, err := m.GetChat(ctx, workspaceID, chatID)
	if err != nil {
		return err
	}
	return m.session(chat).stop(ctx)
}

// WorkspaceRemoved 在工作空间被删除时调用：结束它全部对话的会话并忘掉它们（数据库行随
// 工作空间级联删除，目录由 RemoveDir 收走）。
func (m *Manager) WorkspaceRemoved(ctx context.Context, workspaceID string) {
	m.mu.Lock()
	var all []*session
	for id, s := range m.sessions {
		if s.wsID == workspaceID {
			all = append(all, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.stop(ctx)
	}
	for _, s := range all {
		s.waitIdle(5 * time.Second)
	}
}

// Shutdown 结束全部会话（进程退出前）。
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.stop(ctx)
	}
	for _, s := range all {
		s.waitIdle(5 * time.Second)
	}
}

// State 是一个对话会话的读数（形态同 hostagent.State）。
type State struct {
	ChatID   string `json:"chat_id"`
	Revision int64  `json:"revision"`
	// Status ∈ idle / waiting / starting / running / stopping。
	Status          string            `json:"status"`
	EngineStartedAt *time.Time        `json:"engine_started_at,omitempty"`
	Current         *store.StudioRun  `json:"current,omitempty"`
	Queue           []store.StudioRun `json:"queue"`
	Live            *hostagent.Live   `json:"live,omitempty"`
}

// Snapshot 读一个对话会话的当前读数（没有会话对象时是空闲读数）。
func (m *Manager) Snapshot(chatID string) State {
	if s := m.peekSession(chatID); s != nil {
		return s.snapshot()
	}
	return State{ChatID: chatID, Status: hostagent.StatusIdle, Queue: []store.StudioRun{}}
}

// Wait 陪等一个对话的会话离开 revision：有变化或 WaitFor 耗尽即回当前读数。对话须存在。
func (m *Manager) Wait(ctx context.Context, workspaceID, chatID string, revision int64) (State, error) {
	chat, err := m.GetChat(ctx, workspaceID, chatID)
	if err != nil {
		return State{}, err
	}
	s := m.session(chat)
	deadline := time.NewTimer(WaitFor)
	defer deadline.Stop()
	for {
		notify := s.notifyCh()
		st := s.snapshot()
		if st.Revision != revision {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-deadline.C:
			return st, nil
		case <-notify:
		}
	}
}

func newToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func metaJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
