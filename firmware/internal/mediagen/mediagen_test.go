package mediagen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// TestMain 把向平台查询的间隔缩到 20 毫秒：整个包只改这一次、不再改回，后台 goroutine 读
// PollInterval 时不会与某个用例的还原写竞争。
func TestMain(m *testing.M) {
	PollInterval = 20 * time.Millisecond
	os.Exit(m.Run())
}

// fakeBackend 是假后端：各钩子缺省为「立刻失败（stub）」「查询回中间状态」「校验放行」，
// 用例按需替换；Generate 收到的请求逐条记下。
type fakeBackend struct {
	name     string
	generate func(context.Context, Request) (Result, error)
	refresh  func(context.Context, store.MediaJob) (Result, error)
	validate func(Request) error

	mu       sync.Mutex
	requests []Request
	refreshs int
}

func (b *fakeBackend) Name() string { return b.name }

func (b *fakeBackend) Validate(r Request) error {
	if b.validate != nil {
		return b.validate(r)
	}
	return nil
}

func (b *fakeBackend) Generate(ctx context.Context, r Request) (Result, error) {
	b.mu.Lock()
	b.requests = append(b.requests, r)
	b.mu.Unlock()
	if b.generate != nil {
		return b.generate(ctx, r)
	}
	return Result{Status: store.MediaStatusFailed, Error: "stub"}, nil
}

func (b *fakeBackend) Refresh(ctx context.Context, j store.MediaJob) (Result, error) {
	b.mu.Lock()
	b.refreshs++
	b.mu.Unlock()
	if b.refresh != nil {
		return b.refresh(ctx, j)
	}
	return Result{Status: "processing"}, nil
}

func (b *fakeBackend) generated() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Request(nil), b.requests...)
}

func (b *fakeBackend) refreshCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refreshs
}

// queuedBackend 模拟「平台先受理再异步生成」：Generate 回 queued + 平台任务 ID（并改钉账号），
// Refresh 前 pending 次回平台中间状态，之后回 final；failing 为真时 Refresh 报错。
type queuedBackend struct {
	fakeBackend
	pending int
	final   Result
	failing bool
}

func newQueuedBackend(pending int, final Result) *queuedBackend {
	q := &queuedBackend{pending: pending, final: final}
	q.name = BackendGrok
	q.generate = func(context.Context, Request) (Result, error) {
		return Result{Status: store.MediaStatusQueued, VendorID: "vendor-1", AccountID: 2}, nil
	}
	q.refresh = func(context.Context, store.MediaJob) (Result, error) {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.failing {
			return Result{}, context.DeadlineExceeded
		}
		if q.refreshs <= q.pending {
			return Result{Status: "processing"}, nil
		}
		return q.final, nil
	}
	return q
}

func (q *queuedBackend) setFailing(v bool) {
	q.mu.Lock()
	q.failing = v
	q.mu.Unlock()
}

// testAccountID 是假授权钉死的订阅账号行。
const testAccountID = 7

// entitleAll 是「两种订阅都钉了账号且可用」的假授权。
func entitleAll(context.Context, int64) (map[string]Entitlement, error) {
	return map[string]Entitlement{
		BackendGrok:  {Configured: true, Available: true, AccountID: testAccountID},
		BackendCodex: {Configured: true, Available: true, AccountID: testAccountID},
	}, nil
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newService 建不落盘的内核（窄进程形态），订阅全部授权。
func newService(t *testing.T, backends ...Backend) (*Service, *store.Store) {
	t.Helper()
	st := openStore(t, t.TempDir())
	s := New(st, slog.New(slog.DiscardHandler), nil, "")
	s.SetEntitlements(entitleAll)
	s.SetBackends(backends...)
	return s, st
}

// newDiskService 建落盘的内核；media 是取回平台媒体用的客户端（可为 nil）。
func newDiskService(t *testing.T, media *http.Client, backends ...Backend) (*Service, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st := openStore(t, dir)
	s := New(st, slog.New(slog.DiscardHandler), media, dir)
	s.SetEntitlements(entitleAll)
	s.SetBackends(backends...)
	return s, st, filepath.Join(dir, MediaDirName)
}

// submit 走完 Check → Submit；不合法即失败。
func submit(t *testing.T, s *Service, in Submission) []store.MediaJob {
	t.Helper()
	p, err := s.Check(context.Background(), in)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	jobs, err := s.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(jobs) != p.Count() {
		t.Fatalf("Submit 回了 %d 条任务，期望 %d", len(jobs), p.Count())
	}
	return jobs
}

// waitTerminal 逐轮陪等到终态。
func waitTerminal(t *testing.T, s *Service, id string) *store.MediaJob {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		got, err := s.Wait(ctx, id)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if !store.MediaStatusActive(got.Status) {
			return got
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待条件超时")
}

func mustCreateJob(t *testing.T, st *store.Store, j store.MediaJob) {
	t.Helper()
	if err := st.CreateMediaJob(context.Background(), j); err != nil {
		t.Fatalf("CreateMediaJob: %v", err)
	}
}

const (
	tinyJPEG = "data:image/jpeg;base64,/9j/4AAQ"
	tinyMP4  = "data:video/mp4;base64,AAAAIGZ0eXBpc29t"
)

var videoDone = Result{Status: store.MediaStatusSucceeded, MediaURL: "https://example.invalid/v.mp4", MediaType: "video"}

// 平台排队中的视频任务由设备后台查到终态：页面只陪等，不需要人去按查询。
func TestQueuedJobIsPolledToCompletion(t *testing.T) {
	b := newQueuedBackend(2, videoDone)
	s, _ := newService(t, b)
	job := submit(t, s, Submission{Model: GrokVideoModel, Prompt: "海边日出", KeyID: 5, KeyDisplay: "sk_a…wxyz"})[0]
	if job.Status != store.MediaStatusRunning || job.Backend != BackendGrok || job.Provider != BackendGrok || job.Kind != store.ModelKindVideo ||
		job.AccountID != testAccountID || job.Operation != store.MediaOpGenerate || job.KeyID != 5 || job.KeyDisplay != "sk_a…wxyz" {
		t.Fatalf("受理时的任务 = %+v", job)
	}
	got := waitTerminal(t, s, job.ID)
	// 后端回的 AccountID 覆盖受理时钉的那一行。
	if got.Status != store.MediaStatusSucceeded || got.VendorID != "vendor-1" || got.MediaURL != videoDone.MediaURL || got.MediaType != "video" || got.AccountID != 2 {
		t.Fatalf("终态任务 = %+v", got)
	}
	if got.FinishedAt == nil {
		t.Fatalf("终态任务没有完成时间")
	}
	if n := b.refreshCount(); n != 3 {
		t.Fatalf("平台查询次数 = %d，期望 3（两次中间状态 + 一次终态）", n)
	}
}

// 查询出错只等下一轮，不把平台侧正常进行的生成判死。
func TestQueuedJobSurvivesRefreshErrors(t *testing.T) {
	b := newQueuedBackend(0, videoDone)
	b.setFailing(true)
	s, st := newService(t, b)
	job := submit(t, s, Submission{Model: GrokVideoModel, Prompt: "p"})[0]
	waitUntil(t, func() bool { return b.refreshCount() >= 3 })
	if cur, err := st.GetMediaJob(context.Background(), job.ID); err != nil || cur.Status != store.MediaStatusQueued || cur.VendorID != "vendor-1" {
		t.Fatalf("查询出错期间任务 = %+v / %v，期望仍是 queued", cur, err)
	}
	b.setFailing(false)
	waitUntil(t, func() bool {
		cur, err := st.GetMediaJob(context.Background(), job.ID)
		return err == nil && cur.Status == store.MediaStatusSucceeded
	})
}

// 进程重启：running 任务判失败，带平台任务 ID 的 queued 任务在注册后端时接着查；后端没
// 注册的那一种留着不动。
func TestQueuedJobResumesAfterRestart(t *testing.T) {
	st := openStore(t, t.TempDir())
	ctx := context.Background()
	mustCreateJob(t, st, store.MediaJob{ID: "running-1", Backend: BackendCodex, Kind: "image", Model: "m", Status: store.MediaStatusRunning})
	mustCreateJob(t, st, store.MediaJob{ID: "queued-1", Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusQueued, VendorID: "vendor-9", AccountID: 2})
	mustCreateJob(t, st, store.MediaJob{ID: "queued-2", Backend: BackendMinimaxVideo, Kind: "video", Model: "v", Status: store.MediaStatusQueued, VendorID: "vendor-10"})
	s := New(st, slog.New(slog.DiscardHandler), nil, "")
	if cur, err := st.GetMediaJob(ctx, "running-1"); err != nil || cur.Status != store.MediaStatusFailed || cur.Error != interruptedError || cur.FinishedAt == nil {
		t.Fatalf("重启后 running 任务 = %+v / %v", cur, err)
	}
	if cur, err := st.GetMediaJob(ctx, "queued-1"); err != nil || cur.Status != store.MediaStatusQueued {
		t.Fatalf("注册后端前 queued 任务不该被动：%+v / %v", cur, err)
	}
	b := newQueuedBackend(0, videoDone)
	s.SetBackends(b)
	waitUntil(t, func() bool {
		cur, err := st.GetMediaJob(ctx, "queued-1")
		return err == nil && cur.Status == store.MediaStatusSucceeded && cur.VendorID == "vendor-9"
	})
	if cur, err := st.GetMediaJob(ctx, "queued-2"); err != nil || cur.Status != store.MediaStatusQueued {
		t.Fatalf("后端没注册的 queued 任务不该被动：%+v / %v", cur, err)
	}
	if len(b.generated()) != 0 {
		t.Fatalf("接续查询不该重新发起生成")
	}
}

// 平台一直不出结果：到超时判失败并写原因，不永远排队。
func TestQueuedJobFailsOnTimeout(t *testing.T) {
	b := newQueuedBackend(1<<30, Result{})
	s, st := newService(t, b)
	job := store.MediaJob{ID: store.NewULID(time.Now()), Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusQueued, VendorID: "vendor-2"}
	mustCreateJob(t, st, job)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	s.pollAndSave(ctx, cancel, b, job, time.Now(), false)
	cur, err := st.GetMediaJob(context.Background(), job.ID)
	if err != nil || cur.Status != store.MediaStatusFailed || cur.Error != timeoutError {
		t.Fatalf("超时后任务 = %+v / %v", cur, err)
	}
}

// 同步生成超过整体上限：后端因 ctx 到期返回的错误落成超时原因，而不是一句 context 报错；
// 后端自己的错误原样落进失败原因。
func TestRunRecordsTimeoutAndBackendError(t *testing.T) {
	b := &fakeBackend{name: BackendCodex}
	b.generate = func(ctx context.Context, r Request) (Result, error) {
		if r.Prompt == "slow" {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		return Result{}, errors.New("平台拒绝了这次请求")
	}
	s, st := newService(t, b)
	p, err := s.Check(context.Background(), Submission{Model: "gpt-image-2", Prompt: "slow"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	job := store.MediaJob{ID: store.NewULID(time.Now()), Backend: BackendCodex, Kind: "image", Model: "gpt-image-2", Prompt: "slow", Status: store.MediaStatusRunning}
	mustCreateJob(t, st, job)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	s.run(ctx, cancel, p, job)
	if cur, err := st.GetMediaJob(context.Background(), job.ID); err != nil || cur.Status != store.MediaStatusFailed || cur.Error != timeoutError {
		t.Fatalf("超时后任务 = %+v / %v", cur, err)
	}

	failed := waitTerminal(t, s, submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p"})[0].ID)
	if failed.Status != store.MediaStatusFailed || failed.Error != "平台拒绝了这次请求" {
		t.Fatalf("后端报错的任务 = %+v", failed)
	}
	// 后端回了空状态：按失败收，不让任务停在 running。
	b.generate = func(context.Context, Request) (Result, error) { return Result{}, nil }
	if empty := waitTerminal(t, s, submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p"})[0].ID); empty.Status != store.MediaStatusFailed {
		t.Fatalf("空状态的任务 = %+v", empty)
	}
}

// 后台查询期间任务被删：查询停止，不再写回。
func TestQueuedJobPollStopsWhenDeleted(t *testing.T) {
	b := newQueuedBackend(1<<30, Result{})
	s, _ := newService(t, b)
	job := submit(t, s, Submission{Model: GrokVideoModel, Prompt: "p"})[0]
	waitUntil(t, func() bool { return b.refreshCount() >= 1 })
	if err := s.Delete(context.Background(), job.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	time.Sleep(5 * PollInterval)
	n := b.refreshCount()
	time.Sleep(5 * PollInterval)
	if b.refreshCount() != n {
		t.Fatalf("删除后平台查询仍在继续：%d → %d", n, b.refreshCount())
	}
	if _, err := s.Get(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除的任务被后台写回复活: %v", err)
	}
	if err := s.Delete(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除 = %v，期望 ErrNotFound", err)
	}
}

// 人工「查询平台任务」：平台中间状态不写回（行保持 queued）；平台到终态即写回；行已到终态
// 直接回当前行、不再问平台；平台报错是 ErrRefreshFailed；后端没注册是 ErrUnavailable。
func TestRefresh(t *testing.T) {
	ctx := context.Background()
	b := newQueuedBackend(1, Result{Status: store.MediaStatusFailed, Error: "平台审核未通过"})
	s, st := newService(t, b)
	job := store.MediaJob{ID: store.NewULID(time.Now()), Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusQueued}
	mustCreateJob(t, st, job) // 不带平台任务 ID：后台不会接续，只有人工查询在动它。
	got, err := s.Refresh(ctx, job)
	if err != nil || got.Status != store.MediaStatusQueued {
		t.Fatalf("Refresh = %+v / %v，期望仍是 queued", got, err)
	}
	if cur, _ := st.GetMediaJob(ctx, job.ID); cur.Status != store.MediaStatusQueued {
		t.Fatalf("库里状态被改成 %q", cur.Status)
	}
	if got, err = s.Refresh(ctx, job); err != nil || got.Status != store.MediaStatusFailed || got.Error != "平台审核未通过" {
		t.Fatalf("终态 Refresh = %+v / %v", got, err)
	}
	if cur, _ := st.GetMediaJob(ctx, job.ID); cur.Status != store.MediaStatusFailed || cur.FinishedAt == nil {
		t.Fatalf("终态未写回：%+v", cur)
	}
	n := b.refreshCount()
	if got, err = s.Refresh(ctx, job); err != nil || got.Status != store.MediaStatusFailed || b.refreshCount() != n {
		t.Fatalf("已到终态的 Refresh = %+v / %v（平台查询 %d → %d）", got, err, n, b.refreshCount())
	}

	other := store.MediaJob{ID: store.NewULID(time.Now()), Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusQueued}
	mustCreateJob(t, st, other)
	b.setFailing(true)
	if _, err := s.Refresh(ctx, other); !errors.Is(err, ErrRefreshFailed) {
		t.Fatalf("平台报错 = %v，期望 ErrRefreshFailed", err)
	}
	other.Backend = BackendArkVideo
	if _, err := s.Refresh(ctx, other); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("后端没注册 = %v，期望 ErrUnavailable", err)
	}
}

// 陪等：任务一落终态就回；页面关了（ctx 取消）返回 ctx.Err()、生成不中断；陪等期间任务被删
// 返回 ErrNotFound；不存在的任务直接 ErrNotFound。
func TestWait(t *testing.T) {
	release := make(chan struct{})
	b := &fakeBackend{name: BackendCodex}
	b.generate = func(context.Context, Request) (Result, error) {
		<-release
		return Result{Status: store.MediaStatusFailed, Error: "stub"}, nil
	}
	s, _ := newService(t, b)
	if _, err := s.Wait(context.Background(), "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的任务 = %v，期望 ErrNotFound", err)
	}
	first := submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p"})[0]
	second := submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p"})[0]

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := s.Wait(ctx, first.ID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx 到期的陪等 = %v", err)
	}
	if cur, err := s.Get(context.Background(), first.ID); err != nil || cur.Status != store.MediaStatusRunning {
		t.Fatalf("陪等中止不该影响生成：%+v / %v", cur, err)
	}

	deleted := make(chan error, 1)
	go func() {
		_, err := s.Wait(context.Background(), second.ID)
		deleted <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if err := s.Delete(context.Background(), second.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	select {
	case err := <-deleted:
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("陪等期间被删 = %v，期望 ErrNotFound", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("删除没有唤醒陪等")
	}

	type waited struct {
		job *store.MediaJob
		err error
	}
	done := make(chan waited, 1)
	go func() {
		job, err := s.Wait(context.Background(), first.ID)
		done <- waited{job, err}
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)
	select {
	case got := <-done:
		if got.err != nil || got.job.Status != store.MediaStatusFailed || got.job.Error != "stub" {
			t.Fatalf("终态 = %+v / %v", got.job, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("终态没有唤醒陪等")
	}
}

// 提交带参数时任务行记下参数与输入形态（不含媒体），后端收到同一份类型化参数、媒体输入与
// 归属；不带参数、不带输入的提交行里没有这两列。
func TestSubmitPersistsParamsAndInputShape(t *testing.T) {
	b := &fakeBackend{name: BackendGrok}
	s, st := newService(t, b)
	job := submit(t, s, Submission{Model: GrokVideoModel, Prompt: " walk ", KeyID: 9, KeyDisplay: "sk_k…9999",
		Inputs: Inputs{FirstFrame: "data:image/png;base64,iVBORw0KGgo=", ReferenceImages: []string{"https://example.invalid/a.png", "https://example.invalid/b.png"}},
		Params: map[string]any{"duration": json.Number("10"), "resolution": "720P", "generate_audio": false, "voices": []any{"Eve"}}})[0]
	waitTerminal(t, s, job.ID)
	reqs := b.generated()
	if len(reqs) != 1 {
		t.Fatalf("后端被调用 %d 次", len(reqs))
	}
	in := reqs[0]
	if d, ok := in.Params.Int("duration"); !ok || d != 10 || in.Params.String("resolution") != "720p" || in.Params.Bool("generate_audio") == nil || *in.Params.Bool("generate_audio") ||
		len(in.Params.Strings("voices")) != 1 || in.Params.Strings("voices")[0] != "eve" || in.Params.Bool("missing") != nil {
		t.Fatalf("后端收到的参数 = %+v", in.Params)
	}
	if in.JobID != job.ID || in.Model.ID != GrokVideoModel || in.Operation != store.MediaOpGenerate || in.Prompt != "walk" ||
		in.AccountID != testAccountID || in.KeyID != 9 || in.KeyDisplay != "sk_k…9999" || in.Inputs.FirstFrame == "" || len(in.Inputs.ReferenceImages) != 2 {
		t.Fatalf("后端收到的请求 = %+v", in)
	}
	cur, err := st.GetMediaJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetMediaJob: %v", err)
	}
	if cur.Params["duration"] != float64(10) || cur.Params["resolution"] != "720p" || cur.Params["generate_audio"] != false || len(cur.Params) != 4 {
		t.Fatalf("任务行的参数 = %+v", cur.Params)
	}
	if len(cur.Inputs) != 2 || cur.Inputs[store.MediaRoleFirstFrame] != 1 || cur.Inputs[store.MediaRoleReferenceImages] != 2 {
		t.Fatalf("任务行的输入形态 = %+v", cur.Inputs)
	}
	raw, _ := json.Marshal(cur)
	if strings.Contains(string(raw), "iVBORw0KGgo") || strings.Contains(string(raw), "example.invalid") {
		t.Fatalf("任务行不该带媒体输入: %s", raw)
	}
	if !strings.Contains(string(raw), `"params":{`) || !strings.Contains(string(raw), `"reference_images":2`) {
		t.Fatalf("任务行 JSON 应带参数与输入形态快照: %s", raw)
	}
	plain := submit(t, s, Submission{Model: "grok-imagine-image-2.0", Prompt: "p"})[0]
	if cur, _ := st.GetMediaJob(context.Background(), plain.ID); cur.Params != nil || cur.Inputs != nil {
		t.Fatalf("不带参数的任务行不该有快照: %+v / %+v", cur.Params, cur.Inputs)
	}
}

// 能力表表达不了的搭配由后端的 Validate 补判：它的 *InvalidError 原样成为受理的拒绝，
// 且收到的是规整后的请求。
func TestCheckRunsBackendValidate(t *testing.T) {
	b := &fakeBackend{name: BackendCodex}
	b.validate = func(r Request) error {
		if r.Params.String("background") == "transparent" && r.Params.String("output_format") == "webp" {
			return &InvalidError{Msg: "透明背景只配 PNG 格式"}
		}
		return nil
	}
	s, _ := newService(t, b)
	_, err := s.Check(context.Background(), Submission{Model: "gpt-image-2", Prompt: "p", Params: map[string]any{"background": " Transparent ", "output_format": "WEBP"}})
	var inv *InvalidError
	if !errors.As(err, &inv) || inv.Msg != "透明背景只配 PNG 格式" {
		t.Fatalf("Check = %v，期望后端的 InvalidError", err)
	}
	if _, err := s.Check(context.Background(), Submission{Model: "gpt-image-2", Prompt: "p", Params: map[string]any{"background": "transparent", "output_format": "png"}}); err != nil {
		t.Fatalf("合法搭配被拒: %v", err)
	}
}

// count=3 落成 3 条同批任务，各自独立到终态（部分失败不牵连其余）；count=1 不带批号。
func TestCountCreatesBatch(t *testing.T) {
	b := &fakeBackend{name: BackendGrok}
	var mu sync.Mutex
	calls := 0
	b.generate = func(context.Context, Request) (Result, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 2 {
			return Result{Status: store.MediaStatusFailed, Error: "平台审核未通过"}, nil
		}
		return Result{Status: store.MediaStatusSucceeded, MediaURL: "https://example.invalid/i.png", MediaType: "image"}, nil
	}
	s, _ := newService(t, b)
	jobs := submit(t, s, Submission{Model: "grok-imagine-image-2.0", Prompt: "p", Count: 3})
	ids := map[string]bool{}
	for _, j := range jobs {
		ids[j.ID] = true
		if j.BatchID == "" || j.BatchID != jobs[0].BatchID {
			t.Fatalf("同批任务的批号 = %q / %q", j.BatchID, jobs[0].BatchID)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("同批任务的 ID 重复: %v", ids)
	}
	status := map[string]int{}
	for _, j := range jobs {
		got := waitTerminal(t, s, j.ID)
		status[got.Status]++
		if got.BatchID != jobs[0].BatchID {
			t.Fatalf("批号没有落库: %+v", got)
		}
	}
	if status[store.MediaStatusSucceeded] != 2 || status[store.MediaStatusFailed] != 1 {
		t.Fatalf("同批任务的终态 = %v，期望 2 成功 1 失败", status)
	}
	seen := map[string]bool{}
	for _, r := range b.generated() {
		seen[r.JobID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("每条任务应各发起一次生成: %v", seen)
	}
	if single := submit(t, s, Submission{Model: "grok-imagine-image-2.0", Prompt: "p"}); single[0].BatchID != "" {
		t.Fatalf("单个提交不该带批号: %q", single[0].BatchID)
	}
}

// fakeAdmitter 记下每次准入调用；第 rejectAt 次（从 1 起）拒绝。
type fakeAdmitter struct {
	mu       sync.Mutex
	calls    []int64
	models   []string
	rejectAt int
}

func (a *fakeAdmitter) AdmitMediaJob(_ context.Context, keyID int64, m Model) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, keyID)
	a.models = append(a.models, m.ID)
	if len(a.calls) == a.rejectAt {
		return &RejectedError{Code: "rate_limited", Msg: "请求过于频繁", RetryAfterSec: 12}
	}
	return nil
}

// 准入闸：每个候选过一次；任一被拒则整批不建、后端不被调用。
func TestAdmitter(t *testing.T) {
	b := &fakeBackend{name: BackendGrok}
	s, st := newService(t, b)
	adm := &fakeAdmitter{}
	s.SetAdmitter(adm)
	jobs := submit(t, s, Submission{Model: "grok-imagine-image-2.0", Prompt: "p", Count: 2, KeyID: 3})
	if len(adm.calls) != 2 || adm.calls[0] != 3 || adm.calls[1] != 3 || adm.models[0] != "grok-imagine-image-2.0" {
		t.Fatalf("准入调用 = %v / %v，期望这把 Key 两次", adm.calls, adm.models)
	}
	for _, j := range jobs {
		waitTerminal(t, s, j.ID)
	}
	if _, err := s.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	adm.rejectAt = len(adm.calls) + 2 // 第二个候选被拒。
	p, err := s.Check(context.Background(), Submission{Model: "grok-imagine-image-2.0", Prompt: "p", Count: 2, KeyID: 3})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	got, err := s.Submit(context.Background(), p)
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Code != "rate_limited" || rej.RetryAfterSec != 12 || got != nil {
		t.Fatalf("Submit = %v / %v，期望 RejectedError", got, err)
	}
	if rows, err := st.ListPageMediaJobs(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("被拒的提交建了 %d 条任务 / %v", len(rows), err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(b.generated()); n != 2 {
		t.Fatalf("被拒的提交仍发起了生成：后端共被调用 %d 次", n)
	}
}

// 来源：缺省落成 page；studio / cli 的任务不进页面列表、只能凭 id 访问，归属信息原样落库；
// 清空页面任务（全部或某把 Key 的）不动它们。
func TestOriginScoping(t *testing.T) {
	ctx := context.Background()
	s, st := newService(t, &fakeBackend{name: BackendGrok})
	const model = "grok-imagine-image-2.0"
	page := submit(t, s, Submission{Model: model, Prompt: "p", KeyID: 1})[0]
	pageOther := submit(t, s, Submission{Model: model, Prompt: "p", KeyID: 2})[0]
	studio := submit(t, s, Submission{Model: model, Prompt: "p", KeyID: 1, Origin: store.MediaOriginStudio, Owner: `{"workspace_id":"W1","name":"cover"}`})[0]
	cli := submit(t, s, Submission{Model: model, Prompt: "p", KeyID: 1, Origin: store.MediaOriginCLI})[0]
	for _, j := range []store.MediaJob{page, pageOther, studio, cli} {
		waitTerminal(t, s, j.ID)
	}
	if page.Origin != store.MediaOriginPage || studio.Origin != store.MediaOriginStudio || cli.Origin != store.MediaOriginCLI {
		t.Fatalf("来源 = %q / %q / %q", page.Origin, studio.Origin, cli.Origin)
	}
	rows, err := st.ListPageMediaJobs(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("页面列表 = %d 条 / %v，期望只有两条 page 任务", len(rows), err)
	}
	for _, r := range rows {
		if r.ID == studio.ID || r.ID == cli.ID {
			t.Fatalf("页面列表里出现了 %s 任务", r.Origin)
		}
	}
	if mine, err := st.ListPageMediaJobsByKey(ctx, 1); err != nil || len(mine) != 1 || mine[0].ID != page.ID {
		t.Fatalf("持有人列表 = %+v / %v", mine, err)
	}
	if got, err := s.Get(ctx, studio.ID); err != nil || got.Origin != store.MediaOriginStudio || got.Owner != `{"workspace_id":"W1","name":"cover"}` {
		t.Fatalf("凭 id 读 studio 任务 = %+v / %v", got, err)
	}
	if raw, _ := json.Marshal(studio); strings.Contains(string(raw), "workspace_id") {
		t.Fatalf("归属信息不该进任务行 JSON: %s", raw)
	}

	if n, err := s.ClearKey(ctx, 1); err != nil || n != 1 {
		t.Fatalf("ClearKey = %d / %v，期望只删这把 Key 的页面任务", n, err)
	}
	if n, err := s.Clear(ctx); err != nil || n != 1 {
		t.Fatalf("Clear = %d / %v，期望只删剩下那条页面任务", n, err)
	}
	for _, id := range []string{studio.ID, cli.ID} {
		if _, err := s.Get(ctx, id); err != nil {
			t.Fatalf("清空页面任务把 %s 删了: %v", id, err)
		}
	}
}

// mediaServer 是取回平台媒体用的假平台（TLS）：/v.mp4 回视频字节，其余 404。
func mediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v.mp4" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("fake-mp4-bytes"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 平台 https 结果取回落盘：文件名 <ID>.<扩展名>，行里只记文件名、平台地址留作来源记录；
// 取回失败不改任务状态，行里仍有平台地址可回源。
func TestPersistFetchesPlatformMedia(t *testing.T) {
	srv := mediaServer(t)
	b := &fakeBackend{name: BackendGrok}
	b.generate = func(_ context.Context, r Request) (Result, error) {
		path := "/v.mp4"
		if r.Prompt == "gone" {
			path = "/gone.mp4"
		}
		return Result{Status: store.MediaStatusSucceeded, MediaURL: srv.URL + path, MediaType: "video"}, nil
	}
	s, _, dir := newDiskService(t, srv.Client(), b)
	got := waitTerminal(t, s, submit(t, s, Submission{Model: GrokVideoModel, Prompt: "p"})[0].ID)
	if got.Status != store.MediaStatusSucceeded || got.MediaFile != got.ID+".mp4" || got.MediaURL != srv.URL+"/v.mp4" || got.ThumbFile != "" {
		t.Fatalf("落盘后的任务 = %+v", got)
	}
	path := filepath.Join(dir, got.MediaFile)
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "fake-mp4-bytes" {
		t.Fatalf("结果文件 = %q / %v", raw, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("结果文件权限 = %v / %v，期望 0600", info.Mode().Perm(), err)
	}
	m, err := s.OpenMedia(context.Background(), got)
	if err != nil {
		t.Fatalf("OpenMedia: %v", err)
	}
	raw, _ := io.ReadAll(m.Content)
	m.Close()
	if string(raw) != "fake-mp4-bytes" || m.ContentType != "video/mp4" || m.Filename != got.MediaFile || m.Body != nil {
		t.Fatalf("OpenMedia = %+v / %q", m, raw)
	}
	// 本地文件经 ServeMedia：no-store、附件名、支持 Range。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/media", nil)
	req.Header.Set("Range", "bytes=5-7")
	if m, err = s.OpenMedia(context.Background(), got); err != nil {
		t.Fatalf("OpenMedia: %v", err)
	}
	ServeMedia(rec, req, m, "attachment")
	m.Close()
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "mp4" || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Content-Disposition") != "attachment; filename="+got.MediaFile {
		t.Fatalf("ServeMedia = %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}

	gone := waitTerminal(t, s, submit(t, s, Submission{Model: GrokVideoModel, Prompt: "gone"})[0].ID)
	if gone.Status != store.MediaStatusSucceeded || gone.MediaFile != "" || gone.MediaURL != srv.URL+"/gone.mp4" {
		t.Fatalf("取回失败的任务 = %+v", gone)
	}
	// 文件被外力删掉：ErrMediaMissing，不悄悄回源。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenMedia(context.Background(), got); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("结果文件缺失 = %v，期望 ErrMediaMissing", err)
	}
}

// 没有本地文件的任务按 MediaURL 回源：data URI 就地解码，平台 https 地址经出站客户端透传；
// 平台侧已失效时把任务标为 expired；没有媒体、地址不是 https 各有各的错误。
func TestOpenMediaFallsBackToSource(t *testing.T) {
	ctx := context.Background()
	srv := mediaServer(t)
	st := openStore(t, t.TempDir())
	s := New(st, slog.New(slog.DiscardHandler), srv.Client(), "")
	row := func(id, url string) *store.MediaJob {
		j := store.MediaJob{ID: id, Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusSucceeded, MediaURL: url}
		mustCreateJob(t, st, j)
		return &j
	}
	m, err := s.OpenMedia(ctx, row("HTTPS", srv.URL+"/v.mp4"))
	if err != nil {
		t.Fatalf("回源 https: %v", err)
	}
	raw, _ := io.ReadAll(m.Body)
	m.Close()
	if string(raw) != "fake-mp4-bytes" || m.ContentType != "video/mp4" || m.Filename != "HTTPS.mp4" || m.Content != nil {
		t.Fatalf("回源 https = %+v / %q", m, raw)
	}
	rec := httptest.NewRecorder()
	if m, err = s.OpenMedia(ctx, row("HTTPS2", srv.URL+"/v.mp4")); err != nil {
		t.Fatalf("回源 https: %v", err)
	}
	ServeMedia(rec, httptest.NewRequest(http.MethodGet, "/media", nil), m, "inline")
	m.Close()
	if rec.Body.String() != "fake-mp4-bytes" || rec.Header().Get("Content-Disposition") != "inline; filename=HTTPS2.mp4" {
		t.Fatalf("流式 ServeMedia = %q %v", rec.Body.String(), rec.Header())
	}

	m, err = s.OpenMedia(ctx, row("DATA", "data:image/webp;base64,"+base64.StdEncoding.EncodeToString([]byte("webp-bytes"))))
	if err != nil {
		t.Fatalf("回源 data URI: %v", err)
	}
	raw, _ = io.ReadAll(m.Body)
	if string(raw) != "webp-bytes" || m.ContentType != "image/webp" || m.Filename != "DATA.webp" {
		t.Fatalf("回源 data URI = %+v / %q", m, raw)
	}

	if _, err := s.OpenMedia(ctx, row("GONE", srv.URL+"/gone.mp4")); !errors.Is(err, ErrMediaExpired) {
		t.Fatalf("平台已失效 = %v，期望 ErrMediaExpired", err)
	}
	if cur, _ := st.GetMediaJob(ctx, "GONE"); cur.Status != store.MediaStatusExpired {
		t.Fatalf("平台已失效的任务状态 = %q，期望 expired", cur.Status)
	}
	if _, err := s.OpenMedia(ctx, row("HTTP", "http://example.invalid/v.mp4")); !errors.Is(err, ErrMediaExpired) {
		t.Fatalf("非 https 地址 = %v，期望 ErrMediaExpired", err)
	}
	if _, err := s.OpenMedia(ctx, row("BADDATA", "data:image/png;base64,***")); !errors.Is(err, ErrMediaExpired) {
		t.Fatalf("损坏的 data URI = %v，期望 ErrMediaExpired", err)
	}
	if _, err := s.OpenMedia(ctx, row("EMPTY", "")); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("没有媒体 = %v，期望 ErrMediaMissing", err)
	}
	if _, err := s.OpenThumb(row("NOTHUMB", "")); !errors.Is(err, ErrMediaMissing) {
		t.Fatalf("没有缩略图 = %v，期望 ErrMediaMissing", err)
	}
}

// 启动时清掉结果目录里没有任务行引用的文件（含 .part 半成品），被引用的结果与缩略图留着；
// 清空页面任务连带删掉它们的文件，别的来源的文件不动。
func TestSweepRemovesOrphans(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	mediaDir := filepath.Join(dir, MediaDirName)
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustCreateJob(t, st, store.MediaJob{ID: "PAGE", Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusSucceeded, MediaFile: "PAGE.mp4", ThumbFile: "PAGE" + ThumbExt})
	mustCreateJob(t, st, store.MediaJob{ID: "STUDIO", Origin: store.MediaOriginStudio, Backend: BackendGrok, Kind: "video", Model: "v", Status: store.MediaStatusSucceeded, MediaFile: "STUDIO.mp4"})
	for _, name := range []string{"PAGE.mp4", "PAGE" + ThumbExt, "STUDIO.mp4", "ORPHAN.png", ".ORPHAN.123.part"} {
		if err := os.WriteFile(filepath.Join(mediaDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, slog.New(slog.DiscardHandler), nil, dir)
	if s.MediaDir() != mediaDir {
		t.Fatalf("MediaDir = %q", s.MediaDir())
	}
	assertFiles(t, mediaDir, "PAGE.mp4", "PAGE"+ThumbExt, "STUDIO.mp4")
	if n, err := s.Clear(ctx); err != nil || n != 1 {
		t.Fatalf("Clear = %d / %v", n, err)
	}
	assertFiles(t, mediaDir, "STUDIO.mp4")
	if err := s.Delete(ctx, "STUDIO"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertFiles(t, mediaDir)
}

func assertFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("目录里的文件 = %v，期望 %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("目录里的文件 = %v，期望 %v", got, want)
		}
	}
}

// 搬运：成功任务 Adopt 后 sink 读到完整字节，任务行与结果文件（含缩略图）被删；sink 失败时
// 任务与文件原样留着；还没成功的任务 ErrNotReady；不存在 ErrNotFound。
func TestAdopt(t *testing.T) {
	ctx := context.Background()
	pngBytes := encodePNG(t, 64, 48)
	release := make(chan struct{})
	b := &fakeBackend{name: BackendCodex}
	b.generate = func(_ context.Context, r Request) (Result, error) {
		if r.Prompt == "slow" {
			<-release
		}
		return Result{Status: store.MediaStatusSucceeded, MediaURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes), MediaType: "image"}, nil
	}
	s, _, dir := newDiskService(t, nil, b)
	job := waitTerminal(t, s, submit(t, s, Submission{Model: "gpt-image-2", Prompt: "p", Origin: store.MediaOriginStudio, Owner: `{"name":"cover"}`})[0].ID)
	assertFiles(t, dir, job.ID+".png", job.ID+ThumbExt)

	sinkErr := errors.New("工作空间写入失败")
	if err := s.Adopt(ctx, job.ID, func(context.Context, *store.MediaJob, *Media) error { return sinkErr }); !errors.Is(err, sinkErr) {
		t.Fatalf("sink 失败的 Adopt = %v", err)
	}
	if _, err := s.Get(ctx, job.ID); err != nil {
		t.Fatalf("sink 失败后任务不该被删: %v", err)
	}
	assertFiles(t, dir, job.ID+".png", job.ID+ThumbExt)

	var got []byte
	err := s.Adopt(ctx, job.ID, func(_ context.Context, j *store.MediaJob, m *Media) error {
		if j.ID != job.ID || j.Owner != `{"name":"cover"}` || m.Filename != job.ID+".png" || m.ContentType != "image/png" {
			t.Errorf("sink 收到 %+v / %+v", j, m)
		}
		var err error
		got, err = io.ReadAll(m.Content)
		return err
	})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if string(got) != string(pngBytes) {
		t.Fatalf("sink 读到 %d 字节，期望 %d", len(got), len(pngBytes))
	}
	if _, err := s.Get(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("搬走后任务仍在: %v", err)
	}
	assertFiles(t, dir)
	called := false
	if err := s.Adopt(ctx, job.ID, func(context.Context, *store.MediaJob, *Media) error { called = true; return nil }); !errors.Is(err, ErrNotFound) || called {
		t.Fatalf("再次搬运 = %v（sink 被调用 %v），期望 ErrNotFound", err, called)
	}

	running := submit(t, s, Submission{Model: "gpt-image-2", Prompt: "slow"})[0]
	if err := s.Adopt(ctx, running.ID, func(context.Context, *store.MediaJob, *Media) error { called = true; return nil }); !errors.Is(err, ErrNotReady) || called {
		t.Fatalf("未完成任务的 Adopt = %v（sink 被调用 %v），期望 ErrNotReady", err, called)
	}
	// 放行并等它落完盘：不让后台写文件撞上测试目录的清理。
	close(release)
	waitTerminal(t, s, running.ID)
}

// finisherLog 记下收尾器收到的任务。
type finisherLog struct {
	mu   sync.Mutex
	jobs []store.MediaJob
}

func (f *finisherLog) finish(_ context.Context, j store.MediaJob) {
	f.mu.Lock()
	f.jobs = append(f.jobs, j)
	f.mu.Unlock()
}

func (f *finisherLog) byID() map[string]store.MediaJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]store.MediaJob{}
	for _, j := range f.jobs {
		out[j.ID] = j
	}
	return out
}

// 重启收尾：上次进程留下的、来源已到终态尚未搬走的任务（含这次启动才判失败的 running）
// 交给来源注册的收尾器；别的来源的不交；重启后接续查询的任务到终态时也交；进程内正常提交
// 的任务由调用方自己陪等，不交。
func TestFinisherReceivesLeftoverJobs(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	seed := func(id, origin, status, vendorID string) {
		mustCreateJob(t, st, store.MediaJob{ID: id, Origin: origin, Owner: `{"name":"` + id + `"}`, Backend: BackendGrok, Kind: "video", Model: "v",
			Status: status, VendorID: vendorID, MediaURL: "https://example.invalid/" + id + ".mp4"})
	}
	seed("studio-ok", store.MediaOriginStudio, store.MediaStatusSucceeded, "")
	seed("studio-failed", store.MediaOriginStudio, store.MediaStatusFailed, "")
	seed("studio-running", store.MediaOriginStudio, store.MediaStatusRunning, "")
	seed("studio-queued", store.MediaOriginStudio, store.MediaStatusQueued, "vendor-7")
	seed("page-ok", store.MediaOriginPage, store.MediaStatusSucceeded, "")
	seed("page-queued", store.MediaOriginPage, store.MediaStatusQueued, "vendor-8")
	seed("cli-ok", store.MediaOriginCLI, store.MediaStatusSucceeded, "")

	s := New(st, slog.New(slog.DiscardHandler), nil, "")
	s.SetEntitlements(entitleAll)
	s.SetFinisher(store.MediaOriginStudio, nil) // nil 不注册、不 panic。
	log := &finisherLog{}
	s.SetFinisher(store.MediaOriginStudio, log.finish)
	waitUntil(t, func() bool { return len(log.byID()) == 3 })
	got := log.byID()
	if got["studio-ok"].Status != store.MediaStatusSucceeded || got["studio-failed"].Status != store.MediaStatusFailed ||
		got["studio-running"].Status != store.MediaStatusFailed || got["studio-running"].Error != interruptedError {
		t.Fatalf("收尾器收到 = %+v", got)
	}
	if got["studio-ok"].Owner != `{"name":"studio-ok"}` {
		t.Fatalf("收尾器收到的归属 = %q", got["studio-ok"].Owner)
	}

	// 接续查询：带平台任务 ID 的 queued 任务到终态时交给收尾器；page 的那条照常到终态、不交。
	s.SetBackends(newQueuedBackend(1, videoDone))
	waitUntil(t, func() bool {
		cur, err := st.GetMediaJob(ctx, "page-queued")
		_, handed := log.byID()["studio-queued"]
		return handed && err == nil && cur.Status == store.MediaStatusSucceeded
	})
	if j := log.byID()["studio-queued"]; j.Status != store.MediaStatusSucceeded || j.MediaURL != videoDone.MediaURL || j.Origin != store.MediaOriginStudio {
		t.Fatalf("接续查询后收尾器收到 = %+v", j)
	}

	// 进程内正常提交的 studio 任务：调用方自己陪等、自己搬。
	live := waitTerminal(t, s, submit(t, s, Submission{Model: GrokVideoModel, Prompt: "p", Origin: store.MediaOriginStudio})[0].ID)
	time.Sleep(50 * time.Millisecond)
	got = log.byID()
	if len(got) != 4 {
		t.Fatalf("收尾器共收到 %d 条，期望 4 条：%+v", len(got), got)
	}
	for _, id := range []string{"page-ok", "page-queued", "cli-ok", live.ID} {
		if _, ok := got[id]; ok {
			t.Fatalf("任务 %s 不该交给 studio 的收尾器", id)
		}
	}
}
