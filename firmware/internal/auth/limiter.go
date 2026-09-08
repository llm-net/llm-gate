package auth

// 固定退避器（MVP §8 简版，设计决策 4）：同一 key 连续失败 ≥5 次锁 60 秒；
// 成功清零；内存态（重启清零 = §12「本地物理路径解锁」的简版对应）。
// 不做指数退避。设备只有一个口令，登录路径的 key 因此恒为
// loginLimiterKey——map 仍留着，是因为它同时是「按 key 分桶」这个通用形状，
// 换成单个计数器省不下什么、却把将来多一条认证通道的余地砍掉了。

import (
	"sync"
	"time"
)

const (
	lockThreshold = 5
	lockDuration  = 60 * time.Second

	// limiterSweepAt：新增条目时若 map 已达此规模，先清理「未锁定且
	// 60 秒内无新失败」的陈旧条目。这是纯内存护栏，防 key 空间被灌爆；
	// 当前只有一个 key，正常运行远达不到该规模。
	limiterSweepAt = 4096
)

// LockDuration 是固定退避的锁定时长（= lockDuration）。导出只为一件事：让管理面
// 把**具体等多久**如实写进 429 文案与 Retry-After——现场对着一句不说时长的
// 「请稍后再试」只会反复点，而这把锁在锁定期内**连正确的口令也一起拒**
// （严格锁语义）。
const LockDuration = lockDuration

// limiter 按 key（认证通道名）累计连续失败并施加固定锁定。
type limiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*limitEntry
}

type limitEntry struct {
	failures    int
	lastFailure time.Time
	lockedUntil time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, entries: map[string]*limitEntry{}}
}

// locked 报告 key 当前是否处于锁定期。
func (l *limiter) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	return e != nil && l.now().Before(e.lockedUntil)
}

// recordFailure 记一次失败；返回本次失败是否使 key 进入（或续上）锁定。
// 计数只被 recordSuccess 清零，因此锁定期满后若继续失败会立即再锁
// （固定退避语义：连续失败 ≥5 即锁）。
func (l *limiter) recordFailure(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= limiterSweepAt {
			l.sweep(now)
		}
		e = &limitEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.lastFailure = now
	if e.failures >= lockThreshold {
		e.lockedUntil = now.Add(lockDuration)
		return true
	}
	return false
}

// recordSuccess 清零 key 的连续失败计数。
func (l *limiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// sweep 删除既未锁定又超过一个锁定周期无新失败的条目（调用方须持锁）。
// 仅在内存压力下发生，代价是陈旧账号的失败计数被放弃——两害相权取其轻。
func (l *limiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.lockedUntil) && now.Sub(e.lastFailure) > lockDuration {
			delete(l.entries, k)
		}
	}
}
