// Package hostagent 是「智能体 → 主机/SoC」的 Agent远控：管理员在一台已纳管主机的
// 「Agent远控」页里开若干个对话，在对话里逐条提交指令，设备用智能体引擎（缺省 Codex
// App Server）按提交顺序逐条执行；智能体对主机的每一步操作都经设备这一侧的工具执行并
// 写进时间线，构成可查询、可审计的操作日志；每台主机另有一份 Markdown 主机档案，由
// 智能体维护、管理员可改。
//
// 结构：
//   - 对话（agent_host_chats）：新建时选定 API 密钥、模型、推理档位并注入当时的主机档案
//     与最近操作（开发者指令），之后不改；可删，删了只带走对话内容，对主机的操作记录
//     留在主机名下。
//   - 每个对话一个会话（session.go）：一条指令队列 + 至多一个正在执行的指令 + 一段
//     引擎会话（空闲一段时间自动关掉，下次提交再起——起的时候附上本对话先前的记录）。
//     **不插队**：一条做完才做下一条。
//   - 引擎（engine.go / codex.go）只负责「理解指令、决定做什么、写回复」；动主机的
//     动作全部经 MCP 工具（mcp.go / tools.go）回到设备执行——凭证书走 SSH，引擎自己
//     没有任何主机凭据。
//   - 读数零轮询：页面按会话 revision 陪等（Wait），有新事件 / 状态变化才回一帧。
//
// §15.1：本包日志只记主机 id、对话 id、指令 id、状态与耗时；指令文本、图片、回复、
// 命令与输出都不进日志（它们进的是数据库里的时间线，那是产品功能）。
package hostagent

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
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 错误码。管理面按它映射 HTTP 状态（internal/admin/hostagent.go）。
const (
	CodeEngineNotReady = "agent_engine_not_ready"
	CodeHostNotReady   = "agent_host_not_ready"
	CodeInvalidInput   = "agent_invalid_input"
	CodeKeyInvalid     = "agent_key_invalid"
	CodeChatNotFound   = "agent_chat_not_found"
	CodeRunNotFound    = "agent_run_not_found"
	CodeRunFinished    = "agent_run_finished"
	CodeUnavailable    = "agent_unavailable"
)

// Error 是本包对外的错误形态。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// 输入上限：图片按 data URI 计，正文与标题按 rune 计。
const (
	MaxImages     = 5
	MaxImageBytes = 8 << 20
	MaxTextRunes  = 20000
	MaxTitleRunes = 80
)

// WaitFor 是一次陪等的上限（与媒体生成陪等同量级）。
const WaitFor = 30 * time.Second

// Options 装配 Manager。
type Options struct {
	Store  *store.Store
	Hosts  *agenthost.Manager
	Engine Engine
	Logger *slog.Logger
	Now    func() time.Time
	// IdleTimeout 是引擎会话空闲多久后关闭（零值 15 分钟）。
	IdleTimeout time.Duration
	// MaxParallel 是全设备同时执行的指令数上限（零值 2）：板子只有几个核，引擎实例
	// 各占一份 CPU，超出的指令留在各自队列里等。
	MaxParallel int
	// ToolURL 是引擎访问设备 MCP 工具端点的地址（本机回环，如 http://127.0.0.1:80/agent-mcp）。
	ToolURL string
}

// Manager 是本包入口。并发安全。
type Manager struct {
	st     *store.Store
	hosts  *agenthost.Manager
	engine Engine
	log    *slog.Logger
	now    func() time.Time
	idle   time.Duration
	sem    chan struct{}
	tool   string

	mu sync.Mutex
	// sessions 按对话 id；tokens 按引擎会话令牌。
	sessions map[string]*session
	tokens   map[string]*session
}

// New 装配 Manager；Recover 由调用方在装配后调一次。
func New(o Options) *Manager {
	m := &Manager{st: o.Store, hosts: o.Hosts, engine: o.Engine, log: o.Logger, now: o.Now, idle: o.IdleTimeout,
		tool: o.ToolURL, sessions: map[string]*session{}, tokens: map[string]*session{}}
	if m.tool == "" {
		m.tool = "http://127.0.0.1/agent-mcp"
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
	return m
}

// toolURL 是给引擎的 MCP 端点地址。
func (m *Manager) toolURL() string { return m.tool }

// Recover 把进程重启前残留的未完成指令判失败（引擎会话随进程一起没了，不可恢复）。
func (m *Manager) Recover(ctx context.Context) {
	runs, err := m.st.ListActiveAgentHostRuns(ctx)
	if err != nil {
		m.log.Error("读取未完成指令失败", "error", err.Error())
		return
	}
	for _, r := range runs {
		if _, err := m.st.SetAgentHostRunStatus(ctx, r.ID, store.AgentRunFailed, interruptedReason, m.now()); err != nil {
			m.log.Error("判定中断指令失败", "run_id", r.ID, "error", err.Error())
			continue
		}
		if _, err := m.st.AppendAgentHostEvent(ctx, store.AgentHostEvent{HostID: r.HostID, ChatID: r.ChatID, RunID: r.ID, At: m.now(),
			Kind: store.AgentEventRun, Title: store.AgentRunFailed, Body: interruptedReason}); err != nil {
			m.log.Error("写中断事件失败", "run_id", r.ID, "error", err.Error())
		}
	}
	if len(runs) > 0 {
		m.log.Info("已把进程重启前的未完成指令判为失败", "count", len(runs))
	}
}

const interruptedReason = "设备进程已重启，指令被中断，请重新提交"

// EngineStatus 是引擎可用性读数；没接引擎时报不可用。
func (m *Manager) EngineStatus(ctx context.Context) EngineStatus {
	if m.engine == nil {
		return EngineStatus{Ready: false, Reason: "本设备未接入智能体引擎"}
	}
	return m.engine.Status(ctx)
}

// engineReady 确认引擎可用，否则给出带原因的错误。
func (m *Manager) engineReady(ctx context.Context) (EngineStatus, error) {
	status := m.EngineStatus(ctx)
	if !status.Ready {
		reason := status.Reason
		if reason == "" {
			reason = "智能体引擎未就绪"
		}
		return status, &Error{Code: CodeEngineNotReady, Msg: reason}
	}
	return status, nil
}

// NewChat 是新建对话的入参。
type NewChat struct {
	Title  string
	KeyID  int64
	Model  string
	Effort string
}

// recentOpsForPrompt 是新建对话时塞进开发者指令的最近操作条数。
const recentOpsForPrompt = 30

// CreateChat 新建一个对话：校验引擎与密钥 / 模型 / 档位，把当时的主机档案与最近操作
// 渲染成开发者指令一起存下来。主机不必此刻就绪（提交指令时再看）。
func (m *Manager) CreateChat(ctx context.Context, hostID int64, in NewChat) (*store.AgentHostChat, error) {
	in.Title = strings.TrimSpace(in.Title)
	if len([]rune(in.Title)) > MaxTitleRunes {
		return nil, &Error{Code: CodeInvalidInput, Msg: fmt.Sprintf("对话标题最多 %d 个字符", MaxTitleRunes)}
	}
	host, err := m.st.GetAgentHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	status, err := m.engineReady(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := m.engine.Validate(ctx, ChatConfig{KeyID: in.KeyID, Model: in.Model, Effort: in.Effort})
	if err != nil {
		return nil, err
	}
	profile, err := m.st.GetAgentHostProfile(ctx, hostID)
	if err != nil {
		return nil, err
	}
	recent, err := m.st.ListAgentHostEvents(ctx, store.AgentHostEventQuery{HostID: hostID, Limit: recentOpsForPrompt,
		Kinds: []string{store.AgentEventCommand, store.AgentEventFile, store.AgentEventProfile}})
	if err != nil {
		return nil, err
	}
	chat, err := m.st.CreateAgentHostChat(ctx, store.NewAgentHostChat{HostID: hostID, Title: in.Title, Engine: status.ID,
		KeyID: cfg.KeyID, KeyDisplay: cfg.KeyDisplay, Model: cfg.Model, Effort: cfg.Effort,
		Instructions: buildInstructions(*host, profile.Content, recent), CreatedAt: m.now()})
	if err != nil {
		return nil, err
	}
	m.log.Info("已新建对话", "host_id", hostID, "chat_id", chat.ID, "engine", status.ID)
	return chat, nil
}

// Chat 是对话读数：行 + 会话此刻的状态。
type Chat struct {
	store.AgentHostChat
	// Status 同 State.Status；没有会话对象时是 idle。
	Status string `json:"status"`
	// Busy 表示有指令在执行或排队。
	Busy bool `json:"busy"`
}

// ListChats 列出一台主机的对话（最近活动的在前）。
func (m *Manager) ListChats(ctx context.Context, hostID int64) ([]Chat, error) {
	rows, err := m.st.ListAgentHostChats(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := make([]Chat, 0, len(rows))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		c := Chat{AgentHostChat: row, Status: StatusIdle}
		if s, ok := m.sessions[row.ID]; ok {
			st := s.snapshot()
			c.Status = st.Status
			c.Busy = st.Current != nil || len(st.Queue) > 0
		}
		out = append(out, c)
	}
	return out, nil
}

// GetChat 取一个对话，并确认它属于这台主机。
func (m *Manager) GetChat(ctx context.Context, hostID int64, chatID string) (*store.AgentHostChat, error) {
	chat, err := m.st.GetAgentHostChat(ctx, chatID)
	if isNotFound(err) || (err == nil && chat.HostID != hostID) {
		return nil, &Error{Code: CodeChatNotFound, Msg: "对话不存在"}
	}
	if err != nil {
		return nil, err
	}
	return chat, nil
}

// DeleteChat 删一个对话：结束它的会话（中止执行中的、取消排队的、关引擎），再删行。
func (m *Manager) DeleteChat(ctx context.Context, hostID int64, chatID string) error {
	chat, err := m.GetChat(ctx, hostID, chatID)
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
	if err := m.st.DeleteAgentHostChat(ctx, chat.ID); err != nil {
		if isNotFound(err) {
			return &Error{Code: CodeChatNotFound, Msg: "对话不存在"}
		}
		return err
	}
	m.log.Info("已删除对话", "host_id", hostID, "chat_id", chat.ID)
	return nil
}

// session 取（或建）一个对话的会话对象。会话对象很轻：没有引擎进程时它只是队列与读数。
func (m *Manager) session(chat *store.AgentHostChat) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[chat.ID]
	if !ok {
		s = newSession(m, chat.ID, chat.HostID)
		m.sessions[chat.ID] = s
	}
	return s
}

// peekSession 只取已有的会话对象（读数用），没有即 nil。
func (m *Manager) peekSession(chatID string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[chatID]
}

// Submit 向一个对话提交一条指令：校验输入、主机 / 引擎 / 密钥状态，落库为 queued，
// 追加 user 事件，排进队列。
func (m *Manager) Submit(ctx context.Context, hostID int64, chatID string, in Input) (*store.AgentHostRun, error) {
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	chat, err := m.GetChat(ctx, hostID, chatID)
	if err != nil {
		return nil, err
	}
	host, err := m.st.GetAgentHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if host.Status != store.AgentHostStatusReady {
		return nil, &Error{Code: CodeHostNotReady, Msg: "这台主机当前不可用（证书未装好或最近一次连接失败），请先在主机列表里检查连接。"}
	}
	status, err := m.engineReady(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := m.engine.Validate(ctx, chatConfigOf(chat)); err != nil {
		return nil, err
	}
	return m.session(chat).submit(ctx, chat, in, status.ID)
}

func chatConfigOf(c *store.AgentHostChat) ChatConfig {
	return ChatConfig{KeyID: c.KeyID, KeyDisplay: c.KeyDisplay, Model: c.Model, Effort: c.Effort}
}

// validateInput 收窄输入：正文或图片至少一样；图片是 image/* 的 data URI 且不超上限。
func validateInput(in *Input) error {
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
func (m *Manager) Cancel(ctx context.Context, hostID int64, chatID, runID string) error {
	chat, err := m.GetChat(ctx, hostID, chatID)
	if err != nil {
		return err
	}
	return m.session(chat).cancel(ctx, runID)
}

// Stop 结束一个对话的会话：取消队列里全部指令、中止正在执行的那条、关掉引擎。
func (m *Manager) Stop(ctx context.Context, hostID int64, chatID string) error {
	chat, err := m.GetChat(ctx, hostID, chatID)
	if err != nil {
		return err
	}
	return m.session(chat).stop(ctx)
}

// HostRemoved 在主机被解除纳管时调用：结束它全部对话的会话并忘掉它们（数据库行随
// 主机级联删除）。
func (m *Manager) HostRemoved(ctx context.Context, hostID int64) {
	m.mu.Lock()
	var all []*session
	for id, s := range m.sessions {
		if s.hostID == hostID {
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

// State 是一个对话会话的读数。
type State struct {
	ChatID   string `json:"chat_id"`
	Revision int64  `json:"revision"`
	// Status ∈ idle / waiting / starting / running / stopping。
	Status string `json:"status"`
	// EngineStartedAt 是当前引擎会话的起始时刻（没有会话时省略）。
	EngineStartedAt *time.Time           `json:"engine_started_at,omitempty"`
	Current         *store.AgentHostRun  `json:"current,omitempty"`
	Queue           []store.AgentHostRun `json:"queue"`
	Live            *Live                `json:"live,omitempty"`
}

// Live 是正在执行的指令的流式读数（不落库）。
type Live struct {
	RunID string `json:"run_id"`
	Text  string `json:"text"`
	// Activity 是正在做的事（执行命令：…），给界面看，按 Accept-Language 本地化。
	Activity string `json:"activity,omitempty" i18n:"text"`
}

const (
	StatusIdle     = "idle"
	StatusWaiting  = "waiting"
	StatusStarting = "starting"
	StatusRunning  = "running"
	StatusStopping = "stopping"
)

// Snapshot 读一个对话会话的当前读数（没有会话对象时是空闲读数）。
func (m *Manager) Snapshot(chatID string) State {
	if s := m.peekSession(chatID); s != nil {
		return s.snapshot()
	}
	return State{ChatID: chatID, Status: StatusIdle, Queue: []store.AgentHostRun{}}
}

// Wait 陪等一个对话的会话离开 revision：有变化或 WaitFor 耗尽即回当前读数。对话须存在。
func (m *Manager) Wait(ctx context.Context, hostID int64, chatID string, revision int64) (State, error) {
	chat, err := m.GetChat(ctx, hostID, chatID)
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

// SetProfile 由管理员整份替换主机档案，并留一条（主机级的）profile 事件。
func (m *Manager) SetProfile(ctx context.Context, hostID int64, content string) (store.AgentHostProfile, error) {
	content = normalizeProfile(content)
	p, err := m.st.SetAgentHostProfile(ctx, hostID, content, "admin", m.now())
	if err != nil {
		return p, err
	}
	if _, err := m.st.AppendAgentHostEvent(ctx, store.AgentHostEvent{HostID: hostID, At: m.now(),
		Kind: store.AgentEventProfile, Title: "admin", Body: content}); err != nil {
		m.log.Error("写档案事件失败", "host_id", hostID, "error", err.Error())
	}
	return p, nil
}

// MaxProfileBytes 是主机档案的长度上限。
const MaxProfileBytes = 256 << 10

func normalizeProfile(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if len(s) > MaxProfileBytes {
		s = s[:MaxProfileBytes]
	}
	return strings.TrimRight(s, "\n") + "\n"
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

// ErrNotFound 直通仓储的未命中。
var ErrNotFound = store.ErrNotFound

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
