package codexappserver

// 运行期验收：用测试二进制自己充当假 app-server（按 argv[0] 基名分身：codex-app-server /
// bwrap），装进一棵真实布局的槽位树，验证入口解析、布局核对、拉起 / 握手 / 反向请求 / 收尾、
// 运行中拒装拒卸、自检读数，以及残留清理。全部离线，不依赖真 app-server。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/logging"
)

func TestMain(m *testing.M) {
	switch filepath.Base(os.Args[0]) {
	case "codex-app-server":
		os.Exit(fakeAppServer())
	case "bwrap":
		fmt.Println("bubblewrap 0.11.0-fake")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAppServer 模拟 app-server 的 stdio 协议：--version 自述；initialize 应答并推一条
// configWarning；echo/env 回环境与 cwd；auth/present 报 CODEX_HOME/auth.json 是否在；ask 先发
// 反向请求再把客户端的答复带回；hang 不应答；quit 退出。FAKE_STUBBORN=1 时 stdin 关了也不退。
func fakeAppServer() int {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-app-server 0.154.0-fake")
		return 0
	}
	out := bufio.NewWriter(os.Stdout)
	send := func(v any) {
		raw, _ := json.Marshal(v)
		out.Write(raw)
		out.WriteByte('\n')
		out.Flush()
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	pendingAsk := json.RawMessage(nil)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil {
			continue
		}
		if msg.Method == "" && len(msg.ID) > 0 {
			// 客户端对反向请求的答复。
			if pendingAsk != nil {
				send(map[string]any{"id": pendingAsk, "result": map[string]any{"reply": msg.Result}})
				pendingAsk = nil
			}
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{"id": msg.ID, "result": map[string]any{
				"userAgent": "codex-app-server-fake", "codexHome": os.Getenv("CODEX_HOME"), "platformFamily": "unix", "platformOs": "linux"}})
			send(map[string]any{"method": "configWarning", "params": map[string]any{"summary": "fake warning", "details": nil}})
		case "initialized":
		case "echo/env":
			cwd, _ := os.Getwd()
			send(map[string]any{"id": msg.ID, "result": map[string]any{
				"home": os.Getenv("HOME"), "cwd": cwd, "path": os.Getenv("PATH"), "extra": os.Getenv("FAKE_EXTRA")}})
		case "auth/present":
			raw, err := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"))
			send(map[string]any{"id": msg.ID, "result": map[string]any{"present": err == nil, "bytes": len(raw)}})
		case "ask":
			pendingAsk = msg.ID
			send(map[string]any{"id": "srv-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
		case "fail":
			send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32600, "message": "bad request"}})
		case "hang":
		case "quit":
			return 0
		case "fail-exit":
			fmt.Fprintln(os.Stderr, "fake-sensitive-stderr")
			return 17
		default:
			send(map[string]any{"id": msg.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
	if os.Getenv("FAKE_STUBBORN") == "1" {
		time.Sleep(time.Minute)
	}
	return 0
}

// installFakeSlot 在 componentsDir 下铺一棵 slot 树：入口与捆绑 bwrap 都是本测试二进制的副本
// （按名字分身），其余是占位文件；current → slots/<slot>。
func installFakeSlot(t *testing.T, componentsDir, slot string, omit ...string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(componentsDir, ComponentName, "slots", slot)
	skip := map[string]bool{}
	for _, o := range omit {
		skip[o] = true
	}
	files := map[string]string{ // rel → 来源（"self" 复制测试二进制）
		EntrypointPath:                "self",
		"bin/codex-code-mode-host":    "placeholder",
		PackageMetaFile:               `{"layoutVersion":1,"version":"0.154.0","entrypoint":"bin/codex-app-server"}`,
		"codex-path/rg":               "placeholder",
		"codex-resources/bwrap":       "self",
		"codex-resources/zsh/bin/zsh": "placeholder",
	}
	for rel, src := range files {
		if skip[rel] {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if src == "self" {
			raw, err := os.ReadFile(self)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, raw, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(dst, []byte(src), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(componentsDir, ComponentName, "current")
	os.Remove(link)
	if err := os.Symlink(filepath.Join("slots", slot), link); err != nil {
		t.Fatal(err)
	}
}

func newRuntimeManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	comps := filepath.Join(root, "components")
	m := NewManager(Options{DataDir: filepath.Join(root, "data"), Settings: newFakeSettings(), Engine: &fakeEngine{},
		Keys: newSigner(t).keys, ComponentsDir: comps, ClientVersion: "test"})
	return m, comps
}

func TestInstalledLayout(t *testing.T) {
	m, comps := newRuntimeManager(t)
	if _, err := m.Installed(); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("没有 current 应是 ErrComponentMissing: %v", err)
	}
	if info := m.RuntimeInfo(); info.LayoutOK || info.Running != 0 {
		t.Fatalf("未装读数: %+v", info)
	}
	// 缺 code-mode host：不算已装，错误点名缺的文件。
	installFakeSlot(t, comps, "a", "bin/codex-code-mode-host")
	inst, err := m.Installed()
	if !errors.Is(err, ErrComponentMissing) || inst == nil || len(inst.Missing) != 1 || inst.Missing[0] != "bin/codex-code-mode-host" || !strings.Contains(err.Error(), "codex-code-mode-host") {
		t.Fatalf("缺伴侣应报 ErrComponentMissing 并点名: %v %+v", err, inst)
	}
	if info := m.RuntimeInfo(); info.LayoutOK || info.Slot != "a" {
		t.Fatalf("缺伴侣读数: %+v", info)
	}
	// 只缺捆绑工具：算已装，记进 Missing。
	installFakeSlot(t, comps, "b", "codex-path/rg")
	inst, err = m.Installed()
	if err != nil || inst.Slot != "b" || len(inst.Missing) != 1 || inst.Missing[0] != "codex-path/rg" {
		t.Fatalf("缺捆绑工具: %v %+v", err, inst)
	}
	if !strings.HasSuffix(inst.Entrypoint, filepath.Join("slots", "b", "bin", "codex-app-server")) {
		t.Fatalf("入口应是真实槽位路径: %s", inst.Entrypoint)
	}
	// 齐全。
	installFakeSlot(t, comps, "a")
	if inst, err = m.Installed(); err != nil || inst.Slot != "a" || len(inst.Missing) != 0 {
		t.Fatalf("齐全: %v %+v", err, inst)
	}
	if info := m.RuntimeInfo(); !info.LayoutOK || info.Slot != "a" || info.Entrypoint != inst.Entrypoint {
		t.Fatalf("齐全读数: %+v", info)
	}
	// current 指向不认识的目标。
	link := filepath.Join(comps, ComponentName, "current")
	os.Remove(link)
	if err := os.Symlink("slots/c", link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Installed(); !errors.Is(err, ErrComponentMissing) {
		t.Fatalf("current 指向 slots/c 应算未装: %v", err)
	}
}

func TestLaunchHandshakeAndClose(t *testing.T) {
	m, comps := newRuntimeManager(t)
	installFakeSlot(t, comps, "b")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p, err := m.Launch(LaunchOptions{AuthJSON: []byte(`{"fake":"cred"}`), ConfigTOML: []byte("model = \"x\"\n"), Env: []string{"FAKE_EXTRA=1"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if p.Slot != "b" || !strings.HasPrefix(p.Home, filepath.Join(m.dir(), "homes")) {
		t.Fatalf("实例落位不对: slot=%s home=%s", p.Slot, p.Home)
	}
	if info, err := os.Stat(p.Home); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("实例目录应 0700: %v %v", info, err)
	}
	for _, name := range []string{"auth.json", "config.toml"} {
		if info, err := os.Stat(filepath.Join(p.Home, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s 应 0600: %v %v", name, info, err)
		}
	}
	if m.RunningCount() != 1 || m.RuntimeInfo().Running != 1 {
		t.Fatalf("运行计数应为 1: %d", m.RunningCount())
	}
	init, err := p.Initialize(ctx)
	if err != nil || init.UserAgent != "codex-app-server-fake" || init.CodexHome != p.Home {
		t.Fatalf("Initialize: %+v %v", init, err)
	}
	// 环境与工作目录：HOME/CODEX_HOME 是实例目录，cwd 是实例目录下的 work/，PATH 只有系统目录，
	// 追加的环境变量到了。
	var env struct {
		Home, Cwd, Path, Extra string
	}
	if err := p.Client.Call(ctx, "echo/env", nil, &env); err != nil {
		t.Fatal(err)
	}
	if env.Home != p.Home || env.Cwd != filepath.Join(p.Home, "work") || env.Path != "/usr/local/bin:/usr/bin:/bin" || env.Extra != "1" {
		t.Fatalf("实例环境不对: %+v", env)
	}
	var auth struct {
		Present bool
		Bytes   int
	}
	if err := p.Client.Call(ctx, "auth/present", nil, &auth); err != nil || !auth.Present || auth.Bytes != len(`{"fake":"cred"}`) {
		t.Fatalf("凭据应原样写进 CODEX_HOME: %+v %v", auth, err)
	}
	// 握手后推来的 configWarning 在 Incoming 里。
	select {
	case msg := <-p.Client.Incoming():
		if msg.Method != "configWarning" || len(msg.ID) != 0 {
			t.Fatalf("应先收到 configWarning 通知: %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没收到 configWarning")
	}
	// 服务端错误以 *RPCError 返回。
	var rpcErr *RPCError
	if err := p.Client.Call(ctx, "fail", nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		t.Fatalf("服务端 error 应是 RPCError: %v", err)
	}
	// 反向请求：服务端问、客户端答、答复带回。
	type askResult struct {
		Reply map[string]any `json:"reply"`
	}
	askDone := make(chan error, 1)
	var ask askResult
	go func() { askDone <- p.Client.Call(ctx, "ask", nil, &ask) }()
	select {
	case msg := <-p.Client.Incoming():
		if msg.Method != "item/commandExecution/requestApproval" || string(msg.ID) != `"srv-1"` {
			t.Fatalf("反向请求不对: %+v", msg)
		}
		if err := p.Client.Reply(msg.ID, map[string]any{"decision": "accept"}); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没收到反向请求")
	}
	if err := <-askDone; err != nil || ask.Reply["decision"] != "accept" {
		t.Fatalf("反向请求答复没带回: %+v %v", ask, err)
	}
	// 超时：ctx 到期 Call 返回，不挂死。
	hctx, hcancel := context.WithTimeout(ctx, 200*time.Millisecond)
	err = p.Client.Call(hctx, "hang", nil, nil)
	hcancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang 应超时: %v", err)
	}
	// 运行中：安装与卸载都拒。
	if _, err := m.Install(ctx); !errors.Is(err, ErrComponentInUse) {
		t.Fatalf("运行中安装应拒: %v", err)
	}
	if err := m.Remove(ctx); !errors.Is(err, ErrComponentInUse) {
		t.Fatalf("运行中卸载应拒: %v", err)
	}
	if st := m.Status(ctx); st.Runtime == nil || st.Runtime.Running != 1 || !st.Runtime.LayoutOK {
		t.Fatalf("读数应带运行期: %+v", st.Runtime)
	}
	// 收尾：进程退出、实例目录（含凭据）删除、计数归零、再次 Call 报连接已关。
	home := p.Home
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("Close 后进程应已退出")
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("实例目录应已删除: %v", err)
	}
	if m.RunningCount() != 0 {
		t.Fatal("Close 后运行计数应归零")
	}
	if err := p.Client.Call(ctx, "echo/env", nil, nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("关闭后 Call 应报 ErrClientClosed: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	// 没有运行实例：安装守卫放行（进到「没有就绪制品」）。
	if _, err := m.Install(ctx); !errors.Is(err, ErrNoStaged) {
		t.Fatalf("无实例时安装应走到 staging 检查: %v", err)
	}
}

func TestCloseKillsStubbornProcess(t *testing.T) {
	m, comps := newRuntimeManager(t)
	installFakeSlot(t, comps, "a")
	p, err := m.Launch(LaunchOptions{Env: []string{"FAKE_STUBBORN=1"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > closeGrace+2*time.Second {
		t.Fatalf("Close 应在宽限后 SIGKILL 收尾，用了 %v", d)
	}
	select {
	case <-p.Exited():
	default:
		t.Fatal("Close 返回时进程应已退出")
	}
}

func TestProcessExitClosesClient(t *testing.T) {
	m, comps := newRuntimeManager(t)
	installFakeSlot(t, comps, "a")
	p, err := m.Launch(LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// quit 让服务端退出且不应答：Call 以 ErrClientClosed 返回，Done 关闭。
	if err := p.Client.Call(ctx, "quit", nil, nil); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("服务端退出后 Call 应报连接已关: %v", err)
	}
	select {
	case <-p.Client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done 应关闭")
	}
	select {
	case <-p.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("进程应已退出")
	}
}

func TestProcessExitLogsOnlyMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		code, signal int
	}{
		{"normal", "quit", 0, 0},
		{"failure", "fail-exit", 17, 0},
		{"signal", "", -1, int(syscall.SIGTERM)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, comps := newRuntimeManager(t)
			installFakeSlot(t, comps, "a")
			var logs bytes.Buffer
			m.logger = logging.New(&logs, slog.LevelDebug)
			p, err := m.Launch(LaunchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if tc.signal != 0 {
				if err := p.cmd.Process.Signal(syscall.Signal(tc.signal)); err != nil {
					t.Fatal(err)
				}
			} else if err := p.Client.Call(ctx, tc.method, nil, nil); !errors.Is(err, ErrClientClosed) {
				t.Fatalf("Call = %v", err)
			}
			select {
			case <-p.Exited():
			case <-ctx.Done():
				t.Fatal("process did not exit")
			}
			found := false
			for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte{'\n'}) {
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "app-server 进程已退出" {
					continue
				}
				found = true
				if record["exit_code"] != float64(tc.code) || record["signal"] != float64(tc.signal) || record["pid"] != float64(p.PID()) || record["slot"] != "a" || record["uptime_ms"] == nil {
					t.Fatalf("exit metadata = %v", record)
				}
			}
			if !found || strings.Contains(logs.String(), "fake-sensitive-stderr") {
				t.Fatalf("missing metadata or leaked stderr: %s", logs.String())
			}
		})
	}
}

func TestSelfCheck(t *testing.T) {
	m, comps := newRuntimeManager(t)
	ctx := context.Background()
	// 未装。
	res := m.SelfCheck(ctx)
	if res.OK || res.Handshake || !strings.Contains(res.Error, "尚未安装") {
		t.Fatalf("未装自检: %+v", res)
	}
	// 缺 code-mode host：不起进程，直接报缺。
	installFakeSlot(t, comps, "a", "bin/codex-code-mode-host")
	res = m.SelfCheck(ctx)
	if res.OK || res.Handshake || res.Slot != "a" || len(res.Missing) != 1 || !strings.Contains(res.Error, "codex-code-mode-host") {
		t.Fatalf("缺伴侣自检: %+v", res)
	}
	// 齐全：版本、捆绑 bwrap、握手、警告都到位；跑完不留实例与目录。
	installFakeSlot(t, comps, "a")
	res = m.SelfCheck(ctx)
	if !res.OK || !res.Handshake || res.Version != "codex-app-server 0.154.0-fake" || res.Sandbox.BundledBwrap != "bubblewrap 0.11.0-fake" ||
		res.UserAgent != "codex-app-server-fake" || len(res.Warnings) != 1 || res.Warnings[0] != "fake warning" || res.Error != "" || len(res.Missing) != 0 {
		t.Fatalf("齐全自检: %+v", res)
	}
	if res.CheckedAt.IsZero() || res.DurationMs < 0 {
		t.Fatalf("自检应带时间戳与耗时: %+v", res)
	}
	if res.CheckedAt.IsZero() || res.DurationMs < 0 {
		t.Fatalf("自检应带时间戳与耗时: %+v", res)
	}
	if !strings.HasPrefix(res.CodexHome, filepath.Join(m.dir(), "homes")) {
		t.Fatalf("自检 CODEX_HOME 应在 DataDir 下: %s", res.CodexHome)
	}
	if m.RunningCount() != 0 {
		t.Fatal("自检后不该留实例")
	}
	if entries, _ := os.ReadDir(m.homesDir()); len(entries) != 0 {
		t.Fatalf("自检后实例目录应清空: %v", entries)
	}
	if raw, err := json.Marshal(res); err != nil || !strings.Contains(string(raw), `"sandbox"`) {
		t.Fatalf("自检读数应可序列化: %v", err)
	}
}

func TestCleanupTempOnStart(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	dir := filepath.Join(dataDir, "components", ComponentName)
	for _, d := range []string{"homes/home-x", "unpack-abc", "staging/bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"download.tmp", "upload-1.tmp", "unpack.tmp", "homes/home-x/auth.json", "staging/bin/codex-app-server", "staged.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	NewManager(Options{DataDir: dataDir, Settings: newFakeSettings(), Engine: &fakeEngine{}, Keys: newSigner(t).keys})
	for _, gone := range []string{"download.tmp", "upload-1.tmp", "unpack.tmp", "unpack-abc", "homes"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s 应被清理: %v", gone, err)
		}
	}
	for _, kept := range []string{"staging/bin/codex-app-server", "staged.json"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s 应保留: %v", kept, err)
		}
	}
}

// 让 io 包在本文件保持被引用（fakeAppServer 的输出经 bufio；这里给 Stderr 选项一个用例）。
func TestLaunchStderrSink(t *testing.T) {
	m, comps := newRuntimeManager(t)
	installFakeSlot(t, comps, "a")
	var sink strings.Builder
	p, err := m.Launch(LaunchOptions{Stderr: io.MultiWriter(&sink)})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestProbeSandbox 钉住沙箱读数：Ubuntu 的 AppArmor userns 限制要把「可用」扣掉并单列出来。
func TestProbeSandbox(t *testing.T) {
	noBwrap := func(string) (string, error) { return "", errors.New("not found") }
	cases := []struct {
		name  string
		files map[string]string
		want  SandboxInfo
	}{
		{"Debian 放开", map[string]string{
			"/proc/sys/kernel/unprivileged_userns_clone": "1\n",
			"/sys/kernel/security/lsm":                   "lockdown,capability,yama,apparmor\n",
		}, SandboxInfo{UserNamespaces: true}},
		{"只有 max_user_namespaces", map[string]string{
			"/proc/sys/user/max_user_namespaces": "15000\n",
		}, SandboxInfo{UserNamespaces: true}},
		{"Ubuntu 24.04 缺省", map[string]string{
			"/proc/sys/kernel/unprivileged_userns_clone":             "1\n",
			"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1\n",
			"/sys/kernel/security/lsm":                               "lockdown,capability,landlock,yama,apparmor\n",
		}, SandboxInfo{AppArmorRestrictsUserNS: true, Landlock: true}},
		{"AppArmor 限制已关", map[string]string{
			"/proc/sys/kernel/unprivileged_userns_clone":             "1\n",
			"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "0\n",
		}, SandboxInfo{UserNamespaces: true}},
		{"内核关掉 userns", map[string]string{
			"/proc/sys/kernel/unprivileged_userns_clone": "0\n",
		}, SandboxInfo{}},
	}
	for _, c := range cases {
		read := func(p string) ([]byte, error) {
			if s, ok := c.files[p]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		}
		if got := probeSandboxWith(read, noBwrap); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	got := probeSandboxWith(func(string) ([]byte, error) { return nil, os.ErrNotExist },
		func(string) (string, error) { return "/usr/bin/bwrap", nil })
	if got != (SandboxInfo{SystemBwrap: true}) {
		t.Errorf("系统 bwrap: got %+v", got)
	}
}
