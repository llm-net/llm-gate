package modeld

// 引擎会话：在本机直接拉起 Codex App Server（`codex app-server`，stdio JSON-RPC）或 Claude Code
// （`claude -p --input-format stream-json --output-format stream-json`），给设备的管理台当智能体。
// 大模型 API 从设备走：创建会话时带来设备地址（api_base）与 API 密钥（api_key），密钥只落在
// 0600 的实例目录里（Codex 的 config.toml、Claude 的 key 文件），不进 argv、不进引擎环境变量。
//
// 一段会话 = 一个 CLI 子进程（独立进程组）+ 一个实例目录 `<state_dir>/engine/s.<随机>`（0700，关闭
// 时删除）+ 一条事件流（内存里最多 eventKeep 条，界面按 seq 增量取、可长轮询）。一段会话同一时刻
// 只跑一条指令（turn）；空闲超过 idleTimeout 自动关闭。
//
// §15.1：事件里的正文是任务数据、不进日志；日志只记会话 id、引擎、结局与时长。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	EngineCodex  = "codex"
	EngineClaude = "claude"

	eventKeep     = 2000
	idleTimeout   = 30 * time.Minute
	maxSessions   = 8
	closeGrace    = 3 * time.Second
	handshakeWait = 90 * time.Second
	stderrTail    = 4 << 10
)

// Event 是会话事件流里的一条。
type Event struct {
	Seq  int64  `json:"seq"`
	At   string `json:"at"`
	Type string `json:"type"` // system | user | message | reasoning | activity | command | file | usage | error | done
	Text string `json:"text,omitempty"`
	// 附加字段（command 的退出码 / 输出，usage 的 token 数，done 的结局）。
	ExitCode *int   `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
	InputTok int64  `json:"input_tokens,omitempty"`
	OutTok   int64  `json:"output_tokens,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
}

// EnginesInfo 是 GET /v1/engines：本机找得到哪些 CLI。
type EnginesInfo struct {
	Engines  []EngineInfo `json:"engines"`
	Sessions int          `json:"sessions"`
}

// EngineInfo 是一种引擎在本机的现状。
type EngineInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Binary string `json:"binary,omitempty"`
	Ready  bool   `json:"ready"`
}

// SessionView 是一段会话的读数。
type SessionView struct {
	ID           string `json:"id"`
	Engine       string `json:"engine"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	Workdir      string `json:"workdir"`
	Binary       string `json:"binary"`
	Status       string `json:"status"` // starting | idle | running | closed
	Title        string `json:"title,omitempty"`
	Partial      string `json:"partial,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Seq          int64  `json:"seq"`
	CreatedAt    string `json:"created_at"`
	LastActiveAt string `json:"last_active_at"`
}

// engineDriver 是两种 CLI 的共同抽象。
type engineDriver interface {
	// Run 执行一条指令，把事实写进 sink，直到这一轮结束。
	Run(ctx context.Context, text string, sink *sessionSink) (outcome string, errMsg string)
	Interrupt(ctx context.Context) error
	Done() <-chan struct{}
}

// session 是一段会话。
type session struct {
	ID      string
	Engine  string
	Model   string
	Effort  string
	Workdir string
	Binary  string
	dir     string
	cmd     *exec.Cmd
	exited  chan struct{}
	stderr  *tailBuffer
	driver  engineDriver
	created time.Time

	mu       sync.Mutex
	status   string
	title    string
	events   []Event
	seq      int64
	partial  strings.Builder
	lastErr  string
	usageIn  int64
	usageOut int64
	lastAct  time.Time
	closed   bool
	turnStop context.CancelFunc
	notify   chan struct{}
}

type engineManager struct {
	s *Server

	mu       sync.Mutex
	sessions map[string]*session
}

func newEngineManager(s *Server) *engineManager {
	return &engineManager{s: s, sessions: map[string]*session{}}
}

// ---- 发现 ----

// findBinary 找一种引擎的 CLI：先看 ~/.local/bin（gate 关联的位置），再看 PATH。
func (s *Server) findBinary(tool string) string {
	if s.home != "" {
		p := filepath.Join(s.home, ".local", "bin", tool)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := lookPath(tool); err == nil {
		return p
	}
	return ""
}

func (m *engineManager) info() EnginesInfo {
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	out := EnginesInfo{Sessions: n}
	for _, e := range []struct{ id, label, tool string }{{EngineCodex, "Codex", "codex"}, {EngineClaude, "Claude Code", "claude"}} {
		bin := m.s.findBinary(e.tool)
		out.Engines = append(out.Engines, EngineInfo{ID: e.id, Label: e.label, Binary: bin, Ready: bin != ""})
	}
	return out
}

// ---- 创建 ----

// sessionInput 是创建会话的入参。
type sessionInput struct {
	Engine       string `json:"engine"`
	Binary       string `json:"binary"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Workdir      string `json:"workdir"`
	Instructions string `json:"instructions"`
	Title        string `json:"title"`
	// APIBase 是设备地址（http(s)://…），引擎的模型调用打到它的 /agents/<tool>/…；APIKey 是这把
	// 客户端密钥的明文（只落实例目录）。
	APIBase string `json:"api_base"`
	APIKey  string `json:"api_key"`
}

func (m *engineManager) create(ctx context.Context, in sessionInput) (*session, error) {
	tool := ""
	switch in.Engine {
	case EngineCodex:
		tool = "codex"
	case EngineClaude:
		tool = "claude"
	default:
		return nil, bad("engine 须是 codex 或 claude")
	}
	in.APIBase = strings.TrimRight(strings.TrimSpace(in.APIBase), "/")
	if !strings.HasPrefix(in.APIBase, "http://") && !strings.HasPrefix(in.APIBase, "https://") {
		return nil, bad("api_base 须是设备的 http(s) 地址")
	}
	if strings.TrimSpace(in.APIKey) == "" {
		return nil, bad("api_key 不能为空")
	}
	if strings.ContainsAny(in.APIKey, "\n\r\x00") || strings.ContainsAny(in.Model, "\n\r\x00\"") {
		return nil, bad("入参含非法字符")
	}
	bin := strings.TrimSpace(in.Binary)
	if bin == "" {
		bin = m.s.findBinary(tool)
	}
	if bin == "" {
		return nil, &Error{Code: CodeEngineMissing, Msg: "本机找不到 " + tool + "：先在工具配置页用 gate 安装它"}
	}
	if !filepath.IsAbs(bin) {
		return nil, bad("binary 须是绝对路径")
	}
	if info, err := os.Stat(bin); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil, &Error{Code: CodeEngineMissing, Msg: "程序不存在或不可执行：" + bin}
	}
	workdir := strings.TrimSpace(in.Workdir)
	if workdir == "" {
		workdir = m.s.home
	}
	if !filepath.IsAbs(workdir) {
		return nil, bad("workdir 须是绝对路径")
	}
	if info, err := os.Stat(workdir); err != nil || !info.IsDir() {
		return nil, bad("工作目录不存在：" + workdir)
	}
	m.mu.Lock()
	if len(m.sessions) >= maxSessions {
		m.mu.Unlock()
		return nil, conflict(fmt.Sprintf("同时最多 %d 段会话，先关掉一些", maxSessions))
	}
	m.mu.Unlock()

	base := filepath.Join(m.s.stateDir, "engine")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(base, "s.")
	if err != nil {
		return nil, err
	}
	sess := &session{ID: newID("ses_"), Engine: in.Engine, Model: in.Model, Effort: in.Effort, Workdir: workdir, Binary: bin, dir: dir,
		created: m.s.now(), status: "starting", title: strings.TrimSpace(in.Title), exited: make(chan struct{}), stderr: &tailBuffer{max: stderrTail}, notify: make(chan struct{})}
	sess.lastAct = sess.created
	ok := false
	defer func() {
		if !ok {
			sess.kill()
			os.RemoveAll(dir)
		}
	}()
	var start func(context.Context, *session, sessionInput) (engineDriver, error)
	switch in.Engine {
	case EngineCodex:
		start = m.startCodex
	case EngineClaude:
		start = m.startClaude
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeWait)
	defer cancel()
	driver, err := start(hctx, sess, in)
	if err != nil {
		return nil, &Error{Code: CodeEngineFailed, Msg: err.Error() + tailNote(sess.stderr.String())}
	}
	sess.driver = driver
	m.mu.Lock()
	m.sessions[sess.ID] = sess
	m.mu.Unlock()
	sess.setStatus("idle")
	sess.push(Event{Type: "system", Text: "会话已启动（" + in.Engine + "）"})
	m.s.log.Info("引擎会话已启动", "session", sess.ID, "engine", in.Engine)
	ok = true
	go m.watch(sess)
	return sess, nil
}

// spawn 以独立进程组拉起 CLI；stdout / stdin 交给协议客户端，stderr 只留尾段。
func (m *engineManager) spawn(sess *session, args []string, env []string) (stdin *os.File, stdout *os.File, err error) {
	cmd := exec.Command(sess.Binary, args...)
	cmd.Dir = sess.Workdir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = sess.stderr
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, nil, err
	}
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, nil, errors.New("拉起 " + filepath.Base(sess.Binary) + " 失败：" + err.Error())
	}
	inR.Close()
	outW.Close()
	sess.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(sess.exited)
	}()
	return inW, outR, nil
}

// baseEnv 是交给引擎子进程的环境：清掉登录环境里可能把请求改道或换凭据的变量。
func (m *engineManager) baseEnv() []string {
	drop := map[string]bool{}
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_SMALL_FAST_MODEL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		drop[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	if m.s.home != "" {
		env = append(env, "HOME="+m.s.home)
	}
	if m.s.user != "" {
		env = append(env, "USER="+m.s.user)
	}
	path := os.Getenv("PATH")
	if m.s.home != "" {
		path = filepath.Join(m.s.home, ".local", "bin") + ":" + path
	}
	env = append(env, "PATH="+path)
	return env
}

// watch 在 CLI 退出或空闲超时后收掉会话。
func (m *engineManager) watch(sess *session) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-sess.exited:
			sess.mu.Lock()
			closed := sess.closed
			sess.mu.Unlock()
			if !closed {
				sess.push(Event{Type: "error", Text: "引擎进程已退出" + tailNote(sess.stderr.String())})
				m.close(sess.ID, "引擎进程退出")
			}
			return
		case <-t.C:
			sess.mu.Lock()
			idle := sess.status == "idle" && m.s.now().Sub(sess.lastAct) > idleTimeout
			sess.mu.Unlock()
			if idle {
				m.close(sess.ID, "空闲超时")
				return
			}
		}
	}
}

func (m *engineManager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *engineManager) views() []SessionView {
	m.mu.Lock()
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	out := make([]SessionView, 0, len(list))
	for _, s := range list {
		out = append(out, s.view())
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt < out[j-1].CreatedAt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// close 结束一段会话：中止指令、关 stdin、等 CLI 自己退，超时 KILL 整个进程组，删实例目录。
func (m *engineManager) close(id, reason string) error {
	m.mu.Lock()
	sess := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if sess == nil {
		return notFound("会话不存在")
	}
	sess.mu.Lock()
	sess.closed = true
	stop := sess.turnStop
	sess.mu.Unlock()
	if stop != nil {
		stop()
	}
	sess.push(Event{Type: "system", Text: "会话已关闭：" + reason})
	sess.setStatus("closed")
	sess.kill()
	os.RemoveAll(sess.dir)
	m.s.log.Info("引擎会话已关闭", "session", id, "engine", sess.Engine, "reason", reason, "duration_s", int64(m.s.now().Sub(sess.created).Seconds()))
	return nil
}

func (m *engineManager) closeAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.close(id, "守护进程停止")
	}
}

// kill 收掉 CLI 进程组。
func (s *session) kill() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	select {
	case <-s.exited:
		return
	default:
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(closeGrace):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-s.exited:
		case <-time.After(closeGrace):
		}
	}
}

// ---- 事件与指令 ----

func (s *session) push(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	e.Seq = s.seq
	e.At = time.Now().UTC().Format(time.RFC3339)
	s.events = append(s.events, e)
	if len(s.events) > eventKeep {
		s.events = s.events[len(s.events)-eventKeep:]
	}
	s.wakeLocked()
}

func (s *session) wakeLocked() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *session) setStatus(st string) {
	s.mu.Lock()
	s.status = st
	s.lastAct = time.Now()
	s.wakeLocked()
	s.mu.Unlock()
}

func (s *session) view() SessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionView{
		ID: s.ID, Engine: s.Engine, Model: s.Model, Effort: s.Effort, Workdir: s.Workdir, Binary: s.Binary, Status: s.status, Title: s.title,
		Partial: s.partial.String(), LastError: s.lastErr, InputTokens: s.usageIn, OutputTokens: s.usageOut, Seq: s.seq,
		CreatedAt: s.created.UTC().Format(time.RFC3339), LastActiveAt: s.lastAct.UTC().Format(time.RFC3339),
	}
}

// eventsAfter 取 seq 之后的事件；wait 为真且没有新事件时最多等 timeout。
func (s *session) eventsAfter(ctx context.Context, after int64, wait time.Duration) []Event {
	deadline := time.Now().Add(wait)
	for {
		s.mu.Lock()
		var out []Event
		for _, e := range s.events {
			if e.Seq > after {
				out = append(out, e)
			}
		}
		ch := s.notify
		s.mu.Unlock()
		if len(out) > 0 || wait <= 0 || time.Now().After(deadline) {
			if out == nil {
				out = []Event{}
			}
			return out
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return []Event{}
		case <-time.After(time.Until(deadline)):
		}
	}
}

// sessionSink 是驱动往会话里写事实的入口。
type sessionSink struct{ s *session }

func (k *sessionSink) Delta(text string) {
	k.s.mu.Lock()
	k.s.partial.WriteString(text)
	k.s.wakeLocked()
	k.s.mu.Unlock()
}

func (k *sessionSink) Message(text string) {
	k.s.mu.Lock()
	k.s.partial.Reset()
	k.s.mu.Unlock()
	k.s.push(Event{Type: "message", Text: text})
}

func (k *sessionSink) Reasoning(text string) { k.s.push(Event{Type: "reasoning", Text: text}) }
func (k *sessionSink) Activity(text string) {
	if text != "" {
		k.s.push(Event{Type: "activity", Text: text})
	}
}

func (k *sessionSink) Command(cmd, output string, code *int) {
	if len(output) > 8<<10 {
		output = output[len(output)-8<<10:]
	}
	k.s.push(Event{Type: "command", Text: cmd, Output: output, ExitCode: code})
}

func (k *sessionSink) File(path, action string) {
	k.s.push(Event{Type: "file", Text: action + " " + path})
}

func (k *sessionSink) Usage(in, out int64) {
	k.s.mu.Lock()
	k.s.usageIn += in
	k.s.usageOut += out
	k.s.mu.Unlock()
	k.s.push(Event{Type: "usage", InputTok: in, OutTok: out})
}

// runTurn 在后台执行一条指令。
func (m *engineManager) runTurn(sess *session, text string) error {
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return conflict("会话已关闭")
	}
	if sess.status != "idle" {
		sess.mu.Unlock()
		return &Error{Code: CodeSessionBusy, Msg: "会话上已有指令在执行"}
	}
	if sess.title == "" {
		sess.title = firstLine(text)
	}
	ctx, cancel := context.WithCancel(m.s.bg)
	sess.turnStop = cancel
	sess.status = "running"
	sess.lastAct = time.Now()
	sess.partial.Reset()
	sess.lastErr = ""
	sess.wakeLocked()
	sess.mu.Unlock()
	sess.push(Event{Type: "user", Text: text})
	m.s.wg.Add(1)
	go func() {
		defer m.s.wg.Done()
		defer cancel()
		started := time.Now()
		outcome, errMsg := sess.driver.Run(ctx, text, &sessionSink{s: sess})
		sess.mu.Lock()
		sess.turnStop = nil
		sess.lastErr = errMsg
		sess.partial.Reset()
		closed := sess.closed
		sess.mu.Unlock()
		sess.push(Event{Type: "done", Outcome: outcome, Text: errMsg})
		if !closed {
			sess.setStatus("idle")
		}
		m.s.log.Info("引擎指令结束", "session", sess.ID, "engine", sess.Engine, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())
	}()
	return nil
}

func (m *engineManager) interrupt(sess *session) error {
	sess.mu.Lock()
	running := sess.status == "running"
	sess.mu.Unlock()
	if !running {
		return &Error{Code: CodeTaskNotRunning, Msg: "没有正在执行的指令"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return sess.driver.Interrupt(ctx)
}

// ---- 管理面端点 ----

func (s *Server) handleEnginesInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engines.info())
}

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.engines.views(), "engines": s.engines.info().Engines})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var in sessionInput
	if err := decodeJSON(r, &in, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	sess, err := s.engines.create(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"session": sess.view()})
}

func (s *Server) sessionOf(w http.ResponseWriter, r *http.Request) *session {
	sess := s.engines.get(r.PathValue("id"))
	if sess == nil {
		writeErr(w, notFound("会话不存在"))
	}
	return sess
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	if sess := s.sessionOf(w, r); sess != nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": sess.view()})
	}
}

func (s *Server) handleSessionClose(w http.ResponseWriter, r *http.Request) {
	if err := s.engines.close(r.PathValue("id"), "管理员关闭"); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionTurn(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(w, r)
	if sess == nil {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &in, 1<<20); err != nil {
		writeErr(w, err)
		return
	}
	if strings.TrimSpace(in.Text) == "" {
		writeErr(w, bad("指令不能为空"))
		return
	}
	if err := s.engines.runTurn(sess, in.Text); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"session": sess.view()})
}

func (s *Server) handleSessionInterrupt(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(w, r)
	if sess == nil {
		return
	}
	if err := s.engines.interrupt(sess); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess.view()})
}

// handleSessionEvents 取 after 之后的事件；wait=1 时最多陪等 25 秒。
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(w, r)
	if sess == nil {
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	wait := time.Duration(0)
	if r.URL.Query().Get("wait") == "1" {
		wait = 25 * time.Second
	}
	events := sess.eventsAfter(r.Context(), after, wait)
	writeJSON(w, http.StatusOK, map[string]any{"session": sess.view(), "events": events})
}

// ---- 小工具 ----

// tailBuffer 只留最后 max 字节。
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func tailNote(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 4 {
		lines = lines[len(lines)-4:]
	}
	return "：" + strings.Join(lines, " ")
}

func writeInstanceFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
