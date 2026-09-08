package usage

// iteration-9 Phase 4 的可执行验收（准入内核这一半）：检查序、四档限额的
// nil/0 语义、RPM 的 60s 两桶滑窗与它的 Retry-After，以及日/周/月预算的
// Retry-After 落在本地时区的下一个窗口首秒上。
//
// 数值口径钉死在这里（时钟可注入）；HTTP 形态那一半在 internal/gateway。

import (
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func lim(v int64) *int64 { return &v }

// keyAuth 造一份鉴权点查的返回（限额全不限）。
func keyAuth() store.KeyAuth {
	return store.KeyAuth{KeyID: 7, KeyDisplay: "sk_prefix12…wxyz"}
}

// recordSpend 给密钥预置已用额（走 Record，与生产路径同一条口径）。
func recordSpend(m *Meter, at time.Time, micro int64) {
	s := textSample(at)
	// 1 元/百万 × (prompt 1e6 + completion 5e5) = 1_500_000 微元；这里换成
	// 单价直接给出想要的金额：只用输入一项，出价 = 目标金额。
	s.Tokens = Tokens{Prompt: 1_000_000}
	s.Pricing = `{"in":` + itoa(micro) + `}`
	m.Record(s)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// 不设任何限额 = 一律放行（升级既有库后的默认形态：行为一字不变）。
func TestAdmitNoLimitsAlwaysAllows(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)
	recordSpend(m, at, 999_000_000) // 花了 999 元也不该被拦
	for i := 0; i < 100; i++ {
		if d := m.Admit(keyAuth()); !d.Allowed {
			t.Fatalf("无限额时被拒: %+v", d)
		}
	}
}

// 没有按量额度时，检查序是 RPM → 日 → 周 → 月，**首违即拒**。每一档单独
// 打满时报出的必须是它自己，而不是排在后面的那一档。
func TestAdmitCheckOrderFirstViolationWins(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)

	// zeroBudgets 把三档预算全设为 0：报出来的必须仍是最靠前那一档。
	zeroBudgets := func(ka *store.KeyAuth) {
		ka.KeyBudgetDayMicro, ka.KeyBudgetWeekMicro, ka.KeyBudgetMonthMicro = lim(0), lim(0), lim(0)
	}
	cases := []struct {
		name   string
		mutate func(*store.KeyAuth)
		want   string
	}{
		{"RPM 最先", func(ka *store.KeyAuth) {
			ka.KeyRPMLimit = lim(0)
			zeroBudgets(ka)
		}, RejectRPM},
		{"日先于周", func(ka *store.KeyAuth) {
			zeroBudgets(ka)
		}, RejectKeyBudgetDay},
		{"周先于月", func(ka *store.KeyAuth) {
			ka.KeyBudgetWeekMicro, ka.KeyBudgetMonthMicro = lim(0), lim(0)
		}, RejectKeyBudgetWeek},
		{"月兜底", func(ka *store.KeyAuth) {
			ka.KeyBudgetMonthMicro = lim(0)
		}, RejectKeyBudgetMonth},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMeter(t, shanghai, at)
			ka := keyAuth()
			tc.mutate(&ka)
			d := m.Admit(ka)
			if d.Allowed {
				t.Fatalf("应被拒: %+v", d)
			}
			if d.Reason != tc.want {
				t.Errorf("拒绝档位 = %q，期望 %q", d.Reason, tc.want)
			}
			if d.RetryAfterSec < 1 {
				t.Errorf("Retry-After = %d，必须 ≥ 1 秒", d.RetryAfterSec)
			}
		})
	}
}

// 0 与 nil 是两个状态：nil = 不限，0 = 零额度（一律拒绝）。
// 而额度未用满时照常放行——判据是「已用 >= 限额」。
func TestAdmitZeroIsNotUnlimited(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)

	ka := keyAuth()
	ka.KeyBudgetDayMicro = lim(0)
	if d := m.Admit(ka); d.Allowed {
		t.Error("日预算 0 且没有按量额度时应一律拒绝")
	}

	ka.KeyBudgetDayMicro = lim(3_000_000) // 3 元
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("额度未用满不该拒: %+v", d)
	}
	recordSpend(m, at, 2_999_999)
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("差 1 微元用满，仍该放行: %+v", d)
	}
	recordSpend(m, at, 1)
	if d := m.Admit(ka); d.Allowed || d.Reason != RejectKeyBudgetDay {
		t.Errorf("恰好用满应拒: %+v", d)
	}
}

// 预算用尽后，按量额度有剩余就继续放行；尚未落库的待扣也要从热路径带回的
// 数据库剩余中扣掉。RPM 是速率治理，不由按量额度绕过。
func TestAdmitMeteredAllowanceBackstopsBudgetsOnly(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)
	ka := keyAuth()
	ka.KeyBudgetDayMicro = lim(0)
	ka.KeyMeteredAllowanceMicro = 5_000_000
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("预算用尽但有按量额度应放行: %+v", d)
	}

	// 最终记账 6 元，待扣已经超过热路径快照里的 5 元；下一次必须拒绝。
	m.Record(textSample(at))
	if pending := m.Spend(ka.KeyID).MeteredAllowancePendingMicro; pending != 6_000_000 {
		t.Fatalf("按量额度待扣 = %d，期望 6000000", pending)
	}
	if d := m.Admit(ka); d.Allowed || d.Reason != RejectKeyBudgetDay {
		t.Fatalf("有效按量额度耗尽后应按日预算拒绝: %+v", d)
	}

	ka.KeyRPMLimit = lim(0)
	ka.KeyMeteredAllowanceMicro = 50_000_000
	if d := m.Admit(ka); d.Allowed || d.Reason != RejectRPM {
		t.Fatalf("按量额度不得绕过 RPM: %+v", d)
	}
}

// 预算的 Retry-After 指向**本地时区**的下一个窗口首秒（日 → 明天 0 点，
// 周 → 下周一 0 点，月 → 下月 1 日 0 点），向上取整。
func TestAdmitBudgetRetryAfterPointsAtNextWindow(t *testing.T) {
	// 上海时间 8 月 8 日（周六）22:30:30 → 距 8 月 9 日 0 点 5370 秒；
	// 距下周一（8 月 10 日）0 点 = 5370 + 86400 秒；
	// 距 9 月 1 日 0 点 = 5370 + 23×86400 = 1_992_570 秒。
	at := time.Date(2026, 8, 8, 22, 30, 30, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)

	ka := keyAuth()
	ka.KeyBudgetDayMicro = lim(0)
	if d := m.Admit(ka); d.RetryAfterSec != 5370 {
		t.Errorf("日预算 Retry-After = %d，期望 5370（到本地次日零点）", d.RetryAfterSec)
	}
	ka.KeyBudgetDayMicro = nil
	ka.KeyBudgetWeekMicro = lim(0)
	if d := m.Admit(ka); d.RetryAfterSec != 5370+86400 {
		t.Errorf("周预算 Retry-After = %d，期望 %d（到本地下周一零点）",
			d.RetryAfterSec, 5370+86400)
	}
	ka.KeyBudgetWeekMicro = nil
	ka.KeyBudgetMonthMicro = lim(0)
	if d := m.Admit(ka); d.RetryAfterSec != 5370+23*86400 {
		t.Errorf("月预算 Retry-After = %d，期望 %d（到本地下月首秒）",
			d.RetryAfterSec, 5370+23*86400)
	}
}

// RPM：60s 两桶滑窗。放行才计数（拒绝不计——否则窗口永远滑不出来），
// 窗口滑过自动恢复。
func TestAdmitRPMSlidingWindow(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, now)
	ka := keyAuth()
	ka.KeyRPMLimit = lim(3)

	for i := 0; i < 3; i++ {
		if d := m.Admit(ka); !d.Allowed {
			t.Fatalf("第 %d 次应放行: %+v", i+1, d)
		}
	}
	d := m.Admit(ka)
	if d.Allowed || d.Reason != RejectRPM {
		t.Fatalf("第 4 次应被 RPM 拒: %+v", d)
	}
	// 拒绝不计数：连撞 10 次之后，窗口里仍然只有那 3 次放行。
	for i := 0; i < 10; i++ {
		m.Admit(ka)
	}

	// 桶整体前移一格（本桶清空、上一桶权重随偏移线性衰减）：
	// 12:01:00 时前一桶权重 60/60 = 3，仍然满；12:01:40 时权重
	// 20/60 → floor(3×20/60) = 1，放得下 2 个。
	m.now = func() time.Time { return time.Date(2026, 8, 8, 12, 1, 0, 0, shanghai) }
	if d := m.Admit(ka); d.Allowed {
		t.Error("刚过桶边界时前一桶仍满权重，应继续拒")
	}
	m.now = func() time.Time { return time.Date(2026, 8, 8, 12, 1, 40, 0, shanghai) }
	for i := 0; i < 2; i++ {
		if d := m.Admit(ka); !d.Allowed {
			t.Fatalf("窗口滑过后第 %d 次应放行: %+v", i+1, d)
		}
	}

	// 整整两个窗口无请求 → 估计值归零，完全恢复。
	m.now = func() time.Time { return time.Date(2026, 8, 8, 12, 5, 0, 0, shanghai) }
	for i := 0; i < 3; i++ {
		if d := m.Admit(ka); !d.Allowed {
			t.Fatalf("两个窗口后应完全恢复，第 %d 次仍被拒: %+v", i+1, d)
		}
	}
}

// RPM 的 Retry-After 必须是**真的等到那一刻就能过**：按它等待之后再判一次
// 必须放行。这是逐秒推演而不是求闭式解的全部理由——差一秒就是让听话的客户端
// 再撞一次 429。
func TestAdmitRPMRetryAfterIsActuallyEnough(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 17, 0, shanghai)
	for _, limit := range []int64{1, 2, 5} {
		now := base
		m, _ := newTestMeter(t, shanghai, now)
		m.now = func() time.Time { return now }
		ka := keyAuth()
		ka.KeyRPMLimit = lim(limit)

		for i := int64(0); i < limit; i++ {
			if d := m.Admit(ka); !d.Allowed {
				t.Fatalf("limit=%d 第 %d 次应放行: %+v", limit, i+1, d)
			}
			now = now.Add(time.Second) // 打散在窗口里
		}
		d := m.Admit(ka)
		if d.Allowed {
			t.Fatalf("limit=%d 打满后应拒", limit)
		}
		// 等到它说的那一刻。
		now = now.Add(time.Duration(d.RetryAfterSec) * time.Second)
		if again := m.Admit(ka); !again.Allowed {
			t.Errorf("limit=%d 按 Retry-After=%d 等过之后仍被拒: %+v",
				limit, d.RetryAfterSec, again)
		}
	}
}

// 无归属（未认证不可能走到这里，但 keyID ≤ 0 的防御分支要成立）：
// 没有计数器就没有已用额，限额若已设仍按 0 已用判定。
func TestAdmitWithoutSubjectIDs(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)
	ka := store.KeyAuth{}
	if d := m.Admit(ka); !d.Allowed {
		t.Errorf("零值 KeyAuth（全不限）应放行: %+v", d)
	}
	ka.KeyBudgetDayMicro = lim(0)
	if d := m.Admit(ka); d.Allowed {
		t.Error("零额度即便无归属也该拒")
	}
}

// rpm 表的内存护栏：达到 sweep 阈值时清掉两个窗口内没再出现过的条目。
func TestRPMSweepDropsStaleKeys(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, now)
	m.mu.Lock()
	for i := 0; i < rpmSweepAt; i++ {
		m.rpm[int64(i+1000)] = &rpmWindow{last: now}
	}
	m.mu.Unlock()

	m.now = func() time.Time { return now.Add(5 * time.Minute) }
	ka := keyAuth()
	ka.KeyRPMLimit = lim(10)
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("新密钥应放行: %+v", d)
	}
	m.mu.Lock()
	n := len(m.rpm)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("sweep 后条目数 = %d，期望只剩刚用过的那一把", n)
	}
}
