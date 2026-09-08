package usage

// settle.go 是异步任务的**懒对账**：把「客户端提交完就再也不来轮询」的视频
// 任务的账补记回来。跑在 Meter.Run 的同一个协程里（顺带，不另起 goroutine）。
//
// 为什么需要它：视频任务的 usage 只有厂商知道，而设备侧的观测挂在客户端的
// 查询路径上（迭代 8 的按需代理，不养轮询协程）。客户端提交完就走，那笔钱就
// 永远不会被观测到——而厂商侧的任务记录只保留约 7 天，过了窗口连补都补不了。
//
// 防重复入账的全部依赖是 aigc_tasks.cost_micro 的**条件更新**
// （NULL → 值，store.SettleAIGCTaskWithUsage）：查询路径的顺手清算与本协程
// 天然重叠，把「只算一次」压在数据库上比在应用层加锁靠得住。
//
// 三条与直觉不同、都写在代码里的口径：
//
//   - **入账的桶是清算时刻，不是提交时刻**，价格同样取清算时点价。理由是
//     一致性：预算计数器在清算那一刻加的钱，重启后要能从同一只桶里播回来。
//     回填到提交时刻会让「内存计数器」与「重启后播种」对不上账。
//   - **清算增量不计 requests**：这次请求在提交那一刻已经被访问日志中间件
//     记过一笔了，再记一次就是把一个任务数成两次调用。清算增量带的是金额、
//     计费量（token / 秒 / 张）与估算标记，requests 与耗时留零。
//   - **失败/取消的任务记 0 元但不打 estimated**：厂商对失败与排队期取消
//     不计费，0 是**已知**的准确值。estimated 只留给真的不知道的情况——
//     记录已过期（expired）、或成功了却拿不到 usage。

import (
	"context"
	"errors"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	// settleStaleness 是任务行「多久没变过」才进对账视野。
	//
	// 注意 aigc_tasks.updated_at 只在观测值**真的变了**时才写（迭代 8 的
	// 等值观测零写放大），所以这是「多久没观测到变化」而不是「多久没被
	// 轮询」——一个客户端每秒都在轮询、状态却一直停在 running 的任务，在
	// 这里同样算陈旧。由此产生的重复回查由 settleBatch 与 5 分钟的节奏兜住。
	settleStaleness = 10 * time.Minute
	// settleBatch 是一轮最多处理的任务数。三重闸门：内存、上游配额，以及
	// **升级后的对账洪峰**——0006 把存量任务行一律标成未清算，保留期内的
	// 旧任务会在第一轮全部符合条件。
	settleBatch = 50
	// settleCallTimeout 是一轮对账的整体预算（含逐个任务的厂商回查）。
	settleCallTimeout = 2 * time.Minute
	// settleProbeTimeout 是**单个任务**回查的时限。整轮预算之外还要这一道：
	// 没有它，队头几个挂死的探针就能把整轮 2 分钟耗光，排在后面的任务轮轮
	// 饿死——而任务行的顺序是稳定的（created_at, id），坏行永远排在队头。
	settleProbeTimeout = 30 * time.Second
	// settleProbeFailLimit 是同一个任务连续回查失败多少次后进冷却。
	settleProbeFailLimit = 3
	// settleQuarantine 是冷却时长：坏行在此期间整个跳过（不回查、不清算），
	// 到期后重新给它机会——跳过是**降级不是放弃**，上游故障总会恢复，而按
	// 0 元清算是不可逆的。冷却态只在内存里，进程重启即忘。
	settleQuarantine = time.Hour
)

// 两个状态词：判「0 元究竟是**已知**还是**未知**」要用到它们。
//
// 与「终态判定走 TaskProber 接口」不矛盾：终态是四个词一起变的一整套词汇
// （复制过来必然分叉），而这两个是设备契约里公开的状态名（AGENTS.md 的
// 「Status vocabulary」一节），且此处判的是**计费语义**而非状态语义——
// expired 表示最终用量无从得知，succeeded 表示本该有账却没拿到。
// 词汇真要改，这两个常量与 gateway 的常量组是一次联动变更。
const (
	statusSucceeded = "succeeded"
	statusExpired   = "expired"
)

// TaskProber 是懒对账现查厂商任务的出口。实现在 internal/gateway——迭代 8 的
// 适配器、钉死上游的凭据取用与状态词汇都在那边，本包直接 import 它会成环
// （gateway 要 import 本包挂 Record）。
//
// prober 为 nil 表示未接线：此时**整个对账环节跳过**（Run 的启动日志里
// lazy_settle=false 是唯一的痕迹）。刻意不做「nil 就只清算已终态的行」的
// 降级——判定终态需要一整套状态词汇，在本包里复制一份迟早与 gateway 那份
// 分叉，账就错在没人看的地方。
type TaskProber interface {
	// TaskTerminal 报告任务状态是否终结（结果与 usage 已固化，不必再回查）。
	TaskTerminal(status string) bool
	// ProbeTask 现查厂商并把观测写回任务行，返回更新后的行。厂商已查不到
	// 记录（超保留窗口）应归一为 expired 状态返回，而不是报错。
	ProbeTask(ctx context.Context, task store.AIGCTask) (store.AIGCTask, error)
}

// SettleOnce 跑一轮懒对账——Run 每 FlushInterval 顺带跑的正是这一轮。
//
// 装配层不必调用它（Run 自己会跑）。它存在是为了让**跨包验收**驱动一轮对账
// 而不必空等一个冲刷周期：探针实现在 internal/gateway，那条链路只能从包外
// 装配起来测（本包的内部测试 import gateway 会成环）。
func (m *Meter) SettleOnce(ctx context.Context) { m.settle(ctx) }

// settle 跑一轮懒对账。任何一步失败都只影响本轮（告警按状态翻转压缩，
// 照 sysinfo.Recorder 的 warn/recovered 模式）。
func (m *Meter) settle(ctx context.Context) {
	if m.prober == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, settleCallTimeout)
	defer cancel()

	rd := newSettleRound()
	m.purgeSettleSkips()
	tasks, err := m.st.ListUnsettledAIGCTasks(ctx, m.now().Add(-settleStaleness), settleBatch)
	if err != nil {
		if ctx.Err() == nil {
			m.warnSettle(rd, "列出未清算任务失败", err)
		}
		return
	}
	if len(tasks) == 0 {
		m.settleOK(rd)
		return
	}
	for i := range tasks {
		if ctx.Err() != nil {
			return
		}
		if m.settleOne(ctx, tasks[i], rd) {
			rd.settled++
		}
	}
	if rd.settled > 0 {
		m.log.Info("懒对账已补记任务账单", "settled", rd.settled, "scanned", len(tasks))
	}
	if rd.unbilled > 0 {
		// **按轮聚合的一条告警**，不逐条刷屏：一轮最多 settleBatch 条，
		// 升级后的对账洪峰会让逐条告警一次冲出几十行。带上第一笔的 id
		// 与原因，够人工顺藤摸瓜。
		m.log.Warn("有任务无法取得厂商 usage，已按 0 元记账并标估算",
			"count", rd.unbilled, "first_task_id", rd.firstUnbilled, "first_reason", rd.firstReason)
	}
	if rd.skipped > 0 {
		// 跳过的行没有任何账面痕迹（既没清算也没入账），一条计数是唯一的信号。
		m.log.Info("本轮跳过了暂时判不出金额的任务，留待下轮",
			"skipped", rd.skipped, "quarantined", rd.quarantined)
	}
	m.settleOK(rd)
}

// newSettleRound 起一轮对账的公共状态。
func newSettleRound() *settleRound {
	return &settleRound{types: map[int64]string{}, prices: map[string]string{}}
}

// settleRound 是一轮对账的公共状态：两个窄读数的缓存（同一批任务常挤在同一个
// 上游、同一个模型上，没必要一行一次点查）与告警聚合。
//
// **缓存里只放确定的结论**（行在 / 行确实不在）。临时性的点查失败绝不入缓存：
// 一旦把「库刚才不可用」记成「行已不存在」，同上游、同模型的整批任务会跟着按
// 0 元一次性清算掉——那一步不可逆，钱永久消失。
type settleRound struct {
	types    map[int64]string
	prices   map[string]string
	settled  int
	unbilled int
	// skipped / quarantined 是本轮被跳过的任务数（判不出金额、或坏行在冷却
	// 期内）。它们不清算也不入账，留待下一轮。
	skipped     int
	quarantined int
	// failed 记本轮是否出过故障。settleOK 必须凭它才敢清告警态——否则
	// 「WARN 之后紧跟一条『已恢复』」会在每一轮重演：告警压缩形同虚设，
	// 而那条 INFO 还在撒谎（什么都没恢复）。
	failed bool
	// firstUnbilled / firstReason 只留第一笔，供人工顺藤摸瓜（任务 id 是
	// 不透明句柄，不是 §15.1 的密钥物料）。
	firstUnbilled string
	firstReason   string
}

// noteUnbilled 记一笔「拿不到厂商 usage」的任务。
func (r *settleRound) noteUnbilled(taskID, reason string) {
	r.unbilled++
	if r.firstUnbilled == "" {
		r.firstUnbilled, r.firstReason = taskID, reason
	}
}

// settleOne 对账一个任务行，返回是否真的清算了本次。
func (m *Meter) settleOne(ctx context.Context, task store.AIGCTask, rd *settleRound) bool {
	if !m.prober.TaskTerminal(task.Status) {
		if m.probeQuarantined(task.ID) {
			// 这一行的探针连败到阈值了，冷却期内整个跳过：它每轮都要耗掉
			// 一次回查预算，而排在它后面的任务会跟着饿死。
			rd.quarantined++
			rd.skipped++
			return false
		}
		// 每个任务一份自己的时限：一个挂死的探针不该吃掉整轮预算。
		pctx, cancel := context.WithTimeout(ctx, settleProbeTimeout)
		probed, err := m.prober.ProbeTask(pctx, task)
		cancel()
		if err != nil {
			rd.skipped++
			if ctx.Err() == nil {
				// 上游不可达/凭据不可用/上游行已删：本轮跳过，下轮再来；
				// 连败到阈值就进冷却，行最终会被 14 天保留期清掉。
				m.noteProbeFailure(task.ID)
				m.warnSettle(rd, "任务回查失败，本轮跳过", err)
			}
			return false
		}
		m.clearProbeFailure(task.ID)
		task = probed
		if !m.prober.TaskTerminal(task.Status) {
			return false // 还在跑，等下一轮
		}
	}
	return m.settleTerminal(ctx, task, rd)
}

// SettleTask 就地清算一个**已终态**的任务行——客户端查询路径（handleVideoGet
// 观测到终态）顺手调用的入口。不这么做，一笔账最长要等 15 分钟才由懒对账补上
// （10 分钟陈旧阈值 + 5 分钟轮次）。
//
// 与懒对账**共用同一段代码与同一套口径**（settleTerminal）：金额只有一个算法。
// 重复调用无害——一次性语义压在 SettleAIGCTaskWithUsage 的条件更新上，两条路径
// 天然重叠也只入账一次。
//
// 调用方负责保证 task 已终态（状态词汇归 gateway，本包不复制一份）；
// 已清算的行直接返回。
func (m *Meter) SettleTask(ctx context.Context, task store.AIGCTask) {
	if task.CostMicro != nil {
		return
	}
	rd := newSettleRound()
	m.settleTerminal(ctx, task, rd)
	if rd.unbilled > 0 {
		m.log.Warn("任务无法取得厂商 usage，已按 0 元记账并标估算",
			"task_id", rd.firstUnbilled, "reason", rd.firstReason)
	}
}

// settleTerminal 把一个**已终态**的任务行折价、清算、入账，返回本次是否真的
// 清算了（false = 判不出金额、已被另一条路径清算过，或写库失败）。
func (m *Meter) settleTerminal(ctx context.Context, task store.AIGCTask, rd *settleRound) bool {
	at := m.now()
	cost, estimated, tu, ok := m.taskCost(ctx, task, rd)
	if !ok {
		// 本轮判不出金额（点查临时失败）：**绝不按 0 元清算**——那一步是
		// 一次性的，清算掉的行再也扫不到，钱永久消失。留给下一轮。
		rd.skipped++
		return false
	}
	entry := EntryVideo
	if task.Kind == store.ModelKindImage {
		entry = EntryImage
	}
	// 清算增量**不计 requests**：这次调用的 requests 在提交那一刻已经记过了。
	// 但计费量要带上——秒 / 张 / token 正是这笔金额的依据，只落金额就成了
	// 「可审的钱、不可审的量」（收口走查点名的缺口）。
	q := Sample{Kind: task.Kind, TaskUsage: tu}.quantities()
	delta := store.UsageDelta{
		BucketHour:       at.UTC().Unix() / 3600,
		KeyID:            task.KeyID,
		KeyDisplay:       task.KeyDisplay,
		ModelName:        task.ModelName,
		UpstreamName:     task.UpstreamName,
		Entry:            entry,
		Kind:             task.Kind,
		PromptTokens:     q.tokens.Prompt,
		CompletionTokens: q.tokens.Completion,
		TotalTokens:      q.total,
		VideoSeconds:     q.seconds,
		ImageCount:       q.images,
		CostMicro:        cost,
	}
	if estimated {
		delta.EstimatedRequests = 1
	}
	ok, err := m.st.SettleAIGCTaskWithUsage(ctx, task.ID, cost, estimated, delta)
	if err != nil {
		if ctx.Err() == nil {
			m.warnSettle(rd, "任务清算写库失败", err)
		}
		return false
	}
	if !ok {
		return false // 已被查询路径清算过（条件更新落空），不重复入账
	}
	// 这一笔已经进了账本（同一事务里的 delta）：播种的重试窗口到此为止，
	// 否则下一轮播种会把它从库里读回来再加一遍（见 Meter.Seed）。
	m.markLedgerWritten()
	// 库里已经落过增量了，这里只补内存侧的两件事：预算计数器与明细环。
	// 金额直接传 taskCost 的结果，observe 不再自己算（同一笔账只有一个口径）。
	//
	// **Status / Attempts 留零**：这一条不是一次 HTTP 调用，是一笔清算增量
	// （那次调用在提交时已经有自己的环条目了）。填 200 会让一个在厂商侧失败的
	// 任务在「最近请求」里显示成成功调用 + 0 元——正好是排查「这笔怎么没记钱」
	// 时最容易被带偏的一行。环里认清算行看 TaskID，不看状态码。
	m.observe(Sample{
		At:    at,
		KeyID: task.KeyID, KeyDisplay: task.KeyDisplay,
		ModelName: task.ModelName, UpstreamName: task.UpstreamName,
		Entry: entry, Kind: task.Kind, TaskUsage: tu,
		Estimated: estimated, TaskID: task.ID,
	}, cost, false)
	return ok
}

// taskCost 按**清算时点价**折算任务金额。四个返回值：金额、是否含估算成分、
// 解析出的厂商用量（记进账本的量，判不出时为零值）、**本轮能否判定**。
// 最后一个为 false 表示点查临时失败（库不可用、锁争用、进程正在退出）——
// 调用方必须整笔跳过、留给下一轮，绝不能当成 0 元清算：清算是一次性的，
// 钱清没了就再也扫不回来。
//
// 判定顺序与理由：
//  1. 先认上游族（计价形态与 usage 形态校验都要它），族认不出（行已删）
//     视同 usage 不可得。
//  2. 厂商 usage 按 kind + 上游族可解析 → 取清算时点价计价，estimated = false。
//  3. 状态是 expired（厂商记录已查不到）→ 0 元 + estimated + 告警：最终用量
//     无从得知，这是**真的不知道**。
//  4. succeeded 却没有可用 usage（没回 / 形态对不上 / 上游行已删）→ 同上：
//     厂商该报没报，不能当 0 元真值。
//  5. 其余终态（failed / cancelled 且无 usage）→ 0 元，**不打 estimated**：
//     厂商对失败与排队期取消不计费，0 是已知的准确值。
func (m *Meter) taskCost(ctx context.Context, task store.AIGCTask, rd *settleRound) (int64, bool, TaskUsage, bool) {
	// **判不出金额时一律回零用量**：形态对不上时解析出的那几个数字属于**别家**
	// 的计价口径，把它们记进这一行的秒/张列，等于给一笔「不知道多少钱」的账
	// 配上一份看着很确定的量。0 元 + 估算标已经把话说清楚了。
	var none TaskUsage
	upType, err := m.upstreamType(ctx, task.UpstreamID, rd)
	if err != nil {
		return 0, false, none, false
	}
	tu, usable := ParseTaskUsage(task.UsageJSON, task.Kind, upType)
	if !usable {
		switch task.Status {
		case statusExpired:
			rd.noteUnbilled(task.ID, "厂商记录已过保留窗口")
			return 0, true, none, true
		case statusSucceeded:
			rd.noteUnbilled(task.ID, unusableReason(upType, task.UsageJSON))
			return 0, true, none, true
		}
		return 0, false, none, true
	}
	pricing, exists, err := m.modelPricing(ctx, task.ModelName, rd)
	if err != nil {
		return 0, false, none, false
	}
	if !exists {
		// 模型行已不在（删除，或**改名**——aigc_tasks.model_name 是提交时的
		// 快照，改名后按名字就查不回去了）：拿不到清算时点价，与「上游行已删」
		// 同款处理。不这样处理的话，一次正常的改名会把窗口内的所有任务
		// 一次性钉死成 0 元且不带估算标，账面上看不出任何异常。
		rd.noteUnbilled(task.ID, "模型行已不存在（删除或改名）")
		return 0, true, none, true
	}
	p, perr := ParsePricing(pricing)
	if perr != nil {
		m.log.Warn("模型目录价不合法，任务按未定价记 0 元", "model", task.ModelName, "err", perr.Error())
		// 价签坏了、量却是实的：金额记 0（同「未定价」口径），量照记——
		// 它是这一行唯一还站得住的事实，也是补价后人工核账的依据。
		return 0, false, tu, true
	}
	if p.Priced() && !PricedFor(p, task.Kind, upType) {
		// 有价，但这张表里没有一个字段是这家上游用得上的（典型场景：模型同时
		// 挂 ark 与 minimax 两条来源，管理员只填了另一家的价）。这与「未定价」
		// 不是一回事——Priced() 为真，模型页不挂警示徽章，于是这一笔会以 0 元、
		// **非估算**被一次性清算掉，钱永久消失且账面看不出异常。按「真的不知道」
		// 处置：0 元 + 估算标 + 告警，让缺账在用量页与日志里都看得见。
		rd.noteUnbilled(task.ID, "该模型的目录价没有这家上游用得上的字段")
		// usage 形态是对的（是这家上游的量），只是没配这家的价：量照记，
		// 金额 0 + 估算标——账面上「有量无钱」正是补价的信号。
		return 0, true, tu, true
	}
	return Cost(p, Measure{
		Kind:          task.Kind,
		Entry:         EntryVideo,
		UpstreamType:  upType,
		TaskUsage:     tu,
		HasVideoInput: task.HasVideoInput,
		Resolution:    task.ReqResolution,
	}), false, tu, true
}

// unusableReason 说明「完成了却没有可用 usage」到底是哪一种。三句都是静态
// 文本，原文只被判定不被回显（§15.1：库里的内容不进日志）。
func unusableReason(upType, usageJSON string) string {
	switch {
	case upType == "":
		// 上游行已被删（本表无外键，账单事实照留）：认不出计价形态，
		// 只能记 0 并标估算——绝不按别家的价目表瞎算一笔。
		return "任务钉死的上游行已不存在"
	case usageJSON == "":
		return "任务已完成但厂商未回 usage"
	}
	return "厂商 usage 与该上游的计价形态不符"
}

// upstreamType 取上游产品类型（一轮内缓存）。三态：
//
//	(类型, nil)  行在
//	("",  nil)  行确实已删——这个结论可以缓存
//	("",  err)  临时故障——**不缓存、不判定**，调用方跳过本轮
//
// 用管理视图而非路由视图：对账只需要知道「是哪家」，不该把上游凭证明文
// 拉进一条后台协程（§15.1）。
func (m *Meter) upstreamType(ctx context.Context, id int64, rd *settleRound) (string, error) {
	if t, ok := rd.types[id]; ok {
		return t, nil
	}
	u, err := m.st.GetUpstreamByID(ctx, id)
	switch {
	case err == nil:
		rd.types[id] = u.Type
		return u.Type, nil
	case errors.Is(err, store.ErrNotFound):
		rd.types[id] = ""
		return "", nil
	}
	if ctx.Err() == nil {
		m.warnSettle(rd, "任务上游点查失败，本轮跳过", err)
	} else {
		rd.failed = true
	}
	return "", err
}

// modelPricing 取模型的清算时点价（一轮内缓存）。第二个返回值报告模型行是否
// 还在——「行不在」（删除或改名）与「行在但未定价」是两个状态，前者的 0 元
// 是**不知道**、后者是**已知**，调用方据此决定要不要打估算标。第三个是临时
// 故障：同 upstreamType，不缓存、不判定，调用方跳过本轮。
func (m *Meter) modelPricing(ctx context.Context, name string, rd *settleRound) (string, bool, error) {
	if p, ok := rd.prices[name]; ok {
		return p, p != missingPricing, nil
	}
	v, err := m.st.GetModelPricing(ctx, name)
	switch {
	case err == nil:
		rd.prices[name] = v
		return v, true, nil
	case errors.Is(err, store.ErrNotFound):
		rd.prices[name] = missingPricing
		return "", false, nil
	}
	if ctx.Err() == nil {
		m.warnSettle(rd, "模型目录价点查失败，本轮跳过", err)
	} else {
		rd.failed = true
	}
	return "", false, err
}

// ---- 坏行降级（探针永久失败时的队头阻塞） ----

// probeQuarantined 报告某任务是否还在冷却期内（冷却期内不回查、不清算）。
func (m *Meter) probeQuarantined(taskID string) bool {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.settleSkipUntil[taskID]
	return ok && now.Before(until)
}

// noteProbeFailure 记一次回查失败；连败到阈值即进冷却，并把计数清掉
// （冷却到期后重新给它 settleProbeFailLimit 次机会）。
func (m *Meter) noteProbeFailure(taskID string) {
	until := m.now().Add(settleQuarantine)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settleProbeFails[taskID]++
	if m.settleProbeFails[taskID] < settleProbeFailLimit {
		return
	}
	delete(m.settleProbeFails, taskID)
	m.settleSkipUntil[taskID] = until
}

// clearProbeFailure 在回查成功后清掉这一行的失败记录。
func (m *Meter) clearProbeFailure(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.settleProbeFails, taskID)
	delete(m.settleSkipUntil, taskID)
}

// purgeSettleSkips 在每轮开头丢掉已到期的冷却条目。这也是这两张表的内存闸门：
// 每轮最多新增 settleBatch 条、冷却期一到就被清掉，量级恒在
// settleBatch × (settleQuarantine / FlushInterval) 之内（当前 ≈ 600 条）。
func (m *Meter) purgeSettleSkips() {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, until := range m.settleSkipUntil {
		if !now.Before(until) {
			delete(m.settleSkipUntil, id)
			delete(m.settleProbeFails, id)
		}
	}
}

// missingPricing 是缓存里表示「模型行不存在」的哨兵。用一个不可能是合法
// pricing 的字符串，才能与「行在、未定价」的空串区分开——两者的 0 元含义
// 不同（见 modelPricing）。
const missingPricing = "\x00missing"

// warnSettle 记一条对账告警，按状态翻转压缩（连续失败只报第一条）。
// rd 非 nil 时同时标记本轮失败，settleOK 据此不清告警态。
func (m *Meter) warnSettle(rd *settleRound, msg string, err error) {
	if rd != nil {
		rd.failed = true
	}
	m.mu.Lock()
	first := !m.settleWarned
	m.settleWarned = true
	m.mu.Unlock()
	if first {
		m.log.Warn(msg+"（后续同类失败不再重复告警，恢复时会有一条 INFO）", "err", err.Error())
	}
}

// settleOK 标记本轮对账正常，若此前处于告警态则记一条恢复。
// **本轮出过故障就什么都不做**：否则「WARN 之后紧跟一条已恢复」会在每一轮
// 重演，告警压缩形同虚设，而那条 INFO 还在撒谎。
func (m *Meter) settleOK(rd *settleRound) {
	if rd.failed {
		return
	}
	m.mu.Lock()
	recovered := m.settleWarned
	m.settleWarned = false
	m.mu.Unlock()
	if recovered {
		m.log.Info("懒对账已恢复")
	}
}
