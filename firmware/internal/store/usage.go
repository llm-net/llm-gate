package store

// 用量计量的仓储层（iteration-9 Phase 1）：小时聚合表 usage_hourly 的写入/
// 读取/保留清理，以及 aigc_tasks 的一次性清算。约定同 repo.go：context 化、
// 走 prepared statement、未命中 → ErrNotFound、唯一性冲突 → ErrConflict。
//
// 三条与本包其余部分不同的纪律，都是有意的：
//
//   - **时间用整数桶号，不用文本时间戳**。usage_hourly.bucket_hour 是
//     unix 秒 / 3600（恒 UTC）：读数要做区间扫描与本地时区折算（预算窗口跟
//     设备本地钟表走），整数最直接。表内没有逐条时间戳，所以「字符串比较即
//     时间比较」的既有约定面不受影响。区间参数一律**左闭右开** [from, to)。
//   - **写入是相加而非覆盖**。AddUsageDeltas 落的是增量，UPSERT 的 DO UPDATE
//     恒为 `列 = 列 + excluded.列`：同一维度键在同一小时内多次冲刷会累加。
//     所以调用方冲刷成功后必须清掉自己那份 delta，重放等于重复计数。
//   - **cost_micro 不设 CHECK (>= 0)，但落库的增量事实上恒非负**。写入侧的
//     每一笔金额都出自 usage.Cost，它末端过 clampCost 钳在 [0, 上限]；长流的
//     30s 中间结算（provisional）与流终冲正**只动内存里的预算计数器，一个字节
//     都不进本表**（iteration-9 Phase 3 落地时定死的）。所以库里出现负的
//     cost_micro 是**缺陷信号**，不是正常写入形态——不加 CHECK 只是因为
//     UPSERT 的 `列 = 列 + excluded.列` 让约束违例报在一个难以归因的地方，
//     不是因为负值合法。同一句话在 0006 迁移的列注释里。
//
// §15.1 边界：本文件入库的只有计数、金额、时长与标识（模型名、上游账户名、
// 用户名、密钥展示串）。提示词、响应、任何内容片段、完整密钥一律不进本表，
// 调用方不得把它们塞进任何维度字段。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// usageDimColumns 是 usage_hourly 的维度列（前 5 项即唯一索引的键，kind 由
// entry 唯一决定故不在键内）。
const usageDimColumns = `bucket_hour, key_id, model_name, upstream_name, entry,
	key_display, kind`

// usageCounterColumns 是 usage_hourly 的计数列（AddUsageDeltas 对每一列做
// `列 = 列 + excluded.列`）。
const usageCounterColumns = `requests, errors, rejected_requests, estimated_requests, unavailable_requests,
	prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens, total_tokens,
	video_seconds, image_count,
	cost_micro, duration_ms_sum`

// usageColumns 是全列列表，与 usageArgs / scanUsageRow 的顺序一一对应
// （改任一处必须同步另外两处）。
const usageColumns = usageDimColumns + `,
	` + usageCounterColumns

// UsageDelta 是「一个小时桶 × 一组维度」的用量增量。计数字段是**增量**语义：
// AddUsageDeltas 把它们加到已有行上（新行则以增量为初值）。
//
// 维度快照随行留存，主体被删之后历史账仍可读——本表对 api_keys/models/
// upstreams 一律不设外键（同 audit_events 与 aigc_tasks 的先例）。两类快照的
// 时效性**不一样**，别混为一谈：
//
//   - KeyDisplay 恒定：密钥展示串（前缀…末4位）建后不可改，所以它永不过期。
//   - ModelName / UpstreamName 是**记账时点**的名字：RenameModel 与
//     UpdateUpstream 都能改名，改名之后新行用新名、旧行留旧名，历史因此按名字
//     分成两段。这是有意的（账要还原当时的事实），不是失真；读数端若要合并，
//     得由人来认这两个名字是同一个对象。
//
// 已知边界（iteration-9 Phase 1 记，留给后续迭代裁决）：KeyID 是裸值且
// api_keys 的主键是不带 AUTOINCREMENT 的 INTEGER PRIMARY KEY——删掉表中 id
// 最大的那把密钥后，下一把新签的密钥会**复用**同一个 id，从而继承前任在本表
// 里的历史。见 usage_test.go 的 TestUsageIDReuseInheritsHistory。
type UsageDelta struct {
	// BucketHour 是 unix 秒 / 3600（UTC）。
	BucketHour int64
	KeyID      int64
	// ModelName 是客户端可见的原始模型名；UpstreamName 是上游账户名快照，
	// 准入被拒（429）的样本没走到选路，此项为空串。
	ModelName    string
	UpstreamName string
	// Entry 是数据面入口（chat|messages|video|image），Kind 是模型 kind
	// （text|video|image）。二者不设枚举校验——词汇增补不该要求一次迁移。
	Entry      string
	KeyDisplay string
	Kind       string

	Requests int64
	// Errors 是客户端可见最终状态 ≥400 的请求数（口径比"服务端故障"宽：
	// 4xx 也算，用量页要的是"这些请求没换来结果"）。
	Errors int64
	// RejectedRequests 是被网关准入拦下的请求数（429）：它们没到上游，单列
	// 统计，绝不混进 Errors。
	RejectedRequests int64
	// EstimatedRequests 是 token 含估算成分的请求数。
	EstimatedRequests   int64
	UnavailableRequests int64

	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64

	// VideoSeconds / ImageCount 是**非 token 形态**的计费量（0007 加列）：
	// 秒是 MiniMax H3 的权威计价量（输出秒 + 输入秒，同一单价故合成一列），
	// 张数是 H3 的输入参考图或 Seedream 的出图（一行只可能是其中一种，由
	// kind + 上游账户名区分）。文本行两列恒为 0。
	VideoSeconds int64
	ImageCount   int64

	// CostMicro 是按记账时点目录价折算的消费额，int64 微元（1 元 = 10⁶ 微元）。
	// 冲正场景下可以为负，见文件头注释。
	CostMicro int64
	// DurationMsSum 是本桶内请求耗时之和（求均值用；峰值不存）。
	DurationMsSum int64
}

// UsageRow 是 usage_hourly 的一行读数：字段与 UsageDelta 逐个同名同序（列序与
// 扫描器因此不可能走偏），但语义是**累计值**而非增量。
//
// 刻意用**定义类型而非类型别名**：别名会让 `AddUsageDeltas(ctx, 读回来的行)`
// 编译通过，而那一句会把整段历史当成增量再加一遍——账目直接翻倍且无从察觉。
// 定义类型让编译器挡住它；真要互转就显式写 UsageDelta(row)，那时人是清醒的。
type UsageRow UsageDelta

// UsageCostBucket 是 SumUsageSince 的一行：某个小时桶里某把密钥的消费额合计。
// 启动播种预算计数器用——把 UTC 桶折进设备本地时区的当日/当月窗口是调用方
// （internal/usage）的事，本层只如实按桶给出金额。
type UsageCostBucket struct {
	BucketHour int64
	KeyID      int64
	CostMicro  int64
}

// usageArgs 把一条增量摊成 SQL 参数（顺序与 usageColumns 一一对应）。
func usageArgs(d UsageDelta) []any {
	return []any{
		d.BucketHour, d.KeyID, d.ModelName, d.UpstreamName, d.Entry,
		d.KeyDisplay, d.Kind,
		d.Requests, d.Errors, d.RejectedRequests, d.EstimatedRequests, d.UnavailableRequests,
		d.PromptTokens, d.CompletionTokens, d.CacheReadTokens, d.CacheWriteTokens, d.TotalTokens,
		d.VideoSeconds, d.ImageCount,
		d.CostMicro, d.DurationMsSum,
	}
}

// scanUsageRow 从行扫描一行读数（列序与 usageColumns 一一对应）。
func scanUsageRow(r rowScanner) (UsageRow, error) {
	var d UsageRow
	err := r.Scan(&d.BucketHour, &d.KeyID, &d.ModelName, &d.UpstreamName, &d.Entry,
		&d.KeyDisplay, &d.Kind,
		&d.Requests, &d.Errors, &d.RejectedRequests, &d.EstimatedRequests, &d.UnavailableRequests,
		&d.PromptTokens, &d.CompletionTokens, &d.CacheReadTokens, &d.CacheWriteTokens, &d.TotalTokens,
		&d.VideoSeconds, &d.ImageCount,
		&d.CostMicro, &d.DurationMsSum)
	return d, err
}

// AddUsageDeltas 在**单个事务**里把一批增量累加进 usage_hourly（每 5 分钟一次
// 冲刷的落点）。同一维度键重复出现即依次相加，与「同一维度键分两次冲刷」等价。
// 空切片是合法的空操作（无增量的那一轮整轮跳过，连事务都不开）。
//
// 调用方在本方法返回 nil 之后才可以清掉自己那份内存 delta：失败时整批回滚，
// 保留 delta 下轮重试只会得到同一个结果；而成功后重放会**重复计数**。
func (s *Store) AddUsageDeltas(ctx context.Context, deltas []UsageDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("累加用量: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是空操作
	stmt := tx.StmtContext(ctx, s.stmtAddUsageDelta)
	for _, d := range deltas {
		if _, err := stmt.ExecContext(ctx, usageArgs(d)...); err != nil {
			return fmt.Errorf("累加用量: %w", mapErr(err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("累加用量: %w", err)
	}
	return nil
}

// QueryUsageRange 返回 [fromBucket, toBucket) 内的全部聚合行，按桶号升序
// （管理面 /admin/v1/usage 的口径）。上层（internal/usage）在内存里做维度分解
// 与按日折算——把「按什么分组」留在一处，SQL 面只负责取回区间。
func (s *Store) QueryUsageRange(ctx context.Context, fromBucket, toBucket int64) ([]UsageRow, error) {
	rows, err := s.stmtQueryUsageRange.QueryContext(ctx, fromBucket, toBucket)
	if err != nil {
		return nil, fmt.Errorf("查询用量区间: %w", err)
	}
	return collectUsageRows(rows)
}

// collectUsageRows 把结果集扫成读数切片。
func collectUsageRows(rows *sql.Rows) ([]UsageRow, error) {
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		d, err := scanUsageRow(rows)
		if err != nil {
			return nil, fmt.Errorf("查询用量区间: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("查询用量区间: %w", err)
	}
	return out, nil
}

// SumUsageSince 按 (桶号, 密钥) 汇总 bucket_hour >= sinceBucket 的消费额，
// 桶号升序。gatewayd 启动时用它播种预算计数器：调用方按设备本地时区把桶折进
// 当日/当月窗口，所以这里必须保留桶时刻而不是直接给总额。
//
// 调用方要处理的两件事（都不在本层）：
//   - **半小时偏移时区**（+5:30/+5:45/+9:30 等）的本地零点落在桶中间，取整方向
//     决定 ≤1 小时的偏差方向，见 0006 迁移里 bucket_hour 的注释；
//   - **保留期必须覆盖窗口**：usage_days 若短于 31 天，月预算播不出完整的自然月，
//     重启即缩水（且只会变松不会变紧）。
func (s *Store) SumUsageSince(ctx context.Context, sinceBucket int64) ([]UsageCostBucket, error) {
	rows, err := s.stmtSumUsageSince.QueryContext(ctx, sinceBucket)
	if err != nil {
		return nil, fmt.Errorf("汇总用量: %w", err)
	}
	defer rows.Close()
	var out []UsageCostBucket
	for rows.Next() {
		var b UsageCostBucket
		if err := rows.Scan(&b.BucketHour, &b.KeyID, &b.CostMicro); err != nil {
			return nil, fmt.Errorf("汇总用量: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("汇总用量: %w", err)
	}
	return out, nil
}

// PruneUsage 删除桶号 < beforeBucket 的聚合行，返回删除条数（保留期
// usage_days 由调用方换算成桶号）。谓词直接命中唯一索引的前导列。
func (s *Store) PruneUsage(ctx context.Context, beforeBucket int64) (int64, error) {
	res, err := s.stmtPruneUsage.ExecContext(ctx, beforeBucket)
	if err != nil {
		return 0, fmt.Errorf("清理过期用量: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("清理过期用量: %w", err)
	}
	return n, nil
}

// ---- aigc_tasks 的清算 ----

// ListUnsettledAIGCTasks 列出尚未清算（cost_micro IS NULL）且 updated_at 早于
// olderThan 的任务行，按创建时间升序，至多 limit 条；limit ≤ 0 返回空
// （挡住「负 LIMIT = 不设限」这个 SQL 陷阱）。懒对账协程据此
// 补记「客户端提交完就不再轮询」的任务。
//
// **olderThan 判的是「多久没观测到变化」，不是「多久没被轮询」**——这条区别
// 是硬的，别按后者写调用方：updated_at 只在 UpdateAIGCTaskObserved 发现观测值
// **真的变了**时才写（迭代 8 的等值观测零写放大，SD 卡纪律）。所以一个客户端
// 每秒都在轮询、但状态一直停在 running 的任务，在这里照样是「陈旧」的。
// 由此产生的重复回查由 limit 与调用方的节奏兜住，不靠这个谓词。
//
// 本层刻意**不解读 status**（状态词汇归 gateway 适配器，同 aigc.go 的既有边界）：
// 「完成态直接按 usage 清算」还是「开放态先回查上游」由调用方按状态裁决。
//
// limit 同时是三重闸门：内存（读回的行全在切片里）、上游配额（每行一次回查）、
// 以及**升级后的对账洪峰**——0006 把存量任务行一律标成未清算，保留期内的旧任务
// 会在第一轮全部符合条件。
func (s *Store) ListUnsettledAIGCTasks(ctx context.Context, olderThan time.Time, limit int) ([]AIGCTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.stmtListUnsettledAIGCTasks.QueryContext(ctx, fmtTime(olderThan), limit)
	if err != nil {
		return nil, fmt.Errorf("列出未清算任务: %w", err)
	}
	defer rows.Close()
	var tasks []AIGCTask
	for rows.Next() {
		task, err := scanAIGCTask(rows)
		if err != nil {
			return nil, fmt.Errorf("列出未清算任务: %w", err)
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出未清算任务: %w", err)
	}
	return tasks, nil
}

// SettleAIGCTask 把一笔金额清算进任务行，**一次性语义**：条件更新只在
// cost_micro IS NULL 时命中，已清算的行再清算是空操作并返回 false。
//
// 这一条是防重复入账的全部依赖：客户端查询路径观测到任务完成时会清算，懒对账
// 协程扫到同一行时也会清算，两条路径天然重叠——把「只算一次」压在数据库的
// 条件更新上，比在应用层加锁更靠得住（行不存在，比如已过保留期被清掉，同样
// 返回 false）。
//
// 刻意不动 updated_at：那一列的语义是「最近一次**观测**厂商」，清算是本地记账
// 动作，改它会让 ListUnsettledAIGCTasks 的新鲜度判据失真。
//
// **只清算、不入账**：金额落进任务行，但没有任何一行 usage_hourly 增量随之
// 产生。生产路径请一律用 SettleAIGCTaskWithUsage——两个动作分成两个事务，中间
// 崩溃那笔钱既没进账本、行又已标清算，对账协程再也扫不到它。本方法保留给
// 「确实只要标记不要入账」的场景（测试、将来的人工冲销）。
func (s *Store) SettleAIGCTask(ctx context.Context, id string, costMicro int64, estimated bool) (bool, error) {
	res, err := s.stmtSettleAIGCTask.ExecContext(ctx, costMicro, boolToInt(estimated), id)
	if err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	return n > 0, nil
}

// SettleAIGCTaskWithUsage 在**单个事务**里做完清算的两件事：条件清算任务行
// （同 SettleAIGCTask 的一次性语义）与把同一笔账作为一条增量累加进
// usage_hourly。返回是否真的清算了本次调用（false = 行已清算过或已不存在，
// 此时**不写任何增量**）。
//
// 为什么必须同一个事务：两个分开的事务之间有一道窗口，进程在窗口里退出的那笔
// 钱**永久丢失且再也扫不到**——行已标清算，ListUnsettledAIGCTasks 的谓词
// （cost_micro IS NULL）从此过滤掉它，没有任何一条路径会回来补账。反过来的
// 顺序（先入账后清算）则会在同一窗口里重复入账。放进一个事务，两种错都没了：
// 要么这笔账既在任务行上也在账本里，要么两处都没有、下一轮重来。
//
// delta 的 CostMicro 与 costMicro 是**同一笔钱的两个落点**，调用方负责让它们
// 一致（本方法不代填：增量还带着维度快照与各计数列，那些只有调用方知道）。
//
// 异步任务的清算增量通常**不带 requests 计数**：那次调用在提交的瞬间就已经
// 被访问日志记过一笔了，清算只是把金额与计费量（token / 秒 / 张）补上；
// requests 留零的增量行仍会落，那一行同时是「这个任务被清算过」的账本侧痕迹。
func (s *Store) SettleAIGCTaskWithUsage(ctx context.Context, id string, costMicro int64, estimated bool, delta UsageDelta) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Commit 成功后 Rollback 是空操作
	res, err := tx.StmtContext(ctx, s.stmtSettleAIGCTask).ExecContext(ctx, costMicro, boolToInt(estimated), id)
	if err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	if n == 0 {
		// 已清算过或行已不在：整个事务作废，绝不落增量（这正是重复入账的
		// 入口）。回滚由 defer 完成。
		return false, nil
	}
	if _, err := tx.StmtContext(ctx, s.stmtAddUsageDelta).ExecContext(ctx, usageArgs(delta)...); err != nil {
		return false, fmt.Errorf("清算任务: %w", mapErr(err))
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("清算任务: %w", err)
	}
	return true, nil
}

// ---- 计价所需的窄读数 ----

// GetModelPricing 按模型名取目录价 JSON（懒对账「清算时点价」的读取口）；
// 模型已被删除返回 ErrNotFound。
//
// 刻意只回 pricing 一列而不走 ResolveModelRoute：清算是一条后台协程，它需要
// 的是一个价签，不该顺手把上游凭证明文拉进自己的栈上（路由视图的明文只允许
// 流向出站请求头，见包文档的 §15.1 纪律）。
func (s *Store) GetModelPricing(ctx context.Context, name string) (string, error) {
	var pricing string
	err := s.stmtGetModelPricing.QueryRowContext(ctx, name).Scan(&pricing)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("查询模型目录价: %w", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("查询模型目录价: %w", err)
	}
	return pricing, nil
}

// ListModelPricing 返回整份目录的「模型名 → 目录价原文」（空串 = 未定价）。
// 用量页的「未定价」徽章据此判定：**读目录，不从金额反推**——一个只被
// count_tokens 打过的已定价模型金额同样是 0，而窗口内混着一笔已定价调用的
// 未定价模型金额又大于 0，两个方向都会误判。
//
// 与 GetModelPricing 同样只取名字与价签两列：这份读数要服务 member 视角的
// /me/usage，绝不能顺手把来源与凭证拉进来。目录规模是个位数到几十行，全表
// 一次比逐名点查省事，也比走管理视图（ModelWithSources 带上游名与 last4）
// 安全。
func (s *Store) ListModelPricing(ctx context.Context) (map[string]string, error) {
	rows, err := s.stmtListModelPricing.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("列出模型目录价: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, pricing string
		if err := rows.Scan(&name, &pricing); err != nil {
			return nil, fmt.Errorf("列出模型目录价: %w", err)
		}
		out[name] = pricing
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出模型目录价: %w", err)
	}
	return out, nil
}

// ---- 定价 JSON 的形态无关校验 ----

// MaxPricingMicro 是单个价格字段的量级闸门：10¹² 微元（= 100 万元 / 计价单位，
// 真实价签在 10⁶–5×10⁷ 量级，留了五个数量级余量）。
//
// 它防的不是「录错价」，而是**聚合列的 int64 溢出**：cost_micro 的 UPSERT 是
// `列 = 列 + excluded.列`，一旦某次相加溢出，SQLite 会把该列悄悄转成 REAL，
// 之后整段区间的读数都扫不进 int64 而整体报错——一个手滑多打几位的价格就能
// 让用量页从此打不开。上限只在形态无关这一层，与字段名、kind 都无关。
const MaxPricingMicro int64 = 1_000_000_000_000

// validatePricing 校验 models.pricing 的**形态无关**不变量：空串（未定价）放行，
// 否则必须是一个 JSON 对象，且每个值都是非负整数（int64 内）。
//
// 分工刻意如此：形态字段集（文本三价 / Seedance 两档 / H3 秒价档+附加 / 图片
// 张价）按模型 kind 校验，那是管理层的事；本层只保证「凡是入库的 pricing，
// 计价函数拿到的必然是一张扁平的非负整数表」——于是 internal/usage 的换算不必
// 为脏数据准备防御分支，全程 int64 无浮点也就有了前提。
func validatePricing(pricing string) error {
	if pricing == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(pricing))
	dec.UseNumber()
	// 解进 map[string]any（**不是** map[string]json.Number）：json.Number 的
	// 底层类型是 string，直接以它为目标时 encoding/json 会把带引号的 "1000"
	// 也照收——而管理 API 的契约是整数收发，加引号的价格必须当错误挡掉。
	// 目标为 any 时字符串解成 string、数字才解成 json.Number，两者泾渭分明。
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		// 只带解析器的位置/类型说明，不回显原文——原文是调用方给的请求体。
		return fmt.Errorf("%w: 必须是「字段名 → 非负整数微元」的 JSON 对象: %v", ErrInvalidPricing, err)
	}
	if len(raw) == 0 {
		// 字面量 null 解进 nil map；{} 则是「定了价却一个字段都没有」——它会
		// 变成永远记 0 元、却又不挂「未定价」警示徽章的隐身状态。两者都不是
		// 价格表，一律要求用空串表达「未定价」。
		return fmt.Errorf("%w: 必须是非空 JSON 对象（未定价请用空串）", ErrInvalidPricing)
	}
	// 尾随内容必须用 Token() 判，**不能用 dec.More()**：More 的实现是
	// `c != ']' && c != '}'`，于是 `{"in":1}}` / `{"in":1}]` 这类多一个闭括号的
	// 手输错误会被它判成「没有更多内容」而放行（实测确认）。Token 读到 io.EOF
	// 才是真的干净结尾。
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: JSON 后有多余内容", ErrInvalidPricing)
	}
	for field, val := range raw {
		name := clipFieldName(field)
		num, ok := val.(json.Number)
		if !ok {
			return fmt.Errorf("%w: 字段 %s 必须是整数微元（不接受字符串、对象、数组或 null）", ErrInvalidPricing, name)
		}
		v, err := num.Int64()
		if err != nil {
			return fmt.Errorf("%w: 字段 %s 必须是整数微元（小数、科学计数与越界值不收）", ErrInvalidPricing, name)
		}
		if v < 0 {
			return fmt.Errorf("%w: 字段 %s 不得为负", ErrInvalidPricing, name)
		}
		if v > MaxPricingMicro {
			return fmt.Errorf("%w: 字段 %s 超出上限 %d 微元", ErrInvalidPricing, name, MaxPricingMicro)
		}
	}
	return nil
}

// maxPricingFieldNameInError 是错误文本里回显字段名的长度上限（rune）。
const maxPricingFieldNameInError = 32

// clipFieldName 把字段名裁成可安全进错误文本（进而进日志）的形态。
//
// 字段名是**调用方给的请求体内容**，而本包的错误文本按包文档只带约束名与列名、
// 不回显参数值。但完全不说是哪个字段，管理员就没法改错——所以取中间：截断到
// 定长并用 %q 转义（控制字符与引号都会被转成转义序列），把「无界的请求体片段
// 流进 journal」压成一个定长、可打印的定位提示。
func clipFieldName(field string) string {
	runes := []rune(field)
	if len(runes) > maxPricingFieldNameInError {
		return fmt.Sprintf("%q…", string(runes[:maxPricingFieldNameInError]))
	}
	return fmt.Sprintf("%q", field)
}
