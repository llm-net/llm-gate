package updated

// 第三方组件（cloudflared）的 A/B 槽位安装与 Cloudflare Tunnel connector 的
// systemd unit 控制——都是要 root 才能做的事，所以落在升级引擎里，gatewayd 经
// 同一条 UDS 调用（docs-dev/firmware-cloudflare-tunnel.md §7.2、§7.3、§8）。
//
// 目录形态：
//
//	<components_dir>/cloudflared/slots/a/cloudflared      可执行文件
//	<components_dir>/cloudflared/slots/a/manifest.json    版本/摘要/长度/安装时刻
//	<components_dir>/cloudflared/slots/b/…
//	<components_dir>/cloudflared/current -> slots/a|slots/b（相对符号链接，原子换）
//	<tunnel_runtime_dir>/token                            0600 llmgate-tunnel，tmpfs
//
// 引擎不盲信调用方：安装前对收到的文件**重算 SHA-256、重跑 ELF 架构校验**，再以
// `llmgate-tunnel` 用户跑一次 `--version` 核对自述版本，之后才写进非活动 slot、
// 切换 current。connector 在跑时切换后会重启并等就绪；不就绪就切回旧 slot 重启，
// 旧版本继续服务。
//
// §15.1：token 只经 UDS 请求体进入本包、只写进运行期文件；不进 argv、环境变量、
// unit、状态文件、日志与任何应答。本文件的日志与错误只含版本号、路径与错误类别。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
)

// 组件与 Tunnel 的缺省落位。
const (
	ComponentCloudflared = "cloudflared"
	// ComponentMihomo 是板上代理内核（docs-dev/firmware-egress-proxy.md §8）：与 cloudflared
	// 共用 A/B 槽位与安装校验，服务 unit、运行期目录与就绪探针见 proxycore.go。
	ComponentMihomo = "mihomo"

	DefaultComponentsDir    = "/opt/llmgate/components"
	DefaultTunnelUnit       = "llmgate-cloudflared.service"
	DefaultTunnelUser       = "llmgate-tunnel"
	DefaultTunnelRuntimeDir = "/run/llmgate-cloudflared"
	TunnelTokenFileName     = "token"
	// DefaultTunnelReadyURL 是 cloudflared 只绑定 loopback 的 metrics 就绪探针；
	// unit 的启动参数把 metrics 钉在这个地址上。
	DefaultTunnelReadyURL = "http://127.0.0.1:20241/ready"

	defaultTunnelReadyWindow = 30 * time.Second
	componentVersionTimeout  = 20 * time.Second
)

// 组件相关哨兵错误。
var (
	ErrUnknownComponent  = errors.New("未知组件")
	ErrComponentMissing  = errors.New("组件尚未安装")
	ErrNoPreviousSlot    = errors.New("没有可回退的上一版本组件")
	ErrTunnelUserMissing = errors.New("系统里没有 llmgate-tunnel 用户，请先按部署文档创建")
)

// ComponentSlot 是一个已安装 slot 的清单。
type ComponentSlot struct {
	Version     string    `json:"version"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	InstalledAt time.Time `json:"installed_at"`
}

// ComponentStatus 是组件的引擎侧读数。
type ComponentStatus struct {
	Name         string         `json:"name"`
	Installed    bool           `json:"installed"`
	CurrentSlot  string         `json:"current_slot,omitempty"`
	Current      *ComponentSlot `json:"current,omitempty"`
	PreviousSlot string         `json:"previous_slot,omitempty"`
	Previous     *ComponentSlot `json:"previous,omitempty"`
}

// ComponentInstallRequest 是 UDS POST /components/{name}/install 的载荷：gatewayd
// staging 出来的本地文件路径、声明版本与摘要。引擎重算摘要、重跑校验，不盲信。
type ComponentInstallRequest struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// TunnelStatus 是 connector unit 的引擎侧读数。
type TunnelStatus struct {
	// UnitState 是 systemctl is-active 的状态词（active/inactive/failed/activating…）。
	UnitState string `json:"unit_state"`
	SubState  string `json:"sub_state,omitempty"`
	// Restarts 是 unit 的 NRestarts 计数：持续非零增长通常意味着 token 被拒或边缘不可达。
	Restarts int `json:"restarts"`
	// TokenPresent 报告运行期 token 文件在不在（不含内容）。
	TokenPresent bool             `json:"token_present"`
	Component    *ComponentStatus `json:"component,omitempty"`
}

// tunnelStartWire 是 UDS POST /tunnel/start 的请求体。它只在客户端序列化与服务端
// 反序列化的一瞬存在，不被任何日志或状态持有。
type tunnelStartWire struct {
	Token string `json:"token"`
}

// ---- 路径 ----

func (e *Engine) componentDir(name string) string {
	return filepath.Join(e.opt.ComponentsDir, name)
}

func (e *Engine) slotDir(name, slot string) string {
	return filepath.Join(e.componentDir(name), "slots", slot)
}

func (e *Engine) slotBinary(name, slot string) string {
	return filepath.Join(e.slotDir(name, slot), name)
}

func (e *Engine) slotManifest(name, slot string) string {
	return filepath.Join(e.slotDir(name, slot), "manifest.json")
}

func (e *Engine) currentLink(name string) string {
	return filepath.Join(e.componentDir(name), "current")
}

func (e *Engine) tokenPath() string {
	return filepath.Join(e.opt.TunnelRuntimeDir, TunnelTokenFileName)
}

// currentSlot 读 current 符号链接指向的 slot 名；没有或指向不认识的目标回空串。
func (e *Engine) currentSlot(name string) string {
	target, err := os.Readlink(e.currentLink(name))
	if err != nil {
		return ""
	}
	switch filepath.Base(target) {
	case "a", "b":
		return filepath.Base(target)
	}
	return ""
}

// readSlot 读一个 slot 的清单并确认可执行文件在场。
func (e *Engine) readSlot(name, slot string) (*ComponentSlot, error) {
	raw, err := os.ReadFile(e.slotManifest(name, slot))
	if err != nil {
		return nil, err
	}
	var m ComponentSlot
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	st, err := os.Stat(e.slotBinary(name, slot))
	if err != nil {
		return nil, err
	}
	if st.Size() != m.SizeBytes {
		return nil, errors.New("slot 清单与文件长度不符")
	}
	return &m, nil
}

func otherSlot(slot string) string {
	if slot == "a" {
		return "b"
	}
	return "a"
}

func checkComponentName(name string) error {
	switch name {
	case ComponentCloudflared, ComponentMihomo:
		return nil
	}
	return ErrUnknownComponent
}

// componentService 是一个组件背后的 systemd 服务：安装/回退/卸载时按它重启、等就绪或停掉。
type componentService struct {
	unit           string
	active         func(ctx context.Context) bool
	restartAndWait func(ctx context.Context) error
	// cleanup 在卸载时清掉运行期文件（token / 配置）。
	cleanup func()
}

func (e *Engine) serviceFor(name string) componentService {
	if name == ComponentMihomo {
		return componentService{
			unit:           e.opt.ProxyUnit,
			active:         e.proxyActive,
			restartAndWait: e.restartProxyAndWait,
			cleanup:        e.removeProxyConfig,
		}
	}
	return componentService{
		unit:           e.opt.TunnelUnit,
		active:         e.tunnelActive,
		restartAndWait: e.restartTunnelAndWait,
		cleanup:        e.removeToken,
	}
}

// ---- 读数 ----

// ComponentStatus 汇总一个组件的 slot 状态。
func (e *Engine) ComponentStatus(name string) (*ComponentStatus, error) {
	if err := checkComponentName(name); err != nil {
		return nil, err
	}
	return e.componentStatus(name), nil
}

func (e *Engine) componentStatus(name string) *ComponentStatus {
	st := &ComponentStatus{Name: name}
	cur := e.currentSlot(name)
	if cur != "" {
		if m, err := e.readSlot(name, cur); err == nil {
			st.Installed = true
			st.CurrentSlot = cur
			st.Current = m
		}
	}
	prev := otherSlot(cur)
	if cur == "" {
		// 没有 current 时两个 slot 都可能有残留：都不算「上一版本」——回退语义
		// 只在有当前版本时成立。
		return st
	}
	if m, err := e.readSlot(name, prev); err == nil {
		st.PreviousSlot = prev
		st.Previous = m
	}
	return st
}

// ---- 安装 / 回退 / 卸载 ----

// ComponentInstall 把校验过的文件装进非活动 slot 并切换 current；connector 在跑时
// 重启并等就绪，不就绪切回旧 slot。同步执行（几秒到几十秒），调用方给足预算。
func (e *Engine) ComponentInstall(ctx context.Context, name string, req ComponentInstallRequest) (*ComponentStatus, error) {
	if err := checkComponentName(name); err != nil {
		return nil, err
	}
	if req.Path == "" || req.Version == "" || req.SHA256 == "" {
		return nil, errors.New("安装请求缺少 path/version/sha256")
	}
	e.compMu.Lock()
	defer e.compMu.Unlock()

	sum, err := fileSHA256(req.Path)
	if err != nil {
		return nil, errors.New("读取组件制品失败")
	}
	if sum != strings.ToLower(req.SHA256) {
		return nil, errors.New("组件制品摘要与声明不符")
	}
	if err := e.opt.VerifyComponent(req.Path); err != nil {
		return nil, err
	}
	st, err := os.Stat(req.Path)
	if err != nil {
		return nil, errors.New("读取组件制品失败")
	}

	cur := e.currentSlot(name)
	target := otherSlot(cur)
	if cur == "" {
		target = "a"
	}
	dir := e.slotDir(name, target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建组件目录失败: %w", err)
	}
	bin := e.slotBinary(name, target)
	tmp := bin + ".next"
	if err := copyFile(req.Path, tmp, 0o755); err != nil {
		return nil, fmt.Errorf("写入组件文件失败: %w", err)
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(tmp, 0, 0)
		_ = os.Chmod(tmp, 0o755)
	}
	// 自述版本：以受限身份跑一次版本命令，输出里必须出现声明的版本串。
	ver, err := e.opt.ComponentVersion(ctx, name, tmp)
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("组件自述版本读取失败: %w", err)
	}
	if !strings.Contains(ver, req.Version) {
		os.Remove(tmp)
		return nil, errors.New("组件自述版本与清单声明不符")
	}
	if err := os.Rename(tmp, bin); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("组件文件落位失败: %w", err)
	}
	slot := ComponentSlot{Version: req.Version, SHA256: sum, SizeBytes: st.Size(), InstalledAt: e.opt.Now()}
	raw, _ := json.Marshal(slot)
	if err := atomicWrite(e.slotManifest(name, target), raw, 0o644); err != nil {
		return nil, fmt.Errorf("写入 slot 清单失败: %w", err)
	}
	if err := e.switchCurrent(name, target); err != nil {
		return nil, err
	}
	e.logger.Info("组件已安装", "component", name, "version", req.Version, "slot", target)

	// 服务在跑：换新后重启并等就绪；不就绪切回旧 slot 继续用旧版本。
	svc := e.serviceFor(name)
	if svc.active(ctx) {
		if err := svc.restartAndWait(ctx); err != nil {
			e.logger.Warn("新版本组件未能就绪，切回旧 slot", "component", name, "version", req.Version, "err", err.Error())
			if cur != "" {
				if rerr := e.switchCurrent(name, cur); rerr == nil {
					_ = e.sysRun(ctx, "restart", svc.unit)
				}
			}
			return e.componentStatus(name), fmt.Errorf("新版本 %s 未能在健康窗口内就绪，已回到旧版本继续运行", req.Version)
		}
	}
	return e.componentStatus(name), nil
}

// ComponentRollback 把 current 切回另一个 slot；connector 在跑时重启。
func (e *Engine) ComponentRollback(ctx context.Context, name string) (*ComponentStatus, error) {
	if err := checkComponentName(name); err != nil {
		return nil, err
	}
	e.compMu.Lock()
	defer e.compMu.Unlock()
	cur := e.currentSlot(name)
	if cur == "" {
		return nil, ErrComponentMissing
	}
	prev := otherSlot(cur)
	if _, err := e.readSlot(name, prev); err != nil {
		return nil, ErrNoPreviousSlot
	}
	if err := e.switchCurrent(name, prev); err != nil {
		return nil, err
	}
	e.logger.Info("组件已回退", "component", name, "slot", prev)
	svc := e.serviceFor(name)
	if svc.active(ctx) {
		if err := e.sysRun(ctx, "restart", svc.unit); err != nil {
			return e.componentStatus(name), fmt.Errorf("组件已回退但重启服务失败: %w", err)
		}
	}
	return e.componentStatus(name), nil
}

// ComponentRemove 停掉 connector、清除运行期 token，并删除整个组件目录。
func (e *Engine) ComponentRemove(ctx context.Context, name string) (*ComponentStatus, error) {
	if err := checkComponentName(name); err != nil {
		return nil, err
	}
	e.compMu.Lock()
	defer e.compMu.Unlock()
	svc := e.serviceFor(name)
	if svc.active(ctx) {
		if err := e.sysRun(ctx, "stop", svc.unit); err != nil {
			return nil, fmt.Errorf("停止服务失败: %w", err)
		}
	}
	svc.cleanup()
	if err := os.RemoveAll(e.componentDir(name)); err != nil {
		return nil, fmt.Errorf("删除组件目录失败: %w", err)
	}
	e.logger.Info("组件已卸载", "component", name)
	return e.componentStatus(name), nil
}

// switchCurrent 原子切换 current 符号链接：先建临时链接再 rename 覆盖。
func (e *Engine) switchCurrent(name, slot string) error {
	link := e.currentLink(name)
	tmp := link + ".next"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("slots", slot), tmp); err != nil {
		return fmt.Errorf("创建 current 链接失败: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("切换 current 链接失败: %w", err)
	}
	return nil
}

// ---- Tunnel unit ----

// TunnelStart 写运行期 token 文件并启动（或按需重启）connector。token 与上次相同且
// unit 已在跑时什么都不做——gatewayd 每次开机都会调它一次，不能把健康的连接重启掉。
func (e *Engine) TunnelStart(ctx context.Context, token string) (*TunnelStatus, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("缺少 tunnel token")
	}
	e.compMu.Lock()
	defer e.compMu.Unlock()
	if e.currentSlot(ComponentCloudflared) == "" {
		return nil, ErrComponentMissing
	}
	uid, gid, err := e.tunnelIDs()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.opt.TunnelRuntimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建运行期目录失败: %w", err)
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(e.opt.TunnelRuntimeDir, uid, gid)
		_ = os.Chmod(e.opt.TunnelRuntimeDir, 0o700)
	}
	existing, _ := os.ReadFile(e.tokenPath())
	changed := string(existing) != token
	if changed {
		tmp := e.tokenPath() + ".next"
		if err := os.WriteFile(tmp, []byte(token), 0o600); err != nil {
			return nil, errors.New("写入运行期 token 失败")
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(tmp, uid, gid); err != nil {
				os.Remove(tmp)
				return nil, errors.New("设置运行期 token 属主失败")
			}
		}
		if err := os.Rename(tmp, e.tokenPath()); err != nil {
			os.Remove(tmp)
			return nil, errors.New("落位运行期 token 失败")
		}
	}
	state, _ := e.sysIsActive(ctx, e.opt.TunnelUnit)
	switch {
	case state == "active" && !changed:
		// 已在跑、凭据没变：不动它。
	case state == "active":
		if err := e.sysRun(ctx, "restart", e.opt.TunnelUnit); err != nil {
			return nil, fmt.Errorf("重启 connector 失败: %w", err)
		}
	default:
		if err := e.sysRun(ctx, "start", e.opt.TunnelUnit); err != nil {
			return nil, fmt.Errorf("启动 connector 失败: %w", err)
		}
	}
	e.logger.Info("connector 已启动", "token_replaced", changed)
	return e.tunnelStatus(ctx), nil
}

// TunnelStop 停掉 connector 并清除运行期 token。
func (e *Engine) TunnelStop(ctx context.Context) (*TunnelStatus, error) {
	e.compMu.Lock()
	defer e.compMu.Unlock()
	if e.tunnelActive(ctx) {
		if err := e.sysRun(ctx, "stop", e.opt.TunnelUnit); err != nil {
			return nil, fmt.Errorf("停止 connector 失败: %w", err)
		}
	}
	e.removeToken()
	e.logger.Info("connector 已停止")
	return e.tunnelStatus(ctx), nil
}

// TunnelStatus 汇总 connector unit 的读数。
func (e *Engine) TunnelStatus(ctx context.Context) *TunnelStatus {
	return e.tunnelStatus(ctx)
}

func (e *Engine) tunnelStatus(ctx context.Context) *TunnelStatus {
	st := &TunnelStatus{Component: e.componentStatus(ComponentCloudflared)}
	state, err := e.sysIsActive(ctx, e.opt.TunnelUnit)
	if err != nil || state == "" {
		state = "unknown"
	}
	st.UnitState = state
	if props, err := e.sysShow(ctx, e.opt.TunnelUnit, "SubState", "NRestarts"); err == nil {
		st.SubState = props["SubState"]
		if n, err := strconv.Atoi(props["NRestarts"]); err == nil {
			st.Restarts = n
		}
	}
	if _, err := os.Stat(e.tokenPath()); err == nil {
		st.TokenPresent = true
	}
	return st
}

func (e *Engine) tunnelActive(ctx context.Context) bool {
	state, err := e.sysIsActive(ctx, e.opt.TunnelUnit)
	return err == nil && (state == "active" || state == "activating" || state == "reloading")
}

// restartTunnelAndWait 重启 connector 并在就绪窗口内轮询 metrics /ready。
// 没有配置就绪探针时只重启不等。
func (e *Engine) restartTunnelAndWait(ctx context.Context) error {
	if err := e.sysRun(ctx, "restart", e.opt.TunnelUnit); err != nil {
		return fmt.Errorf("重启 connector 失败: %w", err)
	}
	if e.opt.TunnelReady == nil {
		return nil
	}
	deadline := e.opt.Now().Add(e.opt.TunnelReadyWindow)
	for {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := e.opt.TunnelReady(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if state, serr := e.sysIsActive(ctx, e.opt.TunnelUnit); serr == nil && state == "failed" {
			return errors.New("connector unit 进入 failed 状态")
		}
		if !e.opt.Now().Before(deadline) {
			return errors.New("就绪探针超时")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.opt.PollEvery):
		}
	}
}

func (e *Engine) removeToken() {
	if err := os.Remove(e.tokenPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.logger.Warn("清除运行期 token 失败", "err", err.Error())
	}
}

// tunnelIDs 取 connector 用户的 uid/gid（Options.TunnelIDs）。
func (e *Engine) tunnelIDs() (int, int, error) { return e.opt.TunnelIDs() }

// LookupTunnelUser 返回生产的 uid/gid 解析：root 下必须存在 llmgate-tunnel 用户
// （缺了就是部署没做完，明确报出来）；非 root（dev）用当前身份。
func LookupTunnelUser(name string) func() (int, int, error) {
	return func() (int, int, error) {
		if os.Geteuid() != 0 {
			return os.Getuid(), os.Getgid(), nil
		}
		u, err := user.Lookup(name)
		if err != nil {
			return 0, 0, ErrTunnelUserMissing
		}
		uid, err1 := strconv.Atoi(u.Uid)
		gid, err2 := strconv.Atoi(u.Gid)
		if err1 != nil || err2 != nil {
			return 0, 0, ErrTunnelUserMissing
		}
		return uid, gid, nil
	}
}

func (e *Engine) sysShow(parent context.Context, unit string, props ...string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return e.opt.Sys.Show(ctx, unit, props...)
}

// ---- 生产默认实现 ----

// componentVersionArgs 是各组件打印自述版本的参数：cloudflared 认 --version，
// Mihomo 用 Go flag 只定义了 -v。
func componentVersionArgs(name string) []string {
	if name == ComponentMihomo {
		return []string{"-v"}
	}
	return []string{"--version"}
}

// RunComponentVersion 返回「跑一次版本命令读自述版本」的实现：root 下以该组件的
// 服务用户身份、最小环境执行，超时封顶；输出只取 stdout。users 是组件名 → 用户名。
func RunComponentVersion(users map[string]string) func(ctx context.Context, name, path string) (string, error) {
	return func(ctx context.Context, name, path string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, componentVersionTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path, componentVersionArgs(name)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
		if os.Geteuid() == 0 {
			cred, err := lookupCredential(users[name])
			if err != nil {
				return "", err
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		}
		out, err := cmd.Output()
		if err != nil {
			return "", errors.New("执行失败或超时")
		}
		return strings.TrimSpace(string(out)), nil
	}
}

// lookupCredential 解析系统用户为 exec 凭据；用户不存在按各自的部署缺失错误报出。
func lookupCredential(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		if name == DefaultProxyUser {
			return nil, ErrProxyUserMissing
		}
		return nil, ErrTunnelUserMissing
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}, nil
}

// VerifyComponentFile 是生产的组件形状校验：本机架构的 ELF 可执行文件。
func VerifyComponentFile(path string) error { return elfcheck.Verify(path) }

// HTTPReadyProber 构造对 cloudflared metrics /ready 的探针：200 即就绪。
func HTTPReadyProber(url string) HealthProber {
	// 本机 127.0.0.1 探针：Transport 显式直连（不读 HTTP_PROXY 等环境代理）。
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return errors.New("connector 尚未就绪")
		}
		return nil
	}
}
