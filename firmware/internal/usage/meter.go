package usage

// meter.go 是内存聚合器：请求路径唯一的接触点（Record）、预算计数器、环形
// 缓冲，以及把内存增量批量落盘的后台协程（Run）。设计照 sysinfo.Recorder 的
// 先例裁衣——高频层只在内存，落盘走定时批量追加，SD 卡写放大是第一约束。
//
// 时间的两套坐标，别混用：
//
//	存储侧  桶号 = unix 秒 / 3600，**恒 UTC**（usage_hourly.bucket_hour）。
//	预算侧  自然日 / 自然月，**跟设备本地时区**（管理员的心智模型是「这个月的钱」）。
//
// # 播种的取整方向（Phase 1 handoff ④ 指定在此裁决）
//
// 启动播种要把 UTC 小时桶折进本地的当日/当月窗口。本包的裁决是
// **按桶的起始时刻归属整只桶**：bucket 归入 time.Unix(bucket*3600,0) 所在的
// 本地日/月，不做任何按比例拆分。
//
//   - 目标部署 Asia/Shanghai(+8) 是整点偏移，本地零点恰好落在桶边界上，
//     **精确无偏**。
//   - +5:30 / +5:45 / +9:30 这类半小时偏移时区的本地零点落在桶中间，跨界那只
//     桶整只算给**较早**的那一天：今天头半小时的消费被记进昨天，于是当日计数
//     ≤1 小时的量偏**少**。方向是有意选的——「少记 → 预算变松」与掉电丢一个
//     冲刷窗口同向（方案 §3.2 的方向安全论证），而反过来（把昨天末尾算进今天）
//     会让预算凭空变紧，且那笔多算的钱不会随时间滑出窗口。
//   - 真要做到半小时时区也精确，得改桶粒度（一次前向迁移 + 一倍行数），
//     本迭代不做。

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	// FlushInterval 是内存增量批量落盘的节奏。对照 sysinfo.Recorder 的 10 分钟
	// 归档，5 分钟是「预算播种精度」与「写量」的折中。代码常量，不进配置——
	// 没有一个需要现场调它的理由，配置面每多一项都是长期支持成本。
	FlushInterval = 5 * time.Minute
	// ringSize 是请求级明细环形缓冲的容量。持久层只有小时聚合，明细只答
	// 「刚才发生了什么」，重启即失（方案 §3.3）。
	ringSize = 256
	// flushTimeout 是优雅停机那一次收尾冲刷的预算：父 ctx 已取消，用
	// WithoutCancel 另起一个有界的。
	flushTimeout = 5 * time.Second
	// dayLayout / monthLayout 是预算窗口的键（本地时区）。定长文本，比较即
	// 判等，跨日/跨月一目了然。周窗口没有自己的 layout：它的键是所在周
	// **周一的日期**（dayLayout 格式），见 windowKeys。
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
	// unknownModelsPerFlush 是**一个冲刷窗口内**允许进账本的「目录里没有这一行」
	// 的不同模型名个数上限（见 addDeltaLocked）。
	unknownModelsPerFlush = 50
)

// UnknownModelDim 是超出 unknownModelsPerFlush 之后，账本给「目录里没有」的
// 模型名并用的哨兵维度。串里**有空格**是有意的：管理端点的 validateCatalogName
// 拒收含空白的目录名，所以真模型永远不可能叫这个名字，两者不会撞成一行。
const UnknownModelDim = "(未知模型 · 已合并)"

// Meter 是用量计量的中枢：gateway 往里 Record，admin 从里读数，Run 负责落盘、
// 保留清理与懒对账。零值不可用，必须经 NewMeter 构造；方法均并发安全。
type Meter struct {
	st           *store.Store
	log          *slog.Logger
	loc          *time.Location
	days         int
	now          func() time.Time
	prober       TaskProber
	cursorPrices CursorPriceSource

	// seedMu 只护播种状态（seeded / seedWarned），与 mu 是两把锁：播种本身
	// 要拿 mu 写计数器，合成一把就会自锁。取锁顺序恒为 seedMu → mu。
	seedMu     sync.Mutex
	seeded     bool
	seedWarned bool

	mu sync.Mutex
	// deltas 是尚未落盘的小时桶增量（键即 usage_hourly 的唯一索引六列）。
	deltas map[deltaKey]*store.UsageDelta
	// unknownModels 是本冲刷窗口内已经放行过的「目录里没有」的模型名集合，
	// 账本维度的基数闸门（见 addDeltaLocked）。随每次冲刷清零。
	unknownModels map[string]struct{}
	// ring 是请求级明细环（容量 ringSize），ringNext 是下一个写入槽——
	// 满环之后写指针绕回，恒不搬动已有条目。读序由 recent 还原成「新的在前」。
	ring     []Event
	ringNext int
	// spendKey 是每把密钥的预算计数器（本地自然日 / 自然周 / 自然月累计
	// 消费额）。
	spendKey map[int64]*spend
	// allowance 是每把密钥的按量额度运行态：最近一次准入看见的预算快照，
	// 以及已经判定应扣、尚未批量落库的金额。
	allowance map[int64]*allowanceState

	// lastPruneDay / settleWarned 正常只由 Run 那一个协程读写，仍放在锁下：
	// 「这个字段只有一个协程碰」是**约定**不是**约束**，而它成本为零。
	lastPruneDay string
	// settleWarned 压缩懒对账的失败告警（照 Recorder 的 warn/recovered 模式，
	// 只在状态翻转时各记一条，不每 5 分钟刷屏）。
	settleWarned bool
	// wroteLedger 记「本进程已经往账本里写过增量」。它是播种重试的截止闸门：
	// 库里一旦混进本进程自己记过的账，再播种就是把这部分加第二遍（见 Seed）。
	// **两条写入路径都要标**：定时冲刷（flush）与懒对账的清算
	// （SettleAIGCTaskWithUsage 在同一事务里落 delta，不经过 flush）。
	wroteLedger bool
	// settleProbeFails / settleSkipUntil 是懒对账的坏行降级状态（见 settle.go）：
	// 连续回查失败到阈值的任务进冷却，冷却期内整轮跳过，到期后重新尝试。
	settleProbeFails map[string]int
	settleSkipUntil  map[string]time.Time
	// rpm 是每把密钥的 60s 滑动窗口计数器（见 admit.go）。纯内存、重启清零：
	// 重启后短暂地放宽一分钟的速率，方向与整个计量层一致（宁松勿紧）。
	rpm map[int64]*rpmWindow
}

// deltaKey 是小时桶增量的聚合键，与 usage_hourly 的唯一索引**逐列一致**
// （bucket_hour, key_id, model_name, upstream_name, entry）。
// kind 不在键内：它由 entry 唯一决定。
type deltaKey struct {
	bucket   int64
	keyID    int64
	model    string
	upstream string
	entry    string
}

// spend 是一把密钥的预算计数器。窗口键随时钟翻转，翻转即清零
// 重计——不用后台定时器，读写时顺手对一下键就够了（懒翻转，进程睡着也不会
// 把昨天的额度带进今天）。
type spend struct {
	dayKey    string
	dayMicro  int64
	weekKey   string
	weekMicro int64
	monKey    string
	monMicro  int64
}

// allowanceState 是一把密钥的按量额度内存态。数据库列
// api_keys.metered_allowance_micro 是剩余额的唯一权威；这里仅保存：
//
//   - day/week/month + known：最近一次 Admit 带回的预算快照。Record 与后台
//     任务清算不为限额另开查询；进程启动后还没见过准入时不扣，方向宁松勿紧。
//   - pending：预算用尽后已经消费、等待与账本同节奏落库的扣减额。准入看到的
//     实际可用额度 = 鉴权点查的数据库剩余 - pending。
type allowanceState struct {
	day, week, month *int64
	known            bool
	pending          int64
}

func (m *Meter) allowanceLocked(keyID int64) *allowanceState {
	if keyID <= 0 {
		return nil
	}
	a := m.allowance[keyID]
	if a == nil {
		a = &allowanceState{}
		m.allowance[keyID] = a
	}
	return a
}

// roll 把计数器对齐到给定窗口键，跨窗口即清零。
func (s *spend) roll(dayKey, weekKey, monKey string) {
	if s.dayKey != dayKey {
		s.dayKey, s.dayMicro = dayKey, 0
	}
	if s.weekKey != weekKey {
		s.weekKey, s.weekMicro = weekKey, 0
	}
	if s.monKey != monKey {
		s.monKey, s.monMicro = monKey, 0
	}
}

// windowKeys 折出三个预算窗口的键（本地时区）：自然日、自然周、自然月。
// 周键取所在周**周一的日期**文本——跨周即键变，翻转逻辑与日/月完全同构。
func windowKeys(local time.Time) (day, week, mon string) {
	return local.Format(dayLayout), weekStart(local).Format(dayLayout), local.Format(monthLayout)
}

// weekStart 是 local 所在自然周的起点：**周一** 00:00（本地时区，ISO 口径，
// 与国内「周一开始的一周」一致）。刻意不做「每周从哪天起算」的配置项——
// 那个旋钮会让同一台设备的历史预算语义随现场设置漂移。
func weekStart(local time.Time) time.Time {
	off := (int(local.Weekday()) + 6) % 7 // Monday=0 … Sunday=6
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).AddDate(0, 0, -off)
}

// Spend 是一次准入/展示要用的预算已用额快照（微元）。
type Spend struct {
	DayMicro   int64 `json:"day_micro"`
	WeekMicro  int64 `json:"week_micro"`
	MonthMicro int64 `json:"month_micro"`
	// MeteredAllowancePendingMicro 是已经判定从按量额度扣、尚未写回数据库的
	// 金额。管理 API 用 `库中剩余 - 本值` 展示即时剩余，避免最多五分钟虚高。
	MeteredAllowancePendingMicro int64 `json:"metered_allowance_pending_micro"`
}

// NewMeter 构造聚合器。loc 为 nil 时取 time.Local（预算窗口跟设备本地钟表）；
// days 是 usage_hourly 的保留天数（config.UsageDaysOrDefault，恒 ≥ 31）。
// prober 是懒对账现查厂商的出口，nil = 不接线（见 settle.go）。
func NewMeter(st *store.Store, days int, loc *time.Location, prober TaskProber, logger *slog.Logger) *Meter {
	if loc == nil {
		loc = time.Local
	}
	if days < 1 {
		days = 1
	}
	return &Meter{
		st:               st,
		log:              logger.With("srv", "usage"),
		loc:              loc,
		days:             days,
		now:              time.Now,
		prober:           prober,
		deltas:           make(map[deltaKey]*store.UsageDelta),
		unknownModels:    make(map[string]struct{}),
		spendKey:         make(map[int64]*spend),
		allowance:        make(map[int64]*allowanceState),
		settleProbeFails: make(map[string]int),
		settleSkipUntil:  make(map[string]time.Time),
		rpm:              make(map[int64]*rpmWindow),
	}
}

// Record 记一次消费。数据面在请求收尾时调一次（访问日志中间件同点），
// 懒对账在任务清算后走 recordSettled——两者的区别只在「增量是否已由 store
// 在同一事务里落过」。
//
// 计价在此完成（记账时点价）：Pricing 解析失败按未定价处理并告警，绝不让
// 一张坏价目表把请求路径带崩。
func (m *Meter) Record(s Sample) {
	m.observe(s, m.cost(s), true)
}

// observe 是 Record 与懒对账清算的公共体。两个参数都是「谁算的谁负责」：
//
//   - cost 由**调用方**算定。清算路径的金额出自 taskCost（任务形态、清算
//     时点价、还带着「厂商 usage 不可得就记 0」的判定），让这里再算一遍
//     就是同一笔账两套口径。
//   - addDelta=false 跳过小时桶累加：清算的增量已经由
//     SettleAIGCTaskWithUsage 在同一事务里落过库，往内存 delta 里再加一遍
//     就是双重记账。
func (m *Meter) observe(s Sample, cost int64, addDelta bool) {
	at := s.At
	if at.IsZero() {
		at = m.now()
	}
	// Cursor 透明面看不到实际模型，只知道 RPC 方法。这里再做一次入口级收敛，
	// 保证未来新增调用方即使误传 RPC 名，也不会重新污染模型维度与明细环。
	s.ModelName = canonicalModelDimension(s.Entry, s.ModelName)
	kind := s.kind()
	q := s.quantities()
	ev := Event{
		At:    at,
		KeyID: s.KeyID, KeyDisplay: s.KeyDisplay,
		ModelName: s.ModelName, Upstream: s.UpstreamName,
		Entry: s.Entry, Kind: kind,
		Status: s.Status, Attempts: s.Attempts,
		PromptTokens: q.tokens.Prompt, CompletionTokens: q.tokens.Completion,
		TotalTokens: q.total, VideoSeconds: q.seconds, ImageCount: q.images,
		CacheReadTokens: q.tokens.CacheRead, CacheWriteTokens: q.tokens.CacheWrite, UsageUnavailable: s.UsageUnavailable,
		CostMicro:  cost,
		DurationMs: s.DurationMs, Estimated: s.Estimated,
		Rejected: s.Rejected, RejectReason: s.RejectReason, TaskID: s.TaskID,
	}

	local := at.In(m.loc)
	dayKey, weekKey, monKey := windowKeys(local)

	m.mu.Lock()
	defer m.mu.Unlock()
	if addDelta {
		m.addDeltaLocked(s, at, kind, q, cost)
	}
	// 扣减判定必须在把本笔金额并进预算计数器之前做：跨过预算线的末笔沿用
	// 既有宽松语义，不扣按量额度；从下一笔开始整笔扣。
	m.drainMeteredAllowanceLocked(s.KeyID, cost, dayKey, weekKey, monKey)
	m.addSpendLocked(s.KeyID, cost, dayKey, weekKey, monKey)
	m.pushEventLocked(ev)
}

// drainMeteredAllowanceLocked 在记账前任一预算已经用尽时，把本笔最终金额整笔
// 计入按量额度待扣。临时流式估算不走 Record，因而不会落进这里；它终局会先
// 冲正，再用真实金额调用 Record，避免需要一条不安全的“退回额度”路径。
func (m *Meter) drainMeteredAllowanceLocked(keyID, cost int64, dayKey, weekKey, monKey string) {
	if keyID <= 0 || cost <= 0 {
		return
	}
	a := m.allowance[keyID]
	if a == nil || !a.known {
		return
	}
	day, week, mon := rollSpend(m.spendKey[keyID], dayKey, weekKey, monKey)
	if !exhausted(a.day, day) && !exhausted(a.week, week) && !exhausted(a.month, mon) {
		return
	}
	if a.pending > math.MaxInt64-cost {
		a.pending = math.MaxInt64
		return
	}
	a.pending += cost
}

// addDeltaLocked 把样本累加进它所属的小时桶增量。
//
// 计数口径（与 0006 迁移的列注释一致）：
//   - Requests 恒 +1，**含**被准入拒绝的请求（「请求 − 被拒 = 真正转发出去的」）。
//   - Errors 只数客户端可见最终状态 ≥400 **且非拒绝**的——429 单列进
//     RejectedRequests，混进 errors 会把「用户超限」误读成「上游故障」。
//   - 被拒样本没到上游，token 与金额恒为 0（调用方也不该给），维度里的
//     upstream_name 为空串。
//   - 量取 Sample.quantities()：文本面来自 Tokens，视频/图片面来自厂商 usage
//     （token / 秒 / 张各自落位）——**别直接读 s.Tokens**，那正是视频行整片
//     显示「Token 0」的来路。
func (m *Meter) addDeltaLocked(s Sample, at time.Time, kind string, q quantities, cost int64) {
	k := deltaKey{
		bucket:   at.UTC().Unix() / 3600,
		keyID:    s.KeyID,
		model:    m.modelDimLocked(s),
		upstream: s.UpstreamName,
		entry:    s.Entry,
	}
	d := m.deltas[k]
	if d == nil {
		d = &store.UsageDelta{
			BucketHour: k.bucket, KeyID: k.keyID,
			ModelName: k.model, UpstreamName: k.upstream, Entry: k.entry,
		}
		m.deltas[k] = d
	}
	// 非键的维度快照取最后写入者（与 store 的 UPSERT 同口径）。
	d.KeyDisplay, d.Kind = s.KeyDisplay, kind

	d.Requests++
	if s.Rejected {
		d.RejectedRequests++
	} else if s.Status >= 400 {
		d.Errors++
	}
	if s.Estimated {
		d.EstimatedRequests++
	}
	if s.UsageUnavailable {
		d.UnavailableRequests++
	}
	d.PromptTokens += q.tokens.Prompt
	d.CompletionTokens += q.tokens.Completion
	d.CacheReadTokens += q.tokens.CacheRead
	d.CacheWriteTokens += q.tokens.CacheWrite
	d.TotalTokens += q.total
	d.VideoSeconds += q.seconds
	d.ImageCount += q.images
	d.CostMicro += cost
	d.DurationMsSum += s.DurationMs
}

// addSpendLocked 把金额并进该密钥的预算计数器（先按窗口键翻转）。
// keyID ≤ 0（无归属）不建计数器。
//
// **累计值恒钳在 0 以上**。跨零点冲正那件事本身已由 ReverseProvisional 按窗口
// 键裁掉（Phase 4：记在昨天的那半笔不再回冲到今天），钳零留作最后一道护栏——
// 它挡的是「计数器被压成负数」这一种失效，方向恒为偏紧的反面（预算变松），
// 与掉电丢一个冲刷窗口同向。
func (m *Meter) addSpendLocked(keyID, cost int64, dayKey, weekKey, monKey string) {
	if cost == 0 || keyID <= 0 {
		// 仍要保证计数器存在并对齐窗口？不必：读侧 Spend 也会翻转，
		// 而 0 元样本对额度没有影响，凭空建条目只是白占内存。
		return
	}
	c := m.spendKey[keyID]
	if c == nil {
		c = &spend{dayKey: dayKey, weekKey: weekKey, monKey: monKey}
		m.spendKey[keyID] = c
	}
	c.roll(dayKey, weekKey, monKey)
	c.dayMicro = nonNeg(c.dayMicro + cost)
	c.weekMicro = nonNeg(c.weekMicro + cost)
	c.monMicro = nonNeg(c.monMicro + cost)
}

// modelDimLocked 取样本进账本时用的模型维度，并对**目录里没有这一行**的名字
// 做基数封顶：一个冲刷窗口内至多放行 unknownModelsPerFlush 个不同的未知名，
// 之后一律并进 UnknownModelDim。
//
// 挡的是什么：模型名是客户端在请求体里随手给的字符串，而它会成为内存 delta 表
// 的键与一行 usage_hourly 的维度列。一把有效密钥连发 10 万个不存在的模型名，
// 就是 10 万条内存条目与 10 万行落盘——SD 卡写放大与账本无界增长。长度早已在
// gateway 侧截到 128 rune，个数这一维是本函数补的。
//
// 为什么不是「一律并进哨兵」：目录里存在的模型（含已禁用、含没挂来源的）恒不
// 受限——那些名字的基数由管理台约束，是真正要看的账。而未知名在**上限之内**
// 也照实记：拼错一个模型名的客户，管理员在用量页看得到它，排障不必翻日志。
// 真名在任何情况下都还在访问日志里，哨兵只影响账本的分组粒度。
//
// 窗口随冲刷清零（flush），所以上限是「每 5 分钟 50 个」而不是「每天 50 个」——
// 持续攻击下每天仍可能长出上万行。这是有意的折中：跨窗口记住见过的名字要么
// 无界增长（回到原问题），要么得为一份短命的黑名单养淘汰逻辑，而 SD 卡写量在
// 这个上限下已经回到与正常流量同量级。
func (m *Meter) modelDimLocked(s Sample) string {
	if s.ModelKnown || s.ModelName == "" {
		return s.ModelName
	}
	if _, ok := m.unknownModels[s.ModelName]; ok {
		return s.ModelName
	}
	if len(m.unknownModels) >= unknownModelsPerFlush {
		return UnknownModelDim
	}
	m.unknownModels[s.ModelName] = struct{}{}
	return s.ModelName
}

// pushEventLocked 把明细压进环形缓冲。
//
// 真环（写指针绕回）而不是「整段前移一格」：这是**请求路径**上的代码，
// 满环之后每请求搬一次 256 条记录（几十 KB memmove）纯属白烧板子的 CPU。
// sysinfo.Recorder 那份前移写法成立，是因为它一分钟才动一次。
func (m *Meter) pushEventLocked(ev Event) {
	if len(m.ring) < ringSize {
		m.ring = append(m.ring, ev)
		m.ringNext = len(m.ring) % ringSize
		return
	}
	m.ring[m.ringNext] = ev
	m.ringNext = (m.ringNext + 1) % ringSize
}

// cost 解析记账时点价并折算金额；价目表坏了按未定价处理（0 元）并告警。
func (m *Meter) cost(s Sample) int64 {
	if s.UsageUnavailable {
		return 0
	}
	p, err := ParsePricing(s.Pricing)
	if err != nil {
		// 只报模型名与形态原因——目录价本身是公开数据，但错误文本不回显
		// 库里的字段名与值（ErrInvalidPricing 的纪律）。
		m.log.Warn("模型目录价不合法，本次按未定价记 0 元", "model", s.ModelName, "err", err.Error())
		return 0
	}
	return Cost(p, s.measure())
}

// ProvisionalWindow 标记一笔临时金额记进了**哪一组**预算窗口（本地自然日 /
// 自然周 / 自然月键）。冲正必须带着它回来，见 ReverseProvisional。
type ProvisionalWindow struct {
	Day   string
	Week  string
	Month string
}

// AddProvisional 把一笔**临时**金额并进预算计数器，不进小时桶、不进环，
// 并返回它记进了哪一组窗口。
//
// 长流的 30s 中间结算用它（路线第 7 项点名要的）：没有它，一条十分钟的 SSE
// 在结束前对准入完全隐身，并发长流能把日预算打穿一个量级；有了它，超限暴露
// 窗口收敛到 30s。用法是成对的——每 30s 按估算增量 +delta，流终经
// ReverseProvisional 冲平已记的部分，再走一次 Record 记真值。
//
// deltaMicro 只收正数：冲正有自己的方法，因为它必须携带窗口键才能落对地方。
func (m *Meter) AddProvisional(keyID, deltaMicro int64) ProvisionalWindow {
	local := m.now().In(m.loc)
	day, week, mon := windowKeys(local)
	win := ProvisionalWindow{Day: day, Week: week, Month: mon}
	if deltaMicro <= 0 {
		return win
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addSpendLocked(keyID, deltaMicro, day, week, mon)
	return win
}

// ReverseProvisional 冲正长流已记的临时金额：dayMicro 只在 win.Day 仍是当前的
// 本地日窗口时减回，weekMicro / monthMicro 对周/月窗口同理；**窗口已经翻过去
// 的那部分整个丢弃**。
//
// 为什么要按窗口裁（Phase 3.5 外审点名的第二条）：一条跨过本地零点的长流，
// +X 记在昨天的计数器上，而昨天的计数器在翻转时已被清零；若收尾时照旧把 −X
// 减在今天的计数器上，减掉的就是今天**别人**的合法消费——等于凭空给新的一天
// 放宽了 ≤X 微元的额度。日窗口翻转而周/月窗口没翻（周内、月内跨日）是常态，
// 所以三个窗口分别判、分别裁，不能合成一个。
func (m *Meter) ReverseProvisional(keyID, dayMicro, weekMicro, monthMicro int64, win ProvisionalWindow) {
	local := m.now().In(m.loc)
	dayKey, weekKey, monKey := windowKeys(local)
	if win.Day != dayKey {
		dayMicro = 0
	}
	if win.Week != weekKey {
		weekMicro = 0
	}
	if win.Month != monKey {
		monthMicro = 0
	}
	if dayMicro <= 0 && weekMicro <= 0 && monthMicro <= 0 {
		return
	}
	if keyID <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.spendKey[keyID]
	if c == nil {
		return // 计数器还没建 = 这笔预扣早已随窗口翻转蒸发，无从冲正
	}
	c.roll(dayKey, weekKey, monKey)
	c.dayMicro = nonNeg(c.dayMicro - dayMicro)
	c.weekMicro = nonNeg(c.weekMicro - weekMicro)
	c.monMicro = nonNeg(c.monMicro - monthMicro)
}

// Spend 读出一把密钥的预算已用额快照（读时顺手做窗口翻转，所以跨日/跨周/
// 跨月之后第一次读到的就是清零后的值）。keyID ≤ 0 恒回零值。
func (m *Meter) Spend(keyID int64) Spend {
	if keyID <= 0 {
		return Spend{}
	}
	local := m.now().In(m.loc)
	dayKey, weekKey, monKey := windowKeys(local)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out Spend
	if c := m.spendKey[keyID]; c != nil {
		c.roll(dayKey, weekKey, monKey)
		out.DayMicro, out.WeekMicro, out.MonthMicro = c.dayMicro, c.weekMicro, c.monMicro
	}
	if a := m.allowance[keyID]; a != nil {
		out.MeteredAllowancePendingMicro = a.pending
	}
	return out
}

// ---- 播种 ----

// Seed 从 usage_hourly 播种预算计数器（把 UTC 小时桶折进本地当日/当月窗口，
// 取整方向见包头注释）。Run 会自己调，成功之后**重复调用是空操作**，所以装配
// 层想更早关上「启动到第一次 Record 之间预算为零」的窗口，可以在起服务之前
// 先调它。
//
// **只有成功才封印，失败可以重试**：把失败也 latch 住（原来的 sync.Once 写法）
// 意味着启动那一刻库瞬时不可用——首启初始化、一次锁争用——就让整个进程生命周期
// 的预算都从 0 起算，等于当天预算凭空翻倍。而重试的代价只是多一次点查。
//
// 重试窗口到**第一次成功冲刷**为止：那之后库里混着本进程自己记过的增量，再
// 播种就是把这部分账加第二遍（计数器偏高 = 预算凭空变紧）。错过窗口就记一条
// ERROR 并放弃，本进程的已用额只从自己启动那一刻起算。
func (m *Meter) Seed(ctx context.Context) error {
	m.seedMu.Lock()
	defer m.seedMu.Unlock()
	if m.seeded {
		return nil
	}
	if m.hasWrittenLedger() {
		m.seeded = true
		m.log.Error("用量播种窗口已错过（本进程已往账本写过增量，再播种会把这部分账加第二遍），" +
			"本进程的当日/当月已用额只从启动时刻起算")
		return nil
	}
	if err := m.seed(ctx); err != nil {
		return err
	}
	m.seeded = true
	return nil
}

// seedRetry 是后台协程每一轮的播种重试，只压缩告警（成功时 seed 自己会记一条
// 「预算计数器已播种」，那就是恢复信号）。
func (m *Meter) seedRetry(ctx context.Context) {
	err := m.Seed(ctx)
	if err == nil || ctx.Err() != nil {
		return
	}
	m.seedMu.Lock()
	first := !m.seedWarned
	m.seedWarned = true
	m.seedMu.Unlock()
	if first {
		m.log.Warn("预算计数器播种失败，后续每轮重试（在此之前当日/当月已用额从本进程启动时刻起算）",
			"err", err.Error())
	}
}

// hasWrittenLedger 报告本进程是否已往账本里写过增量。
func (m *Meter) hasWrittenLedger() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wroteLedger
}

// markLedgerWritten 记下「本进程已往账本里写过增量」（关掉播种的重试窗口）。
func (m *Meter) markLedgerWritten() {
	m.mu.Lock()
	m.wroteLedger = true
	m.mu.Unlock()
}

func (m *Meter) seed(ctx context.Context) error {
	local := m.now().In(m.loc)
	monthStart := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, m.loc)
	dayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, m.loc)
	wkStart := weekStart(local)
	// 周窗口可以早于月初（本周周一还在上个月里），播种要从两者中更早的那个
	// 起点读起——周至多回看 6 天，usage_days 的下限 31 天连同月窗口一起盖住。
	// 向下取整取起始桶：早于窗口的桶会被下面的归属判定滤掉，多读几行无害，
	// 少读一行就是漏播。
	earliest := monthStart
	if wkStart.Before(earliest) {
		earliest = wkStart
	}
	sinceBucket := floorDiv(earliest.Unix(), 3600)

	rows, err := m.st.SumUsageSince(ctx, sinceBucket)
	if err != nil {
		return err
	}
	dayKey, weekKey, monKey := windowKeys(local)

	m.mu.Lock()
	defer m.mu.Unlock()
	var seededDay, seededWeek, seededMonth int64
	for _, r := range rows {
		start := time.Unix(r.BucketHour*3600, 0).In(m.loc)
		if start.Before(earliest) {
			continue
		}
		// 三个窗口各自归属：桶可能只在周窗口里（上月末的本周消费）、只在
		// 月窗口里（本月初、上周的消费），或同时在两者里。
		inMonth := !start.Before(monthStart)
		inWeek := !start.Before(wkStart)
		inDay := !start.Before(dayStart)
		if r.KeyID > 0 {
			c := m.spendKey[r.KeyID]
			if c == nil {
				c = &spend{dayKey: dayKey, weekKey: weekKey, monKey: monKey}
				m.spendKey[r.KeyID] = c
			}
			c.roll(dayKey, weekKey, monKey)
			if inMonth {
				c.monMicro += r.CostMicro
			}
			if inWeek {
				c.weekMicro += r.CostMicro
			}
			if inDay {
				c.dayMicro += r.CostMicro
			}
		}
		if inMonth {
			seededMonth += r.CostMicro
		}
		if inWeek {
			seededWeek += r.CostMicro
		}
		if inDay {
			seededDay += r.CostMicro
		}
	}
	// 只记金额与密钥数，没有任何标识（§15.1 的日志纪律）。
	m.log.Info("预算计数器已播种",
		"tz", m.loc.String(), "keys", len(m.spendKey),
		"day_micro", seededDay, "week_micro", seededWeek, "month_micro", seededMonth)
	return nil
}

// ---- 后台协程 ----

// Run 驱动落盘、保留清理与懒对账，直到 ctx 取消；退出前做最后一次冲刷
// （照 sysinfo.Recorder 走 WaitGroup，装配层要等它收尾）。
func (m *Meter) Run(ctx context.Context) {
	// 播不出来只影响「重启后当日/当月已用额」的起点，计量本身照跑——但每轮
	// 都会重试（失败不 latch，见 Seed）。
	m.seedRetry(ctx)
	m.log.Info("用量计量已启动",
		"flush", FlushInterval.String(), "retention_days", m.days, "tz", m.loc.String(),
		// 未接线时视频任务的账只能靠客户端轮询顺手清算——弃轮询的任务永远
		// 不入账。装配层漏注入探针是个静默故障，这里明说一次。
		"lazy_settle", m.prober != nil)
	m.prune(ctx)
	t := time.NewTicker(FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 父 ctx 已取消：另起一个有界 ctx 把内存里的账落完再走。
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
			m.flush(fctx)
			cancel()
			m.log.Info("用量计量已停止")
			return
		case <-t.C:
			// 播种重试**必须排在冲刷之前**：冲刷一旦成功，库里就混进了本
			// 进程自己记过的增量，那之后播种会把它们加第二遍（见 Seed）。
			m.seedRetry(ctx)
			m.flush(ctx)
			m.prune(ctx)
			m.settle(ctx)
		}
	}
}

// flush 把内存里的两类待落盘状态批量落库：账本增量与按量额度待扣。
// 两条写入互相独立（各自整批事务、各自失败回填），一类失败不拖住另一类。
func (m *Meter) flush(ctx context.Context) {
	m.flushDeltas(ctx)
	m.flushMeteredAllowance(ctx)
}

// flushDeltas 把内存增量批量落盘（无增量整轮跳过，连事务都不开）。
//
// 失败即把这批增量**原样加回**内存等下一轮——store 的写是整批事务，失败时
// 库里什么都没落，把它们丢掉就是丢账。加回而非覆盖：这中间可能又来了新样本，
// 同一个键要合并而不是二选一。
func (m *Meter) flushDeltas(ctx context.Context) {
	m.mu.Lock()
	if len(m.deltas) == 0 {
		m.mu.Unlock()
		return
	}
	batch := make([]store.UsageDelta, 0, len(m.deltas))
	keys := make([]deltaKey, 0, len(m.deltas))
	for k, d := range m.deltas {
		batch = append(batch, *d)
		keys = append(keys, k)
	}
	m.deltas = make(map[deltaKey]*store.UsageDelta)
	// 未知模型名的配额随窗口重置（见 modelDimLocked）。已经并进 batch 的那些
	// 名字早已固化在 deltaKey 里，落盘失败回填也不会改口径。
	m.unknownModels = make(map[string]struct{})
	m.mu.Unlock()

	err := m.st.AddUsageDeltas(ctx, batch)
	if err == nil {
		// 库里从此混着本进程记过的增量：播种的重试窗口到此为止（见 Seed）。
		m.markLedgerWritten()
	}
	if err != nil {
		if ctx.Err() != nil {
			m.log.Warn("用量增量落盘中止（进程退出），本批留在内存", "rows", len(batch))
		} else {
			m.log.Error("用量增量落盘失败，本批留待下轮重试", "rows", len(batch), "err", err.Error())
		}
		m.mu.Lock()
		for i, k := range keys {
			if d := m.deltas[k]; d != nil {
				mergeDelta(d, batch[i])
			} else {
				cp := batch[i]
				m.deltas[k] = &cp
			}
		}
		m.mu.Unlock()
	}
}

// flushMeteredAllowance 把按量额度待扣批量落库。失败时原样加回等待下一轮；
// Store 的批量扣减是单事务，所以失败就代表数据库分文未动。异常掉电至多让
// 密钥多留一个冲刷窗口的额度，方向仍是宁松勿紧。
func (m *Meter) flushMeteredAllowance(ctx context.Context) {
	m.mu.Lock()
	var drains map[int64]int64
	for id, a := range m.allowance {
		if a.pending <= 0 {
			continue
		}
		if drains == nil {
			drains = make(map[int64]int64)
		}
		drains[id] = a.pending
		a.pending = 0
	}
	m.mu.Unlock()
	if len(drains) == 0 {
		return
	}
	if err := m.st.DrainAPIKeyMeteredAllowances(ctx, drains); err == nil {
		return
	} else if ctx.Err() != nil {
		m.log.Warn("按量额度扣减落盘中止（进程退出），本批留在内存", "keys", len(drains))
	} else {
		m.log.Error("按量额度扣减落盘失败，本批留待下轮重试", "keys", len(drains), "err", err.Error())
	}
	m.mu.Lock()
	for id, amount := range drains {
		a := m.allowance[id]
		if a == nil {
			a = &allowanceState{}
			m.allowance[id] = a
		}
		if a.pending > math.MaxInt64-amount {
			a.pending = math.MaxInt64
		} else {
			a.pending += amount
		}
	}
	m.mu.Unlock()
}

// prune 每天清一次超出保留期的聚合行（与冲刷同协程串行，绝不与请求路径抢
// 写锁）。日界按 UTC 判——它只决定「一天跑一次」，与预算的本地窗口无关。
func (m *Meter) prune(ctx context.Context) {
	day := m.now().UTC().Format(dayLayout)
	m.mu.Lock()
	skip := day == m.lastPruneDay
	m.mu.Unlock()
	if skip {
		return
	}
	before := floorDiv(m.now().AddDate(0, 0, -m.days).Unix(), 3600)
	n, err := m.st.PruneUsage(ctx, before)
	if err != nil {
		// **失败不打日戳**：打了就要等到下一个 UTC 日才重试，一次瞬时的库锁
		// 争用能让保留期整整跳过一天，而现场只剩一行 ERROR 作证。不打日戳，
		// 下一个冲刷周期（5 分钟）自然重来。
		if ctx.Err() == nil {
			m.log.Error("清理过期用量聚合失败，下一轮重试", "err", err.Error())
		}
		return
	}
	m.mu.Lock()
	m.lastPruneDay = day
	m.mu.Unlock()
	if n > 0 {
		m.log.Info("已清理过期用量聚合", "rows", n, "retention_days", m.days)
	}
}

// mergeDelta 把 src 的计数累加进 dst（落盘失败的回填路径）。
//
// 快照列的方向要看清楚：这里 **dst 是新的、src 是旧的**——dst 是冲刷在途期间
// 新累积起来的那条增量，src 是刚失败退回来的那一批。所以只补 dst 缺的，
// 绝不用 src 覆盖 dst（那是拿旧快照盖新快照，会把一个已经填好的名字倒回空串）。
func mergeDelta(dst *store.UsageDelta, src store.UsageDelta) {
	if dst.KeyDisplay == "" {
		dst.KeyDisplay = src.KeyDisplay
	}
	if dst.Kind == "" {
		dst.Kind = src.Kind
	}
	dst.Requests += src.Requests
	dst.Errors += src.Errors
	dst.RejectedRequests += src.RejectedRequests
	dst.EstimatedRequests += src.EstimatedRequests
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
	dst.TotalTokens += src.TotalTokens
	dst.VideoSeconds += src.VideoSeconds
	dst.ImageCount += src.ImageCount
	dst.CostMicro += src.CostMicro
	dst.DurationMsSum += src.DurationMsSum
}

// floorDiv 是向下取整的整除（Go 的 / 对负数向零截断，而 unix 秒在 1970 年
// 之前为负——桶号必须单调，不能在原点附近折回来）。
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
