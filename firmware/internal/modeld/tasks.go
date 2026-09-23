package modeld

// 任务队列与调度：任务按优先级（大者先）与入队先后排队；调度器为每个排队任务挑一台算力服务器
// （pickBackendLocked），派发后陪等到终态。派发那一跳失败（连不上、非 2xx）换一台重试，最多
// maxAttempts 次；算力服务器回报失败的任务不重试（那是任务自己的事）。
//
// 任务只在 tasks.json 里（整份原子写）；进程重启后带算力服务器任务 id 的 running 任务接着查询，
// 没有 id 的重新排队。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 任务状态。
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

// pollEvery 是陪等已派发任务时查询算力服务器的间隔（测试改短）。
var pollEvery = 2 * time.Second

const (
	maxAttempts = 3
	// taskKeep 是保留的已结束任务条数。
	taskKeep = 500
	// defaultTaskTimeout 是一个任务从派发到终态的上限。
	defaultTaskTimeout = 2 * time.Hour
	maxTaskInput       = 8 << 20
	maxTaskOutput      = 8 << 20
)

// Task 是一个推理任务。
type Task struct {
	ID       string          `json:"id"`
	Seq      int64           `json:"seq"`
	Model    string          `json:"model"`
	Input    json.RawMessage `json:"input"`
	Priority int             `json:"priority"`
	Status   string          `json:"status"`
	// Source 是提交来源（admin / api）；TokenID 是经对外 API 提交时用的令牌。
	Source  string `json:"source"`
	TokenID string `json:"token_id,omitempty"`
	// TimeoutSec 是派发后的上限（0 = 缺省）。
	TimeoutSec    int             `json:"timeout_sec,omitempty"`
	BackendID     string          `json:"backend_id,omitempty"`
	BackendName   string          `json:"backend_name,omitempty"`
	BackendTaskID string          `json:"backend_task_id,omitempty"`
	Attempts      int             `json:"attempts"`
	Output        json.RawMessage `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	StartedAt     time.Time       `json:"started_at,omitzero"`
	FinishedAt    time.Time       `json:"finished_at,omitzero"`

	cancel context.CancelFunc `json:"-"`
}

func (t *Task) terminal() bool {
	return t.Status == TaskSucceeded || t.Status == TaskFailed || t.Status == TaskCancelled
}

// TaskView 是给界面 / 对外 API 的读数。
type TaskView struct {
	ID            string          `json:"id"`
	Model         string          `json:"model"`
	Input         json.RawMessage `json:"input,omitempty"`
	Priority      int             `json:"priority"`
	Status        string          `json:"status"`
	Source        string          `json:"source"`
	Position      int             `json:"position,omitempty"`
	BackendID     string          `json:"backend_id,omitempty"`
	BackendName   string          `json:"backend_name,omitempty"`
	BackendTaskID string          `json:"backend_task_id,omitempty"`
	Attempts      int             `json:"attempts"`
	Output        json.RawMessage `json:"output,omitempty"`
	Error         string          `json:"error,omitempty"`
	CreatedAt     string          `json:"created_at"`
	StartedAt     string          `json:"started_at,omitempty"`
	FinishedAt    string          `json:"finished_at,omitempty"`
}

func (t *Task) view(position int, withInput bool) TaskView {
	v := TaskView{
		ID: t.ID, Model: t.Model, Priority: t.Priority, Status: t.Status, Source: t.Source, Position: position,
		BackendID: t.BackendID, BackendName: t.BackendName, BackendTaskID: t.BackendTaskID, Attempts: t.Attempts,
		Output: t.Output, Error: t.Error, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
	}
	if withInput {
		v.Input = t.Input
	}
	if !t.StartedAt.IsZero() {
		v.StartedAt = t.StartedAt.UTC().Format(time.RFC3339)
	}
	if !t.FinishedAt.IsZero() {
		v.FinishedAt = t.FinishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// QueueStats 是队列一屏读数。
type QueueStats struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
	// Capacity / Busy 是全部启用且在线的算力服务器的槽位总数与占用数。
	Capacity int `json:"capacity"`
	Busy     int `json:"busy"`
}

func (s *Server) queueStats() QueueStats {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	var q QueueStats
	for _, t := range s.st.tasks {
		switch t.Status {
		case TaskQueued:
			q.Queued++
		case TaskRunning:
			q.Running++
		case TaskSucceeded:
			q.Succeeded++
		case TaskFailed:
			q.Failed++
		case TaskCancelled:
			q.Cancelled++
		}
	}
	for _, b := range s.st.backends {
		if b.Enabled && b.rt.status != "error" {
			q.Capacity += b.Slots
			q.Busy += b.rt.active
		}
	}
	return q
}

// taskInput 是提交任务的入参。
type taskInput struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Priority   int             `json:"priority"`
	TimeoutSec int             `json:"timeout_sec"`
}

func (s *Server) submitTask(in taskInput, source, tokenID string) (*Task, error) {
	in.Model = strings.TrimSpace(in.Model)
	if in.Model == "" {
		return nil, bad("须指定模型（model）")
	}
	if len(in.Input) == 0 {
		in.Input = json.RawMessage("{}")
	}
	if !json.Valid(in.Input) {
		return nil, bad("input 不是合法 JSON")
	}
	if in.Priority < -100 || in.Priority > 100 {
		return nil, bad("priority 须在 -100 到 100 之间")
	}
	if in.TimeoutSec < 0 || in.TimeoutSec > 24*3600 {
		return nil, bad("timeout_sec 须在 0 到 86400 之间")
	}
	// 有没有任何一台启用的算力服务器接这个模型：没有就直接拒绝，不让任务永远排着。
	s.st.mu.Lock()
	served := false
	for _, b := range s.st.backends {
		if b.Enabled && b.supports(in.Model) {
			served = true
			break
		}
	}
	if !served {
		s.st.mu.Unlock()
		return nil, &Error{Code: CodeNoBackend, Msg: "没有任何启用的算力服务器承载模型 " + in.Model}
	}
	s.st.seq++
	t := &Task{ID: newID("task_"), Seq: s.st.seq, Model: in.Model, Input: in.Input, Priority: in.Priority, Status: TaskQueued,
		Source: source, TokenID: tokenID, TimeoutSec: in.TimeoutSec, CreatedAt: s.now()}
	s.st.tasks = append(s.st.tasks, t)
	s.trimTasksLocked()
	err := s.st.saveTasksLocked()
	s.st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.log.Info("任务入队", "task", t.ID, "model", t.Model, "source", source)
	s.kick()
	return t, nil
}

// trimTasksLocked 把已结束的任务按时间保留最近 taskKeep 条。
func (s *Server) trimTasksLocked() {
	finished := 0
	for _, t := range s.st.tasks {
		if t.terminal() {
			finished++
		}
	}
	if finished <= taskKeep {
		return
	}
	drop := finished - taskKeep
	kept := s.st.tasks[:0]
	for _, t := range s.st.tasks {
		if t.terminal() && drop > 0 {
			drop--
			continue
		}
		kept = append(kept, t)
	}
	s.st.tasks = kept
}

func (s *Server) findTaskLocked(id string) *Task {
	for _, t := range s.st.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// queuedOrderLocked 是排队任务的调度顺序。
func (s *Server) queuedOrderLocked() []*Task {
	var q []*Task
	for _, t := range s.st.tasks {
		if t.Status == TaskQueued {
			q = append(q, t)
		}
	}
	sort.SliceStable(q, func(i, j int) bool {
		if q[i].Priority != q[j].Priority {
			return q[i].Priority > q[j].Priority
		}
		return q[i].Seq < q[j].Seq
	})
	return q
}

// taskViews 列任务：按 status 过滤、按 token 过滤（对外 API 只看自己的）、最新在前、最多 limit 条。
func (s *Server) taskViews(status, tokenID string, limit int, withInput bool) []TaskView {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	pos := map[string]int{}
	for i, t := range s.queuedOrderLocked() {
		pos[t.ID] = i + 1
	}
	out := make([]TaskView, 0, len(s.st.tasks))
	for i := len(s.st.tasks) - 1; i >= 0; i-- {
		t := s.st.tasks[i]
		if status != "" && t.Status != status {
			continue
		}
		if tokenID != "" && t.TokenID != tokenID {
			continue
		}
		out = append(out, t.view(pos[t.ID], withInput))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Server) taskView(id, tokenID string) (TaskView, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	t := s.findTaskLocked(id)
	if t == nil || (tokenID != "" && t.TokenID != tokenID) {
		return TaskView{}, notFound("任务不存在")
	}
	pos := 0
	for i, q := range s.queuedOrderLocked() {
		if q.ID == id {
			pos = i + 1
		}
	}
	return t.view(pos, true), nil
}

// cancelTask 取消一个任务：排队中的直接结束；执行中的先请算力服务器取消，再结束。
func (s *Server) cancelTask(ctx context.Context, id, tokenID string) (TaskView, error) {
	s.st.mu.Lock()
	t := s.findTaskLocked(id)
	if t == nil || (tokenID != "" && t.TokenID != tokenID) {
		s.st.mu.Unlock()
		return TaskView{}, notFound("任务不存在")
	}
	if t.terminal() {
		s.st.mu.Unlock()
		return TaskView{}, &Error{Code: CodeTaskNotRunning, Msg: "任务已经结束"}
	}
	if t.Status == TaskQueued {
		s.finishLocked(t, TaskCancelled, nil, "")
		_ = s.st.saveTasksLocked()
		s.st.mu.Unlock()
		return s.taskView(id, "")
	}
	cancel := t.cancel
	s.st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// 等执行 goroutine 把状态落成终态（它会去算力服务器取消）。
	deadline := time.Now().Add(15 * time.Second)
	for {
		v, err := s.taskView(id, "")
		if err != nil || v.Status != TaskRunning || time.Now().After(deadline) {
			return v, err
		}
		select {
		case <-ctx.Done():
			return v, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// finishLocked 把任务落成终态并更新算力服务器计数。调用方持锁。
func (s *Server) finishLocked(t *Task, status string, output json.RawMessage, errMsg string) {
	if t.terminal() {
		return
	}
	if t.Status == TaskRunning && t.BackendID != "" {
		if b := s.findBackendLocked(t.BackendID); b != nil {
			if b.rt.active > 0 {
				b.rt.active--
			}
			if status == TaskSucceeded {
				b.rt.completed++
			} else if status == TaskFailed {
				b.rt.failed++
			}
		}
	}
	t.Status, t.Output, t.Error, t.FinishedAt, t.cancel = status, output, errMsg, s.now(), nil
	s.log.Info("任务结束", "task", t.ID, "status", status, "backend", t.BackendID, "attempts", t.Attempts)
}

// ---- 调度 ----

// kick 叫醒调度器。
func (s *Server) kick() {
	select {
	case s.kickCh <- struct{}{}:
	default:
	}
}

func (s *Server) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		s.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.kickCh:
		case <-t.C:
		}
	}
}

// dispatch 把能派的排队任务派出去。
func (s *Server) dispatch(ctx context.Context) {
	for {
		s.st.mu.Lock()
		var picked *Task
		var backend *Backend
		for _, t := range s.queuedOrderLocked() {
			if b := s.pickBackendLocked(t.Model); b != nil {
				picked, backend = t, b
				break
			}
		}
		if picked == nil {
			s.st.mu.Unlock()
			return
		}
		backend.rt.active++
		picked.Status, picked.BackendID, picked.BackendName, picked.BackendTaskID = TaskRunning, backend.ID, backend.Name, ""
		picked.StartedAt = s.now()
		picked.Attempts++
		tctx, cancel := context.WithCancel(ctx)
		picked.cancel = cancel
		_ = s.st.saveTasksLocked()
		s.st.mu.Unlock()
		s.log.Info("任务派发", "task", picked.ID, "backend", backend.ID, "attempt", picked.Attempts)
		s.wg.Add(1)
		go func(t *Task, b *Backend) {
			defer s.wg.Done()
			s.runTask(tctx, t, b)
		}(picked, backend)
	}
}

// backendTask 是算力服务器对任务的应答形状。
type backendTask struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

// runTask 把任务送到算力服务器并陪等到终态。
func (s *Server) runTask(ctx context.Context, t *Task, b *Backend) {
	timeout := defaultTaskTimeout
	if t.TimeoutSec > 0 {
		timeout = time.Duration(t.TimeoutSec) * time.Second
	}
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	body, _ := json.Marshal(map[string]any{"id": t.ID, "model": t.Model, "input": t.Input})
	res, err := s.backendCall(ctx, http.MethodPost, b.BaseURL+"/v1/tasks", body)
	if err != nil {
		s.dispatchFailed(ctx, t, b, err)
		return
	}
	if res.ID == "" {
		res.ID = t.ID
	}
	s.st.mu.Lock()
	t.BackendTaskID = res.ID
	_ = s.st.saveTasksLocked()
	s.st.mu.Unlock()
	s.await(ctx, t, b, res)
}

// dispatchFailed 处理派发那一跳的失败：标算力服务器异常、还有机会就重新排队，否则判失败。
func (s *Server) dispatchFailed(ctx context.Context, t *Task, b *Backend, err error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	if errors.Is(ctx.Err(), context.Canceled) {
		s.finishLocked(t, TaskCancelled, nil, "")
		_ = s.st.saveTasksLocked()
		return
	}
	b.rt.status, b.rt.lastError, b.rt.checkedAt = "error", "派发任务失败："+err.Error(), s.now()
	if b.rt.active > 0 {
		b.rt.active--
	}
	b.rt.failed++
	if t.Attempts < maxAttempts {
		s.log.Warn("任务派发失败，重新排队", "task", t.ID, "backend", b.ID, "attempt", t.Attempts, "error", err.Error())
		t.Status, t.BackendID, t.BackendName, t.BackendTaskID, t.cancel = TaskQueued, "", "", "", nil
		t.Error = "上一次派发失败：" + err.Error()
		_ = s.st.saveTasksLocked()
		s.kick()
		return
	}
	// 走到这里 finishLocked 不该再减 active（上面已减）：先把 BackendID 清掉再判失败。
	t.BackendID = ""
	s.finishLocked(t, TaskFailed, nil, fmt.Sprintf("派发失败 %d 次：%s", t.Attempts, err.Error()))
	_ = s.st.saveTasksLocked()
}

// await 陪等一个已派发的任务到终态。
func (s *Server) await(ctx context.Context, t *Task, b *Backend, res *backendTask) {
	for {
		if done := s.settle(t, res); done {
			return
		}
		select {
		case <-ctx.Done():
			// 取消或超时：请算力服务器取消，再落终态。
			cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = s.backendCall(cctx, http.MethodDelete, b.BaseURL+"/v1/tasks/"+res.ID, nil)
			cancel()
			s.st.mu.Lock()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				s.finishLocked(t, TaskFailed, nil, "任务超时")
			} else {
				s.finishLocked(t, TaskCancelled, nil, "")
			}
			_ = s.st.saveTasksLocked()
			s.st.mu.Unlock()
			return
		case <-time.After(pollEvery):
		}
		next, err := s.backendCall(ctx, http.MethodGet, b.BaseURL+"/v1/tasks/"+res.ID, nil)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			// 查询失败不立刻判死：算力服务器可能在忙；连续失败超过上限才判失败。
			s.st.mu.Lock()
			t.Error = "查询任务状态失败：" + err.Error()
			s.st.mu.Unlock()
			continue
		}
		if next.ID == "" {
			next.ID = res.ID
		}
		res = next
	}
}

// settle 看一份应答是不是终态，是就落库。
func (s *Server) settle(t *Task, res *backendTask) bool {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	switch res.Status {
	case TaskSucceeded, "completed", "done", "success":
		out := res.Output
		if len(out) > maxTaskOutput {
			out = json.RawMessage(strconv.Quote("输出超过上限，已丢弃"))
		}
		s.finishLocked(t, TaskSucceeded, out, "")
	case TaskFailed, "error":
		s.finishLocked(t, TaskFailed, res.Output, firstNonEmpty(res.Error, "算力服务器报告任务失败"))
	case TaskCancelled:
		s.finishLocked(t, TaskCancelled, nil, res.Error)
	default:
		if t.Error != "" {
			t.Error = ""
		}
		return false
	}
	_ = s.st.saveTasksLocked()
	return true
}

// backendCall 对算力服务器发一条 JSON 请求。非 2xx 视为失败（正文里的 error.message 带回来）。
func (s *Server) backendCall(ctx context.Context, method, url string, body []byte) (*backendTask, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskOutput+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		msg := resp.Status
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil {
			msg = firstNonEmpty(e.Error.Message, firstNonEmpty(e.Message, msg))
		}
		return nil, errors.New(msg)
	}
	var out backendTask
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, errors.New("算力服务器应答不是合法 JSON")
		}
	}
	if out.Status == "" {
		if method == http.MethodDelete {
			out.Status = TaskCancelled
		} else {
			out.Status = TaskRunning
		}
	}
	return &out, nil
}

// recoverTasks 在启动时处理上次留下的执行中任务。
func (s *Server) recoverTasks() {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	for _, t := range s.st.tasks {
		if t.Status != TaskRunning {
			continue
		}
		if t.BackendTaskID != "" {
			if b := s.findBackendLocked(t.BackendID); b != nil {
				b.rt.active++
				tctx, cancel := context.WithCancel(s.bg)
				t.cancel = cancel
				res := &backendTask{ID: t.BackendTaskID, Status: TaskRunning}
				s.wg.Add(1)
				go func(t *Task, b *Backend) {
					defer s.wg.Done()
					s.await(tctx, t, b, res)
				}(t, b)
				continue
			}
		}
		t.Status, t.BackendID, t.BackendName, t.BackendTaskID = TaskQueued, "", "", ""
		t.Error = "守护进程重启，任务重新排队"
	}
	_ = s.st.saveTasksLocked()
}

// ---- 管理面端点 ----

func (s *Server) handleTasksList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": s.taskViews(r.URL.Query().Get("status"), "", limit, false),
		"stats": s.queueStats(),
	})
}

func (s *Server) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	var in taskInput
	if err := decodeJSON(r, &in, maxTaskInput); err != nil {
		writeErr(w, err)
		return
	}
	t, err := s.submitTask(in, "admin", "")
	if err != nil {
		writeErr(w, err)
		return
	}
	v, _ := s.taskView(t.ID, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"task": v})
}

func (s *Server) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	v, err := s.taskView(r.PathValue("id"), "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": v})
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	v, err := s.cancelTask(r.Context(), r.PathValue("id"), "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": v})
}

// handleTaskRetry 把一个已结束的任务按原入参重新提交一条。
func (s *Server) handleTaskRetry(w http.ResponseWriter, r *http.Request) {
	s.st.mu.Lock()
	t := s.findTaskLocked(r.PathValue("id"))
	if t == nil {
		s.st.mu.Unlock()
		writeErr(w, notFound("任务不存在"))
		return
	}
	if !t.terminal() {
		s.st.mu.Unlock()
		writeErr(w, conflict("任务还没结束"))
		return
	}
	in := taskInput{Model: t.Model, Input: t.Input, Priority: t.Priority, TimeoutSec: t.TimeoutSec}
	source, token := t.Source, t.TokenID
	s.st.mu.Unlock()
	nt, err := s.submitTask(in, source, token)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, _ := s.taskView(nt.ID, "")
	writeJSON(w, http.StatusAccepted, map[string]any{"task": v})
}

// handleTasksClear 清掉已结束的任务记录。
func (s *Server) handleTasksClear(w http.ResponseWriter, r *http.Request) {
	s.st.mu.Lock()
	kept := s.st.tasks[:0]
	removed := 0
	for _, t := range s.st.tasks {
		if t.terminal() {
			removed++
			continue
		}
		kept = append(kept, t)
	}
	s.st.tasks = kept
	err := s.st.saveTasksLocked()
	s.st.mu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}
