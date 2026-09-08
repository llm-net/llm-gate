// Package cloudflared 是 gatewayd 侧的 Cloudflare Tunnel 公网接入管理器
// （docs-dev/firmware-cloudflare-tunnel.md）。它把四层状态分开管：
//
//   - Component：cloudflared 组件——签名清单、官方直下/手动上传汇合同一条校验、
//     staging，再经升级引擎（llmgate updated，UDS）装进 A/B slot；
//   - Credential：tunnel-scoped token——device-key 密封入库，启用时解封交给引擎写
//     运行期文件；不进 argv、环境变量、日志、审计与 API 响应；
//   - Connector：cloudflared 进程（systemd unit）——引擎启停，本包只读 unit 状态与
//     loopback metrics 的 /ready；
//   - Origin：gatewayd 自己的专用 Unix socket listener（internal/gateway 的 Tunnel
//     listener）——Host/X-Forwarded-Proto/CF-Connecting-IP 语义与路由 profile。
//
// Tunnel 不进入设备健康前置条件：Cloudflare、DNS、token、组件或引擎任一故障只让
// 公网入口不可用；LAN 网关、本地管理台、手动固件上传与回退保持可用。
//
// §15.1：token 明文只出现在 UpdateConfig 的入参、Enable/Resume 交给引擎的那一次
// 调用里；本包日志与错误只含 hostname（管理员主动公布的地址）、版本、错误类别。
package cloudflared

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// 缺省落位。
const (
	// DefaultOriginSocket 是 Cloudflare 回源的专用 Unix socket；Published application
	// 的 Service URL 必须逐字填 unix:/run/llmgate/cloudflare-origin.sock。
	DefaultOriginSocket = "/run/llmgate/cloudflare-origin.sock"
	// DefaultSocketGroup 是 socket 的属组：cloudflared 以该组身份连接。
	DefaultSocketGroup = "llmgate-tunnel"
	// DefaultMetricsReadyURL 是 connector 只绑定 loopback 的就绪探针。
	DefaultMetricsReadyURL = "http://127.0.0.1:20241/ready"

	uploadCap       = maxArtifactBytes
	waitDownloadFor = 60 * time.Second
	downloadBudget  = 15 * time.Minute
	probeTimeout    = 5 * time.Second
	publicTimeout   = 12 * time.Second
)

// 哨兵错误（管理面按它们映射状态码）。
var (
	ErrHostnameRequired  = errors.New("尚未设置 public hostname")
	ErrTokenRequired     = errors.New("尚未设置 tunnel token")
	ErrTokenUnreadable   = errors.New("tunnel token 解封失败（设备密钥被替换或密文损坏），请重新粘贴 token")
	ErrTokenInUse        = errors.New("Tunnel 启用中不能清除 token，请先停用")
	ErrComponentMissing  = errors.New("cloudflared 组件尚未安装")
	ErrComponentInUse    = errors.New("Tunnel 启用中不能卸载 cloudflared 组件，请先在「公网接入」停用")
	ErrAdminGate         = errors.New("管理员口令仍是出厂缺省值，不能把管理面暴露到公网")
	ErrThirdPartyConsent = errors.New("启用前必须确认接受 Cloudflare 的第三方数据路径与组件许可证")
	ErrAdminConsent      = errors.New("把管理面暴露到公网必须二次确认")
	ErrNotEnabled        = errors.New("Cloudflare Tunnel 未启用")
	ErrNoAdvisory        = errors.New("当前没有可安装的组件版本：请先检查更新或导入签名清单")
	ErrNoStaged          = errors.New("没有已就绪的组件制品")
	ErrDownloadBusy      = errors.New("组件制品正在下载中")
	ErrNoManifest        = errors.New("设备上还没有已验证的组件清单")
	ErrUploadNotListed   = errors.New("上传的文件不是清单允许的官方制品（摘要不匹配）")
)

// Settings 是本包对单值配置的全部依赖（生产 *store.Store）。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	GetSealedSetting(ctx context.Context, key string) (string, error)
	SetSealedSetting(ctx context.Context, key, value string) error
}

// Website 是本包对官网的全部依赖（生产 *officialsite.Client）。
type Website interface {
	FetchComponentIndex(ctx context.Context, component string) (index, signature []byte, err error)
	FetchComponentArtifact(ctx context.Context, artifactURL string, pol officialsite.ArtifactPolicy, w io.Writer) (string, int64, error)
}

// Engine 是本包对升级引擎的全部依赖（生产 *updated.Client）。
type Engine interface {
	ComponentStatus(ctx context.Context, name string) (*updated.ComponentStatus, error)
	ComponentInstall(ctx context.Context, name string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error)
	ComponentRollback(ctx context.Context, name string) (*updated.ComponentStatus, error)
	ComponentRemove(ctx context.Context, name string) (*updated.ComponentStatus, error)
	TunnelStatus(ctx context.Context) (*updated.TunnelStatus, error)
	TunnelStart(ctx context.Context, token string) (*updated.TunnelStatus, error)
	TunnelStop(ctx context.Context) (*updated.TunnelStatus, error)
}

// Listener 是本包对网关 Tunnel listener 的全部依赖（生产 *gateway.Server）。
type Listener interface {
	StartTunnel(socket, group string, pol tunnelctx.Policy) error
	StopTunnel() error
	UpdateTunnelPolicy(pol tunnelctx.Policy)
	TunnelListening() bool
	// BeginTunnelProbe / EndTunnelProbe 是公网探测的回执：探测请求带上
	// tunnelctx.ProbeHeader: <nonce>，闸门在 origin socket 上见到即标记；End 报告
	// 它到底有没有经过 socket。这是分辨「Cloudflare 回源填的是 unix socket」与
	// 「填成了 http://localhost:80、绕过闸门」的唯一依据——两种情形 /healthz 都 200。
	BeginTunnelProbe() string
	EndTunnelProbe(nonce string) bool
}

// Options 装配 Manager。Settings/Engine/Listener 必填；Website 可为 nil（官网未
// 装配时在线检查/下载答不可用，手动导入清单与上传制品照常）。
type Options struct {
	DataDir  string
	Settings Settings
	Website  Website
	Engine   Engine
	Listener Listener
	Logger   *slog.Logger

	Socket      string
	SocketGroup string
	// AdminGate 报告此刻允不允许把管理面暴露到公网（生产：口令不是出厂缺省值）。
	// nil = 永不允许。
	AdminGate func(ctx context.Context) bool
	// MetricsReadyURL 是 connector 的 /ready 探针地址。
	MetricsReadyURL string
	Platform        string
	Keys            map[string]ed25519.PublicKey
	Now             func() time.Time
	// PublicClient 是公网探测用的 HTTPS 客户端；nil 取系统信任链的缺省客户端。
	PublicClient *http.Client
}

// Manager 是管理器本体，并发安全。
type Manager struct {
	opt    Options
	logger *slog.Logger

	mu sync.Mutex
	// index 是当前生效（已验签、已防回退）的清单；nil = 还没有。
	index     *VerifiedIndex
	source    string
	checkedAt time.Time

	downloading   bool
	downloadError string
	lastTest      *TestReport
	lastError     string

	stageMu sync.Mutex
	// autoMu 是每日自动更新的单飞锁。
	autoMu   sync.Mutex
	autoWarn bool
}

// NewManager 装配管理器并加载落盘的清单。
func NewManager(opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Socket == "" {
		opt.Socket = DefaultOriginSocket
	}
	if opt.SocketGroup == "" {
		opt.SocketGroup = DefaultSocketGroup
	}
	if opt.MetricsReadyURL == "" {
		opt.MetricsReadyURL = DefaultMetricsReadyURL
	}
	if opt.Platform == "" {
		opt.Platform = HostPlatform()
	}
	if opt.Keys == nil {
		opt.Keys = TrustedKeys()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.PublicClient == nil {
		// 公网探测**有意直连**、不进出站代理策略：它验证的是「从这台设备所在网络能否经
		// 公网到达自己的 Tunnel 主机名」，与 connector（独立进程，本就不走 SOCKS5）同一
		// 视角；经代理探到 200 只会掩盖本地网络的问题。Transport 显式给出，不读环境代理。
		opt.PublicClient = &http.Client{
			Timeout:       publicTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout: 5 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		}
	}
	m := &Manager{opt: opt, logger: opt.Logger.With("srv", "cloudflared")}
	m.loadStoredManifest()
	return m
}

// ---- 路径 ----

func (m *Manager) dir() string             { return filepath.Join(m.opt.DataDir, "components", ComponentName) }
func (m *Manager) manifestPath() string    { return filepath.Join(m.dir(), "manifest.json") }
func (m *Manager) manifestSigPath() string { return filepath.Join(m.dir(), "manifest.sig") }
func (m *Manager) stagedBinPath() string   { return filepath.Join(m.dir(), "staging.bin") }
func (m *Manager) stagedMetaPath() string  { return filepath.Join(m.dir(), "staged.json") }

// Socket 返回 origin socket 路径（界面展示 Service URL 用）。
func (m *Manager) Socket() string { return m.opt.Socket }

// ---- 配置 ----

func (m *Manager) config(ctx context.Context) (Config, error) {
	host, err := m.opt.Settings.GetSetting(ctx, settingHostname)
	if err != nil {
		return Config{}, err
	}
	exp, err := m.opt.Settings.GetSetting(ctx, settingExposure)
	if err != nil {
		return Config{}, err
	}
	auto, err := m.opt.Settings.GetSetting(ctx, settingAutoUpdate)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Hostname: host, Exposure: tunnelctx.Profile(exp), AutoUpdate: auto != "0"}
	if !cfg.Exposure.Valid() {
		cfg.Exposure = tunnelctx.ProfileAPIOnly
	}
	return cfg, nil
}

// Config 返回不含秘密的配置。
func (m *Manager) Config(ctx context.Context) (Config, error) { return m.config(ctx) }

// Enabled 报告 Tunnel 是否已启用（持久化事实，重启后据此恢复）。
func (m *Manager) Enabled(ctx context.Context) (bool, error) {
	v, err := m.opt.Settings.GetSetting(ctx, settingEnabled)
	return v == "1", err
}

// token 解封 tunnel token；未设置回空串、解不开回 ErrTokenUnreadable。
func (m *Manager) token(ctx context.Context) (string, error) {
	t, err := m.opt.Settings.GetSealedSetting(ctx, settingTokenSealed)
	if err != nil {
		return "", ErrTokenUnreadable
	}
	return t, nil
}

// ConfigPatch 是 UpdateConfig 的入参：nil 字段不改；Token 省略即保留、
// ClearToken 才销毁。
type ConfigPatch struct {
	Hostname   *string
	Exposure   *string
	AutoUpdate *bool
	Token      *string
	ClearToken bool
}

// ConfigChange 是 UpdateConfig 的结果摘要（审计用，不含秘密）。
type ConfigChange struct {
	Config        Config
	TokenReplaced bool
	TokenCleared  bool
	Restarted     bool
}

// UpdateConfig 原子性地校验并写入 hostname/exposure/auto-update/token。启用中：
// hostname/exposure 改动即时更新 listener 策略；换 token 让引擎重启 connector；
// 清除 token 被拒（先停用）。
func (m *Manager) UpdateConfig(ctx context.Context, p ConfigPatch) (*ConfigChange, error) {
	cur, err := m.config(ctx)
	if err != nil {
		return nil, err
	}
	next := cur
	if p.Hostname != nil {
		h, err := NormalizeHostname(*p.Hostname)
		if err != nil {
			return nil, err
		}
		next.Hostname = h
	}
	if p.Exposure != nil {
		prof := tunnelctx.Profile(*p.Exposure)
		if !prof.Valid() {
			return nil, invalid("exposure 只能是 api_only 或 api_and_admin")
		}
		next.Exposure = prof
	}
	if p.AutoUpdate != nil {
		next.AutoUpdate = *p.AutoUpdate
	}
	var token string
	if p.Token != nil {
		t, err := ValidateToken(*p.Token)
		if err != nil {
			return nil, err
		}
		token = t
	}
	if p.ClearToken && p.Token != nil {
		return nil, invalid("不能同时提供新 token 与清除 token")
	}
	enabled, err := m.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	if enabled {
		if p.ClearToken {
			return nil, ErrTokenInUse
		}
		if next.Hostname == "" {
			return nil, ErrHostnameRequired
		}
		if next.Exposure == tunnelctx.ProfileAPIAndAdmin && cur.Exposure != tunnelctx.ProfileAPIAndAdmin && !m.adminGate(ctx) {
			return nil, ErrAdminGate
		}
	}
	// 先落库，再动运行态。
	if err := m.opt.Settings.SetSetting(ctx, settingHostname, next.Hostname); err != nil {
		return nil, err
	}
	if err := m.opt.Settings.SetSetting(ctx, settingExposure, string(next.Exposure)); err != nil {
		return nil, err
	}
	if err := m.opt.Settings.SetSetting(ctx, settingAutoUpdate, boolSetting(next.AutoUpdate)); err != nil {
		return nil, err
	}
	change := &ConfigChange{Config: next}
	if token != "" {
		if err := m.opt.Settings.SetSealedSetting(ctx, settingTokenSealed, token); err != nil {
			return nil, err
		}
		change.TokenReplaced = true
	}
	if p.ClearToken {
		if err := m.opt.Settings.SetSetting(ctx, settingTokenSealed, ""); err != nil {
			return nil, err
		}
		change.TokenCleared = true
	}
	if enabled {
		if next.Hostname != cur.Hostname || next.Exposure != cur.Exposure {
			m.opt.Listener.UpdateTunnelPolicy(next.policy())
		}
		if token != "" {
			// 轮换：引擎写新运行期文件并重启 connector（同 token 时引擎不动它）。
			if _, err := m.opt.Engine.TunnelStart(ctx, token); err != nil {
				return change, fmt.Errorf("token 已更新，但 connector 未能按新 token 重启：%w", engineErr(err))
			}
			change.Restarted = true
		}
	}
	return change, nil
}

func boolSetting(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func (m *Manager) adminGate(ctx context.Context) bool {
	return m.opt.AdminGate != nil && m.opt.AdminGate(ctx)
}

// ---- 启用 / 停用 / 删除 / 恢复 ----

// EnableRequest 是启用的两道确认：第三方数据路径与许可证、管理面公网暴露。
type EnableRequest struct {
	AcceptThirdParty     bool
	ConfirmAdminExposure bool
}

// Enable 完成前置检查后依次：起 origin listener → 引擎写 token 并起 connector →
// 落库 enabled。任一步失败回滚已做的一步，不留半开状态。
func (m *Manager) Enable(ctx context.Context, req EnableRequest) error {
	cfg, err := m.config(ctx)
	if err != nil {
		return err
	}
	if cfg.Hostname == "" {
		return ErrHostnameRequired
	}
	token, err := m.token(ctx)
	if err != nil {
		return err
	}
	if token == "" {
		return ErrTokenRequired
	}
	if !req.AcceptThirdParty {
		return ErrThirdPartyConsent
	}
	if cfg.Exposure == tunnelctx.ProfileAPIAndAdmin {
		if !req.ConfirmAdminExposure {
			return ErrAdminConsent
		}
		if !m.adminGate(ctx) {
			return ErrAdminGate
		}
	}
	comp, err := m.opt.Engine.ComponentStatus(ctx, ComponentName)
	if err != nil {
		return engineErr(err)
	}
	if !comp.Installed {
		return ErrComponentMissing
	}
	// origin 先于 connector：不让 connector 把请求送到尚未就绪的 socket。
	if err := m.opt.Listener.StartTunnel(m.opt.Socket, m.opt.SocketGroup, cfg.policy()); err != nil {
		return fmt.Errorf("建立 Tunnel origin socket 失败: %w", err)
	}
	if _, err := m.opt.Engine.TunnelStart(ctx, token); err != nil {
		_ = m.opt.Listener.StopTunnel()
		return engineErr(err)
	}
	if err := m.opt.Settings.SetSetting(ctx, settingEnabled, "1"); err != nil {
		return err
	}
	m.mu.Lock()
	m.lastError = ""
	m.mu.Unlock()
	m.logger.Info("Cloudflare Tunnel 已启用", "hostname", cfg.Hostname, "exposure", string(cfg.Exposure))
	return nil
}

// Disable 停止本机入口：引擎停 connector 并清运行期 token、关闭 origin socket、
// 落库 disabled。keepToken 为假时连密封 token 一并销毁。它不声称已删除
// Cloudflare 侧资源。引擎不可达时本机入口照旧关闭，错误如实带回。
func (m *Manager) Disable(ctx context.Context, keepToken bool) error {
	var errs []error
	if _, err := m.opt.Engine.TunnelStop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("connector 未能停止：%w", engineErr(err)))
	}
	if err := m.opt.Listener.StopTunnel(); err != nil {
		errs = append(errs, err)
	}
	if err := m.opt.Settings.SetSetting(ctx, settingEnabled, "0"); err != nil {
		errs = append(errs, err)
	}
	if !keepToken {
		if err := m.opt.Settings.SetSetting(ctx, settingTokenSealed, ""); err != nil {
			errs = append(errs, err)
		}
	}
	m.logger.Info("Cloudflare Tunnel 已停用", "token_kept", keepToken)
	return errors.Join(errs...)
}

// Delete 销毁本机 Tunnel 配置（含密封 token），可选卸载组件。
func (m *Manager) Delete(ctx context.Context, removeComponent bool) error {
	var errs []error
	if err := m.Disable(ctx, false); err != nil {
		errs = append(errs, err)
	}
	for _, key := range []string{settingHostname, settingExposure, settingAutoUpdate} {
		if err := m.opt.Settings.SetSetting(ctx, key, ""); err != nil {
			errs = append(errs, err)
		}
	}
	if removeComponent {
		if _, err := m.opt.Engine.ComponentRemove(ctx, ComponentName); err != nil {
			errs = append(errs, fmt.Errorf("卸载组件失败：%w", engineErr(err)))
		}
		_ = m.Discard()
	}
	m.mu.Lock()
	m.lastTest = nil
	m.lastError = ""
	m.mu.Unlock()
	m.logger.Info("Cloudflare Tunnel 本机配置已删除", "component_removed", removeComponent)
	return errors.Join(errs...)
}

// RemoveComponent 卸载 cloudflared 组件（A/B slot 整目录），本机 Tunnel 配置与密封 token
// 保留。Tunnel 启用中拒绝：正在承载公网入口的 connector 不能被抽掉，卸载前先在「公网接入」
// 停用——「第三方组件」页只管组件本身，从不替管理员停功能。
func (m *Manager) RemoveComponent(ctx context.Context) error {
	enabled, err := m.Enabled(ctx)
	if err != nil {
		return err
	}
	if enabled {
		return ErrComponentInUse
	}
	if _, err := m.opt.Engine.ComponentRemove(ctx, ComponentName); err != nil {
		return engineErr(err)
	}
	_ = m.Discard()
	m.logger.Info("cloudflared 组件已卸载")
	return nil
}

// Resume 在 gatewayd 启动时恢复已启用的 Tunnel：先起 origin socket，再让引擎
// 确认 connector 在跑（同 token 不重启）。任何失败只记日志与 lastError，不影响
// 网关与本地管理台。
func (m *Manager) Resume(ctx context.Context) {
	enabled, err := m.Enabled(ctx)
	if err != nil || !enabled {
		return
	}
	cfg, err := m.config(ctx)
	if err != nil || cfg.Hostname == "" {
		m.setLastError("config_unreadable")
		m.logger.Warn("Tunnel 已启用但配置读不出，本次不恢复公网入口")
		return
	}
	token, err := m.token(ctx)
	if err != nil || token == "" {
		m.setLastError("credential_unreadable")
		m.logger.Error("Tunnel 已启用但 token 不可用，公网入口未恢复；请重新粘贴 token")
		return
	}
	if err := m.opt.Listener.StartTunnel(m.opt.Socket, m.opt.SocketGroup, cfg.policy()); err != nil {
		m.setLastError("origin_socket_failed")
		m.logger.Error("Tunnel origin socket 建立失败，公网入口未恢复", "err", err.Error())
		return
	}
	if _, err := m.opt.Engine.TunnelStart(ctx, token); err != nil {
		// 引擎不可达时 connector 由 systemd 自己管：上次启动的 unit 可能还在跑。
		m.setLastError("engine_unavailable")
		m.logger.Warn("Tunnel origin 已就绪，但升级引擎未能确认 connector 状态", "err", engineErr(err).Error())
		return
	}
	m.setLastError("")
	m.logger.Info("Cloudflare Tunnel 已恢复", "hostname", cfg.Hostname, "exposure", string(cfg.Exposure))
}

func (m *Manager) setLastError(v string) {
	m.mu.Lock()
	m.lastError = v
	m.mu.Unlock()
}

// engineErr 把引擎侧错误折成本包语义：socket 不可达保留 updated.ErrUnavailable，
// 组件未装映射 ErrComponentMissing，其余原样。
func engineErr(err error) error {
	var ee *updated.EngineError
	if errors.As(err, &ee) && errors.Is(err, updated.ErrComponentMissing) {
		return ErrComponentMissing
	}
	if ee != nil && strings.Contains(ee.Message, updated.ErrComponentMissing.Error()) {
		return ErrComponentMissing
	}
	return err
}

// ---- 清单 ----

// loadStoredManifest 读落盘清单并验签（信任表可能已换：验不过就当没有）。
func (m *Manager) loadStoredManifest() {
	raw, err := os.ReadFile(m.manifestPath())
	if err != nil {
		return
	}
	sig, err := os.ReadFile(m.manifestSigPath())
	if err != nil {
		return
	}
	v, err := ParseIndex(raw, sig, m.opt.Keys)
	if err != nil {
		m.logger.Warn("已存组件清单验证失败，按没有清单处理", "reason", err.Error())
		return
	}
	m.index = v
	m.source = "stored"
}

// Check 匿名读取官网签名清单并接受（验签、防回退、落盘）。
func (m *Manager) Check(ctx context.Context) (*Advisory, error) {
	if m.opt.Website == nil {
		return nil, errors.New("本进程未接入 LLM Gate官网客户端")
	}
	raw, sig, err := m.opt.Website.FetchComponentIndex(ctx, ComponentName)
	if err != nil {
		return nil, err
	}
	return m.accept(ctx, raw, sig, "website")
}

// ImportManifest 接受管理员手工上传的清单与签名（官网不可达时的离线路径），
// 走与在线检查完全相同的验签与防回退。
func (m *Manager) ImportManifest(ctx context.Context, raw, sig []byte) (*Advisory, error) {
	return m.accept(ctx, raw, sig, "upload")
}

func (m *Manager) accept(ctx context.Context, raw, sig []byte, source string) (*Advisory, error) {
	v, err := ParseIndex(raw, sig, m.opt.Keys)
	if err != nil {
		return nil, err
	}
	storedRev, storedSHA, err := m.storedRevision(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkRevision(v, storedRev, storedSHA); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建组件目录失败: %w", err)
	}
	if err := atomicWrite(m.manifestPath(), raw, 0o600); err != nil {
		return nil, fmt.Errorf("保存组件清单失败: %w", err)
	}
	if err := atomicWrite(m.manifestSigPath(), sig, 0o600); err != nil {
		return nil, fmt.Errorf("保存组件清单签名失败: %w", err)
	}
	if err := m.opt.Settings.SetSetting(ctx, settingManifestRevision, strconv.FormatInt(v.Index.Revision, 10)); err != nil {
		return nil, err
	}
	if err := m.opt.Settings.SetSetting(ctx, settingManifestSHA256, v.SHA256); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.index = v
	m.source = source
	m.checkedAt = m.opt.Now()
	m.mu.Unlock()
	adv := m.advisory(ctx)
	m.logger.Info("组件清单已接受", "revision", v.Index.Revision, "source", source,
		"entries", len(v.Index.Releases), "latest", describeRelease(adv.Latest))
	return adv, nil
}

func (m *Manager) storedRevision(ctx context.Context) (int64, string, error) {
	rev, err := m.opt.Settings.GetSetting(ctx, settingManifestRevision)
	if err != nil {
		return 0, "", err
	}
	sha, err := m.opt.Settings.GetSetting(ctx, settingManifestSHA256)
	if err != nil {
		return 0, "", err
	}
	n, _ := strconv.ParseInt(rev, 10, 64)
	return n, sha, nil
}

// installedVersion 问引擎当前装的是哪个版本；引擎不可达回空串与 false。
func (m *Manager) installedVersion(ctx context.Context) (string, bool) {
	st, err := m.opt.Engine.ComponentStatus(ctx, ComponentName)
	if err != nil || st == nil {
		return "", false
	}
	if !st.Installed || st.Current == nil {
		return "", true
	}
	return st.Current.Version, true
}

// advisory 按当前清单与已装版本算安装建议；没有清单回 nil。
func (m *Manager) advisory(ctx context.Context) *Advisory {
	m.mu.Lock()
	v, source, checkedAt := m.index, m.source, m.checkedAt
	m.mu.Unlock()
	if v == nil {
		return nil
	}
	installed, _ := m.installedVersion(ctx)
	adv := v.Index.selectRelease(m.opt.Platform, installed)
	adv.Source = source
	adv.CheckedAt = checkedAt
	return &adv
}

// Advisory 返回当前安装建议（可能为 nil）。
func (m *Manager) Advisory(ctx context.Context) *Advisory { return m.advisory(ctx) }

// ---- staging ----

// Staged 描述已就绪（下载/上传完成且通过校验）的组件制品。
type Staged struct {
	Version   string    `json:"version"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	Source    string    `json:"source"` // website | upload
	StagedAt  time.Time `json:"staged_at"`
}

// Downloading 报告下载在飞与最近一次下载失败原因。
func (m *Manager) Downloading() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloading, m.downloadError
}

// Download 从官方 release 下载 advisory.Latest 到 staging。同步等待至多
// waitDownloadFor；超窗即返，此后以 Downloading/StagedManifest 读进度。
func (m *Manager) Download(ctx context.Context) error {
	if m.opt.Website == nil {
		return errors.New("本进程未接入 LLM Gate官网客户端")
	}
	adv := m.advisory(ctx)
	if adv == nil || adv.Latest == nil {
		return ErrNoAdvisory
	}
	m.mu.Lock()
	if m.downloading {
		m.mu.Unlock()
		return ErrDownloadBusy
	}
	m.downloading = true
	m.downloadError = ""
	m.mu.Unlock()
	target := *adv.Latest

	done := make(chan error, 1)
	go func() {
		err := m.download(&target)
		m.mu.Lock()
		m.downloading = false
		if err != nil {
			m.downloadError = err.Error()
		}
		m.mu.Unlock()
		if err != nil {
			m.logger.Warn("组件制品下载失败", "version", target.Version, "err", err.Error())
		} else {
			m.logger.Info("组件制品已就绪", "version", target.Version)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(waitDownloadFor):
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (m *Manager) download(rel *Release) error {
	ctx, cancel := context.WithTimeout(context.Background(), downloadBudget)
	defer cancel()
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return fmt.Errorf("创建组件目录失败: %w", err)
	}
	tmp := filepath.Join(m.dir(), "download.tmp")
	// staging 文件没有执行权限（0600）：校验完成前它只是一堆字节。
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建下载文件失败: %w", err)
	}
	pol := officialsite.ArtifactPolicy{AllowedHosts: artifactRedirectHosts, MaxBytes: rel.SizeBytes}
	sum, n, err := m.opt.Website.FetchComponentArtifact(ctx, rel.ArtifactURL, pol, f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := m.stage(tmp, rel, sum, n, "website"); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Upload 接收管理员在电脑上下载的官方制品：摘要必须命中当前清单本平台的某个
// 条目——手动上传不是任意版本安装入口。
func (m *Manager) Upload(r io.Reader) (*Staged, error) {
	m.mu.Lock()
	v := m.index
	m.mu.Unlock()
	if v == nil {
		return nil, ErrNoManifest
	}
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建组件目录失败: %w", err)
	}
	f, err := os.CreateTemp(m.dir(), "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建上传文件失败: %w", err)
	}
	tmp := f.Name()
	_ = os.Chmod(tmp, 0o600)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, uploadCap+1))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("接收组件制品失败: %w", err)
	}
	if size > uploadCap {
		os.Remove(tmp)
		return nil, fmt.Errorf("组件制品超过 %d 字节上限", int64(uploadCap))
	}
	if size == 0 {
		os.Remove(tmp)
		return nil, errors.New("组件制品为空")
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	rel := v.Index.findByDigest(m.opt.Platform, sum)
	if rel == nil {
		os.Remove(tmp)
		return nil, ErrUploadNotListed
	}
	if rel.Blocked {
		os.Remove(tmp)
		return nil, errors.New("该版本已被清单阻断，不能安装")
	}
	if err := m.stage(tmp, rel, sum, size, "upload"); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	st, _ := m.StagedManifest()
	return st, nil
}

// stage 是官网下载与手动上传汇合的校验入口：长度精确相等、摘要相等、本机架构
// ELF，然后原子落位。
func (m *Manager) stage(tmpPath string, rel *Release, sum string, size int64, source string) error {
	if size != rel.SizeBytes {
		return fmt.Errorf("组件制品长度 %d 与清单声明 %d 不符", size, rel.SizeBytes)
	}
	if sum != rel.ArtifactSHA256 {
		return errors.New("组件制品摘要与清单声明不符（下载损坏或来源不对）")
	}
	if err := elfcheck.Verify(tmpPath); err != nil {
		return err
	}
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	if err := os.Remove(m.stagedMetaPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("撤下旧制品清单失败: %w", err)
	}
	if err := os.Rename(tmpPath, m.stagedBinPath()); err != nil {
		return fmt.Errorf("组件制品落位失败: %w", err)
	}
	raw, _ := json.Marshal(Staged{Version: rel.Version, SHA256: sum, SizeBytes: size, Source: source, StagedAt: m.opt.Now()})
	return atomicWrite(m.stagedMetaPath(), raw, 0o600)
}

// StagedManifest 返回已就绪的制品；文件缺失或与清单不符视为没有（顺手清掉残缺的一半）。
func (m *Manager) StagedManifest() (*Staged, bool) {
	raw, err := os.ReadFile(m.stagedMetaPath())
	if err != nil {
		return nil, false
	}
	var st Staged
	if err := json.Unmarshal(raw, &st); err != nil {
		_ = m.Discard()
		return nil, false
	}
	info, err := os.Stat(m.stagedBinPath())
	if err != nil || info.Size() != st.SizeBytes {
		_ = m.Discard()
		return nil, false
	}
	return &st, true
}

// Discard 丢弃已就绪的制品。
func (m *Manager) Discard() error {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	var errs []error
	for _, p := range []string{m.stagedBinPath(), m.stagedMetaPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Install 把已就绪制品交给引擎装进非活动 slot（同步等待）。成功即丢弃 staging。
func (m *Manager) Install(ctx context.Context) (*updated.ComponentStatus, error) {
	st, ok := m.StagedManifest()
	if !ok {
		return nil, ErrNoStaged
	}
	abs, err := filepath.Abs(m.stagedBinPath())
	if err != nil {
		return nil, err
	}
	comp, err := m.opt.Engine.ComponentInstall(ctx, ComponentName, updated.ComponentInstallRequest{
		Path: abs, Version: st.Version, SHA256: st.SHA256,
	})
	if err != nil {
		return comp, engineErr(err)
	}
	_ = m.Discard()
	return comp, nil
}

// Rollback 让引擎把组件切回上一个 slot。
func (m *Manager) Rollback(ctx context.Context) (*updated.ComponentStatus, error) {
	st, err := m.opt.Engine.ComponentRollback(ctx, ComponentName)
	return st, engineErr(err)
}

// ---- 自动更新 ----

// AutoUpdate 是每日一次的自动更新入口：只在已启用且 auto_update 打开时工作；
// 检查 → 下载 → 安装（引擎切换时保留旧 slot 并做就绪门）。失败只按翻转记日志，
// 不重试、不影响任何请求路径，也不动固件版本。
func (m *Manager) AutoUpdate(ctx context.Context) {
	enabled, err := m.Enabled(ctx)
	if err != nil || !enabled {
		return
	}
	cfg, err := m.config(ctx)
	if err != nil || !cfg.AutoUpdate {
		return
	}
	if !m.autoMu.TryLock() {
		return
	}
	defer m.autoMu.Unlock()
	reason := ""
	func() {
		adv, err := m.Check(ctx)
		if err != nil {
			reason = "check: " + err.Error()
			return
		}
		if adv.InstalledBlocked {
			m.logger.Warn("已安装的 cloudflared 版本已被清单阻断，请尽快更新")
		}
		if adv.Latest == nil {
			return
		}
		if _, ok := m.StagedManifest(); !ok {
			if err := m.download(adv.Latest); err != nil {
				reason = "download: " + err.Error()
				return
			}
		}
		if _, err := m.Install(ctx); err != nil {
			reason = "install: " + err.Error()
			return
		}
		m.logger.Info("cloudflared 已自动更新", "version", adv.Latest.Version)
	}()
	flipped := reason != "" != m.autoWarn
	m.autoWarn = reason != ""
	if !flipped {
		return
	}
	if reason != "" {
		m.logger.Warn("cloudflared 自动更新未完成（官网与组件不是设备的运行依赖）", "reason", reason)
	} else {
		m.logger.Info("cloudflared 自动更新已恢复")
	}
}

// ---- 状态 ----

// Status 是四层状态的完整读数（§6.3）。
type Status struct {
	Config      Config           `json:"config"`
	Enabled     bool             `json:"enabled"`
	ExternalURL string           `json:"external_url,omitempty"`
	Socket      string           `json:"socket"`
	Platform    string           `json:"platform"`
	Credential  CredentialStatus `json:"credential"`
	Component   ComponentStatus  `json:"component"`
	Connector   ConnectorStatus  `json:"connector"`
	Origin      OriginStatus     `json:"origin"`
	LastTest    *TestReport      `json:"last_test,omitempty"`
	LastError   string           `json:"last_error,omitempty" i18n:"text"`
}

// CredentialStatus 是 token 层：unset / sealed / unreadable。
type CredentialStatus struct {
	State string `json:"state"`
}

// ComponentStatus 是组件层。
type ComponentStatus struct {
	EngineAvailable bool      `json:"engine_available"`
	State           string    `json:"state"`
	Installed       bool      `json:"installed"`
	Version         string    `json:"version,omitempty"`
	Slot            string    `json:"slot,omitempty"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	Staged          *Staged   `json:"staged,omitempty"`
	Advisory        *Advisory `json:"advisory,omitempty"`
	Downloading     bool      `json:"downloading"`
	DownloadError   string    `json:"download_error,omitempty"`
	WebsiteEnabled  bool      `json:"website_enabled"`
}

// ConnectorStatus 是 connector 层：stopped / connecting / connected / degraded / unknown。
type ConnectorStatus struct {
	EngineAvailable  bool   `json:"engine_available"`
	State            string `json:"state"`
	UnitState        string `json:"unit_state,omitempty"`
	Restarts         int    `json:"restarts"`
	ReadyConnections int    `json:"ready_connections"`
}

// OriginStatus 是 origin 层：not_listening / listening / admin_gated。
type OriginStatus struct {
	State        string `json:"state"`
	Listening    bool   `json:"listening"`
	AdminGateOK  bool   `json:"admin_gate_ok"`
	Hostname     string `json:"hostname,omitempty"`
	Exposure     string `json:"exposure,omitempty"`
	SocketExists bool   `json:"socket_exists"`
}

// Status 组装完整读数。任何一层读不到都只降级那一层，不拖垮整份。
func (m *Manager) Status(ctx context.Context) Status {
	cfg, _ := m.config(ctx)
	enabled, _ := m.Enabled(ctx)
	st := Status{Config: cfg, Enabled: enabled, Socket: m.opt.Socket, Platform: m.opt.Platform}
	if enabled {
		st.ExternalURL = cfg.ExternalURL()
	}
	// Credential
	switch t, err := m.token(ctx); {
	case err != nil:
		st.Credential.State = "unreadable"
	case t == "":
		st.Credential.State = "unset"
	default:
		st.Credential.State = "sealed"
	}
	// Component
	st.Component = m.componentStatus(ctx)
	// Connector
	st.Connector = m.connectorStatus(ctx)
	// Origin
	st.Origin = m.originStatus(ctx, cfg)
	m.mu.Lock()
	st.LastTest = m.lastTest
	st.LastError = m.lastError
	m.mu.Unlock()
	return st
}

func (m *Manager) componentStatus(ctx context.Context) ComponentStatus {
	out := ComponentStatus{WebsiteEnabled: m.opt.Website != nil}
	out.Downloading, out.DownloadError = m.Downloading()
	if s, ok := m.StagedManifest(); ok {
		out.Staged = s
	}
	comp, err := m.opt.Engine.ComponentStatus(ctx, ComponentName)
	if err == nil && comp != nil {
		out.EngineAvailable = true
		out.Installed = comp.Installed
		out.Slot = comp.CurrentSlot
		if comp.Current != nil {
			out.Version = comp.Current.Version
		}
		if comp.Previous != nil {
			out.PreviousVersion = comp.Previous.Version
		}
	}
	out.Advisory = m.advisory(ctx)
	switch {
	case !out.EngineAvailable:
		out.State = "engine_unavailable"
	case out.Downloading:
		out.State = "downloading"
	case out.Advisory != nil && out.Advisory.InstalledBlocked:
		out.State = "blocked"
	case out.Staged != nil:
		out.State = "staged"
	case !out.Installed:
		out.State = "not_installed"
	case out.Advisory != nil && out.Advisory.Latest != nil:
		out.State = "update_available"
	default:
		out.State = "installed"
	}
	return out
}

func (m *Manager) connectorStatus(ctx context.Context) ConnectorStatus {
	out := ConnectorStatus{State: "unknown"}
	ts, err := m.opt.Engine.TunnelStatus(ctx)
	if err != nil || ts == nil {
		return out
	}
	out.EngineAvailable = true
	out.UnitState = ts.UnitState
	out.Restarts = ts.Restarts
	if ts.UnitState != "active" && ts.UnitState != "activating" {
		out.State = "stopped"
		return out
	}
	ready, n := m.probeReady(ctx)
	out.ReadyConnections = n
	switch {
	case ready:
		out.State = "connected"
	case ts.Restarts > 0:
		out.State = "degraded"
	default:
		out.State = "connecting"
	}
	return out
}

// probeReady 打一次 connector 的 /ready：200 即至少一条边缘连接就绪。
func (m *Manager) probeReady(ctx context.Context) (bool, int) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, m.opt.MetricsReadyURL, nil)
	if err != nil {
		return false, 0
	}
	// 本机 127.0.0.1 上的 connector metrics：显式直连，不读环境代理。
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	var body struct {
		ReadyConnections int `json:"readyConnections"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
	return resp.StatusCode == http.StatusOK, body.ReadyConnections
}

func (m *Manager) originStatus(ctx context.Context, cfg Config) OriginStatus {
	out := OriginStatus{Hostname: cfg.Hostname, Exposure: string(cfg.Exposure)}
	out.Listening = m.opt.Listener != nil && m.opt.Listener.TunnelListening()
	if _, err := os.Stat(m.opt.Socket); err == nil {
		out.SocketExists = true
	}
	out.AdminGateOK = m.adminGate(ctx)
	switch {
	case !out.Listening:
		out.State = "not_listening"
	case cfg.Exposure == tunnelctx.ProfileAPIAndAdmin && !out.AdminGateOK:
		out.State = "admin_gated"
	default:
		out.State = "listening"
	}
	return out
}

// ---- 自检 ----

// TestCheck 是一层的检查结果：只有类别与一句话，不含请求/响应内容。
type TestCheck struct {
	Layer  string `json:"layer"` // component | connector | origin | public
	OK     bool   `json:"ok"`
	Detail string `json:"detail" i18n:"text"`
}

// TestReport 是一次自检的完整结果。
type TestReport struct {
	At     time.Time   `json:"at"`
	Public bool        `json:"public"`
	OK     bool        `json:"ok"`
	Checks []TestCheck `json:"checks"`
}

// Test 只检查本地 component/process/socket/origin；public 为真时再由设备对
// https://<hostname>/healthz 做一次公网 HTTPS 探测。公网探测失败不回滚任何
// 已验证的本地安装。
func (m *Manager) Test(ctx context.Context, public bool) *TestReport {
	rep := &TestReport{At: m.opt.Now(), Public: public, OK: true}
	add := func(layer string, ok bool, detail string) {
		rep.Checks = append(rep.Checks, TestCheck{Layer: layer, OK: ok, Detail: detail})
		if !ok {
			rep.OK = false
		}
	}
	cfg, _ := m.config(ctx)

	comp, err := m.opt.Engine.ComponentStatus(ctx, ComponentName)
	switch {
	case err != nil:
		add("component", false, "升级引擎不可达，无法读取组件状态")
	case !comp.Installed:
		add("component", false, "cloudflared 尚未安装")
	default:
		add("component", true, "已安装 "+comp.Current.Version)
	}

	ts, err := m.opt.Engine.TunnelStatus(ctx)
	switch {
	case err != nil:
		add("connector", false, "升级引擎不可达，无法读取 connector 状态")
	case ts.UnitState != "active":
		add("connector", false, "connector 未运行（"+ts.UnitState+"）")
	default:
		if ready, n := m.probeReady(ctx); ready {
			add("connector", true, fmt.Sprintf("已连接 Cloudflare 边缘（%d 条连接）", n))
		} else if ts.Restarts > 0 {
			add("connector", false, "connector 反复重启，尚无就绪连接：检查 token 是否有效、UDP/TCP 7844 是否被阻断")
		} else {
			add("connector", false, "connector 正在连接，尚无就绪连接")
		}
	}

	if m.opt.Listener == nil || !m.opt.Listener.TunnelListening() {
		add("origin", false, "origin socket 未监听（Tunnel 未启用）")
	} else if cfg.Hostname == "" {
		add("origin", false, "未设置 hostname")
	} else {
		ok, detail := m.probeOrigin(ctx, cfg.Hostname)
		add("origin", ok, detail)
	}

	if public {
		if cfg.Hostname == "" {
			add("public", false, "未设置 hostname")
		} else {
			ok, detail := m.probePublic(ctx, cfg.Hostname)
			add("public", ok, detail)
		}
	}
	m.mu.Lock()
	m.lastTest = rep
	m.mu.Unlock()
	return rep
}

// probeOrigin 经 socket 自连一次：正确 Host + https 转发头应 200；错 Host 应被
// 拒（421），证明策略真的在挡。
func (m *Manager) probeOrigin(ctx context.Context, hostname string) (bool, string) {
	client := &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", m.opt.Socket)
		}},
	}
	do := func(host string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://origin/healthz", nil)
		if err != nil {
			return 0, err
		}
		req.Host = host
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("CF-Connecting-IP", "198.51.100.1")
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	code, err := do(hostname)
	if err != nil {
		return false, "socket 连不上：" + errCategory(err)
	}
	if code != http.StatusOK {
		return false, fmt.Sprintf("/healthz 经 origin 返回 HTTP %d", code)
	}
	if code, err := do("wrong.invalid"); err == nil && code == http.StatusOK {
		return false, "Host 校验未生效"
	}
	return true, "socket 可达，Host/转发头策略正常"
}

// probePublic 从设备自身对公网 hostname 做一次 HTTPS 探测。它不能代表外网客户端
// 的完整链路（DNS/WAF/Access/套餐都在用户账号里），只作提示。
//
// 请求带一个登记过的 nonce（tunnelctx.ProbeHeader）：/healthz 答 200 但闸门没在
// origin socket 上见到它，说明 Cloudflare 侧的 Service URL 不是 unix socket（多半是
// http://localhost:80）——流量直接打在 LAN listener 上，开放范围、Host 校验与客户端
// IP 审计全都没生效，LAN 上能访问的每条路由（含只限 LAN 的管理功能）都在公网上。
// 这种情形必须报为未通过，不能让「200」冒充验收。
func (m *Manager) probePublic(ctx context.Context, hostname string) (bool, string) {
	pctx, cancel := context.WithTimeout(ctx, publicTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, "https://"+hostname+"/healthz", nil)
	if err != nil {
		return false, "构造请求失败"
	}
	var nonce string
	if m.opt.Listener != nil {
		if nonce = m.opt.Listener.BeginTunnelProbe(); nonce != "" {
			req.Header.Set(tunnelctx.ProbeHeader, nonce)
		}
	}
	resp, err := m.opt.PublicClient.Do(req)
	// 无论成败都取走登记；Do 返回时闸门（若经过）已经标记完毕。
	viaSocket := nonce != "" && m.opt.Listener.EndTunnelProbe(nonce)
	if err != nil {
		return false, "公网探测失败：" + errCategory(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch {
	case resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"ok"`) && nonce != "" && !viaSocket:
		m.logger.Warn("公网探测：请求未经 origin socket 到达，Cloudflare 回源绕过了闸门", "hostname", hostname)
		return false, fmt.Sprintf("https://%s/healthz 返回 200，但请求没有经过 origin socket：Cloudflare 侧 Service URL 不是 unix:%s（多半填成了 http://localhost:80）。这样开放范围、Host 校验与客户端 IP 审计都不生效，局域网里能访问的全部路由（含只限局域网的管理功能）都暴露在公网", hostname, m.opt.Socket)
	case resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"ok"`):
		if viaSocket {
			return true, "https://" + hostname + "/healthz 返回 200，请求经 origin socket 到达，开放范围生效"
		}
		return true, "https://" + hostname + "/healthz 返回 200"
	case resp.StatusCode == 530 || resp.StatusCode == 502 || resp.StatusCode == 503:
		return false, fmt.Sprintf("Cloudflare 返回 HTTP %d：检查 Published application 的 Service URL 是否为 unix:%s、Tunnel 是否 Healthy", resp.StatusCode, m.opt.Socket)
	case resp.StatusCode == http.StatusOK:
		return false, "返回 200 但不是设备的 /healthz（可能被 Access/WAF 页面或缓存拦截）"
	default:
		return false, fmt.Sprintf("公网返回 HTTP %d", resp.StatusCode)
	}
}

// errCategory 把传输错误压成类别（不含 URL、地址或原文）。
func errCategory(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"), strings.Contains(msg, "tls"):
		return "tls_failed"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such file"):
		return "connect_refused"
	default:
		return "connect_failed"
	}
}

// ---- 小工具 ----

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
