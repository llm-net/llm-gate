package mihomo

import (
	"compress/gzip"
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

	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// settings 键名。订阅地址密封（device-key），其余明文且不含秘密。
const (
	settingManifestRevision = "mihomo.manifest_revision"
	settingManifestSHA256   = "mihomo.manifest_sha256"
	settingSubscriptionURL  = "mihomo.subscription_url"
	settingFetchedAt        = "mihomo.subscription_fetched_at"
	settingNodeCount        = "mihomo.subscription_nodes"
	settingDropped          = "mihomo.subscription_dropped"
	settingUserInfo         = "mihomo.subscription_userinfo"
	settingFetchError       = "mihomo.subscription_error"
	settingSelected         = "mihomo.selected_node"
	settingEnabled          = "mihomo.enabled"
	settingLicenseAccepted  = "mihomo.license_accepted"
)

const (
	waitDownloadFor = 60 * time.Second
	downloadBudget  = 20 * time.Minute
	fetchTimeout    = 60 * time.Second
	// uploadCap 是手动上传的请求体上限（gzip 包约 17 MiB）。
	uploadCap = 96 << 20
	// RefreshEvery 是启用中的订阅自动刷新周期。
	RefreshEvery = 24 * time.Hour
)

// 哨兵错误（管理面按它们映射状态码）。
var (
	ErrNoManifest        = errors.New("尚未取得签名组件清单")
	ErrNoAdvisory        = errors.New("当前清单没有本平台可安装的版本")
	ErrDownloadBusy      = errors.New("组件制品正在下载中")
	ErrNoStaged          = errors.New("没有已就绪的组件制品")
	ErrUploadNotListed   = errors.New("上传的文件不是清单里本平台的任何一个官方 gzip 制品")
	ErrComponentMissing  = errors.New("Mihomo 内核尚未安装")
	ErrComponentInUse    = errors.New("内核启用中不能卸载 Mihomo 内核组件，请先在「出站代理」停用")
	ErrNoSubscription    = errors.New("尚未填写 Clash 订阅地址")
	ErrNoNodes           = errors.New("订阅还没有可用节点，请先更新订阅")
	ErrLicenseConsent    = errors.New("启用前必须确认 Mihomo 的 GPL-3.0 许可证与第三方数据路径")
	ErrSubscriptionInUse = errors.New("内核启用中不能清除订阅，请先停用")
	ErrNotEnabled        = errors.New("内置内核未启用")
	ErrProviderNotMihomo = errors.New("出站代理的方式不是「Clash 订阅（设备内置内核）」，请先在代理方式里选择它")
	ErrNodeUnknown       = errors.New("订阅里没有这个节点")
	ErrEngineUnavailable = updated.ErrUnavailable
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
	ProxyStatus(ctx context.Context) (*updated.ProxyStatus, error)
	ProxyStart(ctx context.Context, config string) (*updated.ProxyStatus, error)
	ProxyStop(ctx context.Context) (*updated.ProxyStatus, error)
}

// Egress 是本包对出站策略的全部依赖（生产 *egress.Manager）：内核起来后把 profile 地址
// 指到本机端口、停用时清空；ProviderIsCore 报告当前方式是不是内置内核。
type Egress interface {
	SetCoreAddress(ctx context.Context, addr string, forceDirect bool) error
	ProviderIsCore() bool
}

// Options 装配 Manager。Settings/Engine 必填；Website 可为 nil（离线导入清单与上传制品照常）；
// Fetch 是拉订阅的 HTTP 客户端（出站策略 proxy_subscription 分类），nil 取直连缺省。
type Options struct {
	DataDir  string
	Settings Settings
	Website  Website
	Engine   Engine
	Egress   Egress
	Fetch    *http.Client
	Logger   *slog.Logger
	Platform string
	Keys     map[string]ed25519.PublicKey
	Now      func() time.Time
	// Port 是内核 SOCKS 端口（缺省 updated.DefaultProxySOCKSPort）。
	Port int
}

// Manager 是管理器本体，并发安全。
type Manager struct {
	opt    Options
	logger *slog.Logger

	mu        sync.Mutex
	index     *VerifiedIndex
	source    string
	checkedAt time.Time

	downloading   bool
	downloadError string
	lastError     string
	// latency 是最近一轮节点延迟测试的结果（按节点名），只留内存。
	latency   map[string]NodeLatency
	latencyAt time.Time

	stageMu sync.Mutex
	// opMu 串行化启用/停用/刷新/选节点：它们都要重新生成配置并动内核。
	opMu   sync.Mutex
	autoMu sync.Mutex
	// latencyMu 串行化延迟测试（TryLock，撞上即答忙）。
	latencyMu sync.Mutex
}

// NewManager 装配管理器并加载落盘的清单。
func NewManager(opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Platform == "" {
		opt.Platform = HostPlatform()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Port == 0 {
		opt.Port = updated.DefaultProxySOCKSPort
	}
	if opt.Fetch == nil {
		opt.Fetch = NewFetchClient(nil)
	}
	m := &Manager{opt: opt, logger: opt.Logger.With("srv", "mihomo")}
	m.loadStoredManifest()
	return m
}

// NewFetchClient 构造拉订阅的 HTTP 客户端：不跟随重定向到 https 之外、不读环境代理，经出站
// 策略的 proxy_subscription 分类选路（router 为 nil 恒直连）。
func NewFetchClient(router egress.Router) *http.Client {
	base := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          2,
		IdleConnTimeout:       60 * time.Second,
	}
	return &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" {
				return http.ErrUseLastResponse
			}
			return nil
		},
		Transport: egress.TransportFor(router, egress.ScopeProxySubscription, base),
	}
}

// ---- 路径 ----

func (m *Manager) dir() string             { return filepath.Join(m.opt.DataDir, "components", ComponentName) }
func (m *Manager) manifestPath() string    { return filepath.Join(m.dir(), "manifest.json") }
func (m *Manager) manifestSigPath() string { return filepath.Join(m.dir(), "manifest.sig") }
func (m *Manager) stagedBinPath() string   { return filepath.Join(m.dir(), "staging.bin") }
func (m *Manager) stagedMetaPath() string  { return filepath.Join(m.dir(), "staged.json") }
func (m *Manager) nodesPath() string       { return filepath.Join(m.dir(), "nodes.yaml") }

// Port 返回内核 SOCKS 端口。
func (m *Manager) Port() int { return m.opt.Port }

// CoreAddress 是内核起来后出站代理 profile 里的地址。
func (m *Manager) CoreAddress() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(m.opt.Port))
}

// ---- 清单 ----

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

// ImportManifest 接受管理员手工导入的清单与签名（离线路径），验签与防回退与在线检查相同。
func (m *Manager) ImportManifest(ctx context.Context, raw, sig []byte) (*Advisory, error) {
	return m.accept(ctx, raw, sig, "upload")
}

func (m *Manager) accept(ctx context.Context, raw, sig []byte, source string) (*Advisory, error) {
	v, err := ParseIndex(raw, sig, m.opt.Keys)
	if err != nil {
		return nil, err
	}
	rev, _ := m.opt.Settings.GetSetting(ctx, settingManifestRevision)
	sha, _ := m.opt.Settings.GetSetting(ctx, settingManifestSHA256)
	storedRev, _ := strconv.ParseInt(rev, 10, 64)
	if err := checkRevision(v, storedRev, sha); err != nil {
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

// Staged 描述已就绪（下载/上传完成、解压并通过校验）的组件可执行文件。
type Staged struct {
	Version   string    `json:"version"`
	SHA256    string    `json:"sha256"` // 解压后 ELF 的摘要
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

// Download 从官方 release 下载 advisory.Latest 到 staging（同步等待至多 waitDownloadFor）。
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
	defer os.Remove(tmp)
	return m.stage(tmp, rel, sum, n, "website")
}

// Upload 接收管理员在电脑上下载的官方 gzip 制品：摘要必须命中当前清单本平台的某个条目。
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
	defer os.Remove(tmp)
	_ = os.Chmod(tmp, 0o600)
	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, uploadCap+1))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("接收组件制品失败: %w", err)
	}
	if size > uploadCap {
		return nil, fmt.Errorf("组件制品超过 %d 字节上限", int64(uploadCap))
	}
	if size == 0 {
		return nil, errors.New("组件制品为空")
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	rel := v.Index.findByDigest(m.opt.Platform, sum)
	if rel == nil {
		return nil, ErrUploadNotListed
	}
	if rel.Blocked {
		return nil, errors.New("该版本已被清单阻断，不能安装")
	}
	if err := m.stage(tmp, rel, sum, size, "upload"); err != nil {
		return nil, err
	}
	st, _ := m.StagedManifest()
	return st, nil
}

// stage 是官网下载与手动上传汇合的校验入口：gzip 包长度/摘要精确相等 → 流式解压到
// 解压上限 → 解压后长度/摘要相等 → 本机架构 ELF → 原子落位（staging 文件无执行权限）。
func (m *Manager) stage(gzPath string, rel *Release, sum string, size int64, source string) error {
	if size != rel.SizeBytes {
		return fmt.Errorf("组件制品长度 %d 与清单声明 %d 不符", size, rel.SizeBytes)
	}
	if sum != rel.ArtifactSHA256 {
		return errors.New("组件制品摘要与清单声明不符（下载损坏或来源不对）")
	}
	in, err := os.Open(gzPath)
	if err != nil {
		return fmt.Errorf("读取组件制品失败: %w", err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return errors.New("组件制品不是合法的 gzip 包")
	}
	defer gz.Close()
	unpacked := filepath.Join(m.dir(), "unpack.tmp")
	out, err := os.OpenFile(unpacked, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("创建解压文件失败: %w", err)
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hasher), io.LimitReader(gz, rel.UnpackedSizeBytes+1))
	if cerr := out.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(unpacked)
		return errors.New("解压组件制品失败")
	}
	if n != rel.UnpackedSizeBytes {
		os.Remove(unpacked)
		return fmt.Errorf("解压后长度 %d 与清单声明 %d 不符", n, rel.UnpackedSizeBytes)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != rel.UnpackedSHA256 {
		os.Remove(unpacked)
		return errors.New("解压后摘要与清单声明不符")
	}
	if err := elfcheck.Verify(unpacked); err != nil {
		os.Remove(unpacked)
		return err
	}
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	if err := os.Remove(m.stagedMetaPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.Remove(unpacked)
		return fmt.Errorf("撤下旧制品清单失败: %w", err)
	}
	if err := os.Rename(unpacked, m.stagedBinPath()); err != nil {
		os.Remove(unpacked)
		return fmt.Errorf("组件制品落位失败: %w", err)
	}
	raw, _ := json.Marshal(Staged{Version: rel.Version, SHA256: rel.UnpackedSHA256, SizeBytes: n, Source: source, StagedAt: m.opt.Now()})
	return atomicWrite(m.stagedMetaPath(), raw, 0o600)
}

// StagedManifest 返回已就绪的制品；文件缺失或与清单不符视为没有。
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

// Remove 卸载组件（A/B slot 整目录），订阅与节点选择保留。内核启用中拒绝：卸载前先在
// 「出站代理」停用——「第三方组件」页只管组件本身，从不替管理员停功能。
func (m *Manager) Remove(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if enabled, _ := m.Enabled(ctx); enabled {
		return ErrComponentInUse
	}
	if _, err := m.opt.Engine.ComponentRemove(ctx, ComponentName); err != nil {
		return engineErr(err)
	}
	_ = m.Discard()
	return nil
}

func engineErr(err error) error {
	if err == nil {
		return nil
	}
	var ee *updated.EngineError
	if errors.As(err, &ee) && strings.Contains(ee.Message, updated.ErrComponentMissing.Error()) {
		return ErrComponentMissing
	}
	if errors.Is(err, updated.ErrComponentMissing) {
		return ErrComponentMissing
	}
	return err
}

// ---- 订阅 ----

// SubscriptionStatus 是订阅读数（不含地址与节点凭据）。
type SubscriptionStatus struct {
	Set bool `json:"set"`
	// Host 是订阅地址的主机名（不含路径与令牌），便于管理员认出是哪家。
	Host      string    `json:"host,omitempty"`
	FetchedAt string    `json:"fetched_at,omitempty"`
	NodeCount int       `json:"node_count"`
	Dropped   int       `json:"dropped"`
	UserInfo  *UserInfo `json:"user_info,omitempty"`
	LastError string    `json:"last_error,omitempty" i18n:"text"`
}

// NodeInfo 是节点的公开读数。LatencyMS 是最近一轮测试直连节点服务器的 TCP 连接耗时
// （毫秒，成功至少为 1）；LatencyFailed 表示那轮没连上。两者都是零值 = 还没测过。
type NodeInfo struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	LatencyFailed bool   `json:"latency_failed,omitempty"`
}

// SetSubscription 保存订阅地址（密封）并立即拉取一次。
func (m *Manager) SetSubscription(ctx context.Context, raw string) (*Parsed, error) {
	u, err := ValidateSubscriptionURL(raw)
	if err != nil {
		return nil, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.opt.Settings.SetSealedSetting(ctx, settingSubscriptionURL, u); err != nil {
		return nil, fmt.Errorf("保存订阅地址失败: %w", err)
	}
	return m.refreshLocked(ctx)
}

// Refresh 重新拉取订阅；启用中且节点集变化时重新生成配置并重启内核。
func (m *Manager) Refresh(ctx context.Context) (*Parsed, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.refreshLocked(ctx)
}

func (m *Manager) refreshLocked(ctx context.Context) (*Parsed, error) {
	subURL, err := m.opt.Settings.GetSealedSetting(ctx, settingSubscriptionURL)
	if err != nil {
		return nil, errors.New("订阅地址解封失败（设备密钥被替换或密文损坏），请重新填写")
	}
	if subURL == "" {
		return nil, ErrNoSubscription
	}
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	res, err := fetchSubscription(fctx, m.opt.Fetch, subURL)
	if err != nil {
		_ = m.opt.Settings.SetSetting(ctx, settingFetchError, err.Error())
		m.logger.Warn("订阅拉取失败", "err", err.Error())
		return nil, err
	}
	encoded, err := marshalNodes(res.Parsed.Nodes)
	if err != nil {
		return nil, fmt.Errorf("保存节点失败: %w", err)
	}
	if err := os.MkdirAll(m.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建组件目录失败: %w", err)
	}
	old, _ := os.ReadFile(m.nodesPath())
	changed := string(old) != string(encoded)
	if err := atomicWrite(m.nodesPath(), encoded, 0o600); err != nil {
		return nil, fmt.Errorf("保存节点失败: %w", err)
	}
	_ = m.opt.Settings.SetSetting(ctx, settingFetchedAt, m.opt.Now().UTC().Format(time.RFC3339))
	_ = m.opt.Settings.SetSetting(ctx, settingNodeCount, strconv.Itoa(len(res.Parsed.Nodes)))
	_ = m.opt.Settings.SetSetting(ctx, settingDropped, strconv.Itoa(res.Parsed.Dropped))
	_ = m.opt.Settings.SetSetting(ctx, settingFetchError, "")
	if res.UserInfo != nil {
		raw, _ := json.Marshal(res.UserInfo)
		_ = m.opt.Settings.SetSetting(ctx, settingUserInfo, string(raw))
	} else {
		_ = m.opt.Settings.SetSetting(ctx, settingUserInfo, "")
	}
	m.logger.Info("订阅已更新", "nodes", len(res.Parsed.Nodes), "dropped", res.Parsed.Dropped, "changed", changed)
	// 选定节点若已不在订阅里，退回自动选择。
	if sel, _ := m.opt.Settings.GetSetting(ctx, settingSelected); sel != "" && !hasNode(res.Parsed.Nodes, sel) {
		_ = m.opt.Settings.SetSetting(ctx, settingSelected, "")
	}
	if enabled, _ := m.Enabled(ctx); enabled && changed {
		if err := m.startCoreLocked(ctx); err != nil {
			m.setLastError(err.Error())
			m.logger.Warn("订阅变化后内核重启失败", "err", err.Error())
		}
	}
	return res.Parsed, nil
}

// ClearSubscription 删除订阅地址与节点（内核必须已停用）。
func (m *Manager) ClearSubscription(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if enabled, _ := m.Enabled(ctx); enabled {
		return ErrSubscriptionInUse
	}
	if err := m.opt.Settings.SetSealedSetting(ctx, settingSubscriptionURL, ""); err != nil {
		return err
	}
	for _, k := range []string{settingFetchedAt, settingNodeCount, settingDropped, settingUserInfo, settingFetchError, settingSelected} {
		_ = m.opt.Settings.SetSetting(ctx, k, "")
	}
	if err := os.Remove(m.nodesPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.mu.Lock()
	m.latency = nil
	m.latencyAt = time.Time{}
	m.mu.Unlock()
	return nil
}

// loadNodes 读落盘的节点列表。
func (m *Manager) loadNodes() ([]Node, error) {
	raw, err := os.ReadFile(m.nodesPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	parsed, err := ParseSubscription(raw)
	if err != nil {
		return nil, err
	}
	return parsed.Nodes, nil
}

func hasNode(nodes []Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}

// Nodes 返回节点公开读数（带最近一轮延迟测试结果）。
func (m *Manager) Nodes() []NodeInfo {
	nodes, _ := m.loadNodes()
	m.mu.Lock()
	latency := m.latency
	m.mu.Unlock()
	out := make([]NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		info := NodeInfo{Name: n.Name, Type: n.Type}
		if r, ok := latency[n.Name]; ok {
			info.LatencyMS = r.MS
			info.LatencyFailed = r.Failed
		}
		out = append(out, info)
	}
	return out
}

// SelectNode 选定节点（空 = 自动选择）；启用中即重新生成配置并重启内核。
func (m *Manager) SelectNode(ctx context.Context, name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	name = strings.TrimSpace(name)
	if name != "" {
		nodes, err := m.loadNodes()
		if err != nil {
			return err
		}
		if !hasNode(nodes, name) {
			return ErrNodeUnknown
		}
	}
	if err := m.opt.Settings.SetSetting(ctx, settingSelected, name); err != nil {
		return err
	}
	if enabled, _ := m.Enabled(ctx); enabled {
		return m.startCoreLocked(ctx)
	}
	return nil
}

// ---- 启停 ----

// Enabled 报告管理员是否已启用内置内核。
func (m *Manager) Enabled(ctx context.Context) (bool, error) {
	v, err := m.opt.Settings.GetSetting(ctx, settingEnabled)
	return v == "1", err
}

// LicenseAccepted 报告管理员是否确认过许可证与第三方数据路径。
func (m *Manager) LicenseAccepted(ctx context.Context) bool {
	v, _ := m.opt.Settings.GetSetting(ctx, settingLicenseAccepted)
	return v == "1"
}

// Enable 启用内核：要求出站代理方式已选内置内核、组件已装、有节点、许可证已确认；
// 生成配置交给引擎启动，就绪后把 profile 地址指到本机端口。
func (m *Manager) Enable(ctx context.Context, acceptLicense bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.opt.Egress != nil && !m.opt.Egress.ProviderIsCore() {
		return ErrProviderNotMihomo
	}
	if !acceptLicense && !m.LicenseAccepted(ctx) {
		return ErrLicenseConsent
	}
	if err := m.startCoreLocked(ctx); err != nil {
		return err
	}
	// 确认只在真正启用成功时落库：前置检查失败的那次点击不算「已确认」。
	if acceptLicense {
		if err := m.opt.Settings.SetSetting(ctx, settingLicenseAccepted, "1"); err != nil {
			return err
		}
	}
	if err := m.opt.Settings.SetSetting(ctx, settingEnabled, "1"); err != nil {
		return err
	}
	if m.opt.Egress != nil {
		if err := m.opt.Egress.SetCoreAddress(ctx, m.CoreAddress(), false); err != nil {
			return err
		}
	}
	m.setLastError("")
	m.logger.Info("内置内核已启用", "port", m.opt.Port)
	return nil
}

// startCoreLocked 生成配置并让引擎启动/重启内核。调用方持 opMu。
func (m *Manager) startCoreLocked(ctx context.Context) error {
	if v, ok := m.installedVersion(ctx); !ok {
		return ErrEngineUnavailable
	} else if v == "" {
		return ErrComponentMissing
	}
	nodes, err := m.loadNodes()
	if err != nil {
		return fmt.Errorf("读取节点失败: %w", err)
	}
	if len(nodes) == 0 {
		return ErrNoNodes
	}
	selected, _ := m.opt.Settings.GetSetting(ctx, settingSelected)
	cfg, err := BuildConfig(ConfigOptions{Port: m.opt.Port, Nodes: nodes, Selected: selected})
	if err != nil {
		return err
	}
	if _, err := m.opt.Engine.ProxyStart(ctx, string(cfg)); err != nil {
		return engineErr(err)
	}
	return nil
}

// Disable 停用内核：清空 profile 地址（仍选 proxy 的分类改回直连）、停内核。
func (m *Manager) Disable(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	var errs []error
	if m.opt.Egress != nil {
		if err := m.opt.Egress.SetCoreAddress(ctx, "", true); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.opt.Settings.SetSetting(ctx, settingEnabled, ""); err != nil {
		errs = append(errs, err)
	}
	if _, err := m.opt.Engine.ProxyStop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("停止内核失败：%w", engineErr(err)))
	}
	m.setLastError("")
	m.logger.Info("内置内核已停用")
	return errors.Join(errs...)
}

// Resume 在 gatewayd 启动时恢复已启用的内核（同配置且在跑时引擎不重启）。失败只记
// lastError：profile 地址仍指向本机端口，选了经代理的流量失败关闭直到内核恢复。
func (m *Manager) Resume(ctx context.Context) {
	enabled, err := m.Enabled(ctx)
	if err != nil || !enabled {
		return
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.startCoreLocked(ctx); err != nil {
		m.setLastError(err.Error())
		m.logger.Warn("内置内核未能恢复，经代理的流量将失败关闭", "err", err.Error())
		return
	}
	m.setLastError("")
	m.logger.Info("内置内核已恢复", "port", m.opt.Port)
}

// AutoRefresh 是周期性的订阅刷新入口：只在启用中工作，失败只记日志。
func (m *Manager) AutoRefresh(ctx context.Context) {
	if !m.autoMu.TryLock() {
		return
	}
	defer m.autoMu.Unlock()
	enabled, err := m.Enabled(ctx)
	if err != nil || !enabled {
		return
	}
	if _, err := m.Refresh(ctx); err != nil {
		m.logger.Warn("订阅自动刷新失败", "err", err.Error())
	}
}

func (m *Manager) setLastError(v string) {
	m.mu.Lock()
	m.lastError = v
	m.mu.Unlock()
}

// ---- 读数 ----

// ComponentStatus 是组件层读数（形态与 cloudflared 一致，界面复用同一张卡）。
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
	DownloadError   string    `json:"download_error,omitempty" i18n:"text"`
	WebsiteEnabled  bool      `json:"website_enabled"`
}

// CoreStatus 是内核 unit 读数。
type CoreStatus struct {
	EngineAvailable bool   `json:"engine_available"`
	State           string `json:"state"` // stopped | starting | running | failed | unknown
	UnitState       string `json:"unit_state,omitempty"`
	Restarts        int    `json:"restarts"`
	Ready           bool   `json:"ready"`
}

// Status 是「Clash 订阅（设备内置内核）」的完整读数。
type Status struct {
	Enabled         bool               `json:"enabled"`
	LicenseAccepted bool               `json:"license_accepted"`
	Port            int                `json:"port"`
	Address         string             `json:"address"`
	Platform        string             `json:"platform"`
	Component       ComponentStatus    `json:"component"`
	Core            CoreStatus         `json:"core"`
	Subscription    SubscriptionStatus `json:"subscription"`
	Nodes           []NodeInfo         `json:"nodes"`
	Selected        string             `json:"selected"`
	// LatencyTestedAt 是最近一轮节点延迟测试的时刻（RFC3339；没测过则空）。
	LatencyTestedAt string `json:"latency_tested_at,omitempty"`
	LastError       string `json:"last_error,omitempty" i18n:"text"`
}

// Status 组装读数。
func (m *Manager) Status(ctx context.Context) Status {
	enabled, _ := m.Enabled(ctx)
	st := Status{
		Enabled: enabled, LicenseAccepted: m.LicenseAccepted(ctx), Port: m.opt.Port,
		Address: m.CoreAddress(), Platform: m.opt.Platform, Nodes: m.Nodes(),
	}
	if st.Nodes == nil {
		st.Nodes = []NodeInfo{}
	}
	st.Selected, _ = m.opt.Settings.GetSetting(ctx, settingSelected)
	st.Component = m.componentStatus(ctx)
	st.Core = m.coreStatus(ctx)
	st.Subscription = m.subscriptionStatus(ctx)
	m.mu.Lock()
	st.LastError = m.lastError
	if !m.latencyAt.IsZero() {
		st.LatencyTestedAt = m.latencyAt.UTC().Format(time.RFC3339)
	}
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

func (m *Manager) coreStatus(ctx context.Context) CoreStatus {
	out := CoreStatus{State: "unknown"}
	ps, err := m.opt.Engine.ProxyStatus(ctx)
	if err != nil || ps == nil {
		return out
	}
	out.EngineAvailable = true
	out.UnitState = ps.UnitState
	out.Restarts = ps.Restarts
	out.Ready = ps.Ready
	switch {
	case ps.UnitState == "active" && ps.Ready:
		out.State = "running"
	case ps.UnitState == "active" || ps.UnitState == "activating":
		out.State = "starting"
	case ps.UnitState == "failed":
		out.State = "failed"
	case ps.UnitState == "inactive" || ps.UnitState == "deactivating":
		out.State = "stopped"
	}
	return out
}

func (m *Manager) subscriptionStatus(ctx context.Context) SubscriptionStatus {
	out := SubscriptionStatus{}
	u, err := m.opt.Settings.GetSealedSetting(ctx, settingSubscriptionURL)
	switch {
	case err != nil:
		out.Set = true
		out.LastError = "订阅地址解封失败（设备密钥被替换或密文损坏），请重新填写"
	case u != "":
		out.Set = true
		if parsed, perr := ValidateSubscriptionURL(u); perr == nil {
			out.Host = hostOf(parsed)
		}
	}
	out.FetchedAt, _ = m.opt.Settings.GetSetting(ctx, settingFetchedAt)
	if n, _ := m.opt.Settings.GetSetting(ctx, settingNodeCount); n != "" {
		out.NodeCount, _ = strconv.Atoi(n)
	}
	if n, _ := m.opt.Settings.GetSetting(ctx, settingDropped); n != "" {
		out.Dropped, _ = strconv.Atoi(n)
	}
	if raw, _ := m.opt.Settings.GetSetting(ctx, settingUserInfo); raw != "" {
		var ui UserInfo
		if json.Unmarshal([]byte(raw), &ui) == nil {
			out.UserInfo = &ui
		}
	}
	if out.LastError == "" {
		out.LastError, _ = m.opt.Settings.GetSetting(ctx, settingFetchError)
	}
	return out
}

func hostOf(rawURL string) string {
	if i := strings.Index(rawURL, "://"); i >= 0 {
		rest := rawURL[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	return ""
}

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
