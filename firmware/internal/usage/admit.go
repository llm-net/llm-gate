package usage

// admit.go 是**预算准入**：请求打到上游之前的那道闸（iteration-9 Phase 4）。
//
// 三条贯穿本文件的口径：
//
//   - **纯内存判定**。限额四项与按量额度剩余随鉴权那一次点查带回（store.KeyAuth），
//     已用额在 Meter 的预算计数器里，所以准入不查库、不加一次 IO。改限额
//     即时生效（下一个请求的点查就带上新值），改价同理（计价在 Record
//     时点做）。
//   - **检查序固定**：RPM → 日 → 周 → 月。RPM 命中立即拒绝；预算命中时若
//     该密钥仍有按量额度则兜底放行，额度耗尽才按首违档拒绝。拒绝档位写进
//     Decision.Reason，只进内存环给管理员排障，不外发给客户端。
//   - **拒绝不计 RPM**。被拒的请求没消费任何上游资源，把它也算进窗口会让
//     「窗口滑过自动恢复」变成「越撞越锁」——客户端一旦超速就永远出不来。
//     预算被拒同理（它压根没走到 RPM 记数那一步）。
//
// 方向纪律与整个计量层一致：**宁松勿紧**。计数器落后（5 分钟冲刷窗口、掉电
// 丢一个窗口、半小时偏移时区的播种误差）只会让预算晚一点生效，绝不会凭空
// 拦下合法请求。

import (
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 拒绝档位（Decision.Reason，也是 Sample.RejectReason 的取值）。
// 它们进内存环形缓冲供管理员排障，**不进客户端响应**——对调用方而言
// 「哪一档限额把我拦了」是设备内部的治理细节。
const (
	RejectRPM            = "rpm"
	RejectKeyBudgetDay   = "key_budget_day"
	RejectKeyBudgetWeek  = "key_budget_week"
	RejectKeyBudgetMonth = "key_budget_month"
)

const (
	// rpmWindowSeconds 是 RPM 的窗口长度（每分钟请求数 = 60 秒窗口）。
	rpmWindowSeconds int64 = 60
	// rpmSweepAt 是 rpm 表的内存护栏：新建条目时若已达此规模，先清掉两个
	// 窗口内没再出现过的密钥（照 auth/limiter.go 的 sweep 写法）。正常运行
	// 远达不到——设备上的密钥是管理台一把一把签出来的。
	rpmSweepAt = 4096
)

// Decision 是一次准入判定的结果。Allowed 为假时 Reason 说明是哪一档限额，
// RetryAfterSec 是给客户端的 Retry-After 建议（秒，恒 ≥ 1，向上取整——
// 绝不建议一个还没到就必然再撞一次的时刻）。
type Decision struct {
	Allowed       bool
	Reason        string
	RetryAfterSec int
}

// Admit 判定一次数据面调用是否放行。四个消费入口（chat/messages/video 提交/
// images）在鉴权之后、选路之前各调一次；count_tokens、/v1/models 与任务的
// 查询/下载/取消不过准入——预算拦的是**新消费**，查已经花掉的钱不拦。
//
// ka 是鉴权点查带回的整行：KeyID 定位计数器，四项限额（nil = 不限）与
// 按量额度剩余是判据。**0 是一个有效限额**：预算为 0 且没有按量额度时
// 一律拒绝，与 nil 泾渭分明。
func (m *Meter) Admit(ka store.KeyAuth) Decision {
	now := m.now()
	local := now.In(m.loc)
	dayKey, weekKey, monKey := windowKeys(local)

	m.mu.Lock()
	defer m.mu.Unlock()

	// ① RPM。放在最前不只是因为便宜：它是唯一一档「等一会儿就能过」的限额，
	// 先判它能让高频客户端拿到有意义的 Retry-After。
	var w *rpmWindow
	if ka.KeyRPMLimit != nil && ka.KeyID > 0 {
		w = m.rpmWindowLocked(ka.KeyID, now)
		if w.estimate(now) >= *ka.KeyRPMLimit {
			return Decision{Reason: RejectRPM, RetryAfterSec: w.retryAfter(now, *ka.KeyRPMLimit)}
		}
	}

	// ② 日 → 周 → 月。先刷新预算快照，供请求收尾与后台任务清算判断本笔是否
	// 应扣按量额度。进程刚启动、尚未见过准入的密钥不扣（宁松勿紧）。
	kDay, kWeek, kMon := rollSpend(m.spendKey[ka.KeyID], dayKey, weekKey, monKey)
	a := m.allowanceLocked(ka.KeyID)
	if a != nil {
		a.day, a.week, a.month = ka.KeyBudgetDayMicro, ka.KeyBudgetWeekMicro, ka.KeyBudgetMonthMicro
		a.known = true
	}
	var over Decision
	switch {
	case exhausted(ka.KeyBudgetDayMicro, kDay):
		over = Decision{Reason: RejectKeyBudgetDay, RetryAfterSec: secsUntil(now, nextDayStart(local))}
	case exhausted(ka.KeyBudgetWeekMicro, kWeek):
		over = Decision{Reason: RejectKeyBudgetWeek, RetryAfterSec: secsUntil(now, nextWeekStart(local))}
	case exhausted(ka.KeyBudgetMonthMicro, kMon):
		over = Decision{Reason: RejectKeyBudgetMonth, RetryAfterSec: secsUntil(now, nextMonthStart(local))}
	}
	if over.Reason != "" {
		// 鉴权点查给的是数据库剩余；pending 是本进程已消费但尚未批量落库的
		// 部分。两者相减才是当下真正还可放行的额度。
		if a == nil || ka.KeyMeteredAllowanceMicro-a.pending <= 0 {
			return over
		}
	}

	// 放行才记一次 RPM（见文件头：拒绝不计数，否则窗口永远滑不出来）。
	if w != nil {
		w.hit(now)
	}
	return Decision{Allowed: true}
}

// exhausted 报告某档限额是否已用满。nil = 不限；**0 是「零额度」而非不限**
// （管理员把预算设成 0 就是要把这把密钥停在门外，与删限额是两回事）。

// 判据是 `已用 >= 限额`：额度用尽的那一刻就该拦，而不是等到超出。
func exhausted(limit *int64, spent int64) bool {
	return limit != nil && spent >= *limit
}

// rollSpend 取一个主体的已用额（顺手把计数器翻到当前窗口）；无条目得零。
func rollSpend(c *spend, dayKey, weekKey, monKey string) (day, week, month int64) {
	if c == nil {
		return 0, 0, 0
	}
	c.roll(dayKey, weekKey, monKey)
	return c.dayMicro, c.weekMicro, c.monMicro
}

// nextDayStart / nextWeekStart / nextMonthStart 是预算窗口的下一个起点（设备
// 本地时区）——日/周/月预算的 Retry-After 就是到那一刻的秒数。
func nextDayStart(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).AddDate(0, 0, 1)
}

func nextWeekStart(local time.Time) time.Time {
	return weekStart(local).AddDate(0, 0, 7)
}

func nextMonthStart(local time.Time) time.Time {
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location()).AddDate(0, 1, 0)
}

// secsUntil 是到 target 的秒数，**向上取整**且恒 ≥ 1：Retry-After 若比真实
// 时刻早哪怕一秒，听话的客户端就会再撞一次 429。
func secsUntil(now, target time.Time) int {
	d := target.Sub(now)
	if d <= time.Second {
		return 1
	}
	return int((d + time.Second - 1) / time.Second)
}

// ---- RPM 滑动窗口 ----

// rpmWindow 是一把密钥的 60s 滑动窗口计数器：两只相邻的分钟桶，估计值 =
// 本桶实数 + 前一桶按尚未滑出的比例折算。整数算术，全程无浮点（README 硬
// 约束「金额与配额用定点数」）。
//
// 为什么不是固定窗口：固定窗口在边界上允许两倍突发（59 秒打满 + 第 61 秒
// 再打满）。也不是精确的滑动日志（每请求一个时间戳）：那要为每把密钥留一
// 个 limit 长的环，而两桶加权的误差只在「前一分钟的请求是否均匀分布」这一
// 个假设上，对 RPM 这种粗粒度闸门足够。
//
// 所有方法都要求调用方已持 Meter.mu，且窗口已由 rpmWindowLocked 翻到当前桶。
type rpmWindow struct {
	bucket int64 // 当前桶号 = unix 秒 / rpmWindowSeconds
	cur    int64 // 本桶已放行数
	prev   int64 // 上一桶已放行数
	last   time.Time
}

// rpmWindowLocked 取（必要时新建）某密钥的窗口并翻到 now 所在的桶。
func (m *Meter) rpmWindowLocked(keyID int64, now time.Time) *rpmWindow {
	w := m.rpm[keyID]
	if w == nil {
		if len(m.rpm) >= rpmSweepAt {
			m.sweepRPMLocked(now)
		}
		w = &rpmWindow{bucket: now.Unix() / rpmWindowSeconds, last: now}
		m.rpm[keyID] = w
		return w
	}
	w.roll(now)
	return w
}

// sweepRPMLocked 丢掉两个窗口内没再出现过的条目——它们对估计值的贡献恒为 0。
func (m *Meter) sweepRPMLocked(now time.Time) {
	stale := time.Duration(2*rpmWindowSeconds) * time.Second
	for id, w := range m.rpm {
		if now.Sub(w.last) > stale {
			delete(m.rpm, id)
		}
	}
}

// roll 把窗口对齐到 now 所在的桶：相邻桶前移一格，隔了两桶以上直接清空。
func (w *rpmWindow) roll(now time.Time) {
	b := now.Unix() / rpmWindowSeconds
	switch {
	case b == w.bucket:
	case b == w.bucket+1:
		w.prev, w.cur = w.cur, 0
	default:
		w.prev, w.cur = 0, 0
	}
	w.bucket = b
}

// estimate 是当前的滑窗估计值。
func (w *rpmWindow) estimate(now time.Time) int64 { return w.project(now, 0) }

// project 推演 after 秒之后的估计值（期间没有新请求）——Retry-After 的推算
// 基础。窗口在无新请求时单调递减，最多两个窗口后归零。
func (w *rpmWindow) project(now time.Time, after int64) int64 {
	off := now.Unix()%rpmWindowSeconds + after
	if off >= 2*rpmWindowSeconds {
		return 0
	}
	cur, prev := w.cur, w.prev
	if off >= rpmWindowSeconds {
		prev, cur = cur, int64(0)
		off -= rpmWindowSeconds
	}
	return cur + prev*(rpmWindowSeconds-off)/rpmWindowSeconds
}

// retryAfter 推算「几秒之后估计值会掉到 limit 以下」。逐秒推演而不是求闭式解：
// 至多 120 次整数运算、只在拒绝路径上跑，换来的是「算出来的秒数就是判定函数
// 自己认的那一刻」——闭式解一旦与 estimate 的取整方向差一，客户端就会照着
// 建议再撞一次 429。
func (w *rpmWindow) retryAfter(now time.Time, limit int64) int {
	for d := int64(1); d <= 2*rpmWindowSeconds; d++ {
		if w.project(now, d) < limit {
			return int(d)
		}
	}
	// limit ≤ 0（密钥被彻底关停）：没有任何等待能让它通过，给一个有界建议
	// 而不是让客户端立刻重试。
	return int(rpmWindowSeconds)
}

// hit 记一次**已放行**的请求。
func (w *rpmWindow) hit(now time.Time) {
	w.cur++
	w.last = now
}
