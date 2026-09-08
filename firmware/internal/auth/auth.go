// Package auth 是设备管理面的登录与会话核心：Argon2id 口令哈希、出厂默认
// 口令播种、会话签发与校验、固定退避防爆破、认证事件审计。HTTP 端点与
// Cookie 语义属 internal/admin，本包只提供与传输无关的核心逻辑。
//
// **设备只有一个管理员，登录只有一个口令**（0019 起「用户」概念整个退场，
// 口令是 settings 表里的一条单值配置 [store.SettingAdminPasswordHash]）。
// 会话因此不指向任何主体——它只表示「这个浏览器验过设备口令」。
//
// 架构 §12 落点：人类登录与业务 API密钥 分离（登录只认口令，API密钥 换不到
// 会话）、Argon2id 存储、认证事件全部进追加式审计。
//
// **出厂默认口令**：设备开机即可凭 [DefaultPassword] 登录，由
// [Service.EnsureDefaultPassword] 在启动期播种。这是对架构 §12「无默认密码」的
// 一次明确偏离——口令写在固件里，等同路由器背面的出厂口令：交付后应尽快改密，
// 设备也在每次播种时记一条 Warn 提醒。
//
// §15.1 纪律：口令、会话令牌、API密钥 在任何级别不落日志，也不进入审计 detail；
// 错误信息不回显凭证值。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 会话参数：32B crypto/rand 令牌（base64url ≈43 字符），库存 SHA-256 摘要
// （防库泄露重放），绝对 TTL 7 天，不做滑动续期。
const (
	SessionTTL        = 7 * 24 * time.Hour
	sessionTokenBytes = 32

	sessionCleanupInterval = time.Hour
)

// DefaultPassword 是出厂默认登录口令：新设备开机即可凭它进本地管理台。
// 它恰好压在 [PasswordMinRunes] 上——策略下限就是照着它定的，设备自带的口令
// 按自己的规矩必须合法。
const DefaultPassword = "llm-gate"

// loginLimiterKey 是退避器的唯一键。设备只有一个口令，锁定因此是**设备级**的
// ——与「用户」还在时按唯一管理员的用户名锁语义等价，只是名字没了。
const loginLimiterKey = "device"

// 审计事件名（功能边界：播种/登录成败/锁定/登出都要落审计）。
// Key 与设备配置的变更事件由管理端点层另行记录。
const (
	EventLoginSuccess = "login.success"
	EventLoginFailure = "login.failure"
	EventLoginLockout = "login.lockout"
	EventLogout       = "logout"
	// EventPasswordSeed：启动期播下出厂默认口令（一台设备一生通常只有一次
	// ——库被清空或口令行被抹掉时会再播一次）。detail 里没有口令。
	EventPasswordSeed = "password.seed"
	// EventPasswordChange：管理员改了登录口令（全部旧会话随之失效）。
	// 由管理端点在验过旧口令之后记；本包只提供 SetPassword 这一步动作。
	EventPasswordChange = "password.change"
)

// entityDevicePassword 是口令相关审计行的 entity。设备只有一份口令，
// 不像用户那样需要 id 来指认是哪一个。
const entityDevicePassword = "device:password"

// 对外错误。ErrInvalidCredentials 不区分「口令错误」与「口令尚未播种」——
// 细分原因只进审计，不给客户端枚举面。
var (
	ErrInvalidCredentials = errors.New("auth: 密码错误")
	ErrLocked             = errors.New("auth: 连续失败次数过多，请稍后再试")
	ErrSessionInvalid     = errors.New("auth: 会话无效或已过期")
	ErrPasswordPolicy     = errors.New("auth: 密码不符合策略")
)

// Service 聚合登录与会话核心。方法并发安全。
type Service struct {
	store  *store.Store
	logger *slog.Logger
	now    func() time.Time

	loginLimiter *limiter

	// dummyHash 是启动时随机生成的等参数假哈希：口令尚未播种时也执行一次
	// 同成本验证，避免按响应时长区分「设备还没有口令」与「口令输错了」。
	dummyHash string

	// defaultMu 护住「当前口令是不是出厂缺省值」的备忘。Argon2id 验证一次要几十
	// 毫秒，而 Cloudflare Tunnel 的公网管理面闸门在每个 Admin 档请求上都要问这
	// 一句；答案只在播种与改密时变化，所以备忘一次、由那两处刷新。
	defaultMu    sync.Mutex
	defaultKnown bool
	defaultVal   bool
}

// Option 配置 Service（目前仅测试用的时钟注入）。
type Option func(*Service)

// WithClock 注入时钟（测试用）。
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// LoginResult 是一次成功登录的产物。Token 是令牌明文，
// 只在此处出现一次——调用方写入 Set-Cookie 后即丢弃，绝不落库、落日志。
type LoginResult struct {
	Token   string
	Session *store.Session
}

// New 装配 Service：清理一次过期会话（启动侧清理；每小时循环见
// RunSessionCleanup），删掉旧版初始化码文件。dataDir 与 store.Open 的数据目录相同。
//
// **它不播种默认口令**：那一步是装配层的显式动作
// （[Service.EnsureDefaultPassword]），好让测试装配自己决定库里有没有口令。
func New(ctx context.Context, st *store.Store, dataDir string, logger *slog.Logger, opts ...Option) (*Service, error) {
	if st == nil {
		return nil, errors.New("auth: store 不能为空")
	}
	s := &Service{
		store:  st,
		logger: logger,
		now:    time.Now,
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	for _, o := range opts {
		o(s)
	}
	s.loginLimiter = newLimiter(func() time.Time { return s.now() })

	if n, err := st.DeleteExpiredSessions(ctx, s.now()); err != nil {
		return nil, fmt.Errorf("启动清理过期会话: %w", err)
	} else if n > 0 {
		s.logger.Info("已清理过期会话", "count", n)
	}

	// 等化计时用的假哈希：口令取随机值，参数与真实哈希一致。
	dummySecret := make([]byte, 16)
	if _, err := rand.Read(dummySecret); err != nil {
		return nil, fmt.Errorf("初始化认证服务: %w", err)
	}
	dummy, err := HashPassword(hex.EncodeToString(dummySecret))
	if err != nil {
		return nil, fmt.Errorf("初始化认证服务: %w", err)
	}
	s.dummyHash = dummy
	return s, nil
}

// EnsureDefaultPassword 在库里还没有登录口令时播下出厂默认口令
// （[DefaultPassword]）。装配层在启动期调用一次（见 cmd/llmgate 的
// runGatewayd），返回错误即启动失败——一台谁都登不进去的设备没有继续启动的意义。
//
// 判据是**口令哈希这一行在不在**：重刷、迁库或口令行被抹掉之后同样该把这条路
// 补回来；反过来，只要库里已有口令就一个字节都不动——绝不重置成默认值。
func (s *Service) EnsureDefaultPassword(ctx context.Context) error {
	phc, err := s.store.GetSetting(ctx, store.SettingAdminPasswordHash)
	if err != nil {
		return fmt.Errorf("播种默认口令: %w", err)
	}
	if phc != "" {
		return nil
	}
	// 播种走 HashPassword 不过 ValidatePassword：默认口令是编译期常量，
	// 策略闸管的是**人自己设**的口令。两者宽度其实一致（见 DefaultPassword）。
	hash, err := HashPassword(DefaultPassword)
	if err != nil {
		return fmt.Errorf("播种默认口令: %w", err)
	}
	if err := s.store.SetSetting(ctx, store.SettingAdminPasswordHash, hash); err != nil {
		return fmt.Errorf("播种默认口令: %w", err)
	}
	s.rememberDefault(true)
	s.audit(ctx, store.AuditEvent{
		Event: EventPasswordSeed, Entity: entityDevicePassword, Detail: "播下出厂默认登录口令",
	})
	// 口令一个字都不进日志（§15.1）——它是编译进固件的公开缺省，但纪律不开例外。
	s.logger.Warn("已播下出厂默认登录口令，请登录后立即修改")
	return nil
}

// Login 校验口令并签发会话。设备只有一个口令，所以没有主语——登录回答的是
// 「你知不知道这台设备的口令」。人类登录与业务 API密钥 依旧完全分离：Key
// 永远不能换会话。
func (s *Service) Login(ctx context.Context, password, remoteIP string) (*LoginResult, error) {
	if s.loginLimiter.locked(loginLimiterKey) {
		s.audit(ctx, store.AuditEvent{
			Event: EventLoginLockout, Detail: "锁定期内登录被拒", RemoteIP: remoteIP,
		})
		return nil, ErrLocked
	}

	phc, err := s.store.GetSetting(ctx, store.SettingAdminPasswordHash)
	if err != nil {
		return nil, err
	}
	reason := ""
	if phc == "" {
		reason = "设备尚未设置登录口令"
	}

	ok := false
	if phc != "" {
		ok, err = VerifyPassword(phc, password)
		if err != nil {
			// 库中哈希串损坏属服务端事故；错误只含格式信息，无凭证值。
			s.logger.Error("口令哈希无法校验", "error", err)
			reason, ok = "口令哈希异常", false
		}
	} else {
		// 计时等化：没有口令可验时也执行一次同参数验证。
		_, _ = VerifyPassword(s.dummyHash, password)
	}

	if !ok {
		if reason == "" {
			reason = "密码错误"
		}
		lockedNow := s.loginLimiter.recordFailure(loginLimiterKey)
		s.audit(ctx, store.AuditEvent{
			Event: EventLoginFailure, Detail: reason, RemoteIP: remoteIP,
		})
		if lockedNow {
			s.audit(ctx, store.AuditEvent{
				Event: EventLoginLockout, Detail: lockoutDetail(), RemoteIP: remoteIP,
			})
		}
		return nil, ErrInvalidCredentials
	}

	s.loginLimiter.recordSuccess(loginLimiterKey)
	token, sess, err := s.newSession(ctx, remoteIP)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, store.AuditEvent{
		Event: EventLoginSuccess, Entity: entityDevicePassword, RemoteIP: remoteIP,
	})
	return &LoginResult{Token: token, Session: sess}, nil
}

// ValidateSession 按令牌明文校验会话：摘要点查 + 过期判定。
// 未命中与已过期同样返回 ErrSessionInvalid；过期行由清理循环删除。
func (s *Service) ValidateSession(ctx context.Context, token string) (*store.Session, error) {
	if token == "" {
		return nil, ErrSessionInvalid
	}
	sess, err := s.store.GetSessionByTokenDigest(ctx, digestToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, err
	}
	if !s.now().Before(sess.ExpiresAt) {
		return nil, ErrSessionInvalid
	}
	return sess, nil
}

// Logout 删除令牌对应的会话并落审计；令牌未知或已删则幂等无错（不审计）。
func (s *Service) Logout(ctx context.Context, token, remoteIP string) error {
	if token == "" {
		return nil
	}
	digest := digestToken(token)
	if _, err := s.store.GetSessionByTokenDigest(ctx, digest); errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.store.DeleteSession(ctx, digest); err != nil {
		return err
	}
	s.audit(ctx, store.AuditEvent{
		Event: EventLogout, Entity: entityDevicePassword, RemoteIP: remoteIP,
	})
	return nil
}

// IssueSession 为已在别的通道完成身份确认的请求直接签发会话。当前唯一场景
// 是改密后的会话轮换（管理端点已验旧口令、SetPassword 已删全部旧会话，需为
// 当前浏览器换发新令牌）。不经退避器、不写认证审计——凭据校验与变更审计由
// 调用场景自带；令牌明文同样只出现在返回值这一次。
func (s *Service) IssueSession(ctx context.Context, remoteIP string) (string, error) {
	token, _, err := s.newSession(ctx, remoteIP)
	return token, err
}

// VerifyCurrent 校验给定口令是否就是设备当前口令（改密端点验旧口令用）。
// 库里还没有口令时恒为 false，并同样跑一次等参数验证做计时等化。
// **它不过退避器**：调用方已持有效会话，这不是一条未认证的爆破面。
func (s *Service) VerifyCurrent(ctx context.Context, password string) (bool, error) {
	phc, err := s.store.GetSetting(ctx, store.SettingAdminPasswordHash)
	if err != nil {
		return false, err
	}
	if phc == "" {
		_, _ = VerifyPassword(s.dummyHash, password)
		return false, nil
	}
	return VerifyPassword(phc, password)
}

// SetPassword 校验策略、写入新 Argon2id 哈希并清空全部会话（改密踢会话）。
// 旧口令校验与操作权限由管理面端点负责（VerifyCurrent + 有效会话）；
// password.change 审计同样由端点层记录——本包只记 login/logout/seed。
func (s *Service) SetPassword(ctx context.Context, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.store.SetSetting(ctx, store.SettingAdminPasswordHash, hash); err != nil {
		return err
	}
	s.rememberDefault(newPassword == DefaultPassword)
	return s.store.DeleteAllSessions(ctx)
}

// PasswordIsDefault 报告设备当前口令是否仍是出厂缺省值 [DefaultPassword]。
// 首次调用做一次真实验证并备忘，此后由播种与改密刷新；库读失败原样返回错误，
// 调用方按「不能证明已改密」处理（失败关闭）。
func (s *Service) PasswordIsDefault(ctx context.Context) (bool, error) {
	s.defaultMu.Lock()
	if s.defaultKnown {
		v := s.defaultVal
		s.defaultMu.Unlock()
		return v, nil
	}
	s.defaultMu.Unlock()
	ok, err := s.VerifyCurrent(ctx, DefaultPassword)
	if err != nil {
		return false, err
	}
	s.rememberDefault(ok)
	return ok, nil
}

// rememberDefault 刷新「口令是否为缺省值」的备忘。
func (s *Service) rememberDefault(isDefault bool) {
	s.defaultMu.Lock()
	s.defaultKnown = true
	s.defaultVal = isDefault
	s.defaultMu.Unlock()
}

// CleanupExpiredSessions 删除已过期的会话行，返回删除条数。
func (s *Service) CleanupExpiredSessions(ctx context.Context) (int64, error) {
	return s.store.DeleteExpiredSessions(ctx, s.now())
}

// RunSessionCleanup 先立即清理一次，此后每小时清理一次，直到 ctx 结束。
// 装配层（cmd/llmgate 的 runGatewayd）在独立 goroutine 中调用。
func (s *Service) RunSessionCleanup(ctx context.Context) {
	clean := func() {
		n, err := s.CleanupExpiredSessions(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.logger.Error("清理过期会话失败", "error", err)
		case n > 0:
			s.logger.Info("已清理过期会话", "count", n)
		}
	}
	clean()
	t := time.NewTicker(sessionCleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			clean()
		}
	}
}

// ---- 内部 ----

// newSession 签发 32B crypto/rand 令牌并落库其 SHA-256 摘要。
func (s *Service) newSession(ctx context.Context, remoteIP string) (string, *store.Session, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("生成会话令牌: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sess, err := s.store.CreateSession(ctx, digestToken(token), s.now().Add(SessionTTL), remoteIP)
	if err != nil {
		return "", nil, err
	}
	return token, sess, nil
}

// digestToken 返回令牌明文的 SHA-256 十六进制摘要（库中唯一形态）。
func digestToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// audit 追加审计，失败不阻断动作本身但必须可见（本地 SQLite 写失败时
// 认证链路多半也已不可用；错误日志不含任何凭证值）。
func (s *Service) audit(ctx context.Context, ev store.AuditEvent) {
	if err := s.store.AppendAudit(ctx, ev); err != nil {
		s.logger.Error("审计写入失败", "event", ev.Event, "error", err)
	}
}

// EntityDevicePassword 是口令相关审计行的 entity（管理端点记改密审计时共用
// 同一个值，两边的行才对得上）。
func EntityDevicePassword() string { return entityDevicePassword }

func lockoutDetail() string {
	return fmt.Sprintf("连续失败达 %d 次，锁定 %.0f 秒", lockThreshold, lockDuration.Seconds())
}
