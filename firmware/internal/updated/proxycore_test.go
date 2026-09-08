package updated

// 板上代理内核 unit 控制的可执行验收：运行期配置写入/权限/幂等、内核自检门、
// 就绪窗口与失败切回、停止清理、UDS 往返。systemctl 与探针全部假实现，离线。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyStartStop(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	if _, err := env.e.ProxyStart(ctx, "mixed-port: 1"); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("组件未装应拒: %v", err)
	}
	if _, err := env.e.ComponentInstall(ctx, ComponentMihomo, env.artifact("1.19.30")); err != nil {
		t.Fatalf("安装 mihomo: %v", err)
	}
	if _, err := env.e.ProxyStart(ctx, ""); err == nil {
		t.Fatal("空配置应拒")
	}
	cfg := "socks-port: 7891\nproxies: [{name: n1, type: ss, server: 10.0.0.1, port: 443, cipher: aes-128-gcm, password: fake-secret-pw}]\n"
	st, err := env.e.ProxyStart(ctx, cfg)
	if err != nil {
		t.Fatalf("ProxyStart: %v", err)
	}
	if st.UnitState != "active" || !st.Ready || !st.ConfigPresent || st.Component == nil || !st.Component.Installed {
		t.Fatalf("启动后读数不对: %+v", st)
	}
	raw, err := os.ReadFile(filepath.Join(env.proxyRuntime, ProxyConfigFileName))
	if err != nil || string(raw) != cfg {
		t.Fatalf("运行期配置未写入: %v %q", err, raw)
	}
	if info, _ := os.Stat(filepath.Join(env.proxyRuntime, ProxyConfigFileName)); info.Mode().Perm() != 0o600 {
		t.Fatalf("配置权限 = %o，期望 0600", info.Mode().Perm())
	}
	if len(env.checkedCfgs) != 1 || env.checkedCfgs[0] != cfg {
		t.Fatalf("应先自检一次配置: %d", len(env.checkedCfgs))
	}
	if env.unit.saw("start") != 0 || env.unit.saw("restart") != 1 {
		t.Fatalf("首次启动应经 restart 一次: %v", env.unit.calls)
	}
	// 同配置再启动：不重启、不再自检。
	if _, err := env.e.ProxyStart(ctx, cfg); err != nil {
		t.Fatalf("幂等 ProxyStart: %v", err)
	}
	if env.unit.saw("restart") != 1 || len(env.checkedCfgs) != 1 {
		t.Fatalf("同配置不应重启或再自检: restarts=%d checks=%d", env.unit.saw("restart"), len(env.checkedCfgs))
	}
	// 配置自检失败：旧配置原样、不重启。
	env.checkErr = errors.New("bad")
	if _, err := env.e.ProxyStart(ctx, cfg+"# v2\n"); err == nil || !strings.Contains(err.Error(), "自检未通过") {
		t.Fatalf("自检失败应拒: %v", err)
	}
	if raw, _ := os.ReadFile(filepath.Join(env.proxyRuntime, ProxyConfigFileName)); string(raw) != cfg {
		t.Fatal("自检失败不应替换配置")
	}
	if _, err := os.Stat(filepath.Join(env.proxyRuntime, ProxyConfigFileName+".next")); err == nil {
		t.Fatal("临时文件应被撤掉")
	}
	env.checkErr = nil
	// 配置变更：替换并重启。
	if _, err := env.e.ProxyStart(ctx, cfg+"# v2\n"); err != nil {
		t.Fatalf("变更配置: %v", err)
	}
	if env.unit.saw("restart") != 2 {
		t.Fatalf("配置变更应重启: %v", env.unit.calls)
	}
	// 就绪失败：报错但状态带回。
	env.proxyReady = func(context.Context) error { return errors.New("refused") }
	st, err = env.e.ProxyStart(ctx, cfg+"# v3\n")
	if err == nil || st == nil || !strings.Contains(err.Error(), "未在时限内就绪") {
		t.Fatalf("不就绪应报错并带状态: %v %+v", err, st)
	}
	env.proxyReady = func(context.Context) error { return nil }
	// 停止：unit stop、配置删除。
	st, err = env.e.ProxyStop(ctx)
	if err != nil {
		t.Fatalf("ProxyStop: %v", err)
	}
	if st.UnitState != "inactive" || st.ConfigPresent {
		t.Fatalf("停止后读数不对: %+v", st)
	}
	if env.unit.saw("stop") != 1 {
		t.Fatalf("应 stop 一次: %v", env.unit.calls)
	}
}

func TestProxyInstallRestartsRunningCore(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	if _, err := env.e.ComponentInstall(ctx, ComponentMihomo, env.artifact("1.19.30")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.ProxyStart(ctx, "socks-port: 7891\n"); err != nil {
		t.Fatal(err)
	}
	before := env.unit.saw("restart")
	// 内核在跑时装新版本：重启并等就绪；就绪失败切回旧 slot。
	env.proxyReady = func(context.Context) error { return errors.New("not yet") }
	st, err := env.e.ComponentInstall(ctx, ComponentMihomo, env.artifact("1.19.31"))
	if err == nil || st == nil || st.Current == nil || st.Current.Version != "1.19.30" {
		t.Fatalf("未就绪应切回旧版本: %v %+v", err, st)
	}
	env.proxyReady = func(context.Context) error { return nil }
	st, err = env.e.ComponentInstall(ctx, ComponentMihomo, env.artifact("1.19.31"))
	if err != nil || st.Current.Version != "1.19.31" {
		t.Fatalf("就绪后应换成新版本: %v %+v", err, st)
	}
	if env.unit.saw("restart") <= before {
		t.Fatal("装新版本应重启内核")
	}
	// 卸载：停内核并清运行期配置。
	if _, err := env.e.ComponentRemove(ctx, ComponentMihomo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.proxyRuntime, ProxyConfigFileName)); err == nil {
		t.Fatal("卸载应清除运行期配置")
	}
	if env.unit.saw("stop") != 1 {
		t.Fatalf("卸载应停内核: %v", env.unit.calls)
	}
}

func TestProxyUDSRoundTrip(t *testing.T) {
	env := newCompEnv(t)
	sock := filepath.Join(t.TempDir(), "u.sock")
	srv := NewServer(env.e, nil)
	ln, err := srv.Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	c := NewClient(sock)

	if _, err := c.ProxyStart(ctx, "socks-port: 7891\n"); err == nil {
		t.Fatal("组件未装应报错")
	}
	if _, err := env.e.ComponentInstall(ctx, ComponentMihomo, env.artifact("1.19.30")); err != nil {
		t.Fatal(err)
	}
	st, err := c.ProxyStart(ctx, "socks-port: 7891\n")
	if err != nil || st.UnitState != "active" || !st.Ready {
		t.Fatalf("经 UDS 启动: %v %+v", err, st)
	}
	st, err = c.ProxyStatus(ctx)
	if err != nil || !st.ConfigPresent {
		t.Fatalf("经 UDS 读数: %v %+v", err, st)
	}
	st, err = c.ProxyStop(ctx)
	if err != nil || st.ConfigPresent {
		t.Fatalf("经 UDS 停止: %v %+v", err, st)
	}
}
