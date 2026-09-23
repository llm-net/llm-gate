package codexappserver

// 运行期：把已装槽位里的 app-server 当引擎用。本文件是固件以后拉起 app-server 的唯一入口，
// 把「怎样才算装对了」和「怎样才能跑对」钉在一处：
//
//   - 入口从 <ComponentsDir>/codex-app-server/current 解析到**真实槽位路径**再执行（不经
//     current 符号链接启动），app-server 用 current_exe() 找同目录的 codex-code-mode-host、
//     ../codex-resources/bwrap、../codex-path/rg，因此一个实例整个生命周期都钉在一个槽位上；
//     升级切走 current 不影响它，只有整目录删槽位（安装到该槽 / 卸载）才会，所以那两步在有
//     实例运行时被拒（ErrComponentInUse）。
//   - 每个实例一个独立 CODEX_HOME：<DataDir>/components/codex-app-server/homes/home-*（0700），
//     在 DataDir 下而不是 /tmp——app-server 拒绝在临时目录里落 helper 二进制。auth.json /
//     config.toml 只按调用方给的字节写进去（0600），Close 时覆写再删整个目录。
//   - 环境最小化：HOME / CODEX_HOME 指向实例目录，PATH 只有系统目录，RUST_LOG=error；stderr
//     缺省丢弃。进程放进独立进程组，Close 时整组收尾（code-mode host、bwrap、子 shell）。
//   - 沙箱：app-server 在 Linux 上缺省用 bubblewrap。AppArmor 限制非特权用户命名空间
//     （Ubuntu 23.10+ 缺省）时 bwrap 建不出沙箱，内核有 Landlock 就让实例改走 Landlock
//     （useLegacyLandlock），每次 Launch 现读前提。
//   - 线程一律 ephemeral（调用方在 thread/start 里给 ephemeral: true）：会话 rollout 不落盘，
//     提示词与模型输出不进磁盘（§15.1）。凭据写进实例目录只为让 app-server 自己读，本包不
//     解析、不记录它。
//
// SelfCheck 是安装后的可用性自检：布局齐全 → 入口 --version → 捆绑 bwrap 能跑 → 起一个实例
// 完成 initialize 握手 → 收集 configWarning。它不需要凭据，也不发起任何模型请求。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 包内运行期依赖的文件（相对槽位根）。required 缺一不可；bundled 缺了 app-server 还能起，但
// 系统没装同名工具时沙箱 / 搜索会退化，自检把它们列进 missing。
var (
	requiredFiles = []string{EntrypointPath, "bin/codex-code-mode-host", PackageMetaFile}
	bundledFiles  = []string{"codex-resources/bwrap", "codex-path/rg", "codex-resources/zsh/bin/zsh"}
)

const (
	handshakeTimeout   = 20 * time.Second
	versionTimeout     = 20 * time.Second
	closeGrace         = 3 * time.Second
	warningsWindow     = 500 * time.Millisecond
	incomingBufferSize = 256
)

// Installed 是解析出来的已装槽位。
type Installed struct {
	Slot       string // a | b
	Dir        string // 槽位根（真实路径）
	Entrypoint string // Dir/bin/codex-app-server
	Missing    []string
}

// RuntimeInfo 是随组件读数一起给管理面的运行期摘要。
type RuntimeInfo struct {
	Slot       string   `json:"slot,omitempty"`
	Entrypoint string   `json:"entrypoint,omitempty"`
	LayoutOK   bool     `json:"layout_ok"`
	Missing    []string `json:"missing,omitempty"`
	Running    int      `json:"running"`
}

func (m *Manager) componentRoot() string { return filepath.Join(m.opt.ComponentsDir, ComponentName) }

// Installed 解析 current 指向的槽位并核对布局。current 缺失 / 指向不认识的目标 / 入口不是普通
// 文件都算 ErrComponentMissing；required 之外的文件缺失只记在 Missing 里。
func (m *Manager) Installed() (*Installed, error) {
	link := filepath.Join(m.componentRoot(), "current")
	target, err := os.Readlink(link)
	if err != nil {
		return nil, ErrComponentMissing
	}
	slot := filepath.Base(target)
	if slot != "a" && slot != "b" {
		return nil, ErrComponentMissing
	}
	dir := filepath.Join(m.componentRoot(), "slots", slot)
	inst := &Installed{Slot: slot, Dir: dir, Entrypoint: filepath.Join(dir, filepath.FromSlash(EntrypointPath))}
	for _, rel := range requiredFiles {
		if !regularFile(filepath.Join(dir, filepath.FromSlash(rel))) {
			inst.Missing = append(inst.Missing, rel)
		}
	}
	if len(inst.Missing) > 0 {
		return inst, fmt.Errorf("%w：槽位 %s 缺 %s", ErrComponentMissing, slot, strings.Join(inst.Missing, "、"))
	}
	for _, rel := range bundledFiles {
		if !regularFile(filepath.Join(dir, filepath.FromSlash(rel))) {
			inst.Missing = append(inst.Missing, rel)
		}
	}
	return inst, nil
}

func regularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// RuntimeInfo 组装运行期摘要（只做 stat，不起进程）。
func (m *Manager) RuntimeInfo() RuntimeInfo {
	info := RuntimeInfo{Running: m.RunningCount()}
	inst, err := m.Installed()
	if inst != nil {
		info.Slot, info.Entrypoint, info.Missing = inst.Slot, inst.Entrypoint, inst.Missing
	}
	info.LayoutOK = err == nil
	return info
}

// RunningCount 是本进程里尚未 Close 的实例数。
func (m *Manager) RunningCount() int {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	return len(m.running)
}

// LaunchOptions 是一次拉起的参数。
type LaunchOptions struct {
	// WorkDir 是 app-server 的工作目录（线程的 cwd 缺省值）；空 = 实例目录下的 work/。
	WorkDir string
	// AuthJSON 是写进 CODEX_HOME/auth.json 的原始字节（0600）；nil = 不写（未登录实例，只能
	// 做握手与目录类调用）。本包不解析它。
	AuthJSON []byte
	// ConfigTOML 是写进 CODEX_HOME/config.toml 的原始字节（0600）；nil = 不写。
	ConfigTOML []byte
	// Env 追加到最小环境之后（KEY=VALUE）。
	Env []string
	// Stderr 接收 app-server 的 stderr；nil = 丢弃。§15.1：接它的一方不得把内容写进日志。
	Stderr io.Writer
}

// Process 是一个运行中的 app-server 实例。
type Process struct {
	Client *Client
	Slot   string
	Home   string

	m         *Manager
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt time.Time

	waitOnce sync.Once
	exited   chan struct{}
	waitErr  error

	closeOnce sync.Once
	closeErr  error
}

// Launch 从已装槽位拉起一个实例并连上它的 stdio。调用方随后 Initialize，用完 Close。
func (m *Manager) Launch(opts LaunchOptions) (*Process, error) {
	inst, err := m.Installed()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.homesDir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建运行期目录失败: %w", err)
	}
	home, err := os.MkdirTemp(m.homesDir(), "home-")
	if err != nil {
		return nil, fmt.Errorf("创建实例目录失败: %w", err)
	}
	cleanup := func() { wipeHome(home) }
	if opts.AuthJSON != nil {
		if err := os.WriteFile(filepath.Join(home, "auth.json"), opts.AuthJSON, 0o600); err != nil {
			cleanup()
			return nil, errors.New("写入实例凭据失败")
		}
	}
	if opts.ConfigTOML != nil {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), opts.ConfigTOML, 0o600); err != nil {
			cleanup()
			return nil, errors.New("写入实例配置失败")
		}
	}
	work := opts.WorkDir
	if work == "" {
		work = filepath.Join(home, "work")
		if err := os.Mkdir(work, 0o700); err != nil {
			cleanup()
			return nil, fmt.Errorf("创建工作目录失败: %w", err)
		}
	}

	var args []string
	if useLegacyLandlock(m.opt.ProbeSandbox()) {
		args = append(args, "-c", legacyLandlockOverride)
	}
	cmd := exec.Command(inst.Entrypoint, args...)
	cmd.Dir = work
	cmd.Env = append([]string{
		"HOME=" + home,
		"CODEX_HOME=" + home,
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"RUST_LOG=error",
	}, opts.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("启动 app-server 失败: %w", err)
	}
	p := &Process{Slot: inst.Slot, Home: home, m: m, cmd: cmd, stdin: stdin, startedAt: m.opt.Now(), exited: make(chan struct{})}
	p.Client = NewClient(stdout, stdin, incomingBufferSize)
	m.runMu.Lock()
	m.running[p] = struct{}{}
	m.runMu.Unlock()
	go p.wait()
	m.logger.Info("app-server 实例已拉起", "slot", inst.Slot, "pid", cmd.Process.Pid)
	return p, nil
}

func (p *Process) wait() {
	p.waitOnce.Do(func() {
		p.waitErr = p.cmd.Wait()
		// 只记录操作系统给出的退出元数据；stderr 可能含模型正文或凭据，不能落日志。
		code, signal := -1, 0
		if state := p.cmd.ProcessState; state != nil {
			code = state.ExitCode()
			if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				signal = int(status.Signal())
			}
		}
		p.m.logger.Info("app-server 进程已退出", "slot", p.Slot, "pid", p.PID(),
			"exit_code", code, "signal", signal,
			"uptime_ms", p.m.opt.Now().Sub(p.startedAt).Milliseconds())
		close(p.exited)
	})
}

// Exited 在进程退出后关闭。
func (p *Process) Exited() <-chan struct{} { return p.exited }

// PID 是实例主进程号。
func (p *Process) PID() int { return p.cmd.Process.Pid }

// InitializeResult 是 initialize 应答里本包关心的字段。
type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// Initialize 做协议握手（见 Handshake）。
func (p *Process) Initialize(ctx context.Context) (*InitializeResult, error) {
	return Handshake(ctx, p.Client, p.m.opt.ClientVersion)
}

// Handshake 在任意一条 app-server 连接上做协议握手：initialize（clientInfo 为 llmgate，开
// experimentalApi）→ 通知 initialized。节点端引擎经 SSH 连上工作节点上的 `codex app-server`
// 时也用它。
func Handshake(ctx context.Context, c *Client, version string) (*InitializeResult, error) {
	var res InitializeResult
	params := map[string]any{
		"clientInfo":   map[string]any{"name": "llmgate", "title": "LLM Gate", "version": version},
		"capabilities": map[string]any{"experimentalApi": true},
	}
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		return nil, err
	}
	if err := c.Notify("initialized", nil); err != nil {
		return nil, err
	}
	return &res, nil
}

// Close 结束实例：先关 stdin 让它自然退出，超过 closeGrace 整个进程组 SIGKILL；然后覆写并删掉
// 实例目录，从运行表摘除。可重复调用。
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.Client.Close()
		_ = p.stdin.Close()
		select {
		case <-p.exited:
		case <-time.After(closeGrace):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-p.exited:
			case <-time.After(closeGrace):
				p.closeErr = errors.New("app-server 进程组未能在期限内结束")
			}
		}
		wipeHome(p.Home)
		p.m.runMu.Lock()
		delete(p.m.running, p)
		p.m.runMu.Unlock()
		p.m.logger.Info("app-server 实例已结束", "slot", p.Slot, "pid", p.cmd.Process.Pid,
			"uptime_ms", p.m.opt.Now().Sub(p.startedAt).Milliseconds())
	})
	return p.closeErr
}

// wipeHome 先把实例目录里可能载有凭据的文件覆写成零，再删整个目录。
func wipeHome(home string) {
	for _, name := range []string{"auth.json", "config.toml"} {
		p := filepath.Join(home, name)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
				_, _ = f.Write(make([]byte, info.Size()))
				_ = f.Sync()
				_ = f.Close()
			}
		}
	}
	_ = os.RemoveAll(home)
}

// ---- 自检 ----

// SandboxInfo 是沙箱前提的读数：app-server 在 Linux 上缺省用 bubblewrap（系统的或捆绑的），
// bubblewrap 需要非特权用户命名空间。UserNamespaces 是扣掉 AppArmor 限制后的可用性，
// AppArmorRestrictsUserNS 单列限制本身；LegacyLandlock 表示实例改走 Landlock。
type SandboxInfo struct {
	UserNamespaces          bool   `json:"user_namespaces"`
	AppArmorRestrictsUserNS bool   `json:"apparmor_restricts_userns"`
	Landlock                bool   `json:"landlock"`
	SystemBwrap             bool   `json:"system_bwrap"`
	BundledBwrap            string `json:"bundled_bwrap,omitempty"`
	LegacyLandlock          bool   `json:"legacy_landlock"`
}

// legacyLandlockOverride 让实例改走 Codex 的 Landlock 沙箱。Landlock 模式不支持
// workspace-write（app-server 直接 panic）；设备上的实例只用 read-only 与 danger-full-access。
const legacyLandlockOverride = "features.use_legacy_landlock=true"

// useLegacyLandlock：AppArmor 挡住 bwrap（read-only 下命令一条都起不来）而内核有 Landlock 时改走它。
func useLegacyLandlock(info SandboxInfo) bool { return info.AppArmorRestrictsUserNS && info.Landlock }

// SelfCheckResult 是一次自检的读数。OK 只在布局齐全、入口自述版本可读、握手成功时为真。
type SelfCheckResult struct {
	OK         bool        `json:"ok"`
	Slot       string      `json:"slot,omitempty"`
	Entrypoint string      `json:"entrypoint,omitempty"`
	Version    string      `json:"version,omitempty"`
	Missing    []string    `json:"missing,omitempty"`
	Handshake  bool        `json:"handshake"`
	UserAgent  string      `json:"user_agent,omitempty"`
	CodexHome  string      `json:"codex_home,omitempty"`
	DurationMs int64       `json:"duration_ms"`
	Warnings   []string    `json:"warnings,omitempty" i18n:"text"`
	Sandbox    SandboxInfo `json:"sandbox"`
	Error      string      `json:"error,omitempty" i18n:"text"`
	CheckedAt  time.Time   `json:"checked_at"`
}

// SelfCheck 对已装槽位做一次可用性自检（不需要凭据，不发模型请求）。
func (m *Manager) SelfCheck(ctx context.Context) SelfCheckResult {
	start := m.opt.Now()
	res := m.selfCheck(ctx)
	res.CheckedAt = start
	res.DurationMs = m.opt.Now().Sub(start).Milliseconds()
	return res
}

func (m *Manager) selfCheck(ctx context.Context) SelfCheckResult {
	res := SelfCheckResult{Sandbox: m.opt.ProbeSandbox()}
	res.Sandbox.LegacyLandlock = useLegacyLandlock(res.Sandbox)
	inst, err := m.Installed()
	if inst != nil {
		res.Slot, res.Entrypoint, res.Missing = inst.Slot, inst.Entrypoint, inst.Missing
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if out, err := runVersion(ctx, inst.Entrypoint); err != nil {
		res.Error = "入口自述版本读取失败: " + err.Error()
		return res
	} else {
		res.Version = out
	}
	if bwrap := filepath.Join(inst.Dir, "codex-resources", "bwrap"); regularFile(bwrap) {
		if out, err := runVersion(ctx, bwrap); err == nil {
			res.Sandbox.BundledBwrap = out
		} else {
			res.Warnings = append(res.Warnings, "捆绑的 bubblewrap 无法执行: "+err.Error())
		}
	}

	p, err := m.Launch(LaunchOptions{})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer p.Close()
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	init, err := p.Initialize(hctx)
	if err != nil {
		res.Error = "initialize 握手失败: " + err.Error()
		return res
	}
	res.Handshake = true
	res.UserAgent, res.CodexHome = init.UserAgent, init.CodexHome
	// 握手后 app-server 会把配置类警告以 configWarning 通知推过来；收一小段时间。
	timer := time.NewTimer(warningsWindow)
	defer timer.Stop()
	for done := false; !done; {
		select {
		case msg := <-p.Client.Incoming():
			if msg.Method == "configWarning" {
				var w struct {
					Summary string `json:"summary"`
				}
				if json.Unmarshal(msg.Params, &w) == nil && w.Summary != "" {
					res.Warnings = append(res.Warnings, w.Summary)
				}
			}
		case <-p.Client.Done():
			done = true
		case <-timer.C:
			done = true
		}
	}
	res.OK = true
	return res
}

// runVersion 以当前身份跑 `<bin> --version`，只取 stdout 首行。
func runVersion(parent context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", errors.New("执行失败或超时")
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
	return strings.TrimSpace(line), nil
}

// probeSandbox 读沙箱前提。文件不存在按「不可用」记，不报错。
func probeSandbox() SandboxInfo { return probeSandboxWith(os.ReadFile, exec.LookPath) }

func probeSandboxWith(readFile func(string) ([]byte, error), lookPath func(string) (string, error)) SandboxInfo {
	var info SandboxInfo
	if raw, err := readFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil {
		info.UserNamespaces = strings.TrimSpace(string(raw)) == "1"
	} else if raw, err := readFile("/proc/sys/user/max_user_namespaces"); err == nil {
		info.UserNamespaces = strings.TrimSpace(string(raw)) != "0"
	}
	// Ubuntu 23.10+ 缺省为 1：没有放行 userns 的 AppArmor profile 的程序（含捆绑 bwrap）建出的
	// 命名空间里没有能力，bubblewrap 起不来，因此不算可用。
	if raw, err := readFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil &&
		strings.TrimSpace(string(raw)) == "1" {
		info.AppArmorRestrictsUserNS = true
		info.UserNamespaces = false
	}
	if raw, err := readFile("/sys/kernel/security/lsm"); err == nil {
		info.Landlock = strings.Contains(string(raw), "landlock")
	}
	if _, err := lookPath("bwrap"); err == nil {
		info.SystemBwrap = true
	}
	return info
}
