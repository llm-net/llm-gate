package store

// 创作工作空间的智能体仓储（「智能体 → 工作空间」里 kind = studio 的空间）。约定同 repo.go：
// context 化、走 prepared statement、未命中 → ErrNotFound、时间入库经 fmtTime。形态照
// Agent远控（hostagent.go）：
//
//   - 对话（studio_chats）：一个工作空间多个对话，新建时钉死 API 密钥、模型、推理档位与
//     开发者指令（当时的目录清单与项目说明），之后不改；删对话连带它的指令与事件（级联），
//     目录里的文件不动。
//   - 指令（studio_runs）：属于某个对话，提交即 queued，按提交顺序逐条 running → 终态；
//     图片不入库。
//   - 事件（studio_events）：时间线，id 单调递增，页面按 id 陪等新事件、按 id 向前翻历史。
//   - 文件（studio_files）：目录里每个文件的附注——种类、大小、尺寸、来源（上传 / 生成 /
//     智能体写入）、生成它的提示词 / 订阅 / 模型 / 参数。目录才是真值：文件没了行也要清
//     （internal/studio 在列目录时对账）。
//
// 表里没有凭据；提示词是产品数据（用户自己的创作记录），不进日志（§15.1）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// studio_events.kind 的词汇表。
const (
	StudioEventUser      = "user"      // 管理员提交的指令（body = 文本，meta = 图片张数）
	StudioEventAssistant = "assistant" // 智能体回复（body = 全文）
	StudioEventReasoning = "reasoning" // 智能体的推理摘要（body）
	StudioEventTool      = "tool"      // 一次工具调用（title = 工具名，body = 一句摘要，meta = 小 JSON）
	StudioEventGenerate  = "generate"  // 一次图像 / 视频生成（title = 落成的文件名，body = 提示词，meta = 订阅 / 模型 / 结局）
	StudioEventFile      = "file"      // 目录里的文件变更（title = 文件名，meta = 动作 / 字节数）
	StudioEventCommand   = "command"   // 在工作节点上执行的命令（title = 命令，body = 输出尾段，meta = 退出码 / 是否截断）
	StudioEventRun       = "run"       // 指令状态（title = 状态，body = 原因）
	StudioEventSession   = "session"   // 引擎会话起止（title = 起 / 止，body = 说明）
	StudioEventError     = "error"     // 引擎侧错误（body）
)

// studio_files.origin 的词汇表。
const (
	StudioFileOriginUpload    = "upload"    // 管理员上传
	StudioFileOriginGenerated = "generated" // 智能体经生成工具生成
	StudioFileOriginAgent     = "agent"     // 智能体写入的文本文件
	StudioFileOriginUnknown   = "unknown"   // 目录里出现、没有附注的文件（对账时补行）
)

// studio_files.kind 的词汇表。
const (
	StudioFileImage = "image"
	StudioFileVideo = "video"
	StudioFileAudio = "audio"
	StudioFileText  = "text"
	StudioFileOther = "other"
)

// studioChatColumns 是 SELECT 列表（与 scanStudioChat 的扫描顺序一一对应）。
const studioChatColumns = `id, workspace_id, title, engine, key_id, key_display, model, effort, instructions, created_at, updated_at, archived_at`

// StudioChat 是一个创作工作空间里的一个对话。密钥 / 模型 / 档位与开发者指令在新建时钉死。
type StudioChat struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Title       string `json:"title"`
	Engine      string `json:"engine"`
	// KeyID 是新建时选定的 API 密钥（不是外键：密钥删了对话仍在，只是起不了会话）；
	// KeyDisplay 是它的展示串。
	KeyID      int64  `json:"key_id"`
	KeyDisplay string `json:"key_display"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	// Instructions 是新建时渲染好的开发者指令；不进响应。
	Instructions string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	// UpdatedAt 是最近一次活动的时刻，列表按它倒序。
	UpdatedAt time.Time `json:"updated_at"`
	// ArchivedAt 非空即已归档：只能查看，不再接受指令。
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

// NewStudioChat 是 CreateStudioChat 的入参。ID 空取 NewULID(CreatedAt)。
type NewStudioChat struct {
	ID           string
	WorkspaceID  string
	Title        string
	Engine       string
	KeyID        int64
	KeyDisplay   string
	Model        string
	Effort       string
	Instructions string
	CreatedAt    time.Time
}

// CreateStudioChat 落一个对话。工作空间不存在（外键）返回 ErrNotFound。
func (s *Store) CreateStudioChat(ctx context.Context, nc NewStudioChat) (*StudioChat, error) {
	at := nc.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := nc.ID
	if id == "" {
		id = NewULID(at)
	}
	if _, err := s.stmtCreateStudioChat.ExecContext(ctx, id, nc.WorkspaceID, nc.Title, nc.Engine, nc.KeyID, nc.KeyDisplay,
		nc.Model, nc.Effort, nc.Instructions, fmtTime(at), fmtTime(at)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("落对话: %w", err)
	}
	return s.GetStudioChat(ctx, id)
}

// GetStudioChat 按 id 取一个对话；不存在 ErrNotFound。
func (s *Store) GetStudioChat(ctx context.Context, id string) (*StudioChat, error) {
	c, err := scanStudioChat(s.stmtGetStudioChat.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取对话 %s: %w", id, err)
	}
	return c, nil
}

// ListStudioChats 列出一个工作空间的全部对话，最近活动的在前。
func (s *Store) ListStudioChats(ctx context.Context, workspaceID string) ([]StudioChat, error) {
	rows, err := s.stmtListStudioChats.QueryContext(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("列出对话: %w", err)
	}
	defer rows.Close()
	out := []StudioChat{}
	for rows.Next() {
		c, err := scanStudioChat(rows)
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

// SetStudioChatTitle 改对话标题（并刷新活动时刻）。不存在 ErrNotFound。
func (s *Store) SetStudioChatTitle(ctx context.Context, id, title string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.stmtSetStudioChatTitle.ExecContext(ctx, title, fmtTime(at), id)
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

// TouchStudioChat 刷新对话的活动时刻（提交指令时）。
func (s *Store) TouchStudioChat(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.stmtTouchStudioChat.ExecContext(ctx, fmtTime(at), id); err != nil {
		return fmt.Errorf("刷新对话 %s: %w", id, mapErr(err))
	}
	return nil
}

// ArchiveStudioChat 把一个对话标成已归档（已归档的保持原时刻）。不存在 ErrNotFound。
func (s *Store) ArchiveStudioChat(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now()
	}
	res, err := s.stmtArchiveStudioChat.ExecContext(ctx, fmtTime(at), id)
	if err != nil {
		return fmt.Errorf("归档对话 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("归档对话 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteStudioChat 删一个对话：指令与事件随外键级联删除，文件行不动。不存在 ErrNotFound。
func (s *Store) DeleteStudioChat(ctx context.Context, id string) error {
	res, err := s.stmtDeleteStudioChat.ExecContext(ctx, id)
	if err != nil {
		return fmt.Errorf("删对话 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删对话 %s: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanStudioChat(row interface{ Scan(...any) error }) (*StudioChat, error) {
	var (
		c                StudioChat
		created, updated string
		archived         sql.NullString
	)
	if err := row.Scan(&c.ID, &c.WorkspaceID, &c.Title, &c.Engine, &c.KeyID, &c.KeyDisplay, &c.Model, &c.Effort, &c.Instructions,
		&created, &updated, &archived); err != nil {
		return nil, err
	}
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("解析新建时刻: %w", err)
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("解析活动时刻: %w", err)
	}
	if c.ArchivedAt, err = optionalTime(archived); err != nil {
		return nil, err
	}
	return &c, nil
}

// StudioMediaModel 是一个创作工作空间里启用的一个生成模型：出现在表里即启用（显式白名单），
// Usage 是管理员写的使用场景说明（可空），进创作智能体的开发者指令。
type StudioMediaModel struct {
	Model string `json:"model"`
	Usage string `json:"usage"`
}

// ListStudioMediaModels 列出一个工作空间启用的生成模型（按模型名）。
func (s *Store) ListStudioMediaModels(ctx context.Context, workspaceID string) ([]StudioMediaModel, error) {
	rows, err := s.stmtListStudioMediaModels.QueryContext(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("列出工作空间的生成模型: %w", err)
	}
	defer rows.Close()
	out := []StudioMediaModel{}
	for rows.Next() {
		var m StudioMediaModel
		if err := rows.Scan(&m.Model, &m.Usage); err != nil {
			return nil, fmt.Errorf("扫描工作空间的生成模型: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出工作空间的生成模型: %w", err)
	}
	return out, nil
}

// ReplaceStudioMediaModels 整份替换一个工作空间启用的生成模型。工作空间不存在 ErrNotFound。
func (s *Store) ReplaceStudioMediaModels(ctx context.Context, workspaceID string, models []StudioMediaModel) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("替换工作空间的生成模型: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM studio_media_models WHERE workspace_id = ?`, workspaceID); err != nil {
		return fmt.Errorf("替换工作空间的生成模型: %w", mapErr(err))
	}
	for _, m := range models {
		if _, err := tx.ExecContext(ctx, `INSERT INTO studio_media_models (workspace_id, model, usage) VALUES (?, ?, ?)`,
			workspaceID, m.Model, m.Usage); err != nil {
			err = mapErr(err)
			if errors.Is(err, ErrConflict) {
				return ErrNotFound
			}
			return fmt.Errorf("替换工作空间的生成模型: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("替换工作空间的生成模型: %w", err)
	}
	return nil
}

// studioRunColumns 是 SELECT 列表（与 scanStudioRun 的扫描顺序一一对应）。
const studioRunColumns = `id, workspace_id, chat_id, status, text, image_count, engine, model, error,
	input_tokens, output_tokens, created_at, started_at, finished_at`

// StudioRun 是一条提交给智能体的指令。状态词汇表与 AgentHostRun 相同（AgentRun*）。
type StudioRun struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ChatID      string `json:"chat_id"`
	Status      string `json:"status"`
	Text        string `json:"text"`
	ImageCount  int    `json:"image_count"`
	Engine      string `json:"engine,omitempty"`
	Model       string `json:"model,omitempty"`
	// Error 是失败 / 取消原因（终态之外为空）。
	Error        string     `json:"error,omitempty" i18n:"text"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// NewStudioRun 是 CreateStudioRun 的入参。ID 空取 NewULID(CreatedAt)。
type NewStudioRun struct {
	ID          string
	WorkspaceID string
	ChatID      string
	Text        string
	ImageCount  int
	Engine      string
	Model       string
	CreatedAt   time.Time
}

// CreateStudioRun 落一条 queued 指令。对话不存在（外键）返回 ErrNotFound。
func (s *Store) CreateStudioRun(ctx context.Context, nr NewStudioRun) (*StudioRun, error) {
	at := nr.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	id := nr.ID
	if id == "" {
		id = NewULID(at)
	}
	if _, err := s.stmtCreateStudioRun.ExecContext(ctx, id, nr.WorkspaceID, nr.ChatID, AgentRunQueued, nr.Text, nr.ImageCount,
		nr.Engine, nr.Model, fmtTime(at)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("落指令: %w", err)
	}
	return s.GetStudioRun(ctx, id)
}

// GetStudioRun 按 id 取一条；不存在 ErrNotFound。
func (s *Store) GetStudioRun(ctx context.Context, id string) (*StudioRun, error) {
	r, err := scanStudioRun(s.stmtGetStudioRun.QueryRowContext(ctx, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取指令 %s: %w", id, err)
	}
	return r, nil
}

// ListActiveStudioRuns 列出全部未到终态的指令（按提交顺序），进程启动时判它们失败。
func (s *Store) ListActiveStudioRuns(ctx context.Context) ([]StudioRun, error) {
	rows, err := s.stmtListActiveStudioRuns.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出未完成指令: %w", err)
	}
	defer rows.Close()
	out := []StudioRun{}
	for rows.Next() {
		r, err := scanStudioRun(rows)
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

// SetStudioRunStatus 改一条指令的状态：running 记 started_at，终态记 finished_at 与原因。
// 行不存在 ErrNotFound。
func (s *Store) SetStudioRunStatus(ctx context.Context, id, status, reason string, at time.Time) (*StudioRun, error) {
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
	res, err := s.stmtSetStudioRunStatus.ExecContext(ctx, status, reason, started, finished, id)
	if err != nil {
		return nil, fmt.Errorf("更新指令 %s: %w", id, mapErr(err))
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("更新指令 %s: %w", id, err)
	} else if n == 0 {
		return nil, ErrNotFound
	}
	return s.GetStudioRun(ctx, id)
}

// SetStudioRunUsage 写回一条指令消耗的 token 数。
func (s *Store) SetStudioRunUsage(ctx context.Context, id string, in, out int64) error {
	if _, err := s.stmtSetStudioRunUsage.ExecContext(ctx, in, out, id); err != nil {
		return fmt.Errorf("写回指令用量 %s: %w", id, err)
	}
	return nil
}

func scanStudioRun(row interface{ Scan(...any) error }) (*StudioRun, error) {
	var (
		r                 StudioRun
		created           string
		started, finished sql.NullString
	)
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ChatID, &r.Status, &r.Text, &r.ImageCount, &r.Engine, &r.Model, &r.Error,
		&r.InputTokens, &r.OutputTokens, &created, &started, &finished); err != nil {
		return nil, err
	}
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

// StudioEvent 是时间线上的一条事件。
type StudioEvent struct {
	ID          int64     `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ChatID      string    `json:"chat_id"`
	RunID       string    `json:"run_id,omitempty"`
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title,omitempty"`
	Body        string    `json:"body,omitempty"`
	// Meta 是与 kind 相关的小 JSON（图片张数、订阅 / 模型、动作、字节数、是否出错）。
	Meta       string `json:"meta,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
}

// AppendStudioEvent 追加一条事件并回带 id。At 零值取当前时间；对话不存在 ErrNotFound。
func (s *Store) AppendStudioEvent(ctx context.Context, ev StudioEvent) (*StudioEvent, error) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	res, err := s.stmtAppendStudioEvent.ExecContext(ctx, ev.WorkspaceID, ev.ChatID, ev.RunID, fmtTime(ev.At), ev.Kind,
		ev.Title, ev.Body, ev.Meta, ev.DurationMs)
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

// StudioEventQuery 是 ListStudioEvents 的入参：AfterID 取更新的（升序，陪等用），BeforeID
// 取更早的（仍按升序返回，翻历史用），两者都为零取最近的 Limit 条。
type StudioEventQuery struct {
	ChatID   string
	AfterID  int64
	BeforeID int64
	// Kinds 非空时只取这些种类。
	Kinds []string
	// Limit ≤ 0 取 200，上限 1000。
	Limit int
}

// ListStudioEvents 按 id 升序返回一个对话的事件。
func (s *Store) ListStudioEvents(ctx context.Context, q StudioEventQuery) ([]StudioEvent, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `SELECT id, workspace_id, chat_id, run_id, at, kind, title, body, meta, duration_ms
	            FROM studio_events WHERE chat_id = ?`
	args := []any{q.ChatID}
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
	out := []StudioEvent{}
	for rows.Next() {
		var (
			ev StudioEvent
			at string
		)
		if err := rows.Scan(&ev.ID, &ev.WorkspaceID, &ev.ChatID, &ev.RunID, &at, &ev.Kind, &ev.Title, &ev.Body, &ev.Meta, &ev.DurationMs); err != nil {
			return nil, fmt.Errorf("扫描事件: %w", err)
		}
		if ev.At, err = parseTime(at); err != nil {
			return nil, fmt.Errorf("解析事件时刻: %w", err)
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

// studioFileColumns 是 SELECT 列表（与 scanStudioFile 的扫描顺序一一对应）。
const studioFileColumns = `workspace_id, name, kind, mime, bytes, width, height, origin, provider, model, prompt, params, chat_id, run_id, created_at, updated_at, mod_time`

// StudioFile 是目录里一个文件的附注。
type StudioFile struct {
	WorkspaceID string `json:"-"`
	Name        string `json:"name"`
	// Kind ∈ StudioFileImage / Video / Audio / Text / Other（按扩展名判）。
	Kind  string `json:"kind"`
	Mime  string `json:"mime,omitempty"`
	Bytes int64  `json:"bytes"`
	// Width / Height 只对设备解得开尺寸的图像有值。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// Origin ∈ StudioFileOriginUpload / Generated / Agent / Unknown。
	Origin string `json:"origin"`
	// Provider / Model / Prompt / Params 只对生成的文件有值；Params 是 JSON。
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Params    string    `json:"params,omitempty"`
	ChatID    string    `json:"chat_id,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ModTime 是写附注时文件在目录里的修改时刻（毫秒精度）；零值 = 还没记过。对账据它与大小
	// 判断文件是否被绕过本包改写。
	ModTime time.Time `json:"-"`
}

// UpsertStudioFile 写一个文件的附注（同名覆盖）。CreatedAt 零值取当前时间；UpdatedAt 恒取
// 当前时间。工作空间不存在（外键）返回 ErrNotFound。
func (s *Store) UpsertStudioFile(ctx context.Context, f StudioFile) (*StudioFile, error) {
	now := time.Now()
	if f.CreatedAt.IsZero() {
		f.CreatedAt = now
	}
	if _, err := s.stmtUpsertStudioFile.ExecContext(ctx, f.WorkspaceID, f.Name, f.Kind, f.Mime, f.Bytes, f.Width, f.Height,
		f.Origin, f.Provider, f.Model, f.Prompt, f.Params, f.ChatID, f.RunID, fmtTime(f.CreatedAt), fmtTime(now), fmtZeroTime(f.ModTime)); err != nil {
		err = mapErr(err)
		if errors.Is(err, ErrConflict) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("写文件附注 %s: %w", f.Name, err)
	}
	return s.GetStudioFile(ctx, f.WorkspaceID, f.Name)
}

// GetStudioFile 取一个文件的附注；不存在 ErrNotFound。
func (s *Store) GetStudioFile(ctx context.Context, workspaceID, name string) (*StudioFile, error) {
	f, err := scanStudioFile(s.stmtGetStudioFile.QueryRowContext(ctx, workspaceID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取文件附注 %s: %w", name, err)
	}
	return f, nil
}

// ListStudioFiles 列出一个工作空间的全部文件附注（按创建顺序）。
func (s *Store) ListStudioFiles(ctx context.Context, workspaceID string) ([]StudioFile, error) {
	rows, err := s.stmtListStudioFiles.QueryContext(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("列出文件附注: %w", err)
	}
	defer rows.Close()
	out := []StudioFile{}
	for rows.Next() {
		f, err := scanStudioFile(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描文件附注: %w", err)
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出文件附注: %w", err)
	}
	return out, nil
}

// DeleteStudioFile 删一个文件的附注；不存在也算成功（目录才是真值）。
func (s *Store) DeleteStudioFile(ctx context.Context, workspaceID, name string) error {
	if _, err := s.stmtDeleteStudioFile.ExecContext(ctx, workspaceID, name); err != nil {
		return fmt.Errorf("删文件附注 %s: %w", name, mapErr(err))
	}
	return nil
}

// RenameStudioFile 改一个文件附注的名字；原名不存在也算成功，新名已被占用 ErrConflict。
func (s *Store) RenameStudioFile(ctx context.Context, workspaceID, from, to string) error {
	if _, err := s.stmtRenameStudioFile.ExecContext(ctx, to, fmtTime(time.Now()), workspaceID, from); err != nil {
		return fmt.Errorf("改文件附注名 %s: %w", from, mapErr(err))
	}
	return nil
}

// fmtZeroTime 把可为零值的时刻写成列值：零值为空串。
func fmtZeroTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtTime(t)
}

func scanStudioFile(row interface{ Scan(...any) error }) (*StudioFile, error) {
	var (
		f                         StudioFile
		created, updated, modTime string
	)
	if err := row.Scan(&f.WorkspaceID, &f.Name, &f.Kind, &f.Mime, &f.Bytes, &f.Width, &f.Height, &f.Origin, &f.Provider, &f.Model,
		&f.Prompt, &f.Params, &f.ChatID, &f.RunID, &created, &updated, &modTime); err != nil {
		return nil, err
	}
	var err error
	if modTime != "" {
		if f.ModTime, err = parseTime(modTime); err != nil {
			return nil, fmt.Errorf("解析文件时刻: %w", err)
		}
	}
	if f.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("解析文件时刻: %w", err)
	}
	if f.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("解析文件时刻: %w", err)
	}
	return &f, nil
}
