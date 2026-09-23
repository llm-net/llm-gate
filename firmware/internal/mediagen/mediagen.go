// Package mediagen 是设备内部的媒体生成层：设备自己作为调用方，向订阅或上游平台发起一次
// 图像 / 视频生成，把结果取回落盘。单位是生成任务（store.MediaJob，表 media_jobs）。它有三个
// 调用方：页面（管理员与 Key 持有人手动提交）、创作工作空间的智能体（MCP 工具）、
// `gate media` 子命令；调用方只在各自的处理器里做鉴权、归属裁决、并发闸与编码。
//
// 它不是协议面：外部客户端要生成图像 / 视频仍走厂商协议面与开发工具订阅面，那几条路逐字节
// 透传、不经过本层。本层只负责「出一张图 / 一段视频」，不懂作品、镜头与目录。
//
// 内核管：能力表（capability.go）、可用性裁决（available.go）、受理（校验 / 准入）、后台
// 运行、向平台查询、取回落盘、缩略图、陪等、搬运、删除、清扫与重启恢复。后端（Backend）在
// 装配期注册，生产实现在 internal/gateway：凭据、出站分类、计量入账都在那里，进程内调用。
//
// 提交只落库并立刻返回 running 任务；平台请求在设备后台 goroutine 里跑（上限 TaskTimeout），
// 调用方按任务陪等直到终态。平台先受理再异步生成的任务落成 queued 并记平台任务 ID，同一条
// goroutine 接着按 PollInterval 向平台查询进度；进程重启后带平台任务 ID 的 queued 行在注册
// 后端时重新开启查询，其余未到终态的判失败。结果取回后保存在设备数据目录 preview/ 下，
// 文件名 <任务 ID>.<扩展名>，行里只记文件名；平台链接会过期，本地文件才是结果的真值。
// 图像结果落盘时顺手生成缩略图（thumbnail.go），视频的封面帧由页面回传（SaveThumb）。
// 任务删除时文件一并删除；启动与每次删除后清掉没有任务行引用的孤儿文件。
// §15.1：日志只记任务 id、后端、种类、状态与耗时；提示词只在任务行里，媒体只在内存里经过。
package mediagen

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Request 是交给后端的一次生成：能力表项、校验过的操作 / 输入 / 参数，以及归属。AccountID 是
// 订阅后端钉死的订阅账号行；KeyID / KeyDisplay 是任务归属的那把客户端 Key，后端据它把这一次
// 上游调用记进该密钥的账本。
type Request struct {
	JobID      string
	Model      Model
	Operation  string
	Prompt     string
	Inputs     Inputs
	Params     Params
	AccountID  int64
	KeyID      int64
	KeyDisplay string
}

// Result 是后端的一次回答：Status 为 succeeded（MediaURL 是平台 https 地址或 data URI）、
// failed / expired（Error 给人看）或 queued（平台已受理，VendorID 是平台任务 ID）。
type Result struct {
	Status    string
	VendorID  string
	MediaURL  string
	MediaType string
	Error     string
	AccountID int64
}

// Backend 是一个生成后端。
type Backend interface {
	Name() string
	// Validate 补判能力表表达不了的搭配约束；不合法返回 *InvalidError。
	Validate(Request) error
	// Generate 同步出结果，或回 queued + 平台任务 ID。
	Generate(context.Context, Request) (Result, error)
	// Refresh 查询一条平台排队中的任务；不计模型消费。平台中间状态一律折成 queued。
	Refresh(context.Context, store.MediaJob) (Result, error)
}

// Admitter 是受理时过的准入闸：与数据面同一道（RPM → 日 → 周 → 月预算 → 按量额度），
// 按这把 Key 的限额快照判。拒绝返回 *RejectedError，此时不建任何任务。
type Admitter interface {
	AdmitMediaJob(ctx context.Context, keyID int64, m Model) error
}

// RejectedError 是准入闸的拒绝（调用方答 429）。
type RejectedError struct {
	Code          string
	Msg           string
	RetryAfterSec int
}

func (e *RejectedError) Error() string { return e.Msg }

// Finisher 是来源注册的收尾器：进程重启后，内核把这个来源已到终态、尚未搬运的任务逐条交给
// 它——成功的搬进自己的去处（Adopt），失败的记下原因，随后删任务。
type Finisher func(ctx context.Context, job store.MediaJob)

// TaskTimeout 是一次后台生成的整体上限：与上游客户端的 Overall 缺省（10 分钟）同量级，再留出
// 落库时间。平台画图常要一两分钟，HTTP 请求不能同步等它。
const TaskTimeout = 11 * time.Minute

// WaitFor 是一次陪等的上限：浏览器与中间代理都等得起；任务仍在生成时调用方拿着
// running / queued 帧再发起下一轮。
const WaitFor = 30 * time.Second

// PollInterval 是平台排队中的任务向平台查询进度的间隔（查询不计模型消费）。
// 变量而非常量：测试缩短它。
var PollInterval = 5 * time.Second

// timeoutError 是后台生成 / 查询超过 TaskTimeout 仍未到终态时写入的原因。
const timeoutError = "生成超时，平台未在限定时间内返回"

// interruptedError 是进程重启时仍在 running 的任务被判失败的原因。
const interruptedError = "设备进程已重启，生成被中断，请重新提交"

// MediaDirName 是数据目录下保存生成结果的子目录名。
const MediaDirName = "preview"

// maxMediaBytes 是单个结果文件的上限：平台视频通常几十 MiB，留足余量但不让一个失控的响应
// 写满数据分区。
const maxMediaBytes = 512 << 20

var (
	// ErrUnavailable：没接后端（装配缺陷或窄测试进程）。
	ErrUnavailable = errors.New("生成服务不可用")
	// ErrNotFound：任务不存在。
	ErrNotFound = errors.New("任务不存在")
	// ErrMediaMissing：任务没有媒体或已过期。
	ErrMediaMissing = errors.New("媒体不存在或已过期")
	// ErrMediaExpired：媒体地址不可用（平台侧已过期或 data URI 损坏）。
	ErrMediaExpired = errors.New("媒体已过期")
	// ErrRefreshFailed：向平台查询任务失败。
	ErrRefreshFailed = errors.New("查询平台任务失败")
	// ErrNotReady：任务还没有可搬运的结果。
	ErrNotReady = errors.New("任务还没有结果")
)

// UnavailableError 是可用性裁决的拒绝：Code 是 Reason* 原因码。
type UnavailableError struct{ Code, Msg string }

func (e *UnavailableError) Error() string { return e.Msg }

// Service 是任务内核，进程内只有一份（全部调用方共用），否则陪等唤醒会互相看不见。
type Service struct {
	st    *store.Store
	log   *slog.Logger
	media *http.Client
	// dir 是结果文件目录；空 = 不落盘（窄测试进程），结果只留平台 URL / data URI。
	dir          string
	backends     map[string]Backend
	admitter     Admitter
	entitlements Entitlements
	finishers    map[string]Finisher
	// mu 守护 notify 与 finishers：后台生成一完成就关掉当前通道换新的一条，陪等中的 wait 请求借此立刻醒来
	// 重读任务行（零轮询：页面不定时重取）。
	mu     sync.Mutex
	notify chan struct{}
	// fsMu 让「写结果文件 + 写回任务行」与「删行 + 清孤儿文件」互斥：否则一次清空可能夹在
	// 文件落地与行更新之间，把刚写好的结果当孤儿删掉。
	fsMu sync.Mutex
	// submitMu 让「数这把 Key 未到终态的任务 + 落库新任务」成为一步：否则两次并发提交各自数到
	// 空位，合计越过 RunningPerKey。
	submitMu sync.Mutex
}

// New 建内核：把上次进程留下的 running 任务判失败（后台 goroutine 随进程消亡，没有人再去
// 更新它们），并清掉结果目录里的孤儿文件。media 是取回平台媒体用的出站客户端；dataDir 是
// 设备数据目录。
func New(st *store.Store, log *slog.Logger, media *http.Client, dataDir string) *Service {
	s := &Service{st: st, log: log, media: media, notify: make(chan struct{}), backends: map[string]Backend{}, finishers: map[string]Finisher{}}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, MediaDirName)
	}
	s.recover()
	s.sweep()
	go s.materialize()
	return s
}

// SetEntitlements 注入订阅授权的解析器（装配期一次性）。
func (s *Service) SetEntitlements(e Entitlements) { s.entitlements = e }

// SetAdmitter 注入准入闸（装配期一次性）；nil = 不设闸（计量未装配的窄进程）。
func (s *Service) SetAdmitter(a Admitter) { s.admitter = a }

// SetBackends 注册生成后端（装配期一次性），并为上次进程留下的平台排队中任务重新开启后台
// 查询：它们的平台任务 ID 还在，平台侧多半已经做完。
func (s *Service) SetBackends(backends ...Backend) {
	for _, b := range backends {
		if b != nil {
			s.backends[b.Name()] = b
		}
	}
	if len(s.backends) == 0 {
		return
	}
	queued, err := s.st.ListQueuedMediaJobs(context.Background())
	if err != nil {
		s.log.Warn("读取平台排队中的生成任务失败", "err", err.Error())
		return
	}
	resumed := 0
	for _, j := range queued {
		b := s.backends[j.Backend]
		if b == nil {
			continue
		}
		runCtx, cancel := context.WithTimeout(context.Background(), TaskTimeout)
		go s.pollAndSave(runCtx, cancel, b, j, time.Now(), true)
		resumed++
	}
	if resumed > 0 {
		s.log.Info("已为平台排队中的生成任务重新开启查询", "count", resumed)
	}
}

// SetFinisher 注册一个来源的收尾器（装配期一次性），并把这个来源已到终态、尚未搬运的任务
// 逐条交给它——上次进程在「任务完成」与「搬走」之间退出时留下的就是这些。重启后接续查询
// 的任务到终态时同样交给它；进程内正常提交的任务由调用方自己陪等、自己搬。
func (s *Service) SetFinisher(origin string, f Finisher) {
	if f == nil {
		return
	}
	s.mu.Lock()
	s.finishers[origin] = f
	s.mu.Unlock()
	jobs, err := s.st.ListTerminalMediaJobsByOrigin(context.Background(), origin)
	if err != nil {
		s.log.Warn("读取待收尾的生成任务失败", "origin", origin, "err", err.Error())
		return
	}
	if len(jobs) == 0 {
		return
	}
	go func() {
		for _, j := range jobs {
			f(context.Background(), j)
		}
		s.log.Info("已把上次进程留下的生成任务交给来源收尾", "origin", origin, "count", len(jobs))
	}()
}

func (s *Service) recover() {
	n, err := s.st.FailRunningMediaJobs(context.Background(), interruptedError)
	if err != nil {
		s.log.Warn("清理中断的生成任务失败", "err", err.Error())
		return
	}
	if n > 0 {
		s.log.Info("已把上次进程中断的生成任务判为失败", "count", n)
	}
}

// Prepared 是校验过、尚未受理的一次提交：Check 与 Submit 之间调用方判自己的并发闸。
type Prepared struct {
	sub     Submission
	params  Params
	avail   Availability
	backend Backend
}

// Count 是这次提交要建的任务数。
func (p *Prepared) Count() int { return p.sub.Count }

// Model 是这次提交解析出的能力表项。
func (p *Prepared) Model() Model { return p.avail.Model }

// Check 校验一次提交：模型对这把 Key 的可用性、按能力表的输入与参数、后端的搭配约束。
// 不合法返回 *InvalidError，不可用返回 *UnavailableError / ErrModelNotFound。
func (s *Service) Check(ctx context.Context, in Submission) (*Prepared, error) {
	if len(s.backends) == 0 {
		return nil, ErrUnavailable
	}
	in, err := NormalizeHead(in)
	if err != nil {
		return nil, err
	}
	avail, err := s.Resolve(ctx, in.KeyID, in.Model)
	if err != nil {
		return nil, err
	}
	if !avail.Available {
		return nil, &UnavailableError{Code: avail.ReasonCode, Msg: avail.Reason}
	}
	in, params, err := Normalize(avail.Model, in)
	if err != nil {
		return nil, err
	}
	b := s.backends[avail.Backend]
	if err := b.Validate(Request{Model: avail.Model, Operation: in.Operation, Prompt: in.Prompt, Inputs: in.Inputs, Params: params}); err != nil {
		return nil, err
	}
	return &Prepared{sub: in, params: params, avail: avail, backend: b}, nil
}

// Submit 受理一次校验过的提交：先过每把 Key 的并发闸（running + count > RunningPerKey 即
// BusyError，全部来源合计），再过准入闸（每个候选一次，任一被拒则整批不建）、同批任务一起
// 落库，立刻返回 running 任务；平台请求在后台跑。ctx 只用于落库——调用方断线不该取消已受理
// 的生成。
func (s *Service) Submit(ctx context.Context, p *Prepared) ([]store.MediaJob, error) {
	in := p.sub
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if in.KeyID > 0 {
		running, err := s.st.CountActiveMediaJobsByKey(ctx, in.KeyID)
		if err != nil {
			return nil, fmt.Errorf("统计进行中的生成任务: %w", err)
		}
		if running+int64(in.Count) > RunningPerKey {
			return nil, &BusyError{Running: running}
		}
	}
	if s.admitter != nil {
		for i := 0; i < in.Count; i++ {
			if err := s.admitter.AdmitMediaJob(ctx, in.KeyID, p.avail.Model); err != nil {
				return nil, err
			}
		}
	}
	now := time.Now()
	batch := ""
	if in.Count > 1 {
		batch = store.NewULID(now)
	}
	origin := in.Origin
	if origin == "" {
		origin = store.MediaOriginPage
	}
	jobs := make([]store.MediaJob, 0, in.Count)
	for i := 0; i < in.Count; i++ {
		// 任务 ID 用 ULID：字典序即创建序，也直接充当下载文件名（无需转义）。
		jobs = append(jobs, store.MediaJob{
			ID: store.NewULID(now), Origin: origin, Owner: in.Owner, BatchID: batch,
			Backend: p.avail.Backend, Provider: p.avail.Backend, AccountID: p.avail.AccountID,
			KeyID: in.KeyID, KeyDisplay: in.KeyDisplay, Kind: p.avail.Kind, Model: p.avail.ID,
			Operation: in.Operation, Prompt: in.Prompt, Status: store.MediaStatusRunning,
			Params: p.params.snapshot(), Inputs: in.Inputs.Shape(), CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := s.st.CreateMediaJobs(ctx, jobs); err != nil {
		return nil, fmt.Errorf("保存生成任务失败: %w", err)
	}
	for _, j := range jobs {
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TaskTimeout)
		go s.run(runCtx, cancel, p, j)
	}
	return jobs, nil
}

// run 在后台执行一次平台请求并把结果写回任务行。媒体输入只在这一程里活着：不落任务行，
// 进程重启后也不重放。
func (s *Service) run(ctx context.Context, cancel context.CancelFunc, p *Prepared, t store.MediaJob) {
	defer cancel()
	started := time.Now()
	res, err := p.backend.Generate(ctx, Request{JobID: t.ID, Model: p.avail.Model, Operation: t.Operation, Prompt: t.Prompt,
		Inputs: p.sub.Inputs, Params: p.params, AccountID: t.AccountID, KeyID: t.KeyID, KeyDisplay: t.KeyDisplay})
	if err != nil {
		res = Result{Status: store.MediaStatusFailed, Error: err.Error()}
		if ctx.Err() != nil {
			res.Error = timeoutError
		}
	}
	if res.AccountID != 0 {
		t.AccountID = res.AccountID
	}
	t.VendorID = res.VendorID
	t.Status = res.Status
	t.MediaURL = res.MediaURL
	t.MediaType = res.MediaType
	t.Error = res.Error
	if t.Status == "" {
		t.Status = store.MediaStatusFailed
	}
	if t.Status == store.MediaStatusQueued && t.VendorID != "" {
		// 平台已受理：先把平台任务 ID 与状态落库（页面看到「平台排队中」，进程重启也能接续），
		// 再在同一条 goroutine 里查到终态。
		if err := s.st.UpdateMediaJob(context.WithoutCancel(ctx), t); err != nil {
			s.log.Error("写回生成任务受理状态失败", "job", t.ID, "err", err.Error())
			return
		}
		s.Notify()
		s.pollAndSave(ctx, cancel, p.backend, t, started, false)
		return
	}
	s.finish(t, started, false)
}

// finish 把终态写回任务行并唤醒陪等者。落库用独立上下文：生成已超时也要把失败状态写回，
// 否则任务永远停在 running / queued。resumed 为真（重启后接续的任务）时，终态任务交给来源
// 注册的收尾器——原先陪等它的调用方已随上次进程消亡。
func (s *Service) finish(t store.MediaJob, started time.Time, resumed bool) {
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer saveCancel()
	if err := s.persistAndSave(saveCtx, &t); err != nil {
		s.log.Error("写回生成任务结果失败", "job", t.ID, "err", err.Error())
		return
	}
	s.Notify()
	s.log.Info("生成任务完成", "job", t.ID, "backend", t.Backend, "kind", t.Kind, "origin", t.Origin,
		"status", t.Status, "has_media", t.MediaFile != "", "duration_ms", time.Since(started).Milliseconds())
	if !resumed {
		return
	}
	s.mu.Lock()
	f := s.finishers[t.Origin]
	s.mu.Unlock()
	if f != nil {
		f(context.Background(), t)
	}
}

// pollAndSave 按 PollInterval 向平台查询一条平台排队中的任务，直到终态、任务被删或 ctx 超时
// （写 failed + timeoutError）。查询出错只等下一轮：网络抖动不该把平台侧正常进行的生成判死，
// 超时是唯一的兜底。每轮先重读任务行：行没了就停，行已由人工「查询平台任务」推到终态也停，
// 不重复取回结果。
func (s *Service) pollAndSave(ctx context.Context, cancel context.CancelFunc, b Backend, t store.MediaJob, started time.Time, resumed bool) {
	defer cancel()
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Status = store.MediaStatusFailed
			t.Error = timeoutError
			s.finish(t, started, resumed)
			return
		case <-ticker.C:
		}
		cur, err := s.st.GetMediaJob(context.WithoutCancel(ctx), t.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			continue
		}
		if !store.MediaStatusActive(cur.Status) {
			return
		}
		res, err := b.Refresh(ctx, t)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("查询平台生成任务失败，稍后重试", "job", t.ID, "err", err.Error())
			}
			continue
		}
		if !store.MediaStatusTerminal(res.Status) {
			continue
		}
		if res.AccountID != 0 {
			t.AccountID = res.AccountID
		}
		t.Status = res.Status
		t.MediaURL = res.MediaURL
		t.MediaType = res.MediaType
		t.Error = res.Error
		s.finish(t, started, resumed)
		return
	}
}

// materialize 在后台把旧任务补齐到当前形态：仍以 data URI 留在行里的图像结果写成
// 本地文件并从行里去掉（一份任务列表不该背着几十 MB 的 base64），已落盘却没有
// 缩略图的图像补生成缩略图。每条任务各自持锁写回，不挡住启动，也不挡住删除。
func (s *Service) materialize() {
	if s.dir == "" {
		return
	}
	ctx := context.Background()
	tasks, err := s.st.ListMediaJobsNeedingMedia(ctx)
	if err != nil {
		s.log.Warn("读取待补齐的生成任务失败", "err", err.Error())
		return
	}
	done := 0
	for i := range tasks {
		t := &tasks[i]
		s.fsMu.Lock()
		cur, err := s.st.GetMediaJob(ctx, t.ID)
		if err != nil {
			s.fsMu.Unlock()
			continue // 行已被删。
		}
		t = cur
		if t.MediaFile == "" && strings.HasPrefix(t.MediaURL, "data:") {
			if err := s.persistMedia(ctx, t); err != nil {
				s.log.Warn("补存生成结果到设备失败", "job", t.ID, "err", err.Error())
			}
		} else if t.MediaFile != "" && t.ThumbFile == "" {
			s.thumbFromFile(t)
		}
		err = s.st.UpdateMediaJob(ctx, *t)
		s.fsMu.Unlock()
		if err != nil {
			s.log.Warn("写回补齐的生成任务失败", "job", t.ID, "err", err.Error())
			continue
		}
		done++
	}
	if done > 0 {
		s.Notify()
		s.log.Info("已补齐生成任务的本地结果与缩略图", "count", done)
	}
}

// MediaDir 是结果文件目录（空 = 不落盘）。
func (s *Service) MediaDir() string { return s.dir }

// persistAndSave 把已成功的结果取回保存到本地，再写回任务行；两步在 fsMu 之内，
// 不会被并发的清理当成孤儿。取回失败不改任务状态，只记警告：行里仍有平台 URL，
// 下载走旧的回源路径。
func (s *Service) persistAndSave(ctx context.Context, t *store.MediaJob) error {
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	if t.Status == store.MediaStatusSucceeded && t.MediaFile == "" && t.MediaURL != "" {
		if err := s.persistMedia(ctx, t); err != nil {
			s.log.Warn("保存生成结果到设备失败", "job", t.ID, "err", err.Error())
		}
	}
	return s.st.UpdateMediaJob(ctx, *t)
}

// persistMedia 把 t.MediaURL 指向的结果写成 dir/<ID>.<扩展名>：data URI 就地解码，
// https 地址经出站客户端取回；先写临时文件再改名，半成品不会被当成结果。成功后
// data URI 从行里去掉（结果已在文件里，不再把整段 base64 留在库中），平台 https
// 地址保留作来源记录。
func (s *Service) persistMedia(ctx context.Context, t *store.MediaJob) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	var body io.Reader
	var ct string
	if strings.HasPrefix(t.MediaURL, "data:") {
		raw, dataCT, err := decodeDataURI(t.MediaURL)
		if err != nil {
			return err
		}
		body, ct = strings.NewReader(string(raw)), dataCT
	} else {
		u, err := url.Parse(t.MediaURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("结果地址不是 https")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.MediaURL, nil)
		if err != nil {
			return err
		}
		resp, err := s.media.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("平台返回 HTTP %d", resp.StatusCode)
		}
		body, ct = resp.Body, resp.Header.Get("Content-Type")
	}
	if mt, _, _ := mime.ParseMediaType(ct); mt == "" || mt == "application/octet-stream" {
		ct = ""
	}
	name := DownloadName(t, ct)
	tmp, err := os.CreateTemp(s.dir, "."+t.ID+".*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(body, maxMediaBytes+1))
	if err == nil && n > maxMediaBytes {
		err = fmt.Errorf("结果超过 %d 字节上限", maxMediaBytes)
	}
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, name)); err != nil {
		return err
	}
	t.MediaFile = name
	if strings.HasPrefix(t.MediaURL, "data:") {
		t.MediaURL = ""
	}
	if t.Kind == store.ModelKindImage {
		s.thumbFromFile(t)
	}
	return nil
}

// thumbFromFile 从已落盘的图像结果生成缩略图并写进 t.ThumbFile；解不开（WebP 等
// 标准库之外的格式）只记日志，页面会在浏览器里抓一帧回传。调用方持 fsMu。
func (s *Service) thumbFromFile(t *store.MediaJob) {
	f, err := os.Open(filepath.Join(s.dir, filepath.Base(t.MediaFile)))
	if err != nil {
		return
	}
	defer f.Close()
	data, err := makeThumb(f)
	if err != nil {
		s.log.Info("设备未能生成结果缩略图，等待页面回传封面帧", "job", t.ID, "reason", err.Error())
		return
	}
	name, err := s.writeThumb(t.ID, data)
	if err != nil {
		s.log.Warn("写入结果缩略图失败", "job", t.ID, "err", err.Error())
		return
	}
	t.ThumbFile = name
}

// writeThumb 把缩略图字节写成 dir/<ID>.thumb.jpg（先临时文件再改名），回文件名。
func (s *Service) writeThumb(id string, data []byte) (string, error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.dir, "."+id+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	name := id + ThumbExt
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, name)); err != nil {
		return "", err
	}
	return name, nil
}

// SaveThumb 收页面回传的封面帧（视频的第一帧，或设备解不开的图像格式由浏览器
// 转出的位图）：只在任务已成功且还没有缩略图时接收；字节经 makeThumb 重新解码、
// 缩放、编码后才落盘，回传的原字节不保存。已有缩略图时不改动，直接回当前行
// （多个页面同时回传是良性竞争）。解不开的字节报 *InvalidError。
func (s *Service) SaveThumb(ctx context.Context, id string, upload []byte) (*store.MediaJob, error) {
	if len(upload) > maxThumbUploadBytes {
		return nil, &InvalidError{Msg: "缩略图过大"}
	}
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	t, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.ThumbFile != "" {
		return t, nil
	}
	if t.Status != store.MediaStatusSucceeded || (t.MediaFile == "" && t.MediaURL == "") {
		return nil, ErrMediaMissing
	}
	if s.dir == "" {
		return t, nil
	}
	data, err := makeThumb(bytes.NewReader(upload))
	if err != nil {
		return nil, &InvalidError{Msg: "缩略图不是可识别的图像"}
	}
	name, err := s.writeThumb(t.ID, data)
	if err != nil {
		return nil, err
	}
	t.ThumbFile = name
	if err := s.st.UpdateMediaJob(ctx, *t); err != nil {
		return nil, err
	}
	s.Notify()
	return t, nil
}

// OpenThumb 打开任务的缩略图（JPEG）；没有缩略图返回 ErrMediaMissing。
func (s *Service) OpenThumb(t *store.MediaJob) (*Media, error) {
	if t.ThumbFile == "" || s.dir == "" {
		return nil, ErrMediaMissing
	}
	f, err := os.Open(filepath.Join(s.dir, filepath.Base(t.ThumbFile)))
	if err != nil {
		return nil, ErrMediaMissing
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrMediaMissing
	}
	return &Media{ContentType: "image/jpeg", Filename: t.ThumbFile, ModTime: info.ModTime(), Content: f}, nil
}

// decodeDataURI 解 data URI：返回正文与 MIME（缺省 image/png）。
func decodeDataURI(uri string) ([]byte, string, error) {
	comma := strings.IndexByte(uri, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("data URI 缺少正文")
	}
	raw, err := base64.StdEncoding.DecodeString(uri[comma+1:])
	if err != nil {
		return nil, "", fmt.Errorf("data URI 正文不是 base64")
	}
	// data URI 的 MIME 在 "data:" 与第一个 ";" 或 "," 之间；缺省按 PNG。
	ct := strings.TrimPrefix(uri[:comma], "data:")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if ct == "" {
		ct = "image/png"
	}
	return raw, ct, nil
}

// sweep 清掉结果目录里没有任务行引用的文件（含 .part 半成品）。
func (s *Service) sweep() {
	if s.dir == "" {
		return
	}
	s.fsMu.Lock()
	defer s.fsMu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return // 目录还没建：没有东西可清。
	}
	referenced, err := s.st.MediaJobFiles(context.Background())
	if err != nil {
		s.log.Warn("读取生成结果引用失败", "err", err.Error())
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || referenced[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		s.log.Info("已清理无任务引用的生成结果文件", "count", removed)
	}
}

// Notify 唤醒所有陪等中的 wait 请求：关掉当前通道并换上新的一条。
func (s *Service) Notify() {
	s.mu.Lock()
	close(s.notify)
	s.notify = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) notifyCh() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notify
}

// Get 读一条任务；不存在返回 ErrNotFound。
func (s *Service) Get(ctx context.Context, id string) (*store.MediaJob, error) {
	t, err := s.st.GetMediaJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Wait 陪等一个任务离开 running / queued：任务一落终态或 WaitFor 耗尽就回当前任务行；
// 仍未到终态时调用方继续下一轮。ctx 取消（页面关了）返回 ctx.Err()，生成不中断；
// 陪等期间任务被删返回 ErrNotFound。这是「人发起陪等」的零轮询例外。
func (s *Service) Wait(ctx context.Context, id string) (*store.MediaJob, error) {
	deadline := time.NewTimer(WaitFor)
	defer deadline.Stop()
	for {
		notify := s.notifyCh()
		t, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if !store.MediaStatusActive(t.Status) {
			return t, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return t, nil
		case <-notify:
		}
	}
}

// Refresh 由人发起、立刻向平台查询一条平台排队中的任务并写回；返回更新后的任务行。
// 后台查询照常进行，这只是不想等下一轮的人工插队；行已到终态时直接回当前行。
// 平台回的中间状态不写回：行保持 queued，页面的陪等语义不变。
func (s *Service) Refresh(ctx context.Context, t store.MediaJob) (*store.MediaJob, error) {
	b := s.backends[t.Backend]
	if b == nil {
		return nil, ErrUnavailable
	}
	if cur, err := s.Get(ctx, t.ID); err == nil && !store.MediaStatusActive(cur.Status) {
		return cur, nil
	}
	res, err := b.Refresh(ctx, t)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefreshFailed, err)
	}
	if !store.MediaStatusTerminal(res.Status) {
		return &t, nil
	}
	t.Status = res.Status
	if res.MediaURL != "" {
		t.MediaURL = res.MediaURL
	}
	if res.MediaType != "" {
		t.MediaType = res.MediaType
	}
	t.Error = res.Error
	if err := s.persistAndSave(ctx, &t); err != nil {
		return nil, fmt.Errorf("保存生成任务失败: %w", err)
	}
	s.Notify()
	return &t, nil
}

// Delete 删一条任务记录与其结果文件并唤醒陪等者；不存在返回 ErrNotFound。允许删仍在 running
// 的任务：后台 goroutine 写回时命中 0 行即静默作废；陪等中的请求会以 404 结束。
func (s *Service) Delete(ctx context.Context, id string) error {
	err := s.st.DeleteMediaJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	s.Notify()
	s.sweep()
	return nil
}

// Clear 清空页面提交的全部任务记录与结果文件（管理员），回删除条数。
func (s *Service) Clear(ctx context.Context) (int64, error) {
	n, err := s.st.DeletePageMediaJobs(ctx)
	if err != nil {
		return 0, err
	}
	s.Notify()
	s.sweep()
	return n, nil
}

// ClearKey 清空某把 Key 在页面提交的任务记录与结果文件，回删除条数。
func (s *Service) ClearKey(ctx context.Context, keyID int64) (int64, error) {
	n, err := s.st.DeletePageMediaJobsByKey(ctx, keyID)
	if err != nil {
		return 0, err
	}
	s.Notify()
	s.sweep()
	return n, nil
}

// Adopt 把一条已成功的任务交给调用方搬走：sink 拿到任务行与结果的读取器，把它写进自己的
// 去处；sink 成功返回后内核删任务行与设备上的结果文件，失败则任务原样留着。任务不存在返回
// ErrNotFound（另一条路径已经搬走了），还没有成功的结果返回 ErrNotReady。
func (s *Service) Adopt(ctx context.Context, id string, sink func(context.Context, *store.MediaJob, *Media) error) error {
	t, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != store.MediaStatusSucceeded {
		return ErrNotReady
	}
	media, err := s.OpenMedia(ctx, t)
	if err != nil {
		return err
	}
	err = sink(ctx, t, media)
	media.Close()
	if err != nil {
		return err
	}
	if err := s.Delete(context.WithoutCancel(ctx), t.ID); err != nil && !errors.Is(err, ErrNotFound) {
		s.log.Warn("删除已搬走的生成任务失败", "job", t.ID, "err", err.Error())
	}
	return nil
}

// Media 是可读取的结果：ContentType、下载文件名（`<任务 ID>.<扩展名>`）与正文。
// 本地文件时 Content 非空（可 Seek，处理器用 http.ServeContent 支持视频拖动）；
// 旧任务只有平台 URL / data URI 时退回 Body 流式透传。
type Media struct {
	ContentType string
	Filename    string
	ModTime     time.Time
	Content     io.ReadSeekCloser
	Body        io.ReadCloser
}

// Close 释放底层文件或响应体。
func (m *Media) Close() error {
	if m.Content != nil {
		return m.Content.Close()
	}
	if m.Body != nil {
		return m.Body.Close()
	}
	return nil
}

// ServeMedia 把结果写给 HTTP 响应：disposition 为 "attachment"（下载）或 "inline"
// （页面内联显示）。本地文件经 http.ServeContent（支持 Range，视频可拖动）；回源的
// 流式正文直接拷贝。恒 no-store：结果按任务归属裁决过，不该进共享缓存。
func ServeMedia(w http.ResponseWriter, r *http.Request, m *Media, disposition string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", m.ContentType)
	w.Header().Set("Content-Disposition", disposition+"; filename="+m.Filename)
	if m.Content != nil {
		http.ServeContent(w, r, m.Filename, m.ModTime, m.Content)
		return
	}
	_, _ = io.Copy(w, m.Body)
}

// OpenMedia 打开任务结果：优先设备上的本地文件；没有本地文件的旧任务按 MediaURL
// 回源（data URI 就地解码，平台 https 地址经出站客户端取回并透传），平台侧已失效时
// 把任务标为 expired 并返回 ErrMediaExpired。
func (s *Service) OpenMedia(ctx context.Context, t *store.MediaJob) (*Media, error) {
	if t.MediaFile != "" && s.dir != "" {
		f, err := os.Open(filepath.Join(s.dir, filepath.Base(t.MediaFile)))
		if err != nil {
			return nil, ErrMediaMissing
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, ErrMediaMissing
		}
		ct := mime.TypeByExtension(filepath.Ext(t.MediaFile))
		if ct == "" {
			ct = "application/octet-stream"
		}
		return &Media{ContentType: ct, Filename: t.MediaFile, ModTime: info.ModTime(), Content: f}, nil
	}
	if t.MediaURL == "" {
		return nil, ErrMediaMissing
	}
	if strings.HasPrefix(t.MediaURL, "data:") {
		raw, ct, err := decodeDataURI(t.MediaURL)
		if err != nil {
			return nil, ErrMediaExpired
		}
		return &Media{ContentType: ct, Filename: DownloadName(t, ct), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}
	u, err := url.Parse(t.MediaURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, ErrMediaExpired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.MediaURL, nil)
	if err != nil {
		return nil, ErrMediaExpired
	}
	resp, err := s.media.Do(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp != nil {
			resp.Body.Close()
		}
		t.Status = store.MediaStatusExpired
		_ = s.st.UpdateMediaJob(ctx, *t)
		return nil, ErrMediaExpired
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &Media{ContentType: ct, Filename: DownloadName(t, ct), Body: resp.Body}, nil
}

// mediaExt 按媒体 MIME 选下载扩展名；认不出时按任务类型兜底。
func mediaExt(contentType, kind string) string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(mediaType) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/webm":
		return "webm"
	case "video/quicktime":
		return "mov"
	}
	if kind == store.ModelKindVideo {
		return "mp4"
	}
	return "png"
}

// DownloadName 是下载文件名：任务 ID（ULID）加扩展名。ID 出自设备自己生成的
// base32，不含需要转义的字符；旧格式 ID 里也只有字母、数字与连字符。
func DownloadName(t *store.MediaJob, contentType string) string {
	return t.ID + "." + mediaExt(contentType, t.Kind)
}
