package codexappserver

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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// settings 键名（明文，不含秘密）。
const (
	settingManifestRevision = "codex_app_server.manifest_revision"
	settingManifestSHA256   = "codex_app_server.manifest_sha256"
)

const (
	waitDownloadFor = 60 * time.Second
	// downloadBudget 给板子 Wi-Fi 慢链路留足：官方整包 tar.gz 约 94–100 MiB。
	downloadBudget = 30 * time.Minute
	// UploadCap 是手动上传的请求体上限（与清单允许声明的制品上限一致）。
	UploadCap = maxArtifactBytes
)

// 哨兵错误（管理面按它们映射状态码）。
var (
	ErrNoManifest        = errors.New("尚未取得签名组件清单")
	ErrNoAdvisory        = errors.New("当前清单没有本平台可安装的版本")
	ErrDownloadBusy      = errors.New("组件制品正在下载中")
	ErrNoStaged          = errors.New("没有已就绪的组件制品")
	ErrUploadNotListed   = errors.New("上传的文件不是清单里本平台的任何一个官方 tar.gz 制品")
	ErrComponentMissing  = errors.New("Codex App Server 尚未安装")
	ErrComponentInUse    = errors.New("Codex App Server 正在运行中，先结束运行再安装或卸载")
	ErrEngineUnavailable = updated.ErrUnavailable
)

// Settings 是本包对单值配置的全部依赖（生产 *store.Store）。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Website 是本包对官网的全部依赖（生产 *officialsite.Client）。
type Website interface {
	FetchComponentIndex(ctx context.Context, component string) (index, signature []byte, err error)
	FetchComponentArtifact(ctx context.Context, artifactURL string, pol officialsite.ArtifactPolicy, w io.Writer) (string, int64, error)
}

// Engine 是本包对升级引擎的全部依赖（生产 *updated.Client）：只有槽位四件事，没有启停。
type Engine interface {
	ComponentStatus(ctx context.Context, name string) (*updated.ComponentStatus, error)
	ComponentInstall(ctx context.Context, name string, req updated.ComponentInstallRequest) (*updated.ComponentStatus, error)
	ComponentRollback(ctx context.Context, name string) (*updated.ComponentStatus, error)
	ComponentRemove(ctx context.Context, name string) (*updated.ComponentStatus, error)
}

// Options 装配 Manager。Settings/Engine 必填；Website 可为 nil（离线导入清单与上传制品照常）。
type Options struct {
	DataDir  string
	Settings Settings
	Website  Website
	Engine   Engine
	Logger   *slog.Logger
	Platform string
	Keys     map[string]ed25519.PublicKey
	Now      func() time.Time
	// ComponentsDir 是引擎的组件根目录（缺省 updated.DefaultComponentsDir）：运行期从
	// <ComponentsDir>/codex-app-server/current 解析入口（runtime.go）。
	ComponentsDir string
	// ClientVersion 写进 initialize 的 clientInfo.version（缺省 buildinfo.Version）。
	ClientVersion string
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

	stageMu sync.Mutex
	// opMu 串行化安装 / 回退 / 卸载。
	opMu sync.Mutex

	// runMu 保护 running：本进程经 Launch 拉起、尚未 Close 的 app-server 实例（runtime.go）。
	// 有实例在跑时拒绝安装与卸载——两者都会整目录删掉某个槽位，正在跑的实例随后就找不到
	// code-mode host。
	runMu   sync.Mutex
	running map[*Process]struct{}
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
	if opt.ComponentsDir == "" {
		opt.ComponentsDir = updated.DefaultComponentsDir
	}
	if opt.ClientVersion == "" {
		opt.ClientVersion = buildinfo.Version
	}
	m := &Manager{opt: opt, logger: opt.Logger.With("srv", "codex-app-server"), running: map[*Process]struct{}{}}
	m.cleanupTemp()
	m.loadStoredManifest()
	return m
}

// cleanupTemp 清掉上次进程没走完留下的半成品：下载/上传临时文件、解包临时目录、运行期
// scratch home。staging 与清单不动。
func (m *Manager) cleanupTemp() {
	entries, err := os.ReadDir(m.dir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == "download.tmp", strings.HasPrefix(name, "upload-") && strings.HasSuffix(name, ".tmp"),
			name == "unpack.tmp", strings.HasPrefix(name, "unpack-"):
			_ = os.RemoveAll(filepath.Join(m.dir(), name))
		}
	}
	_ = os.RemoveAll(m.homesDir())
}

// ---- 路径 ----

func (m *Manager) dir() string             { return filepath.Join(m.opt.DataDir, "components", ComponentName) }
func (m *Manager) manifestPath() string    { return filepath.Join(m.dir(), "manifest.json") }
func (m *Manager) manifestSigPath() string { return filepath.Join(m.dir(), "manifest.sig") }
func (m *Manager) stagedMetaPath() string  { return filepath.Join(m.dir(), "staged.json") }

// stagedDir 是就绪制品的目录树（整包布局，见包注释）；stagedEntryPath 是树里的入口文件。
func (m *Manager) stagedDir() string { return filepath.Join(m.dir(), "staging") }
func (m *Manager) stagedEntryPath() string {
	return filepath.Join(m.stagedDir(), filepath.FromSlash(EntrypointPath))
}

// homesDir 放运行期实例各自的 CODEX_HOME（runtime.go）：在 DataDir 下而不是 /tmp——app-server
// 拒绝在临时目录里落 helper 二进制。
func (m *Manager) homesDir() string { return filepath.Join(m.dir(), "homes") }

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

// Staged 描述已就绪（下载/上传完成、解包并通过校验）的组件目录树。SHA256 / SizeBytes 说的
// 是入口文件 bin/codex-app-server；TotalBytes / Files 是整棵树。
type Staged struct {
	Version    string    `json:"version"`
	SHA256     string    `json:"sha256"` // 入口文件的摘要
	SizeBytes  int64     `json:"size_bytes"`
	TotalBytes int64     `json:"total_bytes,omitempty"`
	Files      int       `json:"files,omitempty"`
	Source     string    `json:"source"` // website | upload
	StagedAt   time.Time `json:"staged_at"`
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

// Upload 接收管理员在电脑上下载的官方 tar.gz 制品：摘要必须命中当前清单本平台的某个条目。
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
	size, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(r, UploadCap+1))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("接收组件制品失败: %w", err)
	}
	if size > UploadCap {
		return nil, fmt.Errorf("组件制品超过 %d 字节上限", int64(UploadCap))
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

// stage 是官网下载与手动上传汇合的校验入口：tar.gz 包长度/摘要精确相等 → 按官方包布局流式
// 解到临时目录（extractPackage 同时做路径、类型、总量、ELF 与包自述校验）→ 入口文件长度/摘要
// 与清单相等 → 原子换成 staging 目录（目录 0700，文件只对本进程可读）。
func (m *Manager) stage(tgzPath string, rel *Release, sum string, size int64, source string) error {
	if size != rel.SizeBytes {
		return fmt.Errorf("组件制品长度 %d 与清单声明 %d 不符", size, rel.SizeBytes)
	}
	if sum != rel.ArtifactSHA256 {
		return errors.New("组件制品摘要与清单声明不符（下载损坏或来源不对）")
	}
	// 每次解包用独立临时目录：官网下载（后台 goroutine）与手动上传可能同时在跑。
	unpacked, err := os.MkdirTemp(m.dir(), "unpack-")
	if err != nil {
		return fmt.Errorf("创建解包目录失败: %w", err)
	}
	res, err := extractPackage(tgzPath, unpacked, rel.Version, rel.UnpackedSizeBytes)
	if err != nil {
		os.RemoveAll(unpacked)
		return err
	}
	if res.EntrySize != rel.UnpackedSizeBytes {
		os.RemoveAll(unpacked)
		return fmt.Errorf("解包后长度 %d 与清单声明 %d 不符", res.EntrySize, rel.UnpackedSizeBytes)
	}
	if res.EntrySHA256 != rel.UnpackedSHA256 {
		os.RemoveAll(unpacked)
		return errors.New("解包后摘要与清单声明不符")
	}
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	if err := os.Remove(m.stagedMetaPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		os.RemoveAll(unpacked)
		return fmt.Errorf("撤下旧制品清单失败: %w", err)
	}
	if err := os.RemoveAll(m.stagedDir()); err != nil {
		os.RemoveAll(unpacked)
		return fmt.Errorf("撤下旧制品失败: %w", err)
	}
	if err := os.Rename(unpacked, m.stagedDir()); err != nil {
		os.RemoveAll(unpacked)
		return fmt.Errorf("组件制品落位失败: %w", err)
	}
	raw, _ := json.Marshal(Staged{Version: rel.Version, SHA256: rel.UnpackedSHA256, SizeBytes: res.EntrySize,
		TotalBytes: res.TotalBytes, Files: res.Files, Source: source, StagedAt: m.opt.Now()})
	return atomicWrite(m.stagedMetaPath(), raw, 0o600)
}

// StagedManifest 返回已就绪的制品；文件缺失或与清单不符视为没有。
func (m *Manager) StagedManifest() (*Staged, bool) {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	return m.stagedManifestLocked()
}

func (m *Manager) stagedManifestLocked() (*Staged, bool) {
	raw, err := os.ReadFile(m.stagedMetaPath())
	if err != nil {
		return nil, false
	}
	var st Staged
	if err := json.Unmarshal(raw, &st); err != nil {
		_ = m.discardLocked()
		return nil, false
	}
	info, err := os.Stat(m.stagedEntryPath())
	if err != nil || !info.Mode().IsRegular() || info.Size() != st.SizeBytes {
		_ = m.discardLocked()
		return nil, false
	}
	return &st, true
}

// Discard 丢弃已就绪的制品。
func (m *Manager) Discard() error {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	return m.discardLocked()
}

func (m *Manager) discardLocked() error {
	var errs []error
	if err := os.Remove(m.stagedMetaPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := os.RemoveAll(m.stagedDir()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Install 把已就绪的目录树交给引擎装进非活动 slot（同步等待）：请求的 Path 是 staging 目录，
// 引擎复核其中 bin/codex-app-server 的摘要、ELF 与自述版本后整棵拷进槽位。引擎拷贝期间持有
// stageMu，新的下载/上传要等它结束才能换掉 staging。成功即丢弃 staging。有运行中的实例时拒绝
// （ErrComponentInUse）：安装会整目录清掉非活动槽位，那可能正是某个实例在用的。
func (m *Manager) Install(ctx context.Context) (*updated.ComponentStatus, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.RunningCount() > 0 {
		return nil, ErrComponentInUse
	}
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	st, ok := m.stagedManifestLocked()
	if !ok {
		return nil, ErrNoStaged
	}
	abs, err := filepath.Abs(m.stagedDir())
	if err != nil {
		return nil, err
	}
	comp, err := m.opt.Engine.ComponentInstall(ctx, ComponentName, updated.ComponentInstallRequest{
		Path: abs, Version: st.Version, SHA256: st.SHA256,
	})
	if err != nil {
		return comp, engineErr(err)
	}
	_ = m.discardLocked()
	return comp, nil
}

// Rollback 让引擎把组件切回上一个 slot。
func (m *Manager) Rollback(ctx context.Context) (*updated.ComponentStatus, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	st, err := m.opt.Engine.ComponentRollback(ctx, ComponentName)
	return st, engineErr(err)
}

// Remove 卸载组件（A/B slot 整目录）并丢弃已就绪制品。该组件没有「启用中」的功能开关，唯一
// 守卫是本进程有运行中的实例（ErrComponentInUse）；已接受的签名清单保留。
func (m *Manager) Remove(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.RunningCount() > 0 {
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

// ---- 读数 ----

// ComponentStatus 是组件层读数（形态与 cloudflared / Mihomo 一致，界面复用同一张卡）。
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
	// Runtime 是运行期读数（current 解析出的入口路径、布局是否完整、运行中实例数），见 runtime.go。
	Runtime *RuntimeInfo `json:"runtime"`
}

// Status 组装组件读数。
func (m *Manager) Status(ctx context.Context) ComponentStatus {
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
	info := m.RuntimeInfo()
	out.Runtime = &info
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
