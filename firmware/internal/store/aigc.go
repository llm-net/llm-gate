package store

// aigc_tasks 仓储：视频/图片异步任务行（iteration-8 Phase 1）。约定同
// repo.go：context 化、走 prepared statement、未命中 → ErrNotFound、唯一性
// 冲突 → ErrConflict、时间入库经 fmtTime。
//
// 任务行的三重身份，决定了它的形状：
//
//   - **客户端任务标识的路由锚点**（2026-08-09 厂商官方接口改版起）：客户端
//     可见的任务标识就是厂商 task_id（vendor_task_id）——设备纯转发不改接口
//     格式，厂商响应里的 id 原样到达客户端；行的 id 列（agt- 前缀）降为纯
//     内部主键，不再出现在任何响应里。行的作用是把厂商 id 映射回受理它的
//     上游账户（多上游钉死）与归属用户。
//   - **归属裁决点**：查询/取消全部经 GetAIGCTaskByVendorIDForKey——
//     vendor_task_id 与 key_id 同时命中才有行，「不是你的」与「不存在」同回
//     ErrNotFound，不给存在性 oracle（refs/new-api 调研里「结果下载口最容易
//     漏鉴权」的反例记在迭代计划要点 6）。
//   - **iteration-9 的账单事实**：usage_json 存厂商 usage 原文（两家形态
//     异构，按 kind+上游类型解读），has_video_input/generate_audio/
//     service_tier/req_* 是提交时即固化的计费特征。所以任务行对
//     upstreams/api_keys 一律不设外键并冗余快照名称列——上游/Key 被删后账单
//     事实原样留存（同 audit_events 先例）。
//
// 保留期：任务行只留短期（调用方以 PruneAIGCTasks 按 14 天保留清理，随
// gatewayd 的既有清理节奏跑——接线在任务面入口落地的迭代 8 Phase 3）；厂商侧
// 任务记录本身只存 7 天，本地行比厂商窗口长一倍，列表与排障都够用。
//
// §15.1 边界：本表不存请求体、提示词或产物内容；error_message 由写入方截断
// 后入库；content_url/last_frame_url 是厂商产物 URL 的时效副本（仅供排障，
// 客户端拿到的恒是设备合成的下载地址）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// aigcTaskColumns 是 aigc_tasks 全列的 SELECT 列表（与 scanAIGCTask 的
// 扫描顺序一一对应，改任一侧必须同步另一侧）。
const aigcTaskColumns = `id, vendor_task_id, upstream_id, upstream_name, model_name, kind,
	key_id, key_display, status, error_code, error_message, usage_json,
	has_video_input, generate_audio, service_tier, req_resolution, req_duration,
	content_url, last_frame_url, cost_micro, estimated, created_at, updated_at`

// AIGCTask 是 aigc_tasks 表的一行。Status 词汇由 gateway 适配器归一
// （queued/running/succeeded/failed/cancelled/expired…），本包不校验枚举。
type AIGCTask struct {
	// ID 是设备签发的内部主键（agt- 前缀不透明串），不进任何客户端响应；
	// 用量环里的清算行以它标识任务。
	ID string
	// VendorTaskID 是厂商侧任务 id——客户端可见的任务标识（纯转发架构下厂商
	// 响应原样到达客户端），也是设备→厂商回查的句柄。
	VendorTaskID string
	// UpstreamID/UpstreamName 钉死任务的来源上游（创建后不再故障切换）；
	// 无外键，名称是删除后仍可读的快照。
	UpstreamID   int64
	UpstreamName string
	ModelName    string
	Kind         string // ModelKindVideo | ModelKindImage（同模型 kind 词汇）
	// KeyID/KeyDisplay 是提交时的归属快照；归属校验按 KeyID 值比对，
	// 密钥被删后行照样留存（账单事实）。
	KeyID      int64
	KeyDisplay string
	Status     string
	ErrorCode  string
	// ErrorMessage 由写入方截断后入库。
	ErrorMessage string
	// UsageJSON 是厂商 usage 原文 JSON（iteration-9 计价只读这里）。
	UsageJSON string
	// 计费特征列：提交时即固化。
	HasVideoInput bool
	GenerateAudio bool
	ServiceTier   string
	ReqResolution string
	ReqDuration   string
	// ContentURL/LastFrameURL 是厂商产物 URL 的时效副本（会过期，仅供排障）。
	ContentURL   string
	LastFrameURL string
	// CostMicro 是清算后的消费额（int64 微元）；**nil = 尚未清算**，正是
	// SettleAIGCTask 一次性语义的判据。Estimated 标记该笔金额含估算成分
	// （厂商 usage 缺失或已过查询窗口而记 0 元）。
	CostMicro *int64
	Estimated bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAIGCTask 是 CreateAIGCTask 的入参。大量同型字段，用具名结构体而非位置
// 参数——位置传错不会被编译器发现。错误/usage/产物 URL 不在其中：任务创建于
// 厂商受理成功的瞬间，这些字段生来为空，之后经 UpdateAIGCTaskObserved 回填。
type NewAIGCTask struct {
	ID            string
	VendorTaskID  string
	UpstreamID    int64
	UpstreamName  string
	ModelName     string
	Kind          string
	KeyID         int64
	KeyDisplay    string
	Status        string
	HasVideoInput bool
	GenerateAudio bool
	ServiceTier   string
	ReqResolution string
	ReqDuration   string
	// CreatedAt 零值取当前时间（照 AuditEvent.At 先例；测试与固定时间入库用）。
	CreatedAt time.Time
}

// AIGCObservation 是一次厂商回查观测到的可变字段全量快照。写入语义是整体
// 覆盖：调用方要保留旧值（比如厂商过期后不再回产物 URL）就得自己带上——
// 合并策略归 gateway 适配器，store 只如实落快照。
type AIGCObservation struct {
	Status       string
	ErrorCode    string
	ErrorMessage string
	UsageJSON    string
	ContentURL   string
	LastFrameURL string
}

// CreateAIGCTask 落一条任务行并返回完整行。设备任务 id 重复、或同一上游的
// 同一厂商任务重复（UNIQUE(vendor_task_id, upstream_id)）返回 ErrConflict。
func (s *Store) CreateAIGCTask(ctx context.Context, nt NewAIGCTask) (*AIGCTask, error) {
	at := nt.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	ts := fmtTime(at)
	if _, err := s.stmtCreateAIGCTask.ExecContext(ctx,
		nt.ID, nt.VendorTaskID, nt.UpstreamID, nt.UpstreamName,
		nt.ModelName, nt.Kind, nt.KeyID, nt.KeyDisplay, nt.Status,
		boolToInt(nt.HasVideoInput), boolToInt(nt.GenerateAudio),
		nt.ServiceTier, nt.ReqResolution, nt.ReqDuration, ts, ts); err != nil {
		return nil, fmt.Errorf("创建任务: %w", mapErr(err))
	}
	t, _ := parseTime(ts)
	return &AIGCTask{
		ID: nt.ID, VendorTaskID: nt.VendorTaskID,
		UpstreamID: nt.UpstreamID, UpstreamName: nt.UpstreamName,
		ModelName: nt.ModelName, Kind: nt.Kind,
		KeyID: nt.KeyID, KeyDisplay: nt.KeyDisplay,
		Status:        nt.Status,
		HasVideoInput: nt.HasVideoInput, GenerateAudio: nt.GenerateAudio,
		ServiceTier: nt.ServiceTier, ReqResolution: nt.ReqResolution, ReqDuration: nt.ReqDuration,
		CreatedAt: t, UpdatedAt: t,
	}, nil
}

// GetAIGCTaskByVendorIDForKey 按厂商任务 id + 归属密钥取任务行。归属不符与
// 不存在**同**返回 ErrNotFound——数据面的查询/取消都压在这一条上，不区分两种
// 情况就没有存在性 oracle。跨上游账户撞出同名厂商 id 的病理情形取最新一行。
func (s *Store) GetAIGCTaskByVendorIDForKey(ctx context.Context, vendorTaskID string, keyID int64) (*AIGCTask, error) {
	task, err := scanAIGCTask(s.stmtGetAIGCTaskByVendorForKey.QueryRowContext(ctx, vendorTaskID, keyID))
	if err != nil {
		return nil, fmt.Errorf("查询任务: %w", err)
	}
	return task, nil
}

// UpdateAIGCTaskObserved 把一次厂商回查的观测快照写进任务行，仅在任一观测值
// 变化时才真的写（等值观测零写放大，SD 卡纪律）。返回是否发生写入；行不存在
// （已被清理）也报 false——调用方刚拿着行来回查，两种「没写」都不需要区分。
func (s *Store) UpdateAIGCTaskObserved(ctx context.Context, id string, ob AIGCObservation) (bool, error) {
	res, err := s.stmtUpdateAIGCTaskObserved.ExecContext(ctx,
		ob.Status, ob.ErrorCode, ob.ErrorMessage, ob.UsageJSON, ob.ContentURL, ob.LastFrameURL,
		fmtTime(time.Now()), id,
		ob.Status, ob.ErrorCode, ob.ErrorMessage, ob.UsageJSON, ob.ContentURL, ob.LastFrameURL)
	if err != nil {
		return false, fmt.Errorf("更新任务观测: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("更新任务观测: %w", err)
	}
	return n > 0, nil
}

// PruneAIGCTasks 删除创建时间早于 before 的任务行，返回删除条数（固定宽度
// 时间文本保证 SQL 字符串比较正确）。保留期（14 天）由调用方换算成 before。
// 谓词走全表扫描是**有意的**：行数被保留期钉住（板上规模可忽略），单独给
// created_at 建索引省不下什么、却给每次插入添一份写放大（SD 卡纪律）。
func (s *Store) PruneAIGCTasks(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.stmtPruneAIGCTasks.ExecContext(ctx, fmtTime(before))
	if err != nil {
		return 0, fmt.Errorf("清理过期任务: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理过期任务: %w", err)
	}
	return n, nil
}

// scanAIGCTask 从行扫描 AIGCTask（列序与 aigcTaskColumns 一一对应）；
// 未命中返回 ErrNotFound。
func scanAIGCTask(r rowScanner) (*AIGCTask, error) {
	var (
		task                    AIGCTask
		hasVideoInput, genAudio int
		costMicro               sql.NullInt64
		estimated               int
		created, updated        string
	)
	err := r.Scan(&task.ID, &task.VendorTaskID, &task.UpstreamID, &task.UpstreamName,
		&task.ModelName, &task.Kind, &task.KeyID, &task.KeyDisplay,
		&task.Status, &task.ErrorCode, &task.ErrorMessage, &task.UsageJSON,
		&hasVideoInput, &genAudio, &task.ServiceTier, &task.ReqResolution, &task.ReqDuration,
		&task.ContentURL, &task.LastFrameURL, &costMicro, &estimated, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	task.HasVideoInput = hasVideoInput != 0
	task.GenerateAudio = genAudio != 0
	task.CostMicro = nullableInt64(costMicro)
	task.Estimated = estimated != 0
	if task.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("created_at 非法: %w", err)
	}
	if task.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("updated_at 非法: %w", err)
	}
	return &task, nil
}
