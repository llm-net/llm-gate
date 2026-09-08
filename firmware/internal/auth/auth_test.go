// internal/auth 的可执行验收：
//
//   - Argon2id 往返与错密拒绝、PHC 串参数可解析、verify 按串内参数验证（未来调参兼容）；
//   - 密码策略；
//   - 出厂默认口令：空库播一次、已有口令就一个字节都不动、口令不进日志与审计、
//     播完能直接登进来；
//   - 固定退避：连续 5 败锁 60 秒（时钟注入）、锁定期内正确口令也拒绝；
//   - 会话：TTL 过期判定、改密清空全部会话、登出幂等、过期行清理；
//   - 审计接线与 §15.1 纪律延伸：审计任何字段不含口令/会话令牌。
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"

	_ "modernc.org/sqlite" // 测试直连库文件核对审计行（store 之外的只读通道）
)

// ---- 测试基建 ----

// fakeClock 是可注入 Service 的假时钟。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testEnv 打包一套「临时目录 + store + 假时钟 + 捕获日志的 Service」。
type testEnv struct {
	svc    *Service
	store  *store.Store
	dir    string
	clock  *fakeClock
	logBuf *strings.Builder
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { st.Close() })
	e := &testEnv{store: st, dir: dir, clock: newFakeClock(), logBuf: &strings.Builder{}}
	e.newService(t)
	return e
}

// newService 在同一数据目录上重建 Service（模拟进程重启，store 连接复用）。
func (e *testEnv) newService(t *testing.T) {
	t.Helper()
	logger := logging.New(e.logBuf, slog.LevelDebug)
	svc, err := New(context.Background(), e.store, e.dir, logger, WithClock(e.clock.Now))
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e.svc = svc
}

// mustPassword 直接把设备口令设成 password 并开一条会话。「库里先有个能登进
// 来的口令」是绝大多数用例的起点——生产里这一步由
// [Service.EnsureDefaultPassword] 在启动期做，用例要的却是自选的口令。
func (e *testEnv) mustPassword(t *testing.T, password string) *LoginResult {
	t.Helper()
	ctx := context.Background()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := e.store.SetSetting(ctx, store.SettingAdminPasswordHash, hash); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	token, sess, err := e.svc.newSession(ctx, "192.168.50.9")
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	return &LoginResult{Token: token, Session: sess}
}

// auditRow 是审计表一行的测试视图。
type auditRow struct {
	event    string
	entity   string
	detail   string
	remoteIP string
}

// auditRows 直连库文件读全部审计行（WAL 下第二连接可见已提交数据）。
func auditRows(t *testing.T, dir string) []auditRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, store.DBFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开审计只读连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail, remote_ip FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询 audit_events: %v", err)
	}
	defer rows.Close()
	var got []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.event, &r.entity, &r.detail, &r.remoteIP); err != nil {
			t.Fatalf("scan 审计行: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func countAudit(rows []auditRow, event string) int {
	n := 0
	for _, r := range rows {
		if r.event == event {
			n++
		}
	}
	return n
}

// assertAuditFreeOfSecrets 断言审计任何字段都不含给定敏感物料（§15.1 纪律延伸到审计）。
// 失败信息只报长度，不回显物料本身。
func assertAuditFreeOfSecrets(t *testing.T, dir string, secrets ...string) {
	t.Helper()
	for _, r := range auditRows(t, dir) {
		for _, sec := range secrets {
			if sec == "" {
				continue
			}
			for _, field := range []string{r.event, r.entity, r.detail, r.remoteIP} {
				if strings.Contains(field, sec) {
					t.Errorf("审计字段含敏感物料（event=%s，机密长度 %d）", r.event, len(sec))
				}
			}
		}
	}
}

// ---- Argon2id 与策略 ----

// 往返：正确密码通过、错误密码拒绝、随机盐使两次哈希不同。
func TestPasswordHashRoundTrip(t *testing.T) {
	const pw = "correct-horse-battery"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if ok, err := VerifyPassword(h, pw); err != nil || !ok {
		t.Fatalf("正确密码应通过 (ok=%v err=%v)", ok, err)
	}
	if ok, err := VerifyPassword(h, pw+"x"); err != nil || ok {
		t.Fatalf("错误密码应被拒 (ok=%v err=%v)", ok, err)
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Error("随机盐应使同一密码两次哈希不同")
	}
}

// PHC 形状与参数：OWASP 参数 m=19456/t=2/p=1 落串可解析；
// verify 按串内参数验证——用非缺省参数手工生成的串也能校验（未来调参兼容）。
func TestPasswordPHCFormatAndParams(t *testing.T) {
	h, err := HashPassword("0123456789")
	if err != nil {
		t.Fatal(err)
	}
	const wantPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(h, wantPrefix) {
		t.Errorf("PHC 前缀 = %q, 期望以 %q 开头", h, wantPrefix)
	}
	p, err := parsePHC(h)
	if err != nil {
		t.Fatalf("parsePHC: %v", err)
	}
	if p.memoryKiB != 19456 || p.time != 2 || p.threads != 1 || len(p.salt) != 16 || len(p.key) != 32 {
		t.Errorf("PHC 参数不符: m=%d t=%d p=%d saltLen=%d keyLen=%d",
			p.memoryKiB, p.time, p.threads, len(p.salt), len(p.key))
	}

	const pw = "another-password!"
	h2, err := hashWithParams(pw, 8192, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(h2, pw); err != nil || !ok {
		t.Errorf("非缺省参数 PHC 应按串内参数验证通过 (ok=%v err=%v)", ok, err)
	}
	if ok, _ := VerifyPassword(h2, "wrong-password!!"); ok {
		t.Error("非缺省参数 PHC 错密仍应被拒")
	}
}

// 畸形 PHC 串一律解析失败：算法/版本/参数越界/坏 base64/字段缺失。
func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	salt22 := strings.Repeat("A", 22) // 16 字节盐的 raw base64 长度
	key43 := strings.Repeat("A", 43)  // 32 字节 key 的 raw base64 长度
	cases := []string{
		"",
		"plaintext",
		fmt.Sprintf("$argon2i$v=19$m=19456,t=2,p=1$%s$%s", salt22, key43),  // 变体不符
		fmt.Sprintf("$argon2id$v=18$m=19456,t=2,p=1$%s$%s", salt22, key43), // 版本不符
		fmt.Sprintf("$argon2id$v=19$m=0,t=2,p=1$%s$%s", salt22, key43),     // m 越界
		fmt.Sprintf("$argon2id$v=19$m=19456,t=0,p=1$%s$%s", salt22, key43), // t 越界
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$????",                          // 坏 base64
		"$argon2id$v=19$m=19456,t=2,p=1$" + salt22,                         // 字段缺失
	}
	for _, c := range cases {
		if ok, err := VerifyPassword(c, "whatever-pass"); err == nil || ok {
			t.Errorf("畸形串应解析失败: %q (ok=%v err=%v)", c, ok, err)
		}
	}
}

// 密码策略：长度按 Unicode 字符计，≥8 且 ≤128，无复杂度规则。
// 下限恰好放得下出厂默认口令——设备自带的口令按自己的规矩必须合法。
func TestPasswordPolicy(t *testing.T) {
	if err := ValidatePassword(strings.Repeat("a", 7)); !errors.Is(err, ErrPasswordPolicy) {
		t.Errorf("7 字符应拒绝, got %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 8)); err != nil {
		t.Errorf("8 字符应通过, got %v", err)
	}
	if err := ValidatePassword(DefaultPassword); err != nil {
		t.Errorf("出厂默认口令应过策略闸, got %v", err)
	}
	if err := ValidatePassword(strings.Repeat("密", 8)); err != nil {
		t.Errorf("8 个多字节字符应通过（按字符不按字节）, got %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 128)); err != nil {
		t.Errorf("128 字符应通过, got %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 129)); !errors.Is(err, ErrPasswordPolicy) {
		t.Errorf("129 字符应拒绝, got %v", err)
	}
}

// ---- 出厂默认口令 ----

// 空库播一次默认口令：能直接登进来，审计留一条 password.seed，而**口令一个字
// 都不进日志与审计**（§15.1 对编译期缺省也不开例外）。
func TestEnsureDefaultPasswordSeedsAndLogsIn(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	if err := e.svc.EnsureDefaultPassword(ctx); err != nil {
		t.Fatalf("EnsureDefaultPassword: %v", err)
	}
	res, err := e.svc.Login(ctx, DefaultPassword, "192.168.50.9")
	if err != nil {
		t.Fatalf("用默认口令登录: %v", err)
	}
	if res.Token == "" || res.Session == nil {
		t.Errorf("登录结果不完整: %+v", res)
	}

	rows := auditRows(t, e.dir)
	if got := countAudit(rows, EventPasswordSeed); got != 1 {
		t.Errorf("password.seed 审计 = %d 条，期望 1", got)
	}
	assertAuditFreeOfSecrets(t, e.dir, DefaultPassword, res.Token)
	if strings.Contains(e.logBuf.String(), DefaultPassword) {
		t.Error("默认口令进了日志（§15.1）")
	}
	if !strings.Contains(e.logBuf.String(), "请登录后立即修改") {
		t.Error("播种应记一条提醒改密的 Warn")
	}
}

// 幂等：库里已经有口令时一个字节都不动——不重播、不重置、不写第二条审计。
// 重启（重建 Service）后再调一次也一样。
func TestEnsureDefaultPasswordLeavesExistingAlone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.mustPassword(t, "root-password")

	for i := 0; i < 2; i++ {
		if err := e.svc.EnsureDefaultPassword(ctx); err != nil {
			t.Fatalf("第 %d 次 EnsureDefaultPassword: %v", i+1, err)
		}
		e.newService(t)
	}
	if got := countAudit(auditRows(t, e.dir), EventPasswordSeed); got != 0 {
		t.Errorf("password.seed 审计 = %d 条，期望 0", got)
	}
	// 现有口令没被动过，默认口令也没被偷偷种上。
	if _, err := e.svc.Login(ctx, "root-password", ""); err != nil {
		t.Errorf("既有口令被改动了: %v", err)
	}
	if _, err := e.svc.Login(ctx, DefaultPassword, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("默认口令不该被补种上, got %v", err)
	}
}

// 口令行被抹掉之后（重刷库、误删）再启动一次，把默认口令补回来。
func TestEnsureDefaultPasswordReseedsWhenGone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.mustPassword(t, "root-password")
	if err := e.store.SetSetting(ctx, store.SettingAdminPasswordHash, ""); err != nil {
		t.Fatalf("抹掉口令行: %v", err)
	}
	// 口令空缺时任何口令都登不进来（等化计时那条分支）。
	if _, err := e.svc.Login(ctx, "root-password", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("无口令时应一律拒绝, got %v", err)
	}

	if err := e.svc.EnsureDefaultPassword(ctx); err != nil {
		t.Fatalf("EnsureDefaultPassword: %v", err)
	}
	if _, err := e.svc.Login(ctx, DefaultPassword, ""); err != nil {
		t.Fatalf("补播之后应能登进来: %v", err)
	}
}

// ---- 登录与退避 ----

// 连续 5 败锁 60 秒；锁定期内正确口令也拒绝；期满解锁且成功清零计数；
// 审计事件计数与 §15.1 纪律。
//
// 锁定是**设备级**的：只有一个口令，也就只有一把锁——没有「换个账号继续试」
// 这条路，这正是单口令模型下退避器要挡住的那件事。
func TestLoginLockoutAndRecovery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.mustPassword(t, "root-password")

	for i := 0; i < 5; i++ {
		if _, err := e.svc.Login(ctx, "bad-password-xx", "192.168.50.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次错密应返回 ErrInvalidCredentials, got %v", i+1, err)
		}
	}
	if _, err := e.svc.Login(ctx, "root-password", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("锁定期内正确口令也应被拒, got %v", err)
	}

	e.clock.Advance(61 * time.Second)
	res, err := e.svc.Login(ctx, "root-password", "192.168.50.9")
	if err != nil {
		t.Fatalf("锁定期满应可登录: %v", err)
	}
	// 成功清零：再错 4 次不锁，正确口令仍放行。
	for i := 0; i < 4; i++ {
		if _, err := e.svc.Login(ctx, "bad-password-xx", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("成功后计数应清零（第 %d 次错密）, got %v", i+1, err)
		}
	}
	if _, err := e.svc.Login(ctx, "root-password", ""); err != nil {
		t.Errorf("4 次失败未达阈值，正确口令应放行: %v", err)
	}

	rows := auditRows(t, e.dir)
	if got := countAudit(rows, EventLoginFailure); got != 9 {
		t.Errorf("login.failure 审计 = %d 条, 期望 9", got)
	}
	// 第 5 次失败触发锁定 1 条 + 锁定期内被拒 1 条。
	if got := countAudit(rows, EventLoginLockout); got != 2 {
		t.Errorf("login.lockout 审计 = %d 条, 期望 2", got)
	}
	// 解锁后 2 次成功。
	if got := countAudit(rows, EventLoginSuccess); got != 2 {
		t.Errorf("login.success 审计 = %d 条, 期望 2", got)
	}
	assertAuditFreeOfSecrets(t, e.dir, "root-password", "bad-password-xx", res.Token)
}

// VerifyCurrent 是改密端点验旧口令的那一步：只认当前口令，且**不过退避器**
// （调用方已持有效会话，这不是一条未认证的爆破面）。
func TestVerifyCurrent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.mustPassword(t, "root-password")

	if ok, err := e.svc.VerifyCurrent(ctx, "root-password"); err != nil || !ok {
		t.Errorf("当前口令应通过 = %v (err=%v)", ok, err)
	}
	// 连错十次也不该把登录锁上——它走的不是退避器那条路。
	for i := 0; i < 10; i++ {
		if ok, err := e.svc.VerifyCurrent(ctx, "wrong-password"); err != nil || ok {
			t.Fatalf("错口令应不通过 = %v (err=%v)", ok, err)
		}
	}
	if _, err := e.svc.Login(ctx, "root-password", ""); err != nil {
		t.Errorf("VerifyCurrent 不该影响登录退避: %v", err)
	}

	// 口令空缺：恒 false，不报错（等化计时那条分支）。
	if err := e.store.SetSetting(ctx, store.SettingAdminPasswordHash, ""); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.VerifyCurrent(ctx, "root-password"); err != nil || ok {
		t.Errorf("无口令时应恒 false = %v (err=%v)", ok, err)
	}
}

// ---- 会话 ----

// 过期判定：TTL 内有效、过点失效；伪造/空令牌一律无效。
func TestSessionExpiry(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res := e.mustPassword(t, "root-password")

	if _, err := e.svc.ValidateSession(ctx, res.Token); err != nil {
		t.Fatalf("新会话应有效: %v", err)
	}
	if _, err := e.svc.ValidateSession(ctx, "forged-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("伪造令牌应无效, got %v", err)
	}
	if _, err := e.svc.ValidateSession(ctx, ""); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("空令牌应无效, got %v", err)
	}

	e.clock.Advance(SessionTTL - time.Second)
	if _, err := e.svc.ValidateSession(ctx, res.Token); err != nil {
		t.Errorf("TTL 内应仍有效: %v", err)
	}
	e.clock.Advance(2 * time.Second)
	if _, err := e.svc.ValidateSession(ctx, res.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("过期会话应失效, got %v", err)
	}
}

// 改密清空会话：SetPassword 后全部旧会话失效、旧口令不可登录、新口令可登录；
// 弱新口令被策略拒绝。
func TestPasswordChangeKicksSessions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res := e.mustPassword(t, "root-password")
	res2, err := e.svc.Login(ctx, "root-password", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := e.svc.SetPassword(ctx, "brand-new-pass"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, err := e.svc.ValidateSession(ctx, res.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("改密后会话 1 应失效, got %v", err)
	}
	if _, err := e.svc.ValidateSession(ctx, res2.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("改密后会话 2 应失效, got %v", err)
	}
	if _, err := e.svc.Login(ctx, "root-password", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("旧口令应不可登录, got %v", err)
	}
	if _, err := e.svc.Login(ctx, "brand-new-pass", ""); err != nil {
		t.Errorf("新口令应可登录: %v", err)
	}
	if err := e.svc.SetPassword(ctx, "short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Errorf("弱新口令应被策略拒绝, got %v", err)
	}
}

// 过期行清理：只删已过期的，幂等。
func TestCleanupExpiredSessions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.mustPassword(t, "root-password")
	if _, err := e.svc.Login(ctx, "root-password", ""); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(SessionTTL + time.Hour)
	n, err := e.svc.CleanupExpiredSessions(ctx)
	if err != nil || n != 2 {
		t.Fatalf("清理过期会话 = %d (err=%v), 期望 2", n, err)
	}
	if n, err := e.svc.CleanupExpiredSessions(ctx); err != nil || n != 0 {
		t.Errorf("重复清理应为 0 (n=%d err=%v)", n, err)
	}
}

// 登出：删单会话、幂等、审计不含令牌。
func TestLogout(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res := e.mustPassword(t, "root-password")

	if err := e.svc.Logout(ctx, res.Token, "192.168.50.9"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := e.svc.ValidateSession(ctx, res.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("登出后会话应失效, got %v", err)
	}
	if err := e.svc.Logout(ctx, res.Token, ""); err != nil {
		t.Errorf("重复登出应幂等无错, got %v", err)
	}
	if err := e.svc.Logout(ctx, "unknown-token", ""); err != nil {
		t.Errorf("未知令牌登出应幂等无错, got %v", err)
	}

	rows := auditRows(t, e.dir)
	if got := countAudit(rows, EventLogout); got != 1 {
		t.Fatalf("logout 审计 = %d 条, 期望 1", got)
	}
	for _, r := range rows {
		if r.event == EventLogout && r.entity != EntityDevicePassword() {
			t.Errorf("logout 审计 entity = %q", r.entity)
		}
	}
	assertAuditFreeOfSecrets(t, e.dir, res.Token)
}

// RunSessionCleanup 随 ctx 取消而退出（装配层的停机路径）。
func TestRunSessionCleanupStops(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.svc.RunSessionCleanup(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunSessionCleanup 应随 ctx 取消退出")
	}
}

// ---- 退避器单元语义 ----

// 成功清零；第 5 次失败触发锁定；期满解锁；未清零时再失败立即重新锁定（固定退避）。
func TestLimiterSemantics(t *testing.T) {
	fc := newFakeClock()
	l := newLimiter(fc.Now)
	const key = loginLimiterKey

	for i := 0; i < 4; i++ {
		if l.recordFailure(key) {
			t.Fatalf("第 %d 次失败不应触发锁定", i+1)
		}
	}
	l.recordSuccess(key)
	for i := 0; i < 4; i++ {
		if l.recordFailure(key) {
			t.Fatalf("成功清零后第 %d 次失败不应触发锁定", i+1)
		}
	}
	if !l.recordFailure(key) {
		t.Fatal("第 5 次失败应触发锁定")
	}
	if !l.locked(key) {
		t.Fatal("应处于锁定期")
	}
	fc.Advance(61 * time.Second)
	if l.locked(key) {
		t.Fatal("60 秒后应解锁")
	}
	if !l.recordFailure(key) {
		t.Error("计数未清零时（≥5）再失败应立即重新锁定")
	}
}
