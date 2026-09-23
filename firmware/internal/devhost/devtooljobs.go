package devhost

// 开发工具动作队列：「工具配置」页对 gate 管的开发工具做的安装 / 升级 / 解除关联 / 卸载
// 不再在一条 HTTP 请求里同步等完（Cursor 的整树要下载几分钟，页面一关请求就断、gate
// 被 SIGHUP），而是排进这台主机的队列由设备后台逐个执行：
//
//   - 每台主机一条队列、一次只跑一个动作（同一台主机上并发跑两个 gate 会抢 config.json）；
//     不同主机各自的队列互不等待。
//   - 队列只在内存里：进程重启即清空（正在跑的 SSH 会话也随进程没了），页面看到的就是
//     还没跑完的那几项加上最近完成的一段历史（每台主机最多留 devToolHistory 条）。
//   - 每个动作的进度来自主机上 gate 的输出：阶段（连接 / 探测 / 执行 / 复核）、gate 最近
//     吐的一行与从进度行里读出的百分比（`… 12.3 MB / 45.6 MB  27%`）；输出里没有密钥
//     （gate 的生命周期子命令不打印它，§15.1）。
//   - 动作跑完后顺手把复核那次探测留在队列里（Tools），页面据此换读数、不必再探一次。
//   - 页面用 WaitDevToolJobs 陪等（revision 变化或 WaitFor 即回一帧），离开页面只是不再
//     陪等，队列照跑；回来先读一次快照再接着陪等。
//
// 同一个工具同一种动作已在排队 / 执行中时再入队即忽略（幂等）；取消只对排队与执行中的
// 有效——执行中的取消是关掉那条 SSH 会话（gate 支持续传，之后重来不用从头下载）。
//
// 「安装 gate」本身也走这条队列（Tool = GateJobTool、Action = install，EnqueueGateInstall）：
// 安装脚本要从设备下载压缩包并验证地址与 Key，页面不该卡在对话框里等它。地址与 Key 明文只
// 在队列的 secrets 表里放到动作开始执行（不进 DevToolJob、不进快照，§15.1），跑完即删；
// 同一台主机已有排队 / 执行中的安装时再入队同样忽略。

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devd"
)

// 动作的状态。
const (
	DevToolJobQueued    = "queued"
	DevToolJobRunning   = "running"
	DevToolJobSucceeded = "succeeded"
	DevToolJobFailed    = "failed"
	DevToolJobCancelled = "cancelled"
)

const (
	// devToolHistory 是每台主机保留的已结束动作条数。
	devToolHistory = 20
	// DevToolWaitFor 是陪等一帧的上限。
	DevToolWaitFor = 30 * time.Second
	// devToolTail 是执行中与结束后各保留的输出行数（gate 最近的几行）。
	devToolTail = 8
)

// GateJobTool 是队列里「安装 gate」那种动作的 Tool 值（不在 devd.DevTools 里，页面据此把它
// 摆在 gate 卡片下而不是开发工具清单里）。
const GateJobTool = "gate"

// DevToolSpec 是要排进队列的一个动作。RemoteIP 是发起者的地址，只给结束时的审计用。
type DevToolSpec struct {
	Tool     string
	Action   DevToolAction
	RemoteIP string
}

// GateInstallSpec 是要排进队列的一次「安装 gate」：BaseURL / Key 同 GateInstallRequest（Key
// 用完即弃，不进快照）；KeyLabel 是所选密钥的标签，只给结束时的审计用（明文不进审计）。
type GateInstallSpec struct {
	BaseURL  string
	Key      string
	KeyLabel string
	RemoteIP string
}

// toolJobSecret 暂存安装 gate 的地址 / Key 或创作工具的 sudo 口令，不进快照。
type toolJobSecret struct {
	base     string
	key      string
	password string
}

func (toolJobSecret) String() string               { return "[REDACTED]" }
func (toolJobSecret) GoString() string             { return "[REDACTED]" }
func (toolJobSecret) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }
func (toolJobSecret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// DevToolJob 是队列里一个动作的快照。
type DevToolJob struct {
	ID     int64
	HostID int64
	Tool   string
	Action DevToolAction
	Status string
	// Stage 只在执行中有意义：connecting / probing / running / verifying。
	Stage string
	// Percent 是从 gate 进度行里读出的百分比；-1 = 还没有。
	Percent int
	// Line 是 gate 最近吐的一行；Output 是最近几行（结束后是结尾几行）。
	Line   string
	Output string
	// Error / ErrorCode 只在失败时有。
	Error     string
	ErrorCode string
	// Version 是成功后复核到的该工具版本（关联 / 安装 / 升级之后；安装 gate 是 gate 的版本）。
	Version string
	// KeyLabel 只在「安装 gate」且选了密钥时有：所选密钥的标签（审计用，明文不在这里）。
	KeyLabel   string
	RemoteIP   string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// Done 报告动作已结束（成功、失败或已取消）。
func (j DevToolJob) Done() bool {
	return j.Status == DevToolJobSucceeded || j.Status == DevToolJobFailed || j.Status == DevToolJobCancelled
}

// DevToolQueueState 是一台主机队列的一帧：Revision 每次变化递增；Jobs 按入队顺序（含
// 历史）；Tools 是最近一次动作结束时的复核读数（没跑过任何动作时为 nil）。
type DevToolQueueState struct {
	Revision int64
	Jobs     []DevToolJob
	Tools    *Tools
	ToolsAt  time.Time
}

// devToolQueue 是一台主机的队列。
type devToolQueue struct {
	mu       sync.Mutex
	jobs     []*DevToolJob
	tail     map[int64][]string
	revision int64
	notify   chan struct{}
	running  bool
	cancel   context.CancelFunc
	current  int64
	// aborted 记着执行中的那个动作被取消过：结束时据此标 cancelled 而不是 failed。
	aborted bool
	tools   *Tools
	toolsAt time.Time
	// secrets 按动作 id 暂存地址 / Key 或 sudo 口令，开始执行或取消排队时取走。
	secrets map[int64]toolJobSecret
}

func (q *devToolQueue) bump() {
	q.revision++
	close(q.notify)
	q.notify = make(chan struct{})
}

func (q *devToolQueue) snapshot() DevToolQueueState {
	st := DevToolQueueState{Revision: q.revision, Jobs: make([]DevToolJob, 0, len(q.jobs)), Tools: q.tools, ToolsAt: q.toolsAt}
	for _, j := range q.jobs {
		st.Jobs = append(st.Jobs, *j)
	}
	return st
}

// DevToolHook 在一个动作结束时被调用（成功、失败、取消都算）；管理面用它记审计。
type DevToolHook func(job DevToolJob, tools *Tools)

// SetDevToolHook 登记动作结束的回调。
func (m *Manager) SetDevToolHook(h DevToolHook) {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	m.jobHook = h
}

func (m *Manager) queueFor(hostID int64) *devToolQueue {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if m.jobs == nil {
		m.jobs = map[int64]*devToolQueue{}
	}
	q, ok := m.jobs[hostID]
	if !ok {
		q = &devToolQueue{notify: make(chan struct{}), tail: map[int64][]string{}, secrets: map[int64]toolJobSecret{}}
		m.jobs[hostID] = q
	}
	return q
}

// EnqueueDevTools 把若干动作排进这台主机的队列并立即返回队列快照。工具名 / 动作不在词汇表
// 答 invalid_host（整批都不入队）；主机不存在 / 受控纳管按 workerHost 的错误回；同一个工具
// 同一种动作已在排队或执行中的跳过。
func (m *Manager) EnqueueDevTools(ctx context.Context, hostID int64, specs []DevToolSpec) (DevToolQueueState, error) {
	return m.EnqueueToolJobs(ctx, hostID, specs, "")
}

// EnqueueToolJobs also accepts studio/<name> install jobs. The sudo password is
// held separately from public job metadata and removed when execution starts.
func (m *Manager) EnqueueToolJobs(ctx context.Context, hostID int64, specs []DevToolSpec, password string) (DevToolQueueState, error) {
	if len(specs) == 0 || len(specs) > 32 {
		return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "没有要执行的动作"}
	}
	hasStudio := false
	for _, sp := range specs {
		if IsStudioTool(sp.Tool) {
			if sp.Action != DevToolInstall {
				return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "创作工具只支持安装"}
			}
			hasStudio = true
			continue
		}
		if err := devd.ValidTool(sp.Tool); err != nil {
			return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "不认识的开发工具：" + sp.Tool}
		}
		if _, ok := devToolCommands[sp.Action]; !ok {
			return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "不认识的动作：" + string(sp.Action)}
		}
	}
	host, err := m.workerHost(ctx, hostID)
	if err != nil {
		return DevToolQueueState{}, err
	}
	if hasStudio {
		if strings.ContainsAny(password, "\r\n\x00") {
			return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "sudo 口令不能包含换行或空字符"}
		}
		if err := needRootFor(host, password, "安装创作工具"); err != nil {
			return DevToolQueueState{}, err
		}
	}
	q := m.queueFor(hostID)
	q.mu.Lock()
	defer q.mu.Unlock()
	now := m.now()
	added := false
	for _, sp := range specs {
		dup := false
		for _, j := range q.jobs {
			if !j.Done() && j.Tool == sp.Tool && j.Action == sp.Action {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		m.jobsMu.Lock()
		m.jobSeq++
		id := m.jobSeq
		m.jobsMu.Unlock()
		q.jobs = append(q.jobs, &DevToolJob{ID: id, HostID: hostID, Tool: sp.Tool, Action: sp.Action, Status: DevToolJobQueued, Percent: -1, RemoteIP: sp.RemoteIP, CreatedAt: now})
		if IsStudioTool(sp.Tool) {
			q.secrets[id] = toolJobSecret{password: password}
		}
		added = true
	}
	if added {
		q.startLocked(m, hostID)
	}
	return q.snapshot(), nil
}

// startLocked 在入队之后收历史、通知陪等者，并在没有执行循环时起一个。
func (q *devToolQueue) startLocked(m *Manager, hostID int64) {
	q.trimLocked()
	q.bump()
	if !q.running {
		q.running = true
		go m.runDevToolQueue(hostID, q)
	}
}

// EnqueueGateInstall 把「安装 gate」排进这台主机的队列并立即返回队列快照。地址与 Key 的形态在
// 这里就收窄（invalid_host 立刻回，不入队）；主机上有没有 curl、首次安装有没有选 Key 要连上
// 主机才知道，由执行时判定、落在那条记录的 error 里。已有排队 / 执行中的安装时再入队即忽略。
func (m *Manager) EnqueueGateInstall(ctx context.Context, hostID int64, spec GateInstallSpec) (DevToolQueueState, error) {
	base, key, err := normalizeGateInstall(GateInstallRequest{ID: hostID, BaseURL: spec.BaseURL, Key: spec.Key})
	if err != nil {
		return DevToolQueueState{}, err
	}
	if _, err := m.workerHost(ctx, hostID); err != nil {
		return DevToolQueueState{}, err
	}
	q := m.queueFor(hostID)
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.jobs {
		if !j.Done() && j.Tool == GateJobTool && j.Action == DevToolInstall {
			return q.snapshot(), nil
		}
	}
	m.jobsMu.Lock()
	m.jobSeq++
	id := m.jobSeq
	m.jobsMu.Unlock()
	q.jobs = append(q.jobs, &DevToolJob{ID: id, HostID: hostID, Tool: GateJobTool, Action: DevToolInstall, Status: DevToolJobQueued, Percent: -1,
		KeyLabel: spec.KeyLabel, RemoteIP: spec.RemoteIP, CreatedAt: m.now()})
	q.secrets[id] = toolJobSecret{base: base, key: key}
	q.startLocked(m, hostID)
	return q.snapshot(), nil
}

// trimLocked 把已结束的历史收到 devToolHistory 条以内（先丢最早结束的）。
func (q *devToolQueue) trimLocked() {
	done := 0
	for _, j := range q.jobs {
		if j.Done() {
			done++
		}
	}
	for i := 0; done > devToolHistory && i < len(q.jobs); {
		if q.jobs[i].Done() {
			delete(q.tail, q.jobs[i].ID)
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
			done--
			continue
		}
		i++
	}
}

// DevToolJobs 读这台主机队列的一帧。
func (m *Manager) DevToolJobs(hostID int64) DevToolQueueState {
	q := m.queueFor(hostID)
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.snapshot()
}

// WaitDevToolJobs 陪等：revision 与传入的不同即回，否则等到变化或 DevToolWaitFor。ctx 取消
// （页面关了）按 ctx.Err() 回。
func (m *Manager) WaitDevToolJobs(ctx context.Context, hostID int64, revision int64) (DevToolQueueState, error) {
	q := m.queueFor(hostID)
	deadline := time.NewTimer(DevToolWaitFor)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		notify := q.notify
		st := q.snapshot()
		q.mu.Unlock()
		if st.Revision != revision {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-deadline.C:
			return st, nil
		case <-notify:
		}
	}
}

// CancelDevToolJob 取消一个动作：排队中的直接标为已取消，执行中的关掉那条 SSH 会话（结束
// 后标为已取消），已结束的从历史里移除。不存在答 invalid_host。
func (m *Manager) CancelDevToolJob(hostID, jobID int64) (DevToolQueueState, error) {
	q := m.queueFor(hostID)
	q.mu.Lock()
	var cancelled *DevToolJob
	defer func() {
		q.mu.Unlock()
		if cancelled != nil {
			m.notifyDevToolJob(*cancelled, nil)
		}
	}()
	for i, j := range q.jobs {
		if j.ID != jobID {
			continue
		}
		switch j.Status {
		case DevToolJobQueued:
			j.Status = DevToolJobCancelled
			j.FinishedAt = m.now()
			delete(q.secrets, j.ID)
			copy := *j
			cancelled = &copy
		case DevToolJobRunning:
			q.aborted = true
			if q.cancel != nil {
				q.cancel()
			}
		default:
			delete(q.tail, j.ID)
			q.jobs = append(q.jobs[:i], q.jobs[i+1:]...)
		}
		q.bump()
		return q.snapshot(), nil
	}
	return DevToolQueueState{}, &Error{Code: CodeInvalidHost, Msg: "队列里没有这个动作"}
}

// ClearDevToolJobs 清掉已结束的历史，排队与执行中的保留。
func (m *Manager) ClearDevToolJobs(hostID int64) DevToolQueueState {
	q := m.queueFor(hostID)
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.jobs[:0]
	for _, j := range q.jobs {
		if j.Done() {
			delete(q.tail, j.ID)
			continue
		}
		kept = append(kept, j)
	}
	q.jobs = kept
	q.bump()
	return q.snapshot()
}

var percentRE = regexp.MustCompile(`(^|\s)(\d{1,3})%(\s|$)`)

// runDevToolQueue 逐个跑这台主机排队的动作，队列空了即退出（下次入队再起）。
func (m *Manager) runDevToolQueue(hostID int64, q *devToolQueue) {
	for {
		q.mu.Lock()
		var job *DevToolJob
		for _, j := range q.jobs {
			if j.Status == DevToolJobQueued {
				job = j
				break
			}
		}
		if job == nil {
			q.running = false
			q.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		q.cancel = cancel
		q.current = job.ID
		q.aborted = false
		job.Status = DevToolJobRunning
		job.Stage = "connecting"
		job.StartedAt = m.now()
		q.tail[job.ID] = nil
		q.bump()
		id, req := job.ID, DevToolRequest{ID: hostID, Tool: job.Tool, Action: job.Action}
		// 安装入参只在这一刻从 secrets 取走：之后表里没有它。
		secret, hasSecret := q.secrets[id]
		delete(q.secrets, id)
		q.mu.Unlock()

		progress := func(stage, line string) {
			q.mu.Lock()
			defer q.mu.Unlock()
			j := q.find(id)
			if j == nil || j.Status != DevToolJobRunning {
				return
			}
			j.Stage = stage
			if line != "" {
				j.Line = line
				lines := append(q.tail[id], line)
				if len(lines) > devToolTail {
					lines = lines[len(lines)-devToolTail:]
				}
				q.tail[id] = lines
				j.Output = strings.Join(lines, "\n")
				if mm := percentRE.FindStringSubmatch(line); mm != nil {
					if p, err := strconv.Atoi(mm[2]); err == nil && p >= 0 && p <= 100 {
						j.Percent = p
					}
				} else if strings.Contains(line, "取回完成") { // gate 取回收尾那行的原文，不是本设备的文案
					j.Percent = 100
				}
			}
			q.bump()
		}
		var (
			res *GateInstallResult
			err error
		)
		if req.Tool == GateJobTool {
			if !hasSecret {
				// 只可能是队列状态被动过手脚：没有入参就没法装。
				err = &Error{Code: CodeInvalidHost, Msg: "这条安装 gate 的动作没有地址与密钥"}
			} else {
				res, err = m.installGate(ctx, GateInstallRequest{ID: hostID, BaseURL: secret.base, Key: secret.key}, progress)
			}
		} else if IsStudioTool(req.Tool) {
			res, err = m.installPackage(ctx, PackageInstallRequest{ID: hostID, Name: strings.TrimPrefix(req.Tool, "studio/"), Password: secret.password}, progress)
		} else {
			res, err = m.devTool(ctx, req, progress)
		}
		secret = toolJobSecret{}
		cancel()

		q.mu.Lock()
		j := q.find(id)
		if j != nil {
			j.FinishedAt = m.now()
			j.Stage = ""
			switch {
			case err != nil && q.aborted:
				j.Status = DevToolJobCancelled
				j.Error = "已取消"
			case err != nil:
				j.Status = DevToolJobFailed
				j.Error = err.Error()
				j.ErrorCode = errCode(err)
			default:
				j.Status = DevToolJobSucceeded
				j.Percent = 100
				j.Output = res.Output
				if j.Tool == GateJobTool {
					j.Version = res.Tools.Gate.Version
				}
				if IsStudioTool(j.Tool) {
					j.Version = studioToolVersion(j.Tool, res.Tools.Studio)
				}
				for _, d := range res.Tools.DevTools {
					if d.Name == j.Tool {
						j.Version = d.Version
					}
				}
			}
			if res != nil && res.Tools != nil {
				q.tools, q.toolsAt = res.Tools, j.FinishedAt
			}
		}
		q.cancel, q.current = nil, 0
		delete(q.tail, id)
		var done DevToolJob
		if j != nil {
			done = *j
		}
		var tools *Tools
		if res != nil {
			tools = res.Tools
		}
		q.bump()
		q.mu.Unlock()

		if j != nil {
			m.notifyDevToolJob(done, tools)
		}
	}
}

func (q *devToolQueue) find(id int64) *DevToolJob {
	for _, j := range q.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (m *Manager) notifyDevToolJob(job DevToolJob, tools *Tools) {
	m.jobsMu.Lock()
	hook := m.jobHook
	m.jobsMu.Unlock()
	if hook != nil {
		hook(job, tools)
	}
}
