package updated

// 组件槽位与 connector 控制的可执行验收（docs-dev/firmware-cloudflare-tunnel.md
// §7.2、§7.3）：A/B 安装、自述版本与摘要复核、就绪失败切回旧 slot、回退、卸载，
// 以及 token 文件的写入/权限/幂等与清除。systemctl 与探针全部假实现，离线。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUnit 模拟 connector unit：start/restart 置 active，stop 置 inactive，记录调用。
type fakeUnit struct {
	mu       sync.Mutex
	state    string
	calls    []string
	restarts int
}

func (f *fakeUnit) Run(_ context.Context, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "start":
		f.state = "active"
	case "restart":
		f.state = "active"
		f.restarts++
	case "stop":
		f.state = "inactive"
	}
	return nil
}

func (f *fakeUnit) IsActive(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return "inactive", nil
	}
	return f.state, nil
}

func (f *fakeUnit) Show(context.Context, string, ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]string{"SubState": "running", "NRestarts": "2"}, nil
}

func (f *fakeUnit) saw(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type compEnv struct {
	t      *testing.T
	e      *Engine
	unit   *fakeUnit
	ready  func(context.Context) error
	stage  string
	verOut string // ComponentVersion 的答案；空 = "<组件> version <声明版本>"
	// 代理内核侧：就绪探针答案、配置自检记录（收到的配置正文）与自检答案。
	proxyReady   func(context.Context) error
	checkedCfgs  []string
	checkErr     error
	proxyRuntime string
}

func newCompEnv(t *testing.T) *compEnv {
	t.Helper()
	root := t.TempDir()
	env := &compEnv{t: t, unit: &fakeUnit{}, stage: filepath.Join(root, "staging")}
	if err := os.MkdirAll(env.stage, 0o700); err != nil {
		t.Fatal(err)
	}
	env.ready = func(context.Context) error { return nil }
	env.proxyReady = func(context.Context) error { return nil }
	env.proxyRuntime = filepath.Join(root, "run-proxy")
	e, err := New(Options{
		StateDir:         filepath.Join(root, "state"),
		InstallPath:      filepath.Join(root, "llmgate"),
		ComponentsDir:    filepath.Join(root, "components"),
		TunnelRuntimeDir: filepath.Join(root, "run"),
		ProxyRuntimeDir:  env.proxyRuntime,
		ProxyReady:       func(ctx context.Context) error { return env.proxyReady(ctx) },
		ProxyReadyWindow: 200 * time.Millisecond,
		ProxyIDs:         func() (int, int, error) { return os.Getuid(), os.Getgid(), nil },
		ProxyConfigCheck: func(_ context.Context, _, _, cfg string) error {
			raw, err := os.ReadFile(cfg)
			if err != nil {
				return err
			}
			env.checkedCfgs = append(env.checkedCfgs, string(raw))
			return env.checkErr
		},
		Sys:    env.unit,
		Health: func(context.Context) error { return nil },
		VerifyComponent: func(path string) error {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(string(raw), "ELF:") {
				return errors.New("形状不对")
			}
			return nil
		},
		ComponentVersion: func(_ context.Context, name, path string) (string, error) {
			if env.verOut != "" {
				return env.verOut, nil
			}
			raw, _ := os.ReadFile(path)
			// 假制品正文形如 "ELF:<版本>"，自述版本照它回。
			return name + " version " + strings.TrimPrefix(string(raw), "ELF:") + " (built test)", nil
		},
		TunnelReady:       func(ctx context.Context) error { return env.ready(ctx) },
		TunnelReadyWindow: 200 * time.Millisecond,
		PollEvery:         20 * time.Millisecond,
		// 测试可能以 root 跑（开发机）：不查系统用户，token 文件归当前身份。
		TunnelIDs: func() (int, int, error) { return os.Getuid(), os.Getgid(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	env.e = e
	return env
}

// artifact 造一个假制品并返回安装请求。
func (env *compEnv) artifact(version string) ComponentInstallRequest {
	env.t.Helper()
	body := []byte("ELF:" + version)
	p := filepath.Join(env.stage, "cf-"+version+".bin")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		env.t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return ComponentInstallRequest{Path: p, Version: version, SHA256: hex.EncodeToString(sum[:])}
}

func TestComponentInstallABAndRollback(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	if st, _ := env.e.ComponentStatus(ComponentCloudflared); st.Installed {
		t.Fatal("空目录不该算已安装")
	}
	st, err := env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.8.2"))
	if err != nil {
		t.Fatalf("首次安装: %v", err)
	}
	if !st.Installed || st.CurrentSlot != "a" || st.Current.Version != "2026.8.2" || st.Previous != nil {
		t.Fatalf("首次安装后状态不对: %+v", st)
	}
	bin := filepath.Join(env.e.opt.ComponentsDir, "cloudflared", "current", "cloudflared")
	if raw, err := os.ReadFile(bin); err != nil || string(raw) != "ELF:2026.8.2" {
		t.Fatalf("current 链接没指到装好的文件: %v %q", err, raw)
	}
	if info, _ := os.Stat(bin); info.Mode().Perm()&0o111 == 0 {
		t.Fatal("装好的文件应可执行")
	}

	st, err = env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.9.1"))
	if err != nil {
		t.Fatalf("第二次安装: %v", err)
	}
	if st.CurrentSlot != "b" || st.Current.Version != "2026.9.1" || st.PreviousSlot != "a" || st.Previous.Version != "2026.8.2" {
		t.Fatalf("A/B 交替不对: %+v", st)
	}
	// connector 没在跑：安装不该碰 unit。
	if env.unit.saw("restart") != 0 || env.unit.saw("start") != 0 {
		t.Fatalf("connector 未运行时不该操作 unit: %v", env.unit.calls)
	}

	st, err = env.e.ComponentRollback(ctx, ComponentCloudflared)
	if err != nil {
		t.Fatalf("回退: %v", err)
	}
	if st.CurrentSlot != "a" || st.Current.Version != "2026.8.2" || st.PreviousSlot != "b" {
		t.Fatalf("回退后状态不对: %+v", st)
	}
	if _, err := env.e.ComponentStatus("bogus"); !errors.Is(err, ErrUnknownComponent) {
		t.Fatalf("未知组件应拒: %v", err)
	}
}

func TestComponentInstallRejectsBadArtifacts(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	req := env.artifact("2026.8.2")
	bad := req
	bad.SHA256 = strings.Repeat("0", 64)
	if _, err := env.e.ComponentInstall(ctx, ComponentCloudflared, bad); err == nil || !strings.Contains(err.Error(), "摘要") {
		t.Fatalf("摘要不符应拒: %v", err)
	}
	// 自述版本不符：文件落进 slot 前就被删掉，current 不建。
	env.verOut = "cloudflared version 1.0.0"
	if _, err := env.e.ComponentInstall(ctx, ComponentCloudflared, req); err == nil || !strings.Contains(err.Error(), "自述版本") {
		t.Fatalf("自述版本不符应拒: %v", err)
	}
	env.verOut = ""
	if st, _ := env.e.ComponentStatus(ComponentCloudflared); st.Installed {
		t.Fatal("被拒的安装不该切 current")
	}
	if entries, _ := os.ReadDir(filepath.Join(env.e.opt.ComponentsDir, "cloudflared", "slots", "a")); len(entries) != 0 {
		t.Fatalf("被拒的安装应清掉临时文件: %v", entries)
	}
	// 形状校验失败。
	p := filepath.Join(env.stage, "script")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o600)
	sum := sha256.Sum256([]byte("#!/bin/sh\n"))
	if _, err := env.e.ComponentInstall(ctx, ComponentCloudflared, ComponentInstallRequest{Path: p, Version: "x", SHA256: hex.EncodeToString(sum[:])}); err == nil {
		t.Fatal("形状不对应拒")
	}
	if _, err := env.e.ComponentRollback(ctx, ComponentCloudflared); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("没装过就回退应报 ErrComponentMissing: %v", err)
	}
}

func TestTunnelStartStopAndTokenFile(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	if _, err := env.e.TunnelStart(ctx, "eyJ.token.1"); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("组件未装时启动应拒: %v", err)
	}
	if _, err := env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.8.2")); err != nil {
		t.Fatal(err)
	}
	st, err := env.e.TunnelStart(ctx, "eyJ.token.1")
	if err != nil {
		t.Fatalf("启动: %v", err)
	}
	if st.UnitState != "active" || !st.TokenPresent || st.Restarts != 2 || st.SubState != "running" {
		t.Fatalf("启动后读数不对: %+v", st)
	}
	tokenPath := filepath.Join(env.e.opt.TunnelRuntimeDir, TunnelTokenFileName)
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token 文件权限 = %o，期望 0600", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(tokenPath); string(raw) != "eyJ.token.1" {
		t.Fatal("token 文件内容不对")
	}
	if dir, _ := os.Stat(env.e.opt.TunnelRuntimeDir); dir.Mode().Perm() != 0o700 {
		t.Fatalf("运行期目录权限 = %o，期望 0700", dir.Mode().Perm())
	}
	// 同 token 再启动：不重启（开机恢复路径）。
	if _, err := env.e.TunnelStart(ctx, "eyJ.token.1"); err != nil {
		t.Fatal(err)
	}
	if env.unit.saw("restart") != 0 || env.unit.saw("start") != 1 {
		t.Fatalf("同 token 不该重启: %v", env.unit.calls)
	}
	// 换 token：重启。
	if _, err := env.e.TunnelStart(ctx, "eyJ.token.2"); err != nil {
		t.Fatal(err)
	}
	if env.unit.saw("restart") != 1 {
		t.Fatalf("换 token 应重启一次: %v", env.unit.calls)
	}
	if raw, _ := os.ReadFile(tokenPath); string(raw) != "eyJ.token.2" {
		t.Fatal("新 token 没写进去")
	}
	st, err = env.e.TunnelStop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.UnitState != "inactive" || st.TokenPresent {
		t.Fatalf("停止后读数不对: %+v", st)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("停止后 token 文件应被清除")
	}
	if _, err := env.e.TunnelStart(ctx, "   "); err == nil {
		t.Fatal("空 token 应拒")
	}
}

func TestComponentInstallWhileRunningRevertsWhenNotReady(t *testing.T) {
	env := newCompEnv(t)
	ctx := context.Background()
	if _, err := env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.8.2")); err != nil {
		t.Fatal(err)
	}
	if _, err := env.e.TunnelStart(ctx, "eyJ.token"); err != nil {
		t.Fatal(err)
	}
	// 新版本起不来：切回旧 slot 并重启，返回错误但状态仍是旧版本。
	env.ready = func(context.Context) error { return errors.New("not ready") }
	st, err := env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.9.1"))
	if err == nil || !strings.Contains(err.Error(), "已回到旧版本") {
		t.Fatalf("未就绪应报错: %v", err)
	}
	if st == nil || st.CurrentSlot != "a" || st.Current.Version != "2026.8.2" || st.Previous == nil || st.Previous.Version != "2026.9.1" {
		t.Fatalf("切回后状态不对: %+v", st)
	}
	if env.unit.saw("restart") != 2 {
		t.Fatalf("应重启两次（换新一次、切回一次）: %v", env.unit.calls)
	}
	// 就绪：换新成功。
	env.ready = func(context.Context) error { return nil }
	st, err = env.e.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.9.2"))
	if err != nil {
		t.Fatalf("就绪时安装应成功: %v", err)
	}
	if st.CurrentSlot != "b" || st.Current.Version != "2026.9.2" {
		t.Fatalf("换新后状态不对: %+v", st)
	}
	// 卸载：先停 unit、清 token、删目录。
	st, err = env.e.ComponentRemove(ctx, ComponentCloudflared)
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed || env.unit.saw("stop") != 1 {
		t.Fatalf("卸载后状态不对: %+v calls=%v", st, env.unit.calls)
	}
	if _, err := os.Stat(filepath.Join(env.e.opt.ComponentsDir, "cloudflared")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("组件目录应被删除")
	}
	if _, err := os.Stat(filepath.Join(env.e.opt.TunnelRuntimeDir, TunnelTokenFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("卸载应清除 token")
	}
}

// TestComponentUDSRoundTrip 走真实 UDS：客户端方法与服务端路由的形状契约。
func TestComponentUDSRoundTrip(t *testing.T) {
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
	st, err := c.ComponentStatus(ctx, ComponentCloudflared)
	if err != nil || st.Installed {
		t.Fatalf("初始读数: %+v %v", st, err)
	}
	if _, err := c.ComponentStatus(ctx, "bogus"); err == nil {
		t.Fatal("未知组件应报错")
	} else {
		var ee *EngineError
		if !errors.As(err, &ee) || ee.Status != 404 {
			t.Fatalf("未知组件应是 404 EngineError: %v", err)
		}
	}
	if _, err := c.TunnelStart(ctx, "eyJ.x"); err == nil {
		t.Fatal("组件未装应拒启动")
	} else {
		var ee *EngineError
		if !errors.As(err, &ee) || ee.Status != 409 {
			t.Fatalf("组件未装应是 409: %v", err)
		}
	}
	st, err = c.ComponentInstall(ctx, ComponentCloudflared, env.artifact("2026.8.2"))
	if err != nil || st.CurrentSlot != "a" {
		t.Fatalf("经 UDS 安装: %+v %v", st, err)
	}
	ts, err := c.TunnelStart(ctx, "eyJ.x")
	if err != nil || ts.UnitState != "active" || !ts.TokenPresent || ts.Component == nil || !ts.Component.Installed {
		t.Fatalf("经 UDS 启动: %+v %v", ts, err)
	}
	ts, err = c.TunnelStatus(ctx)
	if err != nil || ts.UnitState != "active" {
		t.Fatalf("经 UDS 读状态: %+v %v", ts, err)
	}
	ts, err = c.TunnelStop(ctx)
	if err != nil || ts.UnitState != "inactive" || ts.TokenPresent {
		t.Fatalf("经 UDS 停止: %+v %v", ts, err)
	}
	if _, err := c.ComponentRollback(ctx, ComponentCloudflared); err == nil {
		t.Fatal("只有一个 slot 时回退应拒")
	}
	st, err = c.ComponentRemove(ctx, ComponentCloudflared)
	if err != nil || st.Installed {
		t.Fatalf("经 UDS 卸载: %+v %v", st, err)
	}
}
