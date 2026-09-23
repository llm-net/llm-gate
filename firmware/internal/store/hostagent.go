package store

// Agent远控仓储（「智能体 → 主机/SoC」每台主机的档案、对话、指令与时间线）。约定同
// repo.go：context 化、走 prepared statement、未命中 → ErrNotFound、时间入库经 fmtTime。
//
//   - 档案（agent_host_profiles）：一台主机一份 Markdown，智能体经工具改、管理员在页面改，
//     只保存当前版本；每次变更在时间线里留一条 profile 事件（审计看得到谁改的、改成什么）。
//   - 对话（agent_host_chats）：一台主机多个对话，新建时钉死 API 密钥、模型、推理档位与
//     开发者指令（当时的主机档案 + 最近操作），之后不改；删对话只删它的指令与对话内容
//     事件，对主机的操作记录留在主机名下。
//   - 指令（agent_host_runs）：属于某个对话，提交即 queued，按提交顺序逐条 running → 终态；
//     图片不入库。
//   - 事件（agent_host_events）：时间线兼操作日志，id 单调递增，页面按 id 陪等新事件、
//     按 id 向前翻历史；chat_id 标明归属（主机级事件留空），command / file / profile 三种
//     就是「对该主机的全部操作」，按主机取时跨全部对话。
//
// 表里没有凭据；命令输出只留截断后的尾段（调用方截）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// agent_host_runs.status 的封闭词汇表（与 0043 迁移的 CHECK 同步维护）。
const (
	AgentRunQueued    = "queued"
	AgentRunRunning   = "running"
	AgentRunSucceeded = "succeeded"
	AgentRunFailed    = "failed"
	AgentRunCancelled = "cancelled"
)

// AgentRunActive 判一条指令是否还没到终态。
func AgentRunActive(status string) bool { return status == AgentRunQueued || status == AgentRunRunning }

// agent_host_events.kind 的词汇表。command / file / profile 是「对主机的操作」，操作日志
// 视图只取这三种加 run / session。
const (
	AgentEventUser      = "user"      // 管理员提交的指令（body = 文本，meta = 图片张数）
	AgentEventAssistant = "assistant" // 智能体回复（body = 全文）
	AgentEventReasoning = "reasoning" // 智能体的推理摘要（body）
	AgentEventCommand   = "command"   // 在主机上执行的命令（title = 命令，body = 输出尾段）
	AgentEventFile      = "file"      // 写到主机上的文件（title = 路径，body = 内容头部）
	AgentEventProfile   = "profile"   // 主机档案变更（title = 改动者，body = 新内容）
	AgentEventRun       = "run"       // 指令状态（title = 状态，body = 原因）
	AgentEventSession   = "session"   // 引擎会话起止（title = 起/止，body = 说明）
	AgentEventError     = "error"     // 引擎侧错误（body）
)

// AgentHostProfile 是一台主机的档案。
type AgentHostProfile struct {
	HostID    int64     `json:"host_id"`
	Content   string    `json:"content"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GetAgentHostProfile 读档案；还没写过返回零值（Content 为空、UpdatedAt 为零值），不算错。
func (s *Store) GetAgentHostProfile(ctx context.Context, hostID int64) (AgentHostProfile, error) {
	var (
		p  = AgentHostProfile{HostID: hostID}
		at string
	)
	err := s.stmtGetAgentHostProfile.QueryRowContext(ctx, hostID).Scan(&p.Content, &p.UpdatedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("读取主机档案 %d: %w", hostID, err)
	}
	if p.UpdatedAt, err = parseTime(at); err != nil {
		return p, fmt.Errorf("解析档案时刻: %w", err)
	}
	return p, nil
}

// SetAgentHostProfile 整份替换档案。主机不存在（外键）返回 ErrNotFound。
func (s *Store) SetAgentHostProfile(ctx context.Context, hostID int64, content, by string, at time.Time) (AgentHostProfile, error) {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.stmtSetAgentHostProfile.ExecContext(ctx, hostID, content, by, fmtTime(at)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return AgentHostProfile{}, ErrNotFound
		}
		return AgentHostProfile{}, fmt.Errorf("写入主机档案 %d: %w", hostID, err)
	}
	return AgentHostProfile{HostID: hostID, Content: content, UpdatedBy: by, UpdatedAt: at.UTC().Truncate(time.Millisecond)}, nil
}

// agentHostChatColumns 是 SELECT 列表（与 scanAgentHostChat 的扫描顺序一一对应）。
const agentHostChatColumns = `id, host_id, title, engine, key_id, key_display, model, effort, instructions, created_at, updated_at`

// AgentHostChat 是一台主机上的一个对话。密钥 / 模型 / 档位与开发者指令在新建时钉死。
type AgentHostChat struct {
	ID     string `json:"id"`
	HostID int64  `json:"host_id"`
	Title  string `json:"title"`
	Engine string `json:"engine"`
	// KeyID 是新建时选定的 API 密钥（不是外键：密钥删了对话仍在，只是起不了会话）；
	// KeyDisplay 是它的展示串（前缀…末四位）。
	KeyID      int64  `json:"key_id"`
	KeyDisplay string `json:"key_display"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	// Instructions 是新建时渲染好的开发者指令（主机档案 + 最近操作）；不进响应。
	Instructions string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	// UpdatedAt 是最近一次活动（新建 / 改标题 / 提交指令）的时刻，列表按它倒序。
	UpdatedAt time.Time `json:"updated_at"`
}

// NewAgentHostChat 是 CreateAgentHostChat 的入参。ID 空取 NewULID(CreatedAt)。
type NewAgentHostChat struct {
	ID           string
	HostID       int64
	Title        string
	Engine       string
	KeyID        int64
	KeyDisplay   string
	Model        string
	Effort       string
	Instructions string
	CreatedAt    time.Time
}

// CreateAgentHostChat 落一个对话。主机不存在（外键）返回 ErrNotFound。
func (s *Store) CreateAgentHostChat(ctx context.Context, nc NewAgentHostChat) (*AgentHostChat, error) {
	at := nc.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := nc.ID
	if id == "" {
		id = NewULID(at)
	}
	if _, err := s.stmtCreateAgentHostChat.ExecContext(ctx, id, nc.HostID, nc.Title, nc.Engine, nc.KeyID, nc.KeyDisplay,
		nc.Model, nc.Effort, nc.Instructions, fmtTime(at), fmtTime(at)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("落对话: %w", err)
	}
	return s.GetAgentHostChat(ctx, id)
}

// GetAgentHostChat 按 id 取一个对话；不存在 ErrNotFound。
func (s *Store) GetAgentHostChat(ctx context.Context, id string) (*AgentHostChat, error) {
	c, err := scanAgentHostChat(s.stmtGetAgentHostChat.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取对话 %s: %w", id, err)
	}
	return c, nil
}

// ListAgentHostChats 列出一台主机的全部对话，最近活动的在前。
func (s *Store) ListAgentHostChats(ctx context.Context, hostID int64) ([]AgentHostChat, error) {
	rows, err := s.stmtListAgentHostChats.QueryContext(ctx, hostID)
	if err != nil {
		return nil, fmt.Errorf("列出对话: %w", err)
	}
	defer rows.Close()
	out := []AgentHostChat{}
	for rows.Next() {
		c, err := scanAgentHostChat(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描对话: %w", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出对话: %w", err)
	}
	return out, nil
}

// SetAgentHostChatTitle 改对话标题（并刷新活动时刻）。不存在 ErrNotFound。
func (s *Store) SetAgentHostChatTitle(ctx context.Context, id, title string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.stmtSetAgentHostChatTitle.ExecContext(ctx, title, fmtTime(at), id)
	if err != nil {
		return fmt.Errorf("改对话标题 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("改对话标题 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAgentHostChat 刷新对话的活动时刻（提交指令时）。
func (s *Store) TouchAgentHostChat(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.stmtTouchAgentHostChat.ExecContext(ctx, fmtTime(at), id); err != nil {
		return fmt.Errorf("刷新对话 %s: %w", id, mapErr(err))
	}
	return nil
}

// agentChatOnlyEventKinds 是只属于对话、随对话一起删除的事件种类；其余（指令、命令、
// 文件、档案、会话边界）是对主机的操作记录，删对话时只摘掉归属、留在主机名下。
var agentChatOnlyEventKinds = []string{AgentEventAssistant, AgentEventReasoning, AgentEventError}

// DeleteAgentHostChat 删一个对话：它的指令行、回复 / 推理 / 错误事件一起删，操作记录改挂
// 主机（chat_id 清空）。不存在 ErrNotFound。
func (s *Store) DeleteAgentHostChat(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("删对话 %s: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM agent_host_chats WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删对话 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删对话 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_host_runs WHERE chat_id = ?`, id); err != nil {
		return fmt.Errorf("删对话 %s 的指令: %w", id, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_host_events WHERE chat_id = ? AND kind IN (?, ?, ?)`,
		id, agentChatOnlyEventKinds[0], agentChatOnlyEventKinds[1], agentChatOnlyEventKinds[2]); err != nil {
		return fmt.Errorf("删对话 %s 的事件: %w", id, mapErr(err))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_host_events SET chat_id = '' WHERE chat_id = ?`, id); err != nil {
		return fmt.Errorf("改挂对话 %s 的操作记录: %w", id, mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("删对话 %s: %w", id, err)
	}
	return nil
}

func scanAgentHostChat(row interface{ Scan(...any) error }) (*AgentHostChat, error) {
	var (
		c                AgentHostChat
		created, updated string
	)
	if err := row.Scan(&c.ID, &c.HostID, &c.Title, &c.Engine, &c.KeyID, &c.KeyDisplay, &c.Model, &c.Effort, &c.Instructions,
		&created, &updated); err != nil {
		return nil, err
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("解析新建时刻: %w", err)
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("解析活动时刻: %w", err)
	}
	return &c, nil
}

// agentHostRunColumns 是 SELECT 列表（与 scanAgentHostRun 的扫描顺序一一对应）。
const agentHostRunColumns = `id, host_id, chat_id, status, text, image_count, engine, model, error,
	input_tokens, output_tokens, created_at, started_at, finished_at`

// AgentHostRun 是一条提交给智能体的指令。
type AgentHostRun struct {
	ID         string `json:"id"`
	HostID     int64  `json:"host_id"`
	ChatID     string `json:"chat_id,omitempty"`
	Status     string `json:"status"`
	Text       string `json:"text"`
	ImageCount int    `json:"image_count"`
	Engine     string `json:"engine,omitempty"`
	Model      string `json:"model,omitempty"`
	// Error 是失败 / 取消原因（终态之外为空）。
	Error        string     `json:"error,omitempty" i18n:"text"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// NewAgentHostRun 是 CreateAgentHostRun 的入参。ID 空取 NewULID(CreatedAt)。
type NewAgentHostRun struct {
	ID         string
	HostID     int64
	ChatID     string
	Text       string
	ImageCount int
	Engine     string
	Model      string
	CreatedAt  time.Time
}

// CreateAgentHostRun 落一条 queued 指令。
func (s *Store) CreateAgentHostRun(ctx context.Context, nr NewAgentHostRun) (*AgentHostRun, error) {
	at := nr.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := nr.ID
	if id == "" {
		id = NewULID(at)
	}
	if _, err := s.stmtCreateAgentHostRun.ExecContext(ctx, id, nr.HostID, nr.ChatID, AgentRunQueued, nr.Text, nr.ImageCount,
		nr.Engine, nr.Model, fmtTime(at)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("落指令: %w", err)
	}
	return s.GetAgentHostRun(ctx, id)
}

// GetAgentHostRun 按 id 取一条；不存在 ErrNotFound。
func (s *Store) GetAgentHostRun(ctx context.Context, id string) (*AgentHostRun, error) {
	r, err := scanAgentHostRun(s.stmtGetAgentHostRun.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取指令 %s: %w", id, err)
	}
	return r, nil
}

// ListAgentHostRuns 按提交时间倒序列出一台主机最近的指令（limit ≤ 0 取 50）。
func (s *Store) ListAgentHostRuns(ctx context.Context, hostID int64, limit int) ([]AgentHostRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.stmtListAgentHostRuns.QueryContext(ctx, hostID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出指令: %w", err)
	}
	defer rows.Close()
	return collectAgentHostRuns(rows)
}

// ListActiveAgentHostRuns 列出全部未到终态的指令（按提交顺序），进程启动时判它们失败。
func (s *Store) ListActiveAgentHostRuns(ctx context.Context) ([]AgentHostRun, error) {
	rows, err := s.stmtListActiveAgentHostRuns.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出未完成指令: %w", err)
	}
	defer rows.Close()
	return collectAgentHostRuns(rows)
}

func collectAgentHostRuns(rows *sql.Rows) ([]AgentHostRun, error) {
	out := []AgentHostRun{}
	for rows.Next() {
		r, err := scanAgentHostRun(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描指令: %w", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出指令: %w", err)
	}
	return out, nil
}

// SetAgentHostRunStatus 改一条指令的状态：running 记 started_at，终态记 finished_at 与原因。
// 行不存在 ErrNotFound。
func (s *Store) SetAgentHostRunStatus(ctx context.Context, id, status, reason string, at time.Time) (*AgentHostRun, error) {
	switch status {
	case AgentRunQueued, AgentRunRunning, AgentRunSucceeded, AgentRunFailed, AgentRunCancelled:
	default:
		return nil, fmt.Errorf("指令 %s: 非法状态 %q", id, status)
	}
	if at.IsZero() {
		at = time.Now()
	}
	var started, finished any
	if status == AgentRunRunning {
		started = fmtTime(at)
	}
	if !AgentRunActive(status) {
		finished = fmtTime(at)
	}
	res, err := s.stmtSetAgentHostRunStatus.ExecContext(ctx, status, reason, started, finished, id)
	if err != nil {
		return nil, fmt.Errorf("更新指令 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("更新指令 %s: %w", id, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAgentHostRun(ctx, id)
}

// SetAgentHostRunUsage 写回一条指令消耗的 token 数（引擎报告的累计值）。
func (s *Store) SetAgentHostRunUsage(ctx context.Context, id string, in, out int64) error {
	if _, err := s.stmtSetAgentHostRunUsage.ExecContext(ctx, in, out, id); err != nil {
		return fmt.Errorf("写回指令用量 %s: %w", id, err)
	}
	return nil
}

func scanAgentHostRun(row interface{ Scan(...any) error }) (*AgentHostRun, error) {
	var (
		r                   AgentHostRun
		created             string
		started, finished   sql.NullString
		inTokens, outTokens int64
	)
	if err := row.Scan(&r.ID, &r.HostID, &r.ChatID, &r.Status, &r.Text, &r.ImageCount, &r.Engine, &r.Model, &r.Error,
		&inTokens, &outTokens, &created, &started, &finished); err != nil {
		return nil, err
	}
	r.InputTokens, r.OutputTokens = inTokens, outTokens
	t, err := parseTime(created)
	if err != nil {
		return nil, fmt.Errorf("解析提交时刻: %w", err)
	}
	r.CreatedAt = t
	if r.StartedAt, err = optionalTime(started); err != nil {
		return nil, err
	}
	if r.FinishedAt, err = optionalTime(finished); err != nil {
		return nil, err
	}
	return &r, nil
}

func optionalTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := parseTime(v.String)
	if err != nil {
		return nil, fmt.Errorf("解析时刻: %w", err)
	}
	return &t, nil
}

// AgentHostEvent 是时间线上的一条事件。
type AgentHostEvent struct {
	ID     int64 `json:"id"`
	HostID int64 `json:"host_id"`
	// ChatID 是事件所属的对话；主机级事件（管理员改档案、进程重启判失败）为空。
	ChatID string    `json:"chat_id,omitempty"`
	RunID  string    `json:"run_id,omitempty"`
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Title  string    `json:"title,omitempty"`
	Body   string    `json:"body,omitempty"`
	// Meta 是与 kind 相关的小 JSON（图片张数、工具名、是否截断）。
	Meta       string `json:"meta,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// AppendAgentHostEvent 追加一条事件并回带 id。At 零值取当前时间；主机不存在 ErrNotFound。
func (s *Store) AppendAgentHostEvent(ctx context.Context, ev AgentHostEvent) (*AgentHostEvent, error) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	var exit any
	if ev.ExitCode != nil {
		exit = *ev.ExitCode
	}
	res, err := s.stmtAppendAgentHostEvent.ExecContext(ctx, ev.HostID, ev.ChatID, ev.RunID, fmtTime(ev.At), ev.Kind,
		ev.Title, ev.Body, ev.Meta, exit, ev.DurationMs)
	if err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("追加事件: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("追加事件: %w", err)
	}
	ev.ID = id
	ev.At = ev.At.UTC().Truncate(time.Millisecond)
	return &ev, nil
}

// AgentHostEventQuery 是 ListAgentHostEvents 的入参：AfterID 取更新的（升序，陪等用），
// BeforeID 取更早的（仍按升序返回，翻历史用），两者都为零取最近的 Limit 条。
type AgentHostEventQuery struct {
	HostID int64
	// ChatID 非空时只取这个对话的事件（对话时间线）；空取整台主机的（操作日志）。
	ChatID   string
	AfterID  int64
	BeforeID int64
	// Kinds 非空时只取这些种类（操作日志视图）。
	Kinds []string
	// Limit ≤ 0 取 200，上限 1000。
	Limit int
}

// ListAgentHostEvents 按 id 升序返回事件。
func (s *Store) ListAgentHostEvents(ctx context.Context, q AgentHostEventQuery) ([]AgentHostEvent, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, host_id, chat_id, run_id, at, kind, title, body, meta, exit_code, duration_ms
	            FROM agent_host_events WHERE host_id = ?`
	args := []any{q.HostID}
	if q.ChatID != "" {
		query += ` AND chat_id = ?`
		args = append(args, q.ChatID)
	}
	if q.AfterID > 0 {
		query += ` AND id > ?`
		args = append(args, q.AfterID)
	}
	if q.BeforeID > 0 {
		query += ` AND id < ?`
		args = append(args, q.BeforeID)
	}
	if len(q.Kinds) > 0 {
		query += ` AND kind IN (`
		for i, k := range q.Kinds {
			if i > 0 {
				query += `, `
			}
			query += `?`
			args = append(args, k)
		}
		query += `)`
	}
	// 陪等（AfterID）要最早的那一段先到；其余取最近的一段再翻成升序。
	if q.AfterID > 0 {
		query += ` ORDER BY id ASC LIMIT ?`
	} else {
		query += ` ORDER BY id DESC LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出事件: %w", err)
	}
	defer rows.Close()
	out := []AgentHostEvent{}
	for rows.Next() {
		var (
			ev   AgentHostEvent
			at   string
			exit sql.NullInt64
		)
		if err := rows.Scan(&ev.ID, &ev.HostID, &ev.ChatID, &ev.RunID, &at, &ev.Kind, &ev.Title, &ev.Body, &ev.Meta, &exit, &ev.DurationMs); err != nil {
			return nil, fmt.Errorf("扫描事件: %w", err)
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("解析事件时刻: %w", err)
		}
		if exit.Valid {
			code := int(exit.Int64)
			ev.ExitCode = &code
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出事件: %w", err)
	}
	if q.AfterID == 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}
