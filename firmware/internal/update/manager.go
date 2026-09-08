// Package update 是 gatewayd 侧的固件升级管理器（docs/firmware-update.md）。
//
// 职责边界：官网发布情报、固件包 staging（官网下载与
// 手动上传**汇合于同一条校验入口**：sha256 + fwimage 形状 + 版本一致）、以及
// 对升级引擎（llmgate updated，UDS）的调用转发。真正动系统的只有引擎；本包全程
// 以非特权身份工作在 <data_dir>/updates 里。
//
// 官网不是设备的运行依赖：官网不可达时手动上传、安装与回退照常可用。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/fwimage"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// Website 是本包对官网静态文件的全部依赖。
type Website interface {
	FirmwareCheck(ctx context.Context) (*officialsite.FirmwareCheckResult, error)
	FetchFirmware(ctx context.Context, artifactURL string, w io.Writer) (string, int64, error)
}

// Engine 是本包对升级引擎的全部依赖（生产实现 *updated.Client；测试注入假）。
type Engine interface {
	Status(ctx context.Context) (*updated.Status, error)
	Install(ctx context.Context, req updated.InstallRequest) (*updated.Status, error)
	Rollback(ctx context.Context) (*updated.Status, error)
}

// 哨兵错误（管理面按它们映射状态码）。
var (
	// ErrNoOffer 表示当前没有已知的可升级版本（先检查更新）。
	ErrNoOffer = errors.New("当前没有可下载的新版本，请先检查更新")
	// ErrDownloadBusy 表示已有一次下载在进行。
	ErrDownloadBusy = errors.New("固件包正在下载中")
	// ErrNoStaged 表示没有已就绪的固件包可安装。
	ErrNoStaged = errors.New("没有已就绪的固件包")
	// ErrNoVersion 表示固件包里读不到构建版本（dev 裸构建或非正式包）。
	ErrNoVersion = errors.New("固件包缺少构建版本信息（正式发布包必须经 make release 构建）")
)

// uploadCap 是手动上传的字节上限，与官网下载上限同一数量级。
const uploadCap = 256 << 20

// waitDownloadFor 是下载动作的同步等待窗：窗口内完成就把结局带回响应，超窗返回「下载中」快照，
// 结果落在状态里、刷新可见——零轮询铁律不破。
const waitDownloadFor = 60 * time.Second

// downloadBudget 是一次下载的总预算（20 MiB 包在慢链路上的十倍余量）。
const downloadBudget = 15 * time.Minute

// Advisory 是最近一次升级情报。
type Advisory struct {
	Latest           *officialsite.FirmwareRelease `json:"latest"`
	Newest           *officialsite.FirmwareRelease `json:"newest"`
	UpgradeAvailable bool                          `json:"upgrade_available"`
	CheckedAt        time.Time                     `json:"checked_at"`
	// Source 是情报来源，当前恒为 website。
	Source string `json:"source"`
}

// StagedManifest 描述已就绪（下载/上传完成且通过校验）的固件包。
type StagedManifest struct {
	Version   string    `json:"version"`
	SHA256    string    `json:"sha256"`
	SizeBytes int64     `json:"size_bytes"`
	Source    string    `json:"source"` // website | upload
	StagedAt  time.Time `json:"staged_at"`
}

// Manager 是升级管理器。经 NewManager 构造；并发安全。
type Manager struct {
	dataDir string
	website Website
	engine  Engine
	logger  *slog.Logger

	// verify 是形状校验注入点（测试换假），生产恒 fwimage.Verify。
	verify func(path string) (*fwimage.Info, error)
	now    func() time.Time

	mu            sync.Mutex
	adv           *Advisory
	downloading   bool
	downloadError string

	// stageMu 串行化 staged 包的落位与丢弃（stage/Discard）：并发的上传与
	// 下载各自写完临时文件后在这里排队，绝不出现「A 的二进制配 B 的清单」。
	stageMu sync.Mutex
}

// NewManager 构造管理器。
func NewManager(dataDir string, website Website, engine Engine, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		dataDir: dataDir,
		website: website,
		engine:  engine,
		logger:  logger.With("srv", "update"),
		verify:  fwimage.Verify,
		now:     time.Now,
	}
}

func (m *Manager) updatesDir() string    { return filepath.Join(m.dataDir, "updates") }
func (m *Manager) stagedBinPath() string { return filepath.Join(m.updatesDir(), "firmware.bin") }
func (m *Manager) manifestPath() string  { return filepath.Join(m.updatesDir(), "manifest.json") }

// ---- advisory ----

// Check 即时向官网查询可升级版本并刷新情报。
func (m *Manager) Check(ctx context.Context) (*Advisory, error) {
	res, err := m.website.FirmwareCheck(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adv = &Advisory{
		Latest:           res.Latest,
		Newest:           res.Newest,
		UpgradeAvailable: res.UpgradeAvailable,
		CheckedAt:        m.now(),
		Source:           "website",
	}
	return m.adv, nil
}

// Advisory 返回最近一次情报（可能为 nil）。
func (m *Manager) Advisory() *Advisory {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adv
}

// ---- staging ----

// Downloading 报告下载在飞与最近一次下载失败原因。
func (m *Manager) Downloading() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloading, m.downloadError
}

// Download 把 advisory 里 Latest 指向的固件包下载到 staging。同步等待至多
// waitDownloadFor；超窗即返，此后以 Downloading/Staged 读进度。
func (m *Manager) Download(ctx context.Context) error {
	m.mu.Lock()
	if m.downloading {
		m.mu.Unlock()
		return ErrDownloadBusy
	}
	adv := m.adv
	if adv == nil || !adv.UpgradeAvailable || adv.Latest == nil {
		m.mu.Unlock()
		return ErrNoOffer
	}
	target := *adv.Latest
	m.downloading = true
	m.downloadError = ""
	m.mu.Unlock()

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
			m.logger.Warn("固件包下载失败", "version", target.Version, "err", err.Error())
		} else {
			m.logger.Info("固件包已就绪", "version", target.Version)
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(waitDownloadFor):
		return nil // 下载继续在后台，状态里可见
	case <-ctx.Done():
		return nil // 请求断开不打断下载
	}
}

// download 执行一次下载 + 校验 + 落位（独立预算，不绑请求生命周期）。
func (m *Manager) download(rel *officialsite.FirmwareRelease) error {
	ctx, cancel := context.WithTimeout(context.Background(), downloadBudget)
	defer cancel()

	if err := os.MkdirAll(m.updatesDir(), 0o700); err != nil {
		return fmt.Errorf("创建下载目录失败: %w", err)
	}
	tmp := filepath.Join(m.updatesDir(), "download.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建下载文件失败: %w", err)
	}
	sum, size, err := m.website.FetchFirmware(ctx, rel.ArtifactURL, f)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	// 完整性检查：官网索引发布的 sha256 必须与下载字节一致。目录行没写摘要的
	// 外链场景不放行——那等于没有可核对的摘要。
	if rel.ArtifactSHA256 == "" {
		os.Remove(tmp)
		return errors.New("发布行缺少固件包摘要")
	}
	if sum != rel.ArtifactSHA256 {
		os.Remove(tmp)
		return errors.New("固件包摘要与发布记录不符（下载损坏或来源不对）")
	}
	if err := m.stage(tmp, rel.Version, sum, size, "website"); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Upload 接收手动上传的固件包（管理台离线升级路径，恒可用）。
func (m *Manager) Upload(r io.Reader) (*StagedManifest, error) {
	if err := os.MkdirAll(m.updatesDir(), 0o700); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}
	// 每次上传独占临时文件（双开页面/连点提交时并发上传不互相搅字节），
	// 落位仍在 stage 里串行。
	f, err := os.CreateTemp(m.updatesDir(), "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("创建上传文件失败: %w", err)
	}
	tmp := f.Name()
	size, err := io.Copy(f, io.LimitReader(r, uploadCap+1))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("接收固件包失败: %w", err)
	}
	if size > uploadCap {
		os.Remove(tmp)
		return nil, fmt.Errorf("固件包超过 %d 字节上限", int64(uploadCap))
	}
	if size == 0 {
		os.Remove(tmp)
		return nil, errors.New("固件包为空")
	}
	sum, err := fileSHA256(tmp)
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("计算固件包摘要失败: %w", err)
	}
	// 上传路径的版本以包内构建信息为准（stage 里校验并读出）。
	if err := m.stage(tmp, "", sum, size, "upload"); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	man, _ := m.Staged()
	return man, nil
}

// stage 是官网下载与手动上传汇合的校验入口：fwimage 形状校验、包内版本必须
// 在场且（声明了的话）与声明一致，然后原子落位为 staged 包。
func (m *Manager) stage(tmpPath, declaredVersion, sum string, size int64, source string) error {
	info, err := m.verify(tmpPath)
	if err != nil {
		return err
	}
	if info.Version == "" {
		return ErrNoVersion
	}
	if declaredVersion != "" && info.Version != declaredVersion {
		return fmt.Errorf("固件包内版本（%s）与发布记录（%s）不符", info.Version, declaredVersion)
	}
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	// 先撤旧清单再换二进制：清单写失败时读到的是「没有 staged 包」，而不是
	// 旧清单配新二进制的错配对（Staged 只核对 SizeBytes，同尺寸时错配会一路
	// 漏到引擎的摘要校验才炸出一个费解的错误）。
	if err := os.Remove(m.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("撤下旧固件包清单失败: %w", err)
	}
	if err := os.Rename(tmpPath, m.stagedBinPath()); err != nil {
		return fmt.Errorf("固件包落位失败: %w", err)
	}
	man := StagedManifest{
		Version:   info.Version,
		SHA256:    sum,
		SizeBytes: size,
		Source:    source,
		StagedAt:  m.now(),
	}
	raw, err := json.Marshal(man)
	if err != nil {
		return fmt.Errorf("写入固件包清单失败: %w", err)
	}
	return atomicWrite(m.manifestPath(), raw, 0o600)
}

// Staged 返回已就绪的固件包清单；文件缺失或与清单不符视为没有（顺手清掉
// 残缺的一半）。
func (m *Manager) Staged() (*StagedManifest, bool) {
	raw, err := os.ReadFile(m.manifestPath())
	if err != nil {
		return nil, false
	}
	var man StagedManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		_ = m.Discard()
		return nil, false
	}
	st, err := os.Stat(m.stagedBinPath())
	if err != nil || st.Size() != man.SizeBytes {
		_ = m.Discard()
		return nil, false
	}
	return &man, true
}

// Discard 丢弃已就绪的固件包。
func (m *Manager) Discard() error {
	m.stageMu.Lock()
	defer m.stageMu.Unlock()
	var errs []error
	for _, p := range []string{m.stagedBinPath(), m.manifestPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- 引擎转发 ----

// Install 请求引擎安装已就绪的固件包。
func (m *Manager) Install(ctx context.Context) (*updated.Status, error) {
	man, ok := m.Staged()
	if !ok {
		return nil, ErrNoStaged
	}
	abs, err := filepath.Abs(m.stagedBinPath())
	if err != nil {
		return nil, fmt.Errorf("解析固件包路径失败: %w", err)
	}
	return m.engine.Install(ctx, updated.InstallRequest{
		Path:    abs,
		Version: man.Version,
		SHA256:  man.SHA256,
	})
}

// Rollback 请求引擎回退到上一版本。
func (m *Manager) Rollback(ctx context.Context) (*updated.Status, error) {
	return m.engine.Rollback(ctx)
}

// EngineStatus 取引擎快照；socket 不可达返回 updated.ErrUnavailable。
func (m *Manager) EngineStatus(ctx context.Context) (*updated.Status, error) {
	return m.engine.Status(ctx)
}

// Reconcile 对齐 staged 与引擎结局：上次安装成功装出去的那份 staged 包已经
// 是死物（引擎不动 gatewayd 的目录，收尾归本侧），看到即清。
func (m *Manager) Reconcile(st *updated.Status) {
	if st == nil || st.LastResult == nil {
		return
	}
	res := st.LastResult
	if res.Kind != updated.KindInstall || res.Outcome != updated.OutcomeSuccess {
		return
	}
	if man, ok := m.Staged(); ok && man.Version == res.ToVersion {
		if err := m.Discard(); err != nil {
			m.logger.Warn("清理已安装的固件包失败", "err", err.Error())
		}
	}
}

// ---- 小工具 ----

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
