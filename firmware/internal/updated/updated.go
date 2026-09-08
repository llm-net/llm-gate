// Package updated 是板上升级引擎（llmgate updated，docs/firmware-update.md）。
//
// 进程与权限模型：llmgate-updated.service 以 root 常驻，监听 UDS
// /run/llmgate-updated/updated.sock（0660 root:llmgate）；gatewayd（非特权 llmgate 用户，
// ProtectSystem=strict 写不了 /usr/local/bin 也 restart 不了服务）是唯一调用
// 方。引擎只认本地文件：下载与形状校验全在 gatewayd 侧完成，引擎安装前对
// 收到的路径**重算 sha256 并重跑 fwimage 校验**，不盲信调用方。
//
// 安装序列（状态机持久化在 <state_dir>/state.json，崩溃可恢复）：
// 校验 → stop gatewayd → 备份 DB + 当前二进制入 prev 槽 → 同分区原子 rename
// 替换 → start → 健康窗口（缺省 90s 内 3 次连续健康）→ success；窗口内失败
// 自动回退：还原 prev 二进制 + 还原 DB 备份（失败的新版本可能跑了一半迁移，
// 还原换确定状态，代价是丢失窗口内 ≤90s 的写入——方向与掉电丢账一致）。
// 手动回退共用同一序列，但**不还原 DB**：数据早已前进，回退只换代码，旧二进
// 制对更高 schema 的容忍由迁移纪律保证（docs/firmware-update.md「版本纪律」）。
//
// §15.1：本包日志与状态文件只含版本号、路径、systemd 单元名与错误类别，
// 没有任何密钥物料过手。
package updated

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
)

// 缺省参数。健康窗口对齐 gatewayd 的启动画像：迁移最多几秒、监听即刻，
// 90s 是十倍余量；3 次连续健康防的是「起来又立刻崩」的假阳性。
const (
	DefaultSocket       = "/run/llmgate-updated/updated.sock"
	DefaultStateDir     = "/var/lib/llmgate-updated"
	DefaultInstallPath  = "/usr/local/bin/llmgate"
	DefaultGatewaydUnit = "llmgate-gatewayd.service"
	DefaultSelfUnit     = "llmgate-updated.service"

	defaultWatchWindow = 90 * time.Second
	defaultOKStreak    = 3
	defaultPollEvery   = 2 * time.Second
	// defaultStartDelay 给调用方（gatewayd）留出把 HTTP 响应发完的时间——
	// 下一步就是把它停掉。
	defaultStartDelay = 1500 * time.Millisecond
)

// 阶段与结局。
const (
	PhaseIdle       = "idle"
	PhaseInstalling = "installing"
	PhaseWatching   = "watching"

	OutcomeSuccess    = "success"
	OutcomeRolledBack = "rolled_back"
	OutcomeFailed     = "failed"

	KindInstall  = "install"
	KindRollback = "rollback"
)

// Systemctl 是引擎对进程管理器的全部依赖（测试注入假实现）。
type Systemctl interface {
	// Run 执行一个 systemctl 动作（stop/start/restart …）。
	Run(ctx context.Context, args ...string) error
	// IsActive 返回单元状态串（active/inactive/failed/activating…）。
	IsActive(ctx context.Context, unit string) (string, error)
	// Show 读单元的若干属性（systemctl show -p …），键为属性名。
	Show(ctx context.Context, unit string, props ...string) (map[string]string, error)
}

// HealthProber 探一次 gatewayd 健康端点；nil 错误即健康。
type HealthProber func(ctx context.Context) error

// Options 装配引擎。零值字段取包缺省；Sys 与 Health 必填（main 装配真实现，
// 测试装假）。
type Options struct {
	StateDir     string
	InstallPath  string
	GatewaydUnit string
	// SelfUnit 是引擎自己的 systemd 单元：二进制换新后 self-restart 用。
	// 空串 = 不自重启（dev/测试）。
	SelfUnit string
	// DataDir 是 gatewayd 的数据目录（DB 备份/还原用）。空串 = 不做 DB
	// 备份（自动回退降级为只还原二进制）。
	DataDir string

	Sys    Systemctl
	Health HealthProber
	Logger *slog.Logger

	WatchWindow time.Duration
	OKStreak    int
	PollEvery   time.Duration
	StartDelay  time.Duration
	Now         func() time.Time

	// 第三方组件与 Cloudflare Tunnel connector（components.go）。零值取包缺省；
	// VerifyComponent / ComponentVersion / TunnelReady 由 main 装真实现，测试装假。
	ComponentsDir    string
	TunnelUnit       string
	TunnelUser       string
	TunnelRuntimeDir string
	// VerifyComponent 是组件文件的形状校验（生产 VerifyComponentFile：本机架构 ELF）。
	VerifyComponent func(path string) error
	// ComponentVersion 读组件的自述版本（生产 RunComponentVersion：以该组件的服务用户
	// 身份跑版本命令）。
	ComponentVersion func(ctx context.Context, name, path string) (string, error)
	// TunnelReady 探 connector 就绪（生产 HTTPReadyProber(DefaultTunnelReadyURL)）；
	// nil = 组件更新后只重启不等就绪。
	TunnelReady       HealthProber
	TunnelReadyWindow time.Duration
	// TunnelIDs 解析 connector 用户的 uid/gid（生产 LookupTunnelUser：root 下查
	// TunnelUser，非 root 用自己）；测试注入固定值。
	TunnelIDs func() (uid, gid int, err error)

	// 板上代理内核（proxycore.go）：unit、服务用户、运行期目录（配置文件所在，tmpfs）、
	// 就绪探针与配置自检。零值取包缺省；ProxyReady / ProxyConfigCheck 由 main 装真实现。
	ProxyUnit       string
	ProxyUser       string
	ProxyRuntimeDir string
	// ProxyReady 探本机 SOCKS 端口是否可连（生产 TCPReadyProber）；nil = 启动后不等就绪。
	ProxyReady       HealthProber
	ProxyReadyWindow time.Duration
	// ProxyConfigCheck 在启动前让内核自检配置（生产 RunMihomoConfigTest：受限身份跑 -t）；
	// nil = 不自检。
	ProxyConfigCheck func(ctx context.Context, bin, dir, cfg string) error
	ProxyIDs         func() (uid, gid int, err error)
}

// Job 是一次在飞的安装/回退（state.json 持久化，崩溃后按它恢复）。
type Job struct {
	Kind        string    `json:"kind"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	SHA256      string    `json:"sha256"`
	SourcePath  string    `json:"source_path"`
	RestoreDB   bool      `json:"restore_db"`
	StartedAt   time.Time `json:"started_at"`
	Deadline    time.Time `json:"deadline"`
}

// Result 是最近一次安装/回退的结局。
type Result struct {
	Kind        string    `json:"kind"`
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	Outcome     string    `json:"outcome"`
	Reason      string    `json:"reason,omitempty" i18n:"text"`
	FinishedAt  time.Time `json:"finished_at"`
}

// Slot 描述 prev 槽里保存的上一版二进制。
type Slot struct {
	Version string    `json:"version"`
	SHA256  string    `json:"sha256"`
	SavedAt time.Time `json:"saved_at"`
}

// state 是 state.json 的全部内容。
type state struct {
	Phase      string  `json:"phase"`
	Job        *Job    `json:"job,omitempty"`
	LastResult *Result `json:"last_result,omitempty"`
}

// Engine 是升级引擎本体。经 New 构造；ResumeIfNeeded 处理崩溃残留。
type Engine struct {
	opt    Options
	logger *slog.Logger

	mu   sync.Mutex
	st   state
	busy bool

	// 安装路径二进制自述版本的备忘（installedVersion）：fwimage.Verify 要把
	// ~20 MB 文件整个扫一遍找版本标记，而 /status 在每次设置页加载都要它。
	// 失效判据取 (size, mtime)——二进制只经 rename 原子替换，两者必变。
	verMu     sync.Mutex
	verSize   int64
	verMTime  time.Time
	verCached string

	// compMu 串行化组件安装/回退/卸载与 connector 启停（components.go）：它们改的
	// 是同一组文件与同一个 unit，与固件升级的单飞位（busy）互不相关。
	compMu sync.Mutex
}

// New 构造引擎并加载持久化状态。stateDir 会被建出（0700）。
func New(opt Options) (*Engine, error) {
	if opt.StateDir == "" {
		opt.StateDir = DefaultStateDir
	}
	if opt.InstallPath == "" {
		opt.InstallPath = DefaultInstallPath
	}
	if opt.GatewaydUnit == "" {
		opt.GatewaydUnit = DefaultGatewaydUnit
	}
	if opt.WatchWindow <= 0 {
		opt.WatchWindow = defaultWatchWindow
	}
	if opt.OKStreak <= 0 {
		opt.OKStreak = defaultOKStreak
	}
	if opt.PollEvery <= 0 {
		opt.PollEvery = defaultPollEvery
	}
	if opt.StartDelay <= 0 {
		opt.StartDelay = defaultStartDelay
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Sys == nil {
		return nil, errors.New("updated: Options.Sys 必填")
	}
	if opt.Health == nil {
		return nil, errors.New("updated: Options.Health 必填")
	}
	if opt.ComponentsDir == "" {
		opt.ComponentsDir = DefaultComponentsDir
	}
	if opt.TunnelUnit == "" {
		opt.TunnelUnit = DefaultTunnelUnit
	}
	if opt.TunnelUser == "" {
		opt.TunnelUser = DefaultTunnelUser
	}
	if opt.TunnelRuntimeDir == "" {
		opt.TunnelRuntimeDir = DefaultTunnelRuntimeDir
	}
	if opt.VerifyComponent == nil {
		opt.VerifyComponent = VerifyComponentFile
	}
	if opt.ProxyUnit == "" {
		opt.ProxyUnit = DefaultProxyUnit
	}
	if opt.ProxyUser == "" {
		opt.ProxyUser = DefaultProxyUser
	}
	if opt.ProxyRuntimeDir == "" {
		opt.ProxyRuntimeDir = DefaultProxyRuntimeDir
	}
	if opt.ComponentVersion == nil {
		opt.ComponentVersion = RunComponentVersion(map[string]string{
			ComponentCloudflared: opt.TunnelUser,
			ComponentMihomo:      opt.ProxyUser,
		})
	}
	if opt.TunnelReadyWindow <= 0 {
		opt.TunnelReadyWindow = defaultTunnelReadyWindow
	}
	if opt.TunnelIDs == nil {
		opt.TunnelIDs = LookupTunnelUser(opt.TunnelUser)
	}
	if opt.ProxyReadyWindow <= 0 {
		opt.ProxyReadyWindow = defaultProxyReadyWindow
	}
	if opt.ProxyIDs == nil {
		opt.ProxyIDs = LookupProxyUser(opt.ProxyUser)
	}
	if err := os.MkdirAll(opt.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建状态目录: %w", err)
	}
	e := &Engine{opt: opt, logger: opt.Logger}
	e.loadState()
	return e, nil
}

// ---- 状态持久化 ----

func (e *Engine) statePath() string { return filepath.Join(e.opt.StateDir, "state.json") }
func (e *Engine) prevBinPath() string {
	return filepath.Join(e.opt.StateDir, "prev", "llmgate")
}
func (e *Engine) prevManifestPath() string {
	return filepath.Join(e.opt.StateDir, "prev", "manifest.json")
}
func (e *Engine) backupDir() string { return filepath.Join(e.opt.StateDir, "backup") }

func (e *Engine) loadState() {
	raw, err := os.ReadFile(e.statePath())
	if err != nil {
		e.st = state{Phase: PhaseIdle}
		return
	}
	if err := json.Unmarshal(raw, &e.st); err != nil {
		e.logger.Warn("状态文件损坏，按空状态处理", "err", err.Error())
		e.st = state{Phase: PhaseIdle}
	}
	if e.st.Phase == "" {
		e.st.Phase = PhaseIdle
	}
}

// persistLocked 原子写 state.json；调用方持锁。
func (e *Engine) persistLocked() {
	raw, err := json.Marshal(e.st)
	if err != nil {
		e.logger.Error("序列化引擎状态失败", "err", err.Error())
		return
	}
	if err := atomicWrite(e.statePath(), raw, 0o600); err != nil {
		e.logger.Error("写入引擎状态失败", "err", err.Error())
	}
}

// ---- 对外读数 ----

// Status 是引擎快照（UDS GET /status 的载荷）。
type Status struct {
	Busy  bool   `json:"busy"`
	Phase string `json:"phase"`
	// InstalledVersion 现读安装路径上的二进制（无 ldflags 版本时为空串）。
	InstalledVersion string  `json:"installed_version"`
	Prev             *Slot   `json:"prev,omitempty"`
	Job              *Job    `json:"job,omitempty"`
	LastResult       *Result `json:"last_result,omitempty"`
}

// Snapshot 汇总当前状态。
func (e *Engine) Snapshot() *Status {
	e.mu.Lock()
	s := &Status{Busy: e.busy, Phase: e.st.Phase, LastResult: e.st.LastResult}
	if e.st.Job != nil {
		// 深拷贝：工作协程会在锁下改在飞任务的字段（如 watch 的 Deadline），
		// 把活指针交给锁外的 JSON 序列化就是数据竞争。LastResult 整体替换、
		// 发布后不再改，共享指针无碍。
		j := *e.st.Job
		s.Job = &j
	}
	e.mu.Unlock()

	s.InstalledVersion = e.installedVersion()
	if slot, err := e.readPrevSlot(); err == nil {
		s.Prev = slot
	}
	return s
}

// installedVersion 读安装路径上二进制的自述版本，(size, mtime) 备忘：
// 值只在引擎自己换二进制（或手工替换）后变化，不必每个 /status 都重扫文件。
// Verify 失败（文件损坏/缺标记）备忘空串，与逐次现读的结果一致。
func (e *Engine) installedVersion() string {
	st, err := os.Stat(e.opt.InstallPath)
	if err != nil {
		return ""
	}
	e.verMu.Lock()
	defer e.verMu.Unlock()
	if e.verSize == st.Size() && e.verMTime.Equal(st.ModTime()) {
		return e.verCached
	}
	version := ""
	if info, err := fwimage.Verify(e.opt.InstallPath); err == nil {
		version = info.Version
	}
	e.verSize, e.verMTime, e.verCached = st.Size(), st.ModTime(), version
	return version
}

func (e *Engine) readPrevSlot() (*Slot, error) {
	raw, err := os.ReadFile(e.prevManifestPath())
	if err != nil {
		return nil, err
	}
	var slot Slot
	if err := json.Unmarshal(raw, &slot); err != nil {
		return nil, err
	}
	if _, err := os.Stat(e.prevBinPath()); err != nil {
		return nil, err
	}
	return &slot, nil
}

// ---- 动作入口 ----

// InstallRequest 是 UDS POST /install 的载荷：gatewayd staging 出来的本地
// 文件路径、声明版本与摘要。引擎重算摘要、重跑形状校验，不盲信。
type InstallRequest struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// ErrBusy 表示已有一次安装/回退在飞。
var ErrBusy = errors.New("已有一次安装或回退正在进行")

// Install 受理一次安装并异步执行。返回即受理（响应先行，随后 gatewayd
// 才会被停掉）；结果落在 LastResult。
func (e *Engine) Install(req InstallRequest) error {
	if req.Path == "" || req.Version == "" || req.SHA256 == "" {
		return errors.New("安装请求缺少 path/version/sha256")
	}
	job := &Job{
		Kind:        KindInstall,
		FromVersion: e.installedVersion(),
		ToVersion:   req.Version,
		SHA256:      req.SHA256,
		SourcePath:  req.Path,
		// 安装失败连 DB 一起还原（刚备份的），除非没有数据目录可备份。
		RestoreDB: e.opt.DataDir != "",
		StartedAt: e.opt.Now(),
	}
	return e.begin(job)
}

// Rollback 受理一次回退到 prev 槽。
//
// 回退源必须先离开 prev 槽：安装序列会把「当前」二进制存进 prev（槽位互换，
// 回退之后还能再升回去），若直接以槽内文件为源，savePrev 会先把它覆盖成
// 当前版本——把要装的旧固件冲掉。所以这里先把槽内文件复制到 scratch 工作
// 文件，任务收尾时删除（engine_test.go 钉着这一幕）。
func (e *Engine) Rollback() error {
	slot, err := e.readPrevSlot()
	if err != nil {
		return errors.New("没有可回退的上一版本")
	}
	// 先占单飞位再做 ~20 MB 的 scratch 复制：升级在飞时连点「回退」不该
	// 每次白拷一份并把残件永久留在 state 目录（拒绝路径上没人清理它），
	// 两个并发 Rollback 同写一个 scratch 路径还会把对方的源文件搅坏。
	if err := e.claim(); err != nil {
		return err
	}
	scratch := filepath.Join(e.opt.StateDir, "rollback-src.bin")
	if err := copyFile(e.prevBinPath(), scratch, 0o755); err != nil {
		e.release()
		return fmt.Errorf("准备回退源文件失败: %w", err)
	}
	job := &Job{
		Kind:        KindRollback,
		FromVersion: e.installedVersion(),
		ToVersion:   slot.Version,
		SHA256:      slot.SHA256,
		SourcePath:  scratch,
		RestoreDB:   false, // 手动回退不动数据：docs/firmware-update.md
		StartedAt:   e.opt.Now(),
	}
	e.start(job)
	return nil
}

// claim 抢占单飞位（不落状态、不起协程）：受理前还有准备工作要做的动作
// （Rollback 的 scratch 复制）先占位，防并发受理互搅。
func (e *Engine) claim() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy {
		return ErrBusy
	}
	e.busy = true
	return nil
}

// release 放弃已抢到的单飞位（claim 之后、start 之前的准备失败时回退）。
func (e *Engine) release() {
	e.mu.Lock()
	e.busy = false
	e.mu.Unlock()
}

// start 持久化 installing 态并起工作协程（调用方必须已 claim 成功）。
func (e *Engine) start(job *Job) {
	e.mu.Lock()
	e.st.Phase = PhaseInstalling
	e.st.Job = job
	e.persistLocked()
	e.mu.Unlock()

	go e.run(job)
}

// begin 单飞受理：置 busy、持久化 installing 态、起工作协程。
func (e *Engine) begin(job *Job) error {
	if err := e.claim(); err != nil {
		return err
	}
	e.start(job)
	return nil
}

// ResumeIfNeeded 处理崩溃残留：watching 未过期就接着看护，过期按当下健康度
// 一次性裁决；installing 残留按「二进制换没换成」决定继续看护还是回退。
// 引擎启动时调用一次。
func (e *Engine) ResumeIfNeeded() {
	e.mu.Lock()
	if e.busy || e.st.Phase == PhaseIdle || e.st.Job == nil {
		if e.st.Phase != PhaseIdle && e.st.Job == nil {
			// 有阶段没任务：残缺状态，归位。
			e.st.Phase = PhaseIdle
			e.persistLocked()
		}
		e.mu.Unlock()
		return
	}
	job := e.st.Job
	phase := e.st.Phase
	e.busy = true
	e.mu.Unlock()

	e.logger.Warn("发现上次未完成的升级任务，恢复处理",
		"phase", phase, "kind", job.Kind, "to", job.ToVersion)
	go e.resume(phase, job)
}

func (e *Engine) resume(phase string, job *Job) {
	ctx := context.Background()
	switch phase {
	case PhaseInstalling:
		// 可能停在换二进制前后的任意一步。以「装上的是不是目标」为准绳：
		// 是 → 拉起并看护；不是 → 按失败路径收拾（还原 prev / DB / 拉起）。
		if sum, err := fileSHA256(e.opt.InstallPath); err == nil && sum == job.SHA256 {
			e.startAndWatch(ctx, job)
			return
		}
		e.fail(ctx, job, "安装中断，二进制未完成替换")
	case PhaseWatching:
		if e.opt.Now().Before(job.Deadline) {
			e.watch(ctx, job)
			return
		}
		// 窗口早已过去：按当下健康度一次性裁决。
		if e.healthyNow(ctx) {
			e.succeed(ctx, job)
		} else {
			e.fail(ctx, job, "重启后健康检查未通过（恢复裁决）")
		}
	default:
		e.finish(job, OutcomeFailed, "未知的残留阶段 "+phase)
	}
}

// ---- 工作序列 ----

func (e *Engine) run(job *Job) {
	ctx := context.Background()
	if e.opt.StartDelay > 0 {
		time.Sleep(e.opt.StartDelay)
	}

	// 1. 复核来源文件：摘要 + 形状。任何不符都在动系统之前拒绝。
	sum, err := fileSHA256(job.SourcePath)
	if err != nil {
		e.finish(job, OutcomeFailed, "读取固件包失败")
		return
	}
	if sum != job.SHA256 {
		e.finish(job, OutcomeFailed, "固件包摘要与声明不符")
		return
	}
	info, err := fwimage.Verify(job.SourcePath)
	if err != nil {
		e.finish(job, OutcomeFailed, err.Error())
		return
	}
	if info.Version != "" && job.ToVersion != "" && info.Version != job.ToVersion {
		e.finish(job, OutcomeFailed, "固件包内版本与声明不符")
		return
	}

	// 2. 停网关。停不下来就不动二进制。
	if err := e.sysRun(ctx, "stop", e.opt.GatewaydUnit); err != nil {
		e.finish(job, OutcomeFailed, "停止网关服务失败")
		return
	}

	// 3. 备份：DB（仅安装）+ 当前二进制入 prev 槽。备份失败 = 没有回退保险，
	// 宁可放弃安装、原样拉起。
	if job.Kind == KindInstall && job.RestoreDB {
		if err := e.backupDB(); err != nil {
			e.logger.Error("备份数据库失败，放弃安装", "err", err.Error())
			_ = e.sysRun(ctx, "start", e.opt.GatewaydUnit)
			e.finish(job, OutcomeFailed, "备份数据库失败，安装未开始")
			return
		}
	}
	if err := e.savePrev(job.FromVersion); err != nil {
		e.logger.Error("保存上一版本失败，放弃", "err", err.Error())
		_ = e.sysRun(ctx, "start", e.opt.GatewaydUnit)
		e.finish(job, OutcomeFailed, "保存上一版本失败，安装未开始")
		return
	}

	// 4. 同分区原子替换。运行中的旧 inode 不受影响（rename 语义），
	// ETXTBSY 是 rm+install 老路的坑，这里没有。
	if err := installFile(job.SourcePath, e.opt.InstallPath); err != nil {
		e.logger.Error("替换二进制失败，回退", "err", err.Error())
		e.fail(ctx, job, "替换二进制失败")
		return
	}

	e.startAndWatch(ctx, job)
}

// startAndWatch 拉起网关并进入健康窗口。
func (e *Engine) startAndWatch(ctx context.Context, job *Job) {
	e.mu.Lock()
	job.Deadline = e.opt.Now().Add(e.opt.WatchWindow)
	e.st.Phase = PhaseWatching
	e.st.Job = job
	e.persistLocked()
	e.mu.Unlock()

	if err := e.sysRun(ctx, "start", e.opt.GatewaydUnit); err != nil {
		e.fail(ctx, job, "启动网关服务失败")
		return
	}
	e.watch(ctx, job)
}

// watch 是健康窗口：连续 OKStreak 次健康即成功；单元 failed 或窗口耗尽即
// 自动回退。
func (e *Engine) watch(ctx context.Context, job *Job) {
	streak := 0
	for {
		if e.opt.Now().After(job.Deadline) {
			e.fail(ctx, job, "健康检查超时")
			return
		}
		if st, err := e.sysIsActive(ctx, e.opt.GatewaydUnit); err == nil && st == "failed" {
			e.fail(ctx, job, "网关服务进入 failed 状态")
			return
		}
		if e.probeOnce(ctx) {
			streak++
			if streak >= e.opt.OKStreak {
				e.succeed(ctx, job)
				return
			}
		} else {
			streak = 0
		}
		time.Sleep(e.opt.PollEvery)
	}
}

func (e *Engine) probeOnce(ctx context.Context) bool {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return e.opt.Health(pctx) == nil
}

func (e *Engine) healthyNow(ctx context.Context) bool {
	if st, err := e.sysIsActive(ctx, e.opt.GatewaydUnit); err != nil || st != "active" {
		return false
	}
	return e.probeOnce(ctx)
}

// succeed 收尾一次成功的安装/回退，并在二进制已换新后 self-restart。
func (e *Engine) succeed(ctx context.Context, job *Job) {
	e.finish(job, OutcomeSuccess, "")
	e.logger.Info("固件升级完成", "kind", job.Kind, "from", job.FromVersion, "to", job.ToVersion)
	if e.opt.SelfUnit != "" {
		// 引擎自己也在那个二进制里：换新后重启自己，让看护逻辑跟上新版。
		// --no-block：先让 systemd 受理，再由它来终止本进程。
		if err := e.sysRun(ctx, "restart", "--no-block", e.opt.SelfUnit); err != nil {
			e.logger.Warn("引擎自重启失败（不影响已完成的升级）", "err", err.Error())
		}
	}
}

// fail 走自动回退：停网关 → 还原 prev 二进制 →（安装场景）还原 DB 备份 →
// 拉起。还原不动的部分记录在案，绝不静默。
func (e *Engine) fail(ctx context.Context, job *Job, reason string) {
	e.logger.Error("升级失败，自动回退", "kind", job.Kind, "to", job.ToVersion, "reason", reason)
	_ = e.sysRun(ctx, "stop", e.opt.GatewaydUnit)

	outcome := OutcomeRolledBack
	if _, err := os.Stat(e.prevBinPath()); err == nil {
		if err := installFile(e.prevBinPath(), e.opt.InstallPath); err != nil {
			e.logger.Error("还原上一版本失败", "err", err.Error())
			outcome = OutcomeFailed
			reason += "；还原上一版本失败"
		}
	} else {
		outcome = OutcomeFailed
		reason += "；没有可还原的上一版本"
	}
	if job.RestoreDB && outcome == OutcomeRolledBack {
		if err := e.restoreDB(); err != nil {
			// 二进制已还原而数据没还原：旧版本对更高 schema 的容忍由迁移
			// 纪律兜底，如实记录即可。
			e.logger.Error("还原数据库备份失败", "err", err.Error())
			reason += "；数据库备份未能还原"
		}
	}
	if err := e.sysRun(ctx, "start", e.opt.GatewaydUnit); err != nil {
		e.logger.Error("回退后拉起网关失败", "err", err.Error())
		reason += "；回退后拉起网关失败"
		outcome = OutcomeFailed
	}
	e.finish(job, outcome, reason)
}

// finish 落盘结局并释放单飞位。
func (e *Engine) finish(job *Job, outcome, reason string) {
	if job.Kind == KindRollback {
		// 回退的 scratch 源文件是终局垃圾（见 Rollback）。
		_ = os.Remove(job.SourcePath)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.Phase = PhaseIdle
	e.st.Job = nil
	e.st.LastResult = &Result{
		Kind:        job.Kind,
		FromVersion: job.FromVersion,
		ToVersion:   job.ToVersion,
		Outcome:     outcome,
		Reason:      reason,
		FinishedAt:  e.opt.Now(),
	}
	e.busy = false
	e.persistLocked()
}

// ---- 备份/还原 ----

// dbFiles 是要随安装备份/还原的 SQLite 三件套。
var dbFiles = []string{"llmgate.db", "llmgate.db-wal", "llmgate.db-shm"}

// backupDB 在网关已停止后拷贝数据库三件套（此刻文件是一致的）。
func (e *Engine) backupDB() error {
	dir := e.backupDir()
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range dbFiles {
		src := filepath.Join(e.opt.DataDir, name)
		if _, err := os.Stat(src); err != nil {
			continue // wal/shm 不在场是常态
		}
		if err := copyFile(src, filepath.Join(dir, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// restoreDB 用备份覆盖数据库三件套；备份里没有的（wal/shm）删掉现场残留。
func (e *Engine) restoreDB() error {
	dir := e.backupDir()
	if _, err := os.Stat(filepath.Join(dir, "llmgate.db")); err != nil {
		return errors.New("没有数据库备份")
	}
	var errs []error
	for _, name := range dbFiles {
		dst := filepath.Join(e.opt.DataDir, name)
		src := filepath.Join(dir, name)
		if _, err := os.Stat(src); err != nil {
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if err := copyFile(src, dst, 0o600); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// savePrev 把当前安装的二进制存进 prev 槽。
func (e *Engine) savePrev(version string) error {
	if err := os.MkdirAll(filepath.Dir(e.prevBinPath()), 0o700); err != nil {
		return err
	}
	sum, err := fileSHA256(e.opt.InstallPath)
	if err != nil {
		return err
	}
	if err := copyFile(e.opt.InstallPath, e.prevBinPath(), 0o755); err != nil {
		return err
	}
	slot := Slot{Version: version, SHA256: sum, SavedAt: e.opt.Now()}
	raw, err := json.Marshal(slot)
	if err != nil {
		return err
	}
	return atomicWrite(e.prevManifestPath(), raw, 0o600)
}

// ---- 文件工具 ----

// installFile 把 src 装到 dst：同目录临时名写入 → fsync → chown root（尽力）
// → rename 原子替换。
func installFile(src, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), ".llmgate.next")
	if err := copyFile(src, tmp, 0o755); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(tmp, 0, 0)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

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

// atomicWrite 通过临时文件写入后 rename，避免留下半份状态。
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

// sysRun / sysIsActive 给每次 systemctl 调用一个独立 30s 预算。
func (e *Engine) sysRun(parent context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return e.opt.Sys.Run(ctx, args...)
}

func (e *Engine) sysIsActive(parent context.Context, unit string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	return e.opt.Sys.IsActive(ctx, unit)
}
