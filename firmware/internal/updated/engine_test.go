package updated

// 引擎状态机的可执行验收（docs/firmware-update.md「安装序列」）：成功安装、
// 健康窗口失败自动回退（含 DB 还原）、手动回退（不还原 DB）、单飞互斥与
// 崩溃恢复。systemctl 与健康探针全部假实现，全程离线、不碰真 systemd。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSys 记录 systemctl 调用并按脚本回答 is-active。
type fakeSys struct {
	mu     sync.Mutex
	calls  [][]string
	active string // is-active 的答案
}

func (f *fakeSys) Run(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	return nil
}

func (f *fakeSys) IsActive(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == "" {
		return "active", nil
	}
	return f.active, nil
}

func (f *fakeSys) Show(context.Context, string, ...string) (map[string]string, error) {
	return map[string]string{"SubState": "running", "NRestarts": "0"}, nil
}

func (f *fakeSys) sawStop(t *testing.T) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "stop" {
			return true
		}
	}
	return false
}

// testEnv 铺好一套假板子：install 路径、数据目录、两份内容不同但都能过
// fwimage 校验的「固件」（测试二进制加不同后缀字节）。
type testEnv struct {
	t          *testing.T
	stateDir   string
	dataDir    string
	installDir string
	install    string
	oldImage   []byte
	newImage   []byte
	newPath    string
	newSHA     string
	sys        *fakeSys
	healthy    atomic.Bool
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	base, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("读取测试二进制: %v", err)
	}
	env := &testEnv{
		t:          t,
		stateDir:   t.TempDir(),
		dataDir:    t.TempDir(),
		installDir: t.TempDir(),
		sys:        &fakeSys{},
	}
	// ELF 尾部追加字节不破坏头与 buildinfo，但让两份内容、摘要都不同。
	env.oldImage = append(append([]byte(nil), base...), []byte("\nOLD")...)
	env.newImage = append(append([]byte(nil), base...), []byte("\nNEW")...)
	env.install = filepath.Join(env.installDir, "llmgate")
	if err := os.WriteFile(env.install, env.oldImage, 0o755); err != nil {
		t.Fatal(err)
	}
	env.newPath = filepath.Join(t.TempDir(), "staged.bin")
	if err := os.WriteFile(env.newPath, env.newImage, 0o644); err != nil {
		t.Fatal(err)
	}
	sha, err := fileSHA256(env.newPath)
	if err != nil {
		t.Fatal(err)
	}
	env.newSHA = sha
	if err := os.WriteFile(filepath.Join(env.dataDir, "llmgate.db"), []byte("db-before-install"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.healthy.Store(true)
	return env
}

func (env *testEnv) engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Options{
		StateDir:     env.stateDir,
		InstallPath:  env.install,
		GatewaydUnit: "gw.service",
		DataDir:      env.dataDir,
		Sys:          env.sys,
		Health: func(context.Context) error {
			if env.healthy.Load() {
				return nil
			}
			return errors.New("unhealthy")
		},
		WatchWindow: 500 * time.Millisecond,
		OKStreak:    2,
		PollEvery:   10 * time.Millisecond,
		StartDelay:  time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// waitIdle 轮询到引擎归位并返回结局。
func waitIdle(t *testing.T, e *Engine) *Result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s := e.Snapshot()
		if !s.Busy && s.Phase == PhaseIdle && s.LastResult != nil {
			return s.LastResult
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("引擎未在期限内归位")
	return nil
}

func TestInstallSuccess(t *testing.T) {
	env := newTestEnv(t)
	e := env.engine(t)
	if err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: env.newSHA}); err != nil {
		t.Fatal(err)
	}
	res := waitIdle(t, e)
	if res.Outcome != OutcomeSuccess || res.Kind != KindInstall || res.ToVersion != "v9.9.9" {
		t.Fatalf("结局不对: %+v", res)
	}
	got, _ := os.ReadFile(env.install)
	if string(got) != string(env.newImage) {
		t.Fatal("安装路径不是新固件")
	}
	prev, _ := os.ReadFile(filepath.Join(env.stateDir, "prev", "llmgate"))
	if string(prev) != string(env.oldImage) {
		t.Fatal("prev 槽不是旧固件")
	}
	if !env.sys.sawStop(t) {
		t.Fatal("没有停过网关")
	}
	// 备份应当在场（成功后保留，供灾难恢复）。
	if _, err := os.Stat(filepath.Join(env.stateDir, "backup", "llmgate.db")); err != nil {
		t.Fatal("成功安装后应保留 DB 备份")
	}
}

func TestInstallHealthFailureAutoRollsBack(t *testing.T) {
	env := newTestEnv(t)
	env.healthy.Store(false)
	e := env.engine(t)
	if err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: env.newSHA}); err != nil {
		t.Fatal(err)
	}
	// 模拟坏版本起来后污染了数据库。
	time.Sleep(50 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(env.dataDir, "llmgate.db"), []byte("db-corrupted-by-new"), 0o600)

	res := waitIdle(t, e)
	if res.Outcome != OutcomeRolledBack {
		t.Fatalf("应自动回退，得到 %+v", res)
	}
	got, _ := os.ReadFile(env.install)
	if string(got) != string(env.oldImage) {
		t.Fatal("自动回退后安装路径应是旧固件")
	}
	db, _ := os.ReadFile(filepath.Join(env.dataDir, "llmgate.db"))
	if string(db) != "db-before-install" {
		t.Fatalf("自动回退应还原 DB 备份，得到 %q", db)
	}
}

func TestManualRollbackSwapsWithoutDBRestore(t *testing.T) {
	env := newTestEnv(t)
	e := env.engine(t)
	if err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: env.newSHA}); err != nil {
		t.Fatal(err)
	}
	if res := waitIdle(t, e); res.Outcome != OutcomeSuccess {
		t.Fatalf("前置安装失败: %+v", res)
	}
	// 升级完成后数据继续前进。
	_ = os.WriteFile(filepath.Join(env.dataDir, "llmgate.db"), []byte("db-moved-on"), 0o600)

	if err := e.Rollback(); err != nil {
		t.Fatal(err)
	}
	res := waitIdle(t, e)
	if res.Outcome != OutcomeSuccess || res.Kind != KindRollback {
		t.Fatalf("回退结局不对: %+v", res)
	}
	got, _ := os.ReadFile(env.install)
	if string(got) != string(env.oldImage) {
		t.Fatal("回退后安装路径应是旧固件")
	}
	// 槽位互换：prev 现在存着被换下的新固件，可再「回退」升回去。
	prev, _ := os.ReadFile(filepath.Join(env.stateDir, "prev", "llmgate"))
	if string(prev) != string(env.newImage) {
		t.Fatal("回退后 prev 槽应是刚被换下的新固件")
	}
	// 手动回退不动数据。
	db, _ := os.ReadFile(filepath.Join(env.dataDir, "llmgate.db"))
	if string(db) != "db-moved-on" {
		t.Fatalf("手动回退不应还原 DB，得到 %q", db)
	}
}

func TestInstallRejectsDigestMismatch(t *testing.T) {
	env := newTestEnv(t)
	e := env.engine(t)
	bad := "0000000000000000000000000000000000000000000000000000000000000000"
	if err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: bad}); err != nil {
		t.Fatal(err) // 受理成功，失败落在结局里
	}
	res := waitIdle(t, e)
	if res.Outcome != OutcomeFailed {
		t.Fatalf("摘要不符应失败: %+v", res)
	}
	if env.sys.sawStop(t) {
		t.Fatal("摘要校验失败不应动到 systemd")
	}
	got, _ := os.ReadFile(env.install)
	if string(got) != string(env.oldImage) {
		t.Fatal("安装路径不应被改动")
	}
}

func TestSingleFlight(t *testing.T) {
	env := newTestEnv(t)
	env.healthy.Store(false) // 让第一单在健康窗口里多待一会
	e := env.engine(t)
	if err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: env.newSHA}); err != nil {
		t.Fatal(err)
	}
	err := e.Install(InstallRequest{Path: env.newPath, Version: "v9.9.9", SHA256: env.newSHA})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("在飞期间应拒绝第二单，得到 %v", err)
	}
	waitIdle(t, e)
}

func TestResumeExpiredWatchJudgesByHealth(t *testing.T) {
	env := newTestEnv(t)
	e := env.engine(t)
	// 伪造一份「看护中、窗口已过」的残留状态：装的是新固件、健康。
	if err := os.WriteFile(env.install, env.newImage, 0o755); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.st = state{Phase: PhaseWatching, Job: &Job{
		Kind: KindInstall, FromVersion: "", ToVersion: "v9.9.9", SHA256: env.newSHA,
		SourcePath: env.newPath, StartedAt: time.Now().Add(-time.Hour),
		Deadline: time.Now().Add(-time.Hour),
	}}
	e.persistLocked()
	e.mu.Unlock()

	e2 := env.engine(t) // 重新加载持久化状态，模拟进程重启
	e2.ResumeIfNeeded()
	res := waitIdle(t, e2)
	if res.Outcome != OutcomeSuccess {
		t.Fatalf("健康的过期看护应裁决为成功: %+v", res)
	}
}
