package updated

// 板上代理内核（Mihomo）的 systemd unit 控制（docs-dev/firmware-egress-proxy.md §8.4、§8.5）：
// 与 cloudflared 共用 A/B 槽位（components.go），本文件只管「把 gatewayd 生成的受限配置
// 写进运行期目录 → 让内核自检 → 起/重启 unit → 等本机 SOCKS 端口就绪」与停止/读数。
//
//	<proxy_runtime_dir>/config.yaml   0600 llmgate-proxy，tmpfs；也是内核的工作目录（cache.db）
//
// 配置正文里有订阅节点的凭据：只经 UDS 请求体进入本包、只写进运行期文件；不进 argv、
// 环境变量、unit、状态文件、日志与任何应答（§15.1）。内核不 enable，gatewayd 每次开机
// 由管理器按已存订阅重新生成配置并调 ProxyStart（同配置且在跑就不重启）。

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultProxyUnit       = "llmgate-mihomo.service"
	DefaultProxyUser       = "llmgate-proxy"
	DefaultProxyRuntimeDir = "/run/llmgate-mihomo"
	ProxyConfigFileName    = "config.yaml"
	// DefaultProxySOCKSPort 是内核只绑定 127.0.0.1 的 SOCKS 端口；gatewayd 生成的配置与
	// 出站代理 profile 都钉在这个端口上。
	DefaultProxySOCKSPort = 7891
	// DefaultProxyReadyAddr 是就绪探针地址（TCP 可连即就绪）。
	DefaultProxyReadyAddr = "127.0.0.1:7891"
	// ProxyConfigCap 是内核配置正文的上限（订阅几百个节点的配置通常不到 1 MiB）。
	ProxyConfigCap = 4 << 20

	defaultProxyReadyWindow = 30 * time.Second
	proxyConfigTestTimeout  = 30 * time.Second
)

// ErrProxyUserMissing：部署没建 llmgate-proxy 用户。
var ErrProxyUserMissing = errors.New("系统里没有 llmgate-proxy 用户，请先按部署文档创建")

// ProxyStatus 是内核 unit 的引擎侧读数。
type ProxyStatus struct {
	UnitState string `json:"unit_state"`
	SubState  string `json:"sub_state,omitempty"`
	Restarts  int    `json:"restarts"`
	// ConfigPresent 报告运行期配置文件在不在（不含内容）。
	ConfigPresent bool `json:"config_present"`
	// Ready 报告本机 SOCKS 端口此刻可连。
	Ready     bool             `json:"ready"`
	Component *ComponentStatus `json:"component,omitempty"`
}

// proxyStartWire 是 UDS POST /proxy-core/start 的请求体：只在序列化/反序列化的一瞬存在。
type proxyStartWire struct {
	Config string `json:"config"`
}

func (e *Engine) proxyConfigPath() string {
	return filepath.Join(e.opt.ProxyRuntimeDir, ProxyConfigFileName)
}

// ProxyStart 写运行期配置并启动（或按需重启）内核，同步等就绪。配置与上次相同且 unit
// 已在跑时什么都不做——gatewayd 每次开机都会调它一次。返回 (status, err) 且 status 非 nil
// 表示「已启动但未在时限内就绪」。
func (e *Engine) ProxyStart(ctx context.Context, config string) (*ProxyStatus, error) {
	if strings.TrimSpace(config) == "" {
		return nil, errors.New("缺少内核配置")
	}
	if len(config) > ProxyConfigCap {
		return nil, errors.New("内核配置超过长度上限")
	}
	e.compMu.Lock()
	defer e.compMu.Unlock()
	cur := e.currentSlot(ComponentMihomo)
	if cur == "" {
		return nil, ErrComponentMissing
	}
	uid, gid, err := e.opt.ProxyIDs()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.opt.ProxyRuntimeDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建运行期目录失败: %w", err)
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(e.opt.ProxyRuntimeDir, uid, gid)
		_ = os.Chmod(e.opt.ProxyRuntimeDir, 0o700)
	}
	existing, _ := os.ReadFile(e.proxyConfigPath())
	changed := string(existing) != config
	if changed {
		tmp := e.proxyConfigPath() + ".next"
		if err := os.WriteFile(tmp, []byte(config), 0o600); err != nil {
			return nil, errors.New("写入运行期配置失败")
		}
		if os.Geteuid() == 0 {
			if err := os.Chown(tmp, uid, gid); err != nil {
				os.Remove(tmp)
				return nil, errors.New("设置运行期配置属主失败")
			}
		}
		// 内核自检：配置有问题就不动正在跑的实例，把临时文件撤掉。
		if e.opt.ProxyConfigCheck != nil {
			if err := e.opt.ProxyConfigCheck(ctx, e.slotBinary(ComponentMihomo, cur), e.opt.ProxyRuntimeDir, tmp); err != nil {
				os.Remove(tmp)
				return nil, fmt.Errorf("内核配置自检未通过: %w", err)
			}
		}
		if err := os.Rename(tmp, e.proxyConfigPath()); err != nil {
			os.Remove(tmp)
			return nil, errors.New("落位运行期配置失败")
		}
	}
	state, _ := e.sysIsActive(ctx, e.opt.ProxyUnit)
	switch {
	case state == "active" && !changed:
		// 已在跑、配置没变：不动它。
	default:
		if err := e.restartProxyAndWait(ctx); err != nil {
			e.logger.Warn("代理内核未就绪", "err", err.Error())
			return e.proxyStatus(ctx), fmt.Errorf("代理内核已启动但未在时限内就绪: %w", err)
		}
	}
	e.logger.Info("代理内核已启动", "config_replaced", changed)
	return e.proxyStatus(ctx), nil
}

// ProxyStop 停掉内核并清除运行期配置。
func (e *Engine) ProxyStop(ctx context.Context) (*ProxyStatus, error) {
	e.compMu.Lock()
	defer e.compMu.Unlock()
	if e.proxyActive(ctx) {
		if err := e.sysRun(ctx, "stop", e.opt.ProxyUnit); err != nil {
			return nil, fmt.Errorf("停止代理内核失败: %w", err)
		}
	}
	e.removeProxyConfig()
	e.logger.Info("代理内核已停止")
	return e.proxyStatus(ctx), nil
}

// ProxyStatus 汇总内核 unit 的读数。
func (e *Engine) ProxyStatus(ctx context.Context) *ProxyStatus { return e.proxyStatus(ctx) }

func (e *Engine) proxyStatus(ctx context.Context) *ProxyStatus {
	st := &ProxyStatus{Component: e.componentStatus(ComponentMihomo)}
	state, err := e.sysIsActive(ctx, e.opt.ProxyUnit)
	if err != nil || state == "" {
		state = "unknown"
	}
	st.UnitState = state
	if props, err := e.sysShow(ctx, e.opt.ProxyUnit, "SubState", "NRestarts"); err == nil {
		st.SubState = props["SubState"]
		if n, err := strconv.Atoi(props["NRestarts"]); err == nil {
			st.Restarts = n
		}
	}
	if _, err := os.Stat(e.proxyConfigPath()); err == nil {
		st.ConfigPresent = true
	}
	if e.opt.ProxyReady != nil && state == "active" {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st.Ready = e.opt.ProxyReady(pctx) == nil
		cancel()
	}
	return st
}

func (e *Engine) proxyActive(ctx context.Context) bool {
	state, err := e.sysIsActive(ctx, e.opt.ProxyUnit)
	return err == nil && (state == "active" || state == "activating" || state == "reloading")
}

// restartProxyAndWait 重启内核并在就绪窗口内轮询 SOCKS 端口。没有探针时只重启不等。
func (e *Engine) restartProxyAndWait(ctx context.Context) error {
	if err := e.sysRun(ctx, "restart", e.opt.ProxyUnit); err != nil {
		return fmt.Errorf("重启代理内核失败: %w", err)
	}
	if e.opt.ProxyReady == nil {
		return nil
	}
	deadline := e.opt.Now().Add(e.opt.ProxyReadyWindow)
	for {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := e.opt.ProxyReady(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if state, serr := e.sysIsActive(ctx, e.opt.ProxyUnit); serr == nil && state == "failed" {
			return errors.New("内核 unit 进入 failed 状态")
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

func (e *Engine) removeProxyConfig() {
	if err := os.Remove(e.proxyConfigPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.logger.Warn("清除运行期配置失败", "err", err.Error())
	}
}

// LookupProxyUser 返回生产的 uid/gid 解析：root 下必须存在 llmgate-proxy 用户；非 root（dev）
// 用当前身份。
func LookupProxyUser(name string) func() (int, int, error) {
	return func() (int, int, error) {
		if os.Geteuid() != 0 {
			return os.Getuid(), os.Getgid(), nil
		}
		cred, err := lookupCredential(name)
		if err != nil {
			return 0, 0, ErrProxyUserMissing
		}
		return int(cred.Uid), int(cred.Gid), nil
	}
}

// TCPReadyProber 构造「TCP 能连上即就绪」的探针（内核的 127.0.0.1 SOCKS 端口）。
func TCPReadyProber(addr string) HealthProber {
	return func(ctx context.Context) error {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

// RunMihomoConfigTest 返回生产的配置自检：以 proxyUser 身份跑 `<bin> -d <dir> -f <cfg> -t`，
// 最小环境、超时封顶。失败只报「未通过」与退出状态，内核的输出（可能带节点名）不进日志。
func RunMihomoConfigTest(proxyUser string) func(ctx context.Context, bin, dir, cfg string) error {
	return func(ctx context.Context, bin, dir, cfg string) error {
		ctx, cancel := context.WithTimeout(ctx, proxyConfigTestTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-d", dir, "-f", cfg, "-t")
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
		if os.Geteuid() == 0 {
			cred, err := lookupCredential(proxyUser)
			if err != nil {
				return err
			}
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
		}
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("内核退出状态 %d", ee.ExitCode())
			}
			return errors.New("执行失败或超时")
		}
		return nil
	}
}
