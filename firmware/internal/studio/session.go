package studio

// 一个对话的会话：指令队列、正在执行的那条、引擎会话与流式读数。形态与纪律同 Agent远控
// （internal/hostagent/session.go）：一条做完才做下一条，正在执行的只能被中止；引擎会话在
// 第一条指令要执行时才起，空闲超过 IdleTimeout 或管理员结束会话时关掉；再起时开发者指令
// 附上本对话先前的记录。
//
// 锁纪律：s.mu 只保护内存状态，持锁期间不做 I/O。

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
	"github.com/llm-net/llm-gate/firmware/internal/nodeengine"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

type queued struct {
	run             store.StudioRun
	images          []string
	cancelRequested bool
}

type session struct {
	m      *Manager
	chatID string
	wsID   string

	mu        sync.Mutex
	status    string
	queue     []*queued
	current   *queued
	cancelRun context.CancelFunc
	engine    hostagent.EngineSession
	engineAt  time.Time
	token     string
	// engineMedia 是引擎会话最近一次被告知的「可用的生成模型」一节（media.go）。
	engineMedia string
	live        hostagent.Live
	revision    int64
	notify      chan struct{}
	wake        chan struct{}
	stopping    bool
	worker      bool
	idle        chan struct{}
}

func newSession(m *Manager, chatID, wsID string) *session {
	closed := make(chan struct{})
	close(closed)
	return &session{m: m, chatID: chatID, wsID: wsID, status: hostagent.StatusIdle, notify: make(chan struct{}), wake: make(chan struct{}, 1), idle: closed}
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
	st := State{ChatID: s.chatID, Revision: s.revision, Status: s.status, Queue: make([]store.StudioRun, 0, len(s.queue))}
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
func (s *session) appendEvent(ctx context.Context, ev store.StudioEvent) *store.StudioEvent {
	ev.WorkspaceID = s.wsID
	ev.ChatID = s.chatID
	if ev.At.IsZero() {
		ev.At = s.m.now()
	}
	out, err := s.m.st.AppendStudioEvent(ctx, ev)
	if err != nil {
		s.m.log.Error("写时间线事件失败", "workspace_id", s.wsID, "chat_id", s.chatID, "kind", ev.Kind, "error", err.Error())
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

func (s *session) submit(ctx context.Context, chat *store.StudioChat, in hostagent.Input, engineID string) (*store.StudioRun, error) {
	now := s.m.now()
	run, err := s.m.st.CreateStudioRun(ctx, store.NewStudioRun{WorkspaceID: s.wsID, ChatID: s.chatID, Text: in.Text, ImageCount: len(in.Images),
		Engine: engineID, Model: chat.Model, CreatedAt: now})
	if err != nil {
		return nil, err
	}
	if chat.Title == "" && in.Text != "" {
		if err := s.m.st.SetStudioChatTitle(ctx, s.chatID, autoTitle(in.Text), now); err != nil {
			s.m.log.Warn("写对话标题失败", "chat_id", s.chatID, "error", err.Error())
		}
	} else if err := s.m.st.TouchStudioChat(ctx, s.chatID, now); err != nil {
		s.m.log.Warn("刷新对话活动时刻失败", "chat_id", s.chatID, "error", err.Error())
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: run.ID, Kind: store.StudioEventUser, Body: in.Text, Meta: metaJSON(map[string]int{"images": len(in.Images)})})
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
	s.m.log.Info("创作指令已入队", "workspace_id", s.wsID, "chat_id", s.chatID, "run_id", run.ID, "queued", len(s.queue))
	return run, nil
}

const autoTitleRunes = 40

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
		s.status = hostagent.StatusIdle
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

// execute 跑一条指令：等设备级并发槽 → 确保引擎会话 → 执行 → 收尾。
func (s *session) execute(q *queued) {
	ctx, cancel := context.WithTimeout(context.Background(), RunTimeout)
	defer cancel()
	s.mu.Lock()
	s.current = q
	s.cancelRun = cancel
	s.live = hostagent.Live{RunID: q.run.ID}
	s.status = hostagent.StatusWaiting
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
		s.finish(q, hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil)
		return
	}
	if err := s.ensureEngine(ctx); err != nil {
		s.finish(q, hostagent.Outcome{}, err)
		return
	}
	started := s.m.now()
	run, err := s.m.st.SetStudioRunStatus(context.Background(), q.run.ID, store.AgentRunRunning, "", started)
	if err != nil {
		s.finish(q, hostagent.Outcome{}, err)
		return
	}
	text := s.withMediaUpdate(ctx, q)
	s.mu.Lock()
	q.run = *run
	s.status = hostagent.StatusRunning
	eng := s.engine
	s.touch()
	s.mu.Unlock()
	s.m.log.Info("创作指令开始执行", "workspace_id", s.wsID, "chat_id", s.chatID, "run_id", q.run.ID)
	outcome, err := eng.Run(ctx, hostagent.Input{Text: text, Images: q.images}, &runSink{s: s, q: q})
	s.m.log.Info("创作指令执行结束", "workspace_id", s.wsID, "chat_id", s.chatID, "run_id", q.run.ID, "outcome", outcome.Status,
		"duration_ms", s.m.now().Sub(started).Milliseconds())
	s.finish(q, outcome, err)
}

// withMediaUpdate 给出交给引擎的指令正文：工作空间的媒体生成能力（或密钥的可用性）自引擎上次
// 被告知以来变了，就把新的一节作为设备通知放在指令前面，并在时间线留一条 session 事件；没变
// 即原文。读配置失败不挡指令（生成工具自己按当前配置裁决）。
func (s *session) withMediaUpdate(ctx context.Context, q *queued) string {
	chat, err := s.m.st.GetStudioChat(ctx, s.chatID)
	if err != nil {
		return q.run.Text
	}
	media, err := s.m.mediaSection(ctx, s.wsID, chat.KeyID)
	if err != nil {
		s.m.log.Warn("读取工作空间的媒体生成能力失败", "workspace_id", s.wsID, "chat_id", s.chatID, "error", err.Error())
		return q.run.Text
	}
	s.mu.Lock()
	changed := media != s.engineMedia
	s.engineMedia = media
	s.mu.Unlock()
	if !changed {
		return q.run.Text
	}
	s.appendEvent(context.Background(), store.StudioEvent{RunID: q.run.ID, Kind: store.StudioEventSession, Title: sessionMediaUpdated,
		Body: "媒体生成能力有变化，已告知智能体"})
	return mediaUpdateInput(media, q.run.Text)
}

// ensureEngine 没有引擎会话时起一段。
func (s *session) ensureEngine(ctx context.Context) error {
	s.mu.Lock()
	if s.engine != nil {
		s.mu.Unlock()
		return nil
	}
	s.status = hostagent.StatusStarting
	s.touch()
	s.mu.Unlock()

	chat, err := s.m.st.GetStudioChat(ctx, s.chatID)
	if err != nil {
		return err
	}
	prior, err := s.m.st.ListStudioEvents(ctx, store.StudioEventQuery{ChatID: s.chatID, Limit: transcriptEvents,
		Kinds: []string{store.StudioEventUser, store.StudioEventAssistant}})
	if err != nil {
		return err
	}
	if cur := s.currentRunID(); cur != "" {
		for i, ev := range prior {
			if ev.RunID == cur {
				prior = prior[:i]
				break
			}
		}
	}
	ws, err := s.m.st.GetWorkspace(ctx, s.wsID)
	if err != nil {
		return err
	}
	engine, binary, nodeTools, err := s.m.chatEngine(ctx, ws, chat.Engine)
	if err != nil {
		return err
	}
	media, err := s.m.mediaSection(ctx, s.wsID, chat.KeyID)
	if err != nil {
		return err
	}
	token := newToken()
	s.m.mu.Lock()
	s.m.tokens[token] = s
	s.m.mu.Unlock()
	eng, err := engine.Start(ctx, nodeengine.Request{
		HostID: ws.HostID, Binary: binary, Workdir: ws.Path, Chat: chatConfigOf(chat),
		Instructions: chat.Instructions + nodeToolsSection(nodeTools) + media + buildTranscript(prior),
		Tools:        hostagent.ToolEndpoint{URL: s.m.tool, Token: token, Server: ToolServer},
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
	s.engineMedia = media
	s.touch()
	s.mu.Unlock()
	s.appendEvent(context.Background(), store.StudioEvent{Kind: store.StudioEventSession, Title: "started",
		Body: engine.Label(), Meta: metaJSON(map[string]string{"engine": engine.ID(), "model": chat.Model, "effort": chat.Effort, "key": chat.KeyDisplay, "binary": binary})})
	s.m.log.Info("引擎会话已启动", "workspace_id", s.wsID, "chat_id", s.chatID, "engine", engine.ID())
	return nil
}

// finish 收尾一条指令：写终态、留事件、清读数；引擎已死则连会话一起关掉。
func (s *session) finish(q *queued, outcome hostagent.Outcome, err error) {
	s.mu.Lock()
	cancelled := q.cancelRequested
	s.mu.Unlock()
	status, reason := store.AgentRunSucceeded, ""
	switch {
	case cancelled || (err == nil && outcome.Status == hostagent.OutcomeInterrupted):
		status, reason = store.AgentRunCancelled, "已中止"
	case err != nil:
		status, reason = store.AgentRunFailed, err.Error()
	case outcome.Status == hostagent.OutcomeFailed:
		status, reason = store.AgentRunFailed, outcome.Error
		if reason == "" {
			reason = "引擎报告执行失败"
		}
	}
	ctx := context.Background()
	run, uerr := s.m.st.SetStudioRunStatus(ctx, q.run.ID, status, reason, s.m.now())
	if uerr != nil {
		s.m.log.Error("写回指令终态失败", "run_id", q.run.ID, "error", uerr.Error())
	} else {
		q.run = *run
	}
	if status != store.AgentRunSucceeded {
		s.appendEvent(ctx, store.StudioEvent{RunID: q.run.ID, Kind: store.StudioEventRun, Title: status, Body: reason})
	}
	s.mu.Lock()
	s.current = nil
	s.cancelRun = nil
	s.live = hostagent.Live{}
	s.status = hostagent.StatusIdle
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

// closeEngine 关掉引擎会话（幂等）。只有工作协程调它。
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
	if eng != nil {
		if err := eng.Close(); err != nil {
			s.m.log.Warn("关闭引擎会话时出错", "workspace_id", s.wsID, "chat_id", s.chatID, "error", err.Error())
		}
		s.appendEvent(context.Background(), store.StudioEvent{Kind: store.StudioEventSession, Title: "stopped", Body: reason})
		s.m.log.Info("引擎会话已关闭", "workspace_id", s.wsID, "chat_id", s.chatID)
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
		running := s.status == hostagent.StatusRunning
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
	run, err := s.m.st.GetStudioRun(ctx, runID)
	if err != nil || run.ChatID != s.chatID {
		return &Error{Code: CodeRunNotFound, Msg: "指令不存在"}
	}
	return &Error{Code: CodeRunFinished, Msg: "这条指令已经结束"}
}

func (s *session) markCancelled(ctx context.Context, q *queued, reason string) {
	if _, err := s.m.st.SetStudioRunStatus(ctx, q.run.ID, store.AgentRunCancelled, reason, s.m.now()); err != nil {
		s.m.log.Error("写回取消状态失败", "run_id", q.run.ID, "error", err.Error())
	}
	s.appendEvent(ctx, store.StudioEvent{RunID: q.run.ID, Kind: store.StudioEventRun, Title: store.AgentRunCancelled, Body: reason})
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
	running := s.status == hostagent.StatusRunning
	s.status = hostagent.StatusStopping
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
	k.s.appendEvent(context.Background(), store.StudioEvent{RunID: k.q.run.ID, Kind: store.StudioEventAssistant, Body: text})
}

func (k *runSink) Reasoning(text string) {
	if text == "" {
		return
	}
	k.s.appendEvent(context.Background(), store.StudioEvent{RunID: k.q.run.ID, Kind: store.StudioEventReasoning, Body: text})
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

var _ hostagent.ExecSink = (*runSink)(nil)

// Command 把引擎在节点上跑完的一条命令落成 command 事件（输出只留尾段）。
func (k *runSink) Command(command, output string, exitCode *int, dur time.Duration) {
	meta := map[string]any{"truncated": len(output) > storeOutputLimit}
	if exitCode != nil {
		meta["exit_code"] = *exitCode
	}
	k.s.appendEvent(context.Background(), store.StudioEvent{RunID: k.q.run.ID, Kind: store.StudioEventCommand, Title: command,
		Body: tail(output, storeOutputLimit), DurationMs: dur.Milliseconds(), Meta: metaJSON(meta)})
}

// FileChange 把引擎在节点上改动的文件落成 file 事件：工作空间里的文件记「子目录/文件名」的路径
// （时间线能点开素材库里的那个文件），工作空间外的记原样路径。
func (k *runSink) FileChange(path, action string) {
	name := path
	if ws, err := k.s.m.st.GetWorkspace(context.Background(), k.s.wsID); err == nil {
		name = workspaceRelative(ws.Path, path)
	}
	k.s.appendEvent(context.Background(), store.StudioEvent{RunID: k.q.run.ID, Kind: store.StudioEventFile, Title: name,
		Meta: metaJSON(map[string]string{"action": action, "source": "engine"})})
}

// ToolUse 把引擎的只读工具（读文件、查找）落成 tool 事件。
func (k *runSink) ToolUse(name, target string) {
	if ws, err := k.s.m.st.GetWorkspace(context.Background(), k.s.wsID); err == nil {
		target = workspaceRelative(ws.Path, target)
	}
	k.s.appendEvent(context.Background(), store.StudioEvent{RunID: k.q.run.ID, Kind: store.StudioEventTool, Title: name, Body: target})
}

// workspaceRelative 把工作空间目录里的绝对路径折成相对路径，别处的原样返回。
func workspaceRelative(root, path string) string {
	root = strings.TrimRight(root, "/")
	if rel, ok := strings.CutPrefix(path, root+"/"); ok && root != "" {
		return rel
	}
	return path
}

func (k *runSink) Usage(input, output int64) {
	if err := k.s.m.st.SetStudioRunUsage(context.Background(), k.q.run.ID, input, output); err != nil {
		k.s.m.log.Error("写回指令用量失败", "run_id", k.q.run.ID, "error", err.Error())
	}
	k.s.mu.Lock()
	if k.s.current == k.q {
		k.q.run.InputTokens, k.q.run.OutputTokens = input, output
		k.s.touch()
	}
	k.s.mu.Unlock()
}
