package hostagent

// 一个对话的会话：指令队列、正在执行的那条、引擎会话与流式读数。
//
// 队列纪律：**一条做完才做下一条**，中途不插队；正在执行的那条只能被中止。引擎会话
// 在第一条指令要执行时才起，之后同一会话里连续执行（上下文共享），队列空闲超过
// IdleTimeout 或管理员按「结束会话」时关掉——下一条指令会起新的一段（时间线上有
// session 事件标出边界），开发者指令附上本对话先前的记录让模型接得上前文。
//
// 锁纪律：s.mu 只保护内存状态，持锁期间不做 I/O（引擎调用、数据库、SSH 都在锁外）。

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// queued 是队列里的一条指令：行 + 只在内存里的图片。
type queued struct {
	run    store.AgentHostRun
	images []string
	// cancelRequested 由 Cancel / Stop 置位：这条指令收尾时按取消记，而不是按引擎回报的结局。
	cancelRequested bool
}

type session struct {
	m      *Manager
	chatID string
	hostID int64

	mu        sync.Mutex
	status    string
	queue     []*queued
	current   *queued
	cancelRun context.CancelFunc
	engine    EngineSession
	engineAt  time.Time
	token     string
	live      Live
	revision  int64
	notify    chan struct{}
	wake      chan struct{}
	stopping  bool
	worker    bool
	idle      chan struct{}

	// connMu 串行化对主机的工具执行；conn 是懒建的 SSH 连接（tools.go）。
	connMu sync.Mutex
	conn   *agenthost.Conn
}

func newSession(m *Manager, chatID string, hostID int64) *session {
	closed := make(chan struct{})
	close(closed)
	return &session{m: m, chatID: chatID, hostID: hostID, status: StatusIdle, notify: make(chan struct{}), wake: make(chan struct{}, 1), idle: closed}
}

// touch 递增 revision 并唤醒陪等者；调用方须持 s.mu。
func (s *session) touch() {
	s.revision++
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *session) notifyCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

func (s *session) snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{ChatID: s.chatID, Revision: s.revision, Status: s.status, Queue: make([]store.AgentHostRun, 0, len(s.queue))}
	for _, q := range s.queue {
		st.Queue = append(st.Queue, q.run)
	}
	if s.current != nil {
		run := s.current.run
		st.Current = &run
		live := s.live
		st.Live = &live
	}
	if s.engine != nil {
		at := s.engineAt
		st.EngineStartedAt = &at
	}
	return st
}

// appendEvent 追加一条本对话的事件并刷新读数。
func (s *session) appendEvent(ctx context.Context, ev store.AgentHostEvent) *store.AgentHostEvent {
	ev.HostID = s.hostID
	ev.ChatID = s.chatID
	if ev.At.IsZero() {
		ev.At = s.m.now()
	}
	out, err := s.m.st.AppendAgentHostEvent(ctx, ev)
	if err != nil {
		s.m.log.Error("写时间线事件失败", "host_id", s.hostID, "chat_id", s.chatID, "kind", ev.Kind, "error", err.Error())
		return nil
	}
	s.mu.Lock()
	s.touch()
	s.mu.Unlock()
	return out
}

// currentRunID 是正在执行的指令 id（工具事件挂它）。
func (s *session) currentRunID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return ""
	}
	return s.current.run.ID
}

func (s *session) submit(ctx context.Context, chat *store.AgentHostChat, in Input, engineID string) (*store.AgentHostRun, error) {
	now := s.m.now()
	run, err := s.m.st.CreateAgentHostRun(ctx, store.NewAgentHostRun{HostID: s.hostID, ChatID: s.chatID, Text: in.Text, ImageCount: len(in.Images),
		Engine: engineID, Model: chat.Model, CreatedAt: now})
	if err != nil {
		return nil, err
	}
	// 没起标题的对话拿第一条指令的首行当标题；有标题只刷新活动时刻。
	if chat.Title == "" && in.Text != "" {
		if err := s.m.st.SetAgentHostChatTitle(ctx, s.chatID, autoTitle(in.Text), now); err != nil {
			s.m.log.Warn("写对话标题失败", "chat_id", s.chatID, "error", err.Error())
		}
	} else if err := s.m.st.TouchAgentHostChat(ctx, s.chatID, now); err != nil {
		s.m.log.Warn("刷新对话活动时刻失败", "chat_id", s.chatID, "error", err.Error())
	}
	s.appendEvent(ctx, store.AgentHostEvent{RunID: run.ID, Kind: store.AgentEventUser, Body: in.Text, Meta: metaJSON(map[string]int{"images": len(in.Images)})})
	s.mu.Lock()
	s.queue = append(s.queue, &queued{run: *run, images: in.Images})
	if !s.worker {
		s.worker = true
		s.idle = make(chan struct{})
		go s.loop()
	}
	s.touch()
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.m.log.Info("主机指令已入队", "host_id", s.hostID, "chat_id", s.chatID, "run_id", run.ID, "queued", len(s.queue))
	return run, nil
}

// autoTitleRunes 是自动标题的长度上限。
const autoTitleRunes = 40

// autoTitle 取指令首行、截到 autoTitleRunes 个字符。
func autoTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if r := []rune(line); len(r) > autoTitleRunes {
		return string(r[:autoTitleRunes]) + "…"
	}
	return line
}

// loop 是会话工作协程：逐条执行队列，空闲超时或被停止时关引擎退出。
func (s *session) loop() {
	defer func() {
		s.mu.Lock()
		s.worker = false
		s.status = StatusIdle
		s.touch()
		close(s.idle)
		s.mu.Unlock()
	}()
	for {
		q, stopping := s.next()
		if q != nil {
			s.execute(q)
			continue
		}
		if stopping {
			s.closeEngine("会话已由管理员结束")
			s.mu.Lock()
			s.stopping = false
			s.mu.Unlock()
			return
		}
		select {
		case <-s.wake:
		case <-time.After(s.m.idle):
			s.mu.Lock()
			empty := len(s.queue) == 0 && !s.stopping
			s.mu.Unlock()
			if empty {
				s.closeEngine("空闲超时，引擎会话已关闭")
				return
			}
		}
	}
}

// next 取下一条指令；stopping 报告是否收到了停止请求。
func (s *session) next() (*queued, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return nil, true
	}
	if len(s.queue) == 0 {
		return nil, false
	}
	q := s.queue[0]
	s.queue = s.queue[1:]
	return q, false
}

func (s *session) setStatus(status string) {
	s.mu.Lock()
	s.status = status
	s.touch()
	s.mu.Unlock()
}

// execute 跑一条指令：等设备级并发槽 → 确保引擎会话 → 执行 → 收尾。
func (s *session) execute(q *queued) {
	ctx, cancel := context.WithTimeout(context.Background(), RunTimeout)
	defer cancel()
	s.mu.Lock()
	s.current = q
	s.cancelRun = cancel
	s.live = Live{RunID: q.run.ID}
	s.status = StatusWaiting
	s.touch()
	s.mu.Unlock()

	acquired := false
	select {
	case s.m.sem <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	defer func() {
		if acquired {
			<-s.m.sem
		}
	}()
	if !acquired {
		s.finish(q, Outcome{Status: OutcomeInterrupted}, nil)
		return
	}
	if err := s.ensureEngine(ctx); err != nil {
		s.finish(q, Outcome{}, err)
		return
	}
	started := s.m.now()
	run, err := s.m.st.SetAgentHostRunStatus(context.Background(), q.run.ID, store.AgentRunRunning, "", started)
	if err != nil {
		s.finish(q, Outcome{}, err)
		return
	}
	s.mu.Lock()
	q.run = *run
	s.status = StatusRunning
	eng := s.engine
	s.touch()
	s.mu.Unlock()
	s.m.log.Info("主机指令开始执行", "host_id", s.hostID, "chat_id", s.chatID, "run_id", q.run.ID)
	outcome, err := eng.Run(ctx, Input{Text: q.run.Text, Images: q.images}, &runSink{s: s, q: q})
	s.m.log.Info("主机指令执行结束", "host_id", s.hostID, "chat_id", s.chatID, "run_id", q.run.ID, "outcome", outcome.Status,
		"duration_ms", s.m.now().Sub(started).Milliseconds())
	s.finish(q, outcome, err)
}

// ensureEngine 没有引擎会话时起一段。
func (s *session) ensureEngine(ctx context.Context) error {
	s.mu.Lock()
	if s.engine != nil {
		s.mu.Unlock()
		return nil
	}
	s.status = StatusStarting
	s.touch()
	s.mu.Unlock()

	host, err := s.m.st.GetAgentHost(ctx, s.hostID)
	if err != nil {
		return err
	}
	chat, err := s.m.st.GetAgentHostChat(ctx, s.chatID)
	if err != nil {
		return err
	}
	// 本对话先前的指令与回复（引擎中途重启时接续前文用）；第一段会话时还没有。
	prior, err := s.m.st.ListAgentHostEvents(ctx, store.AgentHostEventQuery{HostID: s.hostID, ChatID: s.chatID, Limit: transcriptEvents,
		Kinds: []string{store.AgentEventUser, store.AgentEventAssistant}})
	if err != nil {
		return err
	}
	// 只算正在执行的这条之前的：它自己与排在它后面的 user 事件都不是「先前」。
	if cur := s.currentRunID(); cur != "" {
		for i, ev := range prior {
			if ev.RunID == cur {
				prior = prior[:i]
				break
			}
		}
	}
	status, err := s.m.engineReady(ctx)
	if err != nil {
		return err
	}
	token := newToken()
	s.m.mu.Lock()
	s.m.tokens[token] = s
	s.m.mu.Unlock()
	eng, err := s.m.engine.Start(ctx, StartRequest{
		Host:         *host,
		Chat:         *chat,
		Instructions: chat.Instructions + buildTranscript(prior),
		Tools:        ToolEndpoint{URL: s.m.toolURL(), Token: token},
	})
	if err != nil {
		s.m.mu.Lock()
		delete(s.m.tokens, token)
		s.m.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.engine = eng
	s.engineAt = s.m.now()
	s.token = token
	s.touch()
	s.mu.Unlock()
	s.appendEvent(context.Background(), store.AgentHostEvent{Kind: store.AgentEventSession, Title: "started",
		Body: status.Label, Meta: metaJSON(map[string]string{"engine": status.ID, "model": chat.Model, "effort": chat.Effort, "key": chat.KeyDisplay})})
	s.m.log.Info("引擎会话已启动", "host_id", s.hostID, "chat_id", s.chatID, "engine", status.ID)
	return nil
}

// finish 收尾一条指令：写终态、留事件、清读数；引擎已死则连会话一起关掉。
func (s *session) finish(q *queued, outcome Outcome, err error) {
	s.mu.Lock()
	cancelled := q.cancelRequested
	s.mu.Unlock()
	status, reason := store.AgentRunSucceeded, ""
	switch {
	case cancelled || (err == nil && outcome.Status == OutcomeInterrupted):
		status, reason = store.AgentRunCancelled, "已中止"
	case err != nil:
		status, reason = store.AgentRunFailed, err.Error()
	case outcome.Status == OutcomeFailed:
		status, reason = store.AgentRunFailed, outcome.Error
		if reason == "" {
			reason = "引擎报告执行失败"
		}
	}
	ctx := context.Background()
	run, uerr := s.m.st.SetAgentHostRunStatus(ctx, q.run.ID, status, reason, s.m.now())
	if uerr != nil {
		s.m.log.Error("写回指令终态失败", "run_id", q.run.ID, "error", uerr.Error())
	} else {
		q.run = *run
	}
	if status != store.AgentRunSucceeded {
		s.appendEvent(ctx, store.AgentHostEvent{RunID: q.run.ID, Kind: store.AgentEventRun, Title: status, Body: reason})
	}
	s.mu.Lock()
	s.current = nil
	s.cancelRun = nil
	s.live = Live{}
	s.status = StatusIdle
	eng := s.engine
	s.touch()
	s.mu.Unlock()
	if eng != nil {
		select {
		case <-eng.Done():
			s.closeEngine("引擎进程已退出")
		default:
		}
	}
}

// closeEngine 关掉引擎会话与主机连接（幂等）。只有工作协程调它。读数里的「没有会话」
// 在 stopped 事件落库之后才出现：陪等者看到会话没了时，时间线上已经有那条边界事件。
func (s *session) closeEngine(reason string) {
	s.mu.Lock()
	eng := s.engine
	token := s.token
	s.token = ""
	s.mu.Unlock()
	if token != "" {
		s.m.mu.Lock()
		delete(s.m.tokens, token)
		s.m.mu.Unlock()
	}
	s.connMu.Lock()
	s.dropConn()
	s.connMu.Unlock()
	if eng != nil {
		if err := eng.Close(); err != nil {
			s.m.log.Warn("关闭引擎会话时出错", "host_id", s.hostID, "chat_id", s.chatID, "error", err.Error())
		}
		s.appendEvent(context.Background(), store.AgentHostEvent{Kind: store.AgentEventSession, Title: "stopped", Body: reason})
		s.m.log.Info("引擎会话已关闭", "host_id", s.hostID, "chat_id", s.chatID)
	}
	s.mu.Lock()
	s.engine = nil
	s.touch()
	s.mu.Unlock()
}

// cancel 取消一条指令。
func (s *session) cancel(ctx context.Context, runID string) error {
	s.mu.Lock()
	for i, q := range s.queue {
		if q.run.ID == runID {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.touch()
			s.mu.Unlock()
			s.markCancelled(ctx, q, "管理员取消")
			return nil
		}
	}
	if s.current != nil && s.current.run.ID == runID {
		s.current.cancelRequested = true
		eng := s.engine
		cancelRun := s.cancelRun
		running := s.status == StatusRunning
		s.touch()
		s.mu.Unlock()
		if running && eng != nil {
			if err := eng.Interrupt(ctx); err == nil {
				return nil
			}
		}
		if cancelRun != nil {
			cancelRun()
		}
		return nil
	}
	s.mu.Unlock()
	run, err := s.m.st.GetAgentHostRun(ctx, runID)
	if err != nil || run.HostID != s.hostID {
		return &Error{Code: CodeRunNotFound, Msg: "指令不存在"}
	}
	return &Error{Code: CodeRunFinished, Msg: "这条指令已经结束"}
}

func (s *session) markCancelled(ctx context.Context, q *queued, reason string) {
	if _, err := s.m.st.SetAgentHostRunStatus(ctx, q.run.ID, store.AgentRunCancelled, reason, s.m.now()); err != nil {
		s.m.log.Error("写回取消状态失败", "run_id", q.run.ID, "error", err.Error())
	}
	s.appendEvent(ctx, store.AgentHostEvent{RunID: q.run.ID, Kind: store.AgentEventRun, Title: store.AgentRunCancelled, Body: reason})
}

// stop 结束会话：清空队列、中止当前指令、让工作协程关掉引擎。
func (s *session) stop(ctx context.Context) error {
	s.mu.Lock()
	queuedRuns := s.queue
	s.queue = nil
	if !s.worker {
		s.touch()
		s.mu.Unlock()
		for _, q := range queuedRuns {
			s.markCancelled(ctx, q, "会话已结束")
		}
		return nil
	}
	s.stopping = true
	current := s.current
	if current != nil {
		current.cancelRequested = true
	}
	eng := s.engine
	cancelRun := s.cancelRun
	running := s.status == StatusRunning
	s.status = StatusStopping
	s.touch()
	s.mu.Unlock()
	for _, q := range queuedRuns {
		s.markCancelled(ctx, q, "会话已结束")
	}
	if current != nil {
		interrupted := false
		if running && eng != nil {
			interrupted = eng.Interrupt(ctx) == nil
		}
		if !interrupted && cancelRun != nil {
			cancelRun()
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// waitIdle 等工作协程退出（引擎已关）。
func (s *session) waitIdle(timeout time.Duration) {
	s.mu.Lock()
	ch := s.idle
	s.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

// runSink 把引擎的流式事实落进会话读数与时间线。
type runSink struct {
	s *session
	q *queued
}

func (k *runSink) Delta(text string) {
	k.s.mu.Lock()
	if k.s.current == k.q {
		k.s.live.Text += text
		k.s.touch()
	}
	k.s.mu.Unlock()
}

func (k *runSink) Message(text string) {
	k.s.mu.Lock()
	if k.s.current == k.q {
		k.s.live.Text = ""
	}
	k.s.mu.Unlock()
	k.s.appendEvent(context.Background(), store.AgentHostEvent{RunID: k.q.run.ID, Kind: store.AgentEventAssistant, Body: text})
}

func (k *runSink) Reasoning(text string) {
	if text == "" {
		return
	}
	k.s.appendEvent(context.Background(), store.AgentHostEvent{RunID: k.q.run.ID, Kind: store.AgentEventReasoning, Body: text})
}

func (k *runSink) Activity(label string) { k.s.setActivity(k.q, label) }

func (s *session) setActivity(q *queued, label string) {
	s.mu.Lock()
	if q == nil || s.current == q {
		s.live.Activity = label
		s.touch()
	}
	s.mu.Unlock()
}

func (k *runSink) Usage(input, output int64) {
	if err := k.s.m.st.SetAgentHostRunUsage(context.Background(), k.q.run.ID, input, output); err != nil {
		k.s.m.log.Error("写回指令用量失败", "run_id", k.q.run.ID, "error", err.Error())
	}
	k.s.mu.Lock()
	if k.s.current == k.q {
		k.q.run.InputTokens, k.q.run.OutputTokens = input, output
		k.s.touch()
	}
	k.s.mu.Unlock()
}
