package agentauth

// 令牌提供者：盒子是订阅句柄的**唯一刷新者**（迭代 11 决策 1/5，机制自
// codexauth/token.go 原样迁入，语义逐条保持）。
//
// 决策 1 的整个安全论证都压在「唯一」这两个字上：轮换型 refresh token 用一次
// 就换一份新的（OpenAI 恒轮换；xAI 有时轮换——按轮换型对待覆盖两种），所以
// 只要有第二个消费者也在自轮换同一份句柄，两边就会互相把对方的世代作废，
// 而且谁都不知道自己被废了。盒子不把句柄发给任何人，于是在飞的世代永远只有一个。
//
// 这条不变量在进程内的落实就是 [Provider]：
//
//   - **single-flight 刷新**：一把 sync.Mutex，拿到锁的那个再复查一次到期时间。
//     N 个并发请求撞上过期令牌，只有第一个真去刷新，其余醒来时看到的是已经换好
//     的新令牌（决策 5 明写不为此引 x/sync——单账户下这就是完整语义）。
//   - **新世代立刻重新密封落库**（OnRotate）：不落库的话，重启后盒子手上是一份
//     已经被消费掉的 refresh token，必 401，而管理员那边看到的现象是「昨天还好好的」。
//   - **两类失败两种反应**：确定性拒绝（4xx，见 [OAuthError.Deterministic]）
//     标失效并**停止自动重试**——再打一万次也是同一个答复，只会把日志刷满、
//     把签发方那侧的失败计数刷满；网络错保留现有令牌，下次请求再试。
//
// §15.1：本类型不持有 logger。失败要留痕就靠两个回调——它们的实现方
// （管理面 / 网关）自己有 logger，也只有它们知道该记成什么。

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ExpirySkew 是提前量：令牌还剩这么点寿命就当它过期了。
// 一次代理请求从取令牌到打到订阅后端有网络时延，卡着到期时刻用它
// 会换来一个本可避免的 401。
const ExpirySkew = 60 * time.Second

// MinTokenValidity 是**刷新循环的总闸**：不管签发方怎么说，刚换来的令牌
// 至少按这么久有效。
//
// 少了它，任何一种「拿到即过期」的算法都会变成按请求速率刷新——而 refresh
// token 是轮换型的，那等于按请求速率烧世代，并发一撞就把整条句柄链废掉。
// 能造出「拿到即过期」的情形不止一种（expires_in 比提前量还短、应答不带
// expires_in 而 JWT 的 exp 只剩几十秒、签发方直接给一份已过期的令牌），
// 与其在每处算式上各钳一次，不如在出口设一道下限。
//
// 代价说清楚：若签发方真的只给了 10 秒寿命，这 30 秒里会有请求拿到 401——
// 那条路有 [Provider.Invalidate] + 重试一次接着，且按世代去重（见下），
// 不会退化成风暴。
const MinTokenValidity = 30 * time.Second

// RefreshTimeout 是一次刷新的自有预算。它套在**去掉取消信号**的 ctx 上
// （见 refreshLocked），所以必须自己有上限。
const RefreshTimeout = 30 * time.Second

// CallbackTimeout 是两个回调（落库 / 标状态）的预算。理由与 RefreshTimeout
// 同源，只是更隐蔽：回调**持锁执行**且拿的是去掉取消信号的 ctx，一次卡住的
// SQLite 写（SD 卡停顿、WAL 争用——板子上有先例）就会把 /agents/{codex,grok}/v1/responses 的
// 每一个请求无限期挡在锁上。给它一个上限，卡住的那次自己会返回。
const CallbackTimeout = 10 * time.Second

// RefreshFailCooldown 是**可重试失败**后的冷却期：这段时间内再有请求要令牌，
// 直接把上次的错误还给它，不再打一次网络。
//
// 挡的是排队放大：刷新在持锁期间完成，所以云端不可达时（受限现场会失败）
// 第 K 个排队的请求要等 K×RefreshTimeout 才轮到自己失败。冷却把这条长队压成
// 「一次真尝试 + 一串立即的明确报错」，正是那条「给明确报错并可重试，不静默
// 挂死」的口径。确定性拒绝不走这里——那条路有自己的闩，根本不再重试。
const RefreshFailCooldown = 5 * time.Second

// ProviderOptions 是 [Provider] 的两个外部接线口。两个都可以为 nil
// （测试与自检场景），生产两个都要接：不接 OnRotate，新世代就只活在内存里。
type ProviderOptions struct {
	// OnRotate 在刷新成功后被调用，参数是**新一代的整份句柄 JSON**，
	// 实现方负责密封落库（store.SetAgentAuthJSON）。
	//
	// 它返回的错误**不会让本次请求失败**：令牌已经在手且可用，为一次落库失败
	// 去回绝用户的请求既救不回已经被消费掉的那一代，也毫无意义。代价是下次重启
	// 会拿着旧世代去刷新并被拒——所以实现方**必须自己记一条日志**
	// （本包没有 logger，这是有意的，见文件头）。
	//
	// 调用时持有 Provider 的锁：落库顺序因此与世代顺序一致，永不倒退。
	// 实现方不要在里面回调 Provider 的方法，会死锁。
	OnRotate func(ctx context.Context, authJSON string) error

	// OnAuthExpired 在刷新被**确定性拒绝**时调用一次（此后不再自动重试，
	// 直到 [Provider.Reset]）。实现方据此把库里那行标成 auth_expired，
	// 管理台显「需重新登录」。同样持锁调用。
	OnAuthExpired func(ctx context.Context, cause error)
}

// Provider 持有一份订阅句柄的当前世代与刷新状态。经 [NewProvider] 构造，
// 并发安全。
//
// 生命周期：一个进程里每份订阅账号一个 provider；管理员重新登录或粘贴导入
// 之后由管理面 [Provider.Reset] 换上新句柄（而不是让它一直卡在失效状态上）。
type Provider struct {
	r    Refresher
	opts ProviderOptions

	// mu 既护状态也做 single-flight：刷新**在持锁期间完成**，
	// 于是「等锁 → 醒来复查 → 发现已经有新令牌了」天然就是复用而不是重刷。
	mu   sync.Mutex
	auth Handle
	// expiresAt 是当前 access_token 的到期时刻（已减去 ExpirySkew）。
	// **零值 = 到期时刻未知**，此时按「有效」处理：宁可多打一次 401 再刷新
	// （代理那侧有 Invalidate + 重试一次的路），也不要每次重启都白烧掉一代
	// 轮换型 refresh token。
	expiresAt time.Time
	// dirty 由 [Provider.Invalidate] 置位：上游拿这个令牌回了 401，
	// 不管到期时刻怎么写，下一次取都得先刷新。
	dirty bool
	// authExpired 是确定性拒绝后的**闩**：置位后一切取令牌请求直接回
	// ErrAuthExpired，不再打签发方。只有 Reset 能解开它。
	authExpired bool
	// failedAt / failedErr 记住最近一次**可重试**失败，用于冷却期内直接回绝
	// （见 RefreshFailCooldown）。刷新成功或 Reset 时清空。
	failedAt  time.Time
	failedErr error
}

// NewProvider 用一份已在手的句柄构造提供者。auth 为 nil 会得到一个恒回
// ErrAuthExpired 的 provider——调用方本就该在没有句柄时回 agent_not_configured，
// 不该走到这里。**调用方不得传 typed-nil**（各 provider 包的构造包装负责转换）。
func NewProvider(r Refresher, auth Handle, opts ProviderOptions) *Provider {
	p := &Provider{r: r, opts: opts}
	p.reset(auth)
	return p
}

// Current 返回一个可用的 access_token，必要时先刷新。
//
// 这是本包对外的主路径：代理每次转发前问一次，绝大多数时候是一次纯内存读。
func (p *Provider) Current(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 等锁期间调用方可能已经走了（客户端断开）。先看一眼，别为一个没人要的
	// 应答去消费一代 refresh token。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.authExpired {
		return "", ErrAuthExpired
	}
	// 没有句柄按失效处理：调用方本该先看库里有没有那一行、没有就回
	// agent_not_configured，走到这里说明它没看，回一个明确的错总比回空串好。
	if p.auth == nil {
		return "", ErrAuthExpired
	}
	now := p.r.Now()
	if !p.staleLocked(now) {
		return p.auth.AccessToken(), nil
	}
	// 冷却期内不再打网络，把上次的错误原样还回去（见 RefreshFailCooldown）。
	if p.failedErr != nil && now.Sub(p.failedAt) < RefreshFailCooldown {
		return "", p.failedErr
	}
	return p.refreshLocked(ctx)
}

// Invalidate 报告「**我拿着的这个** access_token 被上游拒了」，于是下一次
// [Provider.Current] 必刷新。代理在收到 401 时调它，然后重试一次。这条路存在的
// 理由是到期时刻不总是可知（见 expiresAt 的注释），以及厂商随时可以在到期之前
// 吊销一个令牌。
//
// **参数不是摆设，它是去重键**：调用方必须交回自己刚才用的那一个。令牌被吊销
// 时通常是 N 个在飞请求同时拿到 401，若无条件置脏，第一个请求换来的新令牌会被
// 第二个请求的「我 401 了」当场作废，接着第三个再作废第四代……N 个请求换 N 代，
// 每代还各带一次落库——正是本包处处在防的那种按请求速率烧世代。比对之下，
// 后到的那些一看「你说的那一代早已换掉」就自然不再触发刷新。
func (p *Provider) Invalidate(rejected string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rejected == "" || p.auth == nil || p.auth.AccessToken() != rejected {
		return
	}
	p.dirty = true
}

// Refresh **强制**换一代令牌，不管当前这一代还剩多少寿命（管理面「自检」）。
//
// 与 [Provider.Current] 的分工：Current 是数据面的主路径，能不刷就不刷；
// Refresh 是管理员明确要求的一次往返——他要的答复就是「这份订阅现在还能不能
// 换出新令牌」，而只有真去换一次才答得了。
//
// **它必须走这个 Provider，而不是另起一个**：本包的整条安全论证压在「在飞的
// 世代永远只有一个」上（见文件头），第二个 Provider 拿着同一份轮换型
// refresh token 自轮换，两边会互相把对方的世代作废——那正是决策 1 要消灭的
// 形态，而它发生在进程内与发生在用户 PC 上一样致命。
//
// 解闩与清冷却是有意的：自检就是「再试一次」。若这一次仍被确定性拒绝，
// 闩会当场重新落下（refreshLocked 里），[ProviderOptions.OnAuthExpired] 也会
// 再叫一次——重复标同一个状态是幂等的。
func (p *Provider) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.auth == nil {
		return ErrAuthExpired
	}
	p.authExpired = false
	p.failedAt, p.failedErr = time.Time{}, nil
	_, err := p.refreshLocked(ctx)
	return err
}

// Reset 换上一份新句柄（管理员重新登录 / 粘贴导入之后），并解开失效闩。
// auth 为 nil 等价于「这台盒子现在没有句柄」（typed-nil 由各 provider 包挡）。
//
// **同一世代的 Reset 必须真的什么都不做**，不能只是"结果看起来一样"：
// 最常见的调用来源恰恰是本 Provider 自己刚刷新完——[ProviderOptions.OnRotate]
// 落库抬高了行的 updated_at，于是下一个请求在数据面走"世代前进"那一支，
// 把库里读回来的**同一代**句柄再 Reset 一次。若照常重置：
//
//   - [expiryOf] 算出的到期时刻（优先用 expires_in，且有 [MinTokenValidity]
//     下限）会被 reset 里那条「只看 JWT exp」的算式盖掉。设备时钟快于签发方
//     （板子无 RTC、现场又封了 NTP——迭代 11 明写要面对这种现场），或签发方给的
//     exp 只剩几十秒时，刚换来的令牌当场又算过期 → 每个请求刷一代，正是那道
//     下限要挡的烧世代，而且这些刷新**都成功**，失败冷却拦不住。
//   - [Provider.Invalidate] 刚置的 dirty 也会被抹掉，于是"上游拒了这一代"
//     这个信号丢失。
//
// 判据取 access_token 相等：一次真正的重新登录必然带来新的 access_token。
func (p *Provider) Reset(auth Handle) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.auth != nil && auth != nil && p.auth.AccessToken() == auth.AccessToken() {
		return
	}
	p.reset(auth)
}

// Snapshot 取当前句柄（可能为 nil）。给需要读 provider 侧明文字段（如 codex
// 的 account_id）的调用方用——句柄不可变，取出后随便读，但**不得进日志与
// API 响应**（§15.1，句柄自遮蔽兜着最后一道）。
func (p *Provider) Snapshot() Handle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auth
}

// AuthExpired 报告失效闩是否已经落下（管理面自检据此不必再打一次签发方）。
func (p *Provider) AuthExpired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.authExpired
}

// reset 是 Reset 与构造共用的实现；调用方必须持锁（构造时无人可见，等价）。
func (p *Provider) reset(auth Handle) {
	p.auth = auth
	p.dirty = false
	p.authExpired = false
	p.expiresAt = time.Time{}
	p.failedAt, p.failedErr = time.Time{}, nil
	if auth != nil {
		// 到期时刻从 access_token 自己的自述来。解不出来就留零值 =
		// 未知 = 按有效处理（见 expiresAt 的注释）。
		if exp := auth.Expiry(); !exp.IsZero() {
			p.expiresAt = exp.Add(-ExpirySkew)
		}
	}
}

// staleLocked 判定当前令牌是否需要刷新；调用方必须持锁。
func (p *Provider) staleLocked(now time.Time) bool {
	if p.dirty {
		return true
	}
	return !p.expiresAt.IsZero() && !now.Before(p.expiresAt)
}

// refreshLocked 换一代令牌；调用方必须持锁，且已确认需要刷新。
func (p *Provider) refreshLocked(ctx context.Context) (string, error) {
	// **刷新用去掉取消信号的 ctx**：这一刻可能有 N 个请求在等这一次刷新，
	// 第一个调用方撤了不该把大家的刷新一起撤掉（而且轮换型 refresh token
	// 半路被撤的后果是「不知道刚才那次到底消费掉没有」）。自有超时兜底。
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RefreshTimeout)
	defer cancel()

	next, expiresIn, err := p.r.RefreshHandle(rctx, p.auth)
	if err != nil {
		if Deterministic(err) {
			// 确定性拒绝：落闩、通知上层标状态，此后不再自动重试（决策 5）。
			p.authExpired = true
			p.notify(ctx, err)
			// 两个 %w：errors.Is 对 ErrAuthExpired 与 *OAuthError 都成立
			// （前者给分支判断，后者留状态码与错误码给排障），
			// 而消息仍是一行——errors.Join 会在两句之间塞换行，进不了界面。
			return "", fmt.Errorf("%w（%w）", ErrAuthExpired, err)
		}
		// 网络错/5xx：现有令牌与 refresh token 原样保留，下次请求再试。
		// dirty 也保持原样——因 401 置位的那次不该被一次网络失败清掉。
		// 记下这一次失败，冷却期内后面排队的请求直接拿它，不再各打一次网络。
		p.failedAt, p.failedErr = p.r.Now(), err
		return "", err
	}

	now := p.r.Now()
	p.auth = next
	p.dirty = false
	p.failedAt, p.failedErr = time.Time{}, nil
	p.expiresAt = expiryOf(expiresIn, next.Expiry(), now)
	p.persist(ctx, next)
	return next.AccessToken(), nil
}

// persist 把新一代交给上层落库；调用方必须持锁。
//
// 持锁调用是有意的（落库顺序 = 世代顺序，永不倒退），所以这里必须给它一个
// 期限：见 CallbackTimeout。返回值有意丢弃——落库失败不该让本次请求失败
// （见 OnRotate 的注释），记日志是实现方的事。
func (p *Provider) persist(ctx context.Context, next Handle) {
	if p.opts.OnRotate == nil {
		return
	}
	// JSON() 在这里失败是**不可达**的：句柄只序列化几个 string 与一张此前已经
	// 解析过、因而必然合法的原文表。留着这个分支是防御性的，而本包没有 logger
	// 可记——所以宁可让它什么都不做，也不要退化成「把一份残缺的凭据写进库」。
	blob, err := next.JSON()
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CallbackTimeout)
	defer cancel()
	_ = p.opts.OnRotate(cctx, blob)
}

// notify 告诉上层这份句柄已经确定性失效；调用方必须持锁。期限同 persist。
func (p *Provider) notify(ctx context.Context, cause error) {
	if p.opts.OnAuthExpired == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), CallbackTimeout)
	defer cancel()
	p.opts.OnAuthExpired(cctx, cause)
}

// expiryOf 算新令牌的到期时刻（已减提前量）。expiresIn 优先——那是签发方
// 就这一次发放给出的权威值；没有就退回令牌自己的自述（selfExp）；两者都没有
// 则返回零值（未知，按有效处理）。
//
// 无论走哪条分支，结果都不早于 now+[MinTokenValidity]：那道下限是刷新循环的
// 总闸，理由写在该常量上。两条分支都需要它——`expires_in` 比提前量短，
// 或应答不带 `expires_in` 而 JWT 的 exp 只剩几十秒，是同一个坑的两个入口。
func expiryOf(expiresIn time.Duration, selfExp time.Time, now time.Time) time.Time {
	var expiry time.Time
	switch {
	case expiresIn > 0:
		expiry = now.Add(expiresIn).Add(-ExpirySkew)
	default:
		if selfExp.IsZero() {
			return time.Time{} // 到期时刻未知：按有效处理，靠 401 来纠正。
		}
		expiry = selfExp.Add(-ExpirySkew)
	}
	if floor := now.Add(MinTokenValidity); expiry.Before(floor) {
		return floor
	}
	return expiry
}
