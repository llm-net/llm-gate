package mediagen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

func availabilityOf(t *testing.T, s *Service, keyID int64) map[string]Availability {
	t.Helper()
	all, err := s.Available(context.Background(), keyID)
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	out := map[string]Availability{}
	for _, a := range all {
		if _, dup := out[a.ID]; dup {
			t.Fatalf("Available 里模型 %s 重复", a.ID)
		}
		out[a.ID] = a
	}
	return out
}

// wantUnavailable 断言 Check 以给定原因码拒绝，且 HTTPError 翻成给定状态。
func wantUnavailable(t *testing.T, s *Service, in Submission, code string, status int) {
	t.Helper()
	_, err := s.Check(context.Background(), in)
	var un *UnavailableError
	if !errors.As(err, &un) || un.Code != code || un.Msg == "" {
		t.Fatalf("Check(%s) = %v，期望 UnavailableError %s", in.Model, err, code)
	}
	if got, gotCode, msg, _, ok := HTTPError(err); !ok || got != status || gotCode != code || msg != un.Msg {
		t.Fatalf("HTTPError(%s) = %d %s %q %v，期望 %d", code, got, gotCode, msg, ok, status)
	}
}

// 订阅后端的可用性按这把 Key 的开发工具策略：未钉账号 = 未获授权（403），钉了但账号不可用
// = 当前不可用（409），后端没注册 = 生成服务不可用（503），不在表里 = 模型不存在（404）；
// 可用时带出钉死的账号行，受理时钉进任务。
func TestAvailabilityBySubscription(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	s := New(st, slog.New(slog.DiscardHandler), nil, "")
	// 一个后端都没接：整个服务不可用。
	if _, err := s.Check(ctx, Submission{Model: codexImageModel, Prompt: "p"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("没接后端的 Check = %v，期望 ErrUnavailable", err)
	}
	s.SetBackends(nil, &fakeBackend{name: BackendGrok}) // nil 后端被忽略；Codex 没注册。
	s.SetEntitlements(func(_ context.Context, keyID int64) (map[string]Entitlement, error) {
		switch keyID {
		case 1: // 钉了 Grok 且可用。
			return map[string]Entitlement{BackendGrok: {Configured: true, Available: true, AccountID: 41}, BackendCodex: {Configured: true, Available: true, AccountID: 42}}, nil
		case 2: // 钉了 Grok，但账号停用 / 失效。
			return map[string]Entitlement{BackendGrok: {Configured: true, AccountID: 41}}, nil
		case 3: // 什么都没钉。
			return nil, nil
		}
		return nil, errors.New("读取开发工具策略失败")
	})

	got := availabilityOf(t, s, 1)
	if len(got) != len(Presets()) {
		t.Fatalf("Available 回了 %d 项，期望恰为 %d 个预设", len(got), len(Presets()))
	}
	for _, m := range Presets() {
		a := got[m.ID]
		switch m.Backend {
		case BackendGrok:
			if !a.Available || a.AccountID != 41 || a.ReasonCode != "" || a.Reason != "" || a.Backend != BackendGrok || len(a.Operations) == 0 {
				t.Fatalf("Grok 模型 %s = %+v", m.ID, a)
			}
		default:
			if a.Available || a.ReasonCode != ReasonBackendUnavailable || a.AccountID != 0 {
				t.Fatalf("后端没注册的模型 %s = %+v", m.ID, a)
			}
		}
	}
	if a := availabilityOf(t, s, 2)[GrokVideoModel]; a.Available || a.ReasonCode != ReasonAgentNotConfigured || a.AccountID != 0 {
		t.Fatalf("账号不可用 = %+v", a)
	}
	if a := availabilityOf(t, s, 3)[GrokVideoModel]; a.Available || a.ReasonCode != ReasonSubscriptionNotAllowed {
		t.Fatalf("未钉账号 = %+v", a)
	}

	wantUnavailable(t, s, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 3}, ReasonSubscriptionNotAllowed, http.StatusForbidden)
	wantUnavailable(t, s, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 2}, ReasonAgentNotConfigured, http.StatusConflict)
	wantUnavailable(t, s, Submission{Model: codexImageModel, Prompt: "p", KeyID: 1}, ReasonBackendUnavailable, http.StatusServiceUnavailable)
	// 可用性先于参数校验：没授权的 Key 拿不到能力表的校验细节。
	wantUnavailable(t, s, Submission{Model: GrokVideoModel, KeyID: 3, Params: map[string]any{"fps": 1}}, ReasonSubscriptionNotAllowed, http.StatusForbidden)

	if _, err := s.Check(ctx, Submission{Model: "no-such-model", Prompt: "p", KeyID: 1}); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("未知模型 = %v，期望 ErrModelNotFound", err)
	}
	if _, err := s.Resolve(ctx, 1, "no-such-model"); !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("Resolve 未知模型 = %v", err)
	}
	if _, err := s.Check(ctx, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 9}); err == nil || err.Error() != "读取开发工具策略失败" {
		t.Fatalf("授权解析出错应原样上抛: %v", err)
	}
	// 模型名与候选数在查能力表之前就判。
	_, err := s.Check(ctx, Submission{Prompt: "p", KeyID: 1})
	wantInvalid(t, err, "请选择模型")
	_, err = s.Check(ctx, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 1, Count: RunningPerKey + 1})
	wantInvalid(t, err, "候选数须在 1–3 之间")
	_, err = s.Check(ctx, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 1, Params: map[string]any{"fps": 1}})
	wantInvalid(t, err, "不接受参数 fps")

	p, err := s.Check(ctx, Submission{Model: " " + GrokVideoModel + " ", Prompt: "p", KeyID: 1, Count: 2})
	if err != nil || p.Count() != 2 || p.Model().ID != GrokVideoModel {
		t.Fatalf("Check = %+v / %v", p, err)
	}
	jobs, err := s.Submit(ctx, p)
	if err != nil || len(jobs) != 2 || jobs[0].AccountID != 41 || jobs[0].Model != GrokVideoModel {
		t.Fatalf("Submit = %+v / %v", jobs, err)
	}
	for _, j := range jobs {
		waitTerminal(t, s, j.ID)
	}
	// 没注入授权解析器（窄进程）：订阅一律按未获授权。
	bare := New(openStore(t, t.TempDir()), slog.New(slog.DiscardHandler), nil, "")
	bare.SetBackends(&fakeBackend{name: BackendGrok})
	wantUnavailable(t, bare, Submission{Model: GrokVideoModel, Prompt: "p"}, ReasonSubscriptionNotAllowed, http.StatusForbidden)
}

// 并发闸在 Submit 里与落库同一把锁：一把 Key 同时发起多次提交，合计不越过 RunningPerKey，
// 多出来的整批得 BusyError；别的 Key 不受影响。
func TestSubmitRunningPerKeyIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := New(openStore(t, t.TempDir()), slog.New(slog.DiscardHandler), nil, "")
	release := make(chan struct{})
	s.SetBackends(&fakeBackend{name: BackendGrok, generate: func(ctx context.Context, _ Request) (Result, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Status: store.MediaStatusFailed, Error: "stub"}, nil
	}})
	s.SetEntitlements(func(_ context.Context, keyID int64) (map[string]Entitlement, error) {
		return map[string]Entitlement{BackendGrok: {Configured: true, Available: true, AccountID: 41}}, nil
	})
	const tries = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok, busy int
		others   []error
	)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.Check(ctx, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 1})
			if err == nil {
				_, err = s.Submit(ctx, p)
			}
			var be *BusyError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.As(err, &be):
				busy++
			default:
				others = append(others, err)
			}
		}()
	}
	wg.Wait()
	if ok != RunningPerKey || busy != tries-RunningPerKey || len(others) != 0 {
		t.Fatalf("并发提交 %d 次：受理 %d、拒绝 %d、其它 %v，期望受理 %d", tries, ok, busy, others, RunningPerKey)
	}
	p, err := s.Check(ctx, Submission{Model: GrokVideoModel, Prompt: "p", KeyID: 2})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := s.Submit(ctx, p); err != nil {
		t.Fatalf("别的 Key 不受这把 Key 的名额影响: %v", err)
	}
	close(release)
}

// HTTPError 的状态码映射：两组任务端点共用这一份。
func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
		retry  int
	}{
		{&InvalidError{Msg: "提示词不能为空"}, http.StatusBadRequest, "media_invalid", 0},
		{fmt.Errorf("受理: %w", &InvalidError{Msg: "x"}), http.StatusBadRequest, "media_invalid", 0},
		{&RejectedError{Code: "budget_exceeded", Msg: "预算已用完", RetryAfterSec: 30}, http.StatusTooManyRequests, "budget_exceeded", 30},
		{&BusyError{Running: 3}, http.StatusTooManyRequests, "media_busy", 0},
		{&UnavailableError{Code: ReasonSubscriptionNotAllowed, Msg: "x"}, http.StatusForbidden, ReasonSubscriptionNotAllowed, 0},
		{&UnavailableError{Code: ReasonAgentNotConfigured, Msg: "x"}, http.StatusConflict, ReasonAgentNotConfigured, 0},
		{&UnavailableError{Code: ReasonModelNotFound, Msg: "x"}, http.StatusNotFound, ReasonModelNotFound, 0},
		{&UnavailableError{Code: ReasonBackendUnavailable, Msg: "x"}, http.StatusServiceUnavailable, ReasonBackendUnavailable, 0},
		{&UnavailableError{Code: "key_disabled", Msg: "x"}, http.StatusConflict, "key_disabled", 0},
		{ErrModelNotFound, http.StatusNotFound, ReasonModelNotFound, 0},
		{ErrUnavailable, http.StatusServiceUnavailable, "media_unavailable", 0},
		{ErrNotFound, http.StatusNotFound, "not_found", 0},
		{ErrMediaMissing, http.StatusNotFound, "media_not_found", 0},
		{ErrNotReady, http.StatusNotFound, "media_not_found", 0},
		{ErrMediaExpired, http.StatusGone, "media_expired", 0},
		{fmt.Errorf("%w: %w", ErrRefreshFailed, errors.New("上游 502")), http.StatusBadGateway, "media_refresh_failed", 0},
		{errors.New("磁盘写满"), http.StatusInternalServerError, "media_save_failed", 0},
	}
	for _, tc := range cases {
		status, code, msg, retry, ok := HTTPError(tc.err)
		if !ok || status != tc.status || code != tc.code || retry != tc.retry || msg == "" {
			t.Fatalf("HTTPError(%v) = %d %s %q %d %v，期望 %d %s", tc.err, status, code, msg, retry, ok, tc.status, tc.code)
		}
	}
	// 认不出的错误不把内部原因透给客户端。
	if _, _, msg, _, _ := HTTPError(errors.New("磁盘写满")); msg != "保存生成任务失败" {
		t.Fatalf("兜底错误的文案 = %q", msg)
	}
	// 调用方断开：不必写响应。
	for _, err := range []error{context.Canceled, fmt.Errorf("陪等: %w", context.DeadlineExceeded)} {
		if _, _, _, _, ok := HTTPError(err); ok {
			t.Fatalf("HTTPError(%v) 应报告不必写响应", err)
		}
	}
}

// 厂商面模型（按量）：管理员建的共享 API 模型行按来源的上游类型套能力基线；可用性按这把
// Key 的可用模型范围与模型 / 来源 / 上游三层启停——停用是 model_not_found，范围外与不存在
// 一样不进结果；不看订阅授权。
func TestAvailabilityOfVendorModels(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, t.TempDir())
	s := New(st, slog.New(slog.DiscardHandler), nil, "")
	backend := &fakeBackend{name: BackendMinimaxVideo}
	s.SetBackends(backend) // 订阅后端一个没接，也没注入授权解析器。

	up, err := st.CreateUpstream(ctx, "mm", config.UpstreamMinimax, "sk-fake-minimax-0000", "")
	if err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	text, err := st.CreateUpstream(ctx, "ds", config.UpstreamDeepseek, "sk-fake-deepseek-0000", "")
	if err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	model, err := st.CreateModel(ctx, "hailuo-video", store.ModelKindVideo, "")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	src, err := st.CreateModelSource(ctx, model.ID, up.ID, "MiniMax-Hailuo-02", 0)
	if err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	// 陪衬：文本模型、没有来源的视频模型、来源不服务视频面的视频模型都不进能力表。
	chat, err := st.CreateModel(ctx, "deepseek-chat", store.ModelKindText, "")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := st.CreateModelSource(ctx, chat.ID, text.ID, "", 0); err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	if _, err := st.CreateModel(ctx, "video-without-source", store.ModelKindVideo, ""); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	stray, err := st.CreateModel(ctx, "video-on-text-upstream", store.ModelKindVideo, "")
	if err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if _, err := st.CreateModelSource(ctx, stray.ID, text.ID, "", 0); err != nil {
		t.Fatalf("CreateModelSource: %v", err)
	}
	key, err := st.CreateAPIKey(ctx, "studio", "digest-mediagen-vendor", "sk_abcd", "wxyz", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got := availabilityOf(t, s, key.ID)
	if len(got) != len(Presets())+1 {
		t.Fatalf("Available 回了 %d 项，期望预设 + 1 个厂商面模型: %v", len(got), got)
	}
	a := got["hailuo-video"]
	if !a.Available || a.Backend != BackendMinimaxVideo || a.Billing != BillingMetered || a.Kind != store.ModelKindVideo || a.AccountID != 0 || a.ReasonCode != "" {
		t.Fatalf("厂商面模型 = %+v", a)
	}
	if op, ok := a.Operation(store.MediaOpGenerate); !ok || len(op.Params) == 0 {
		t.Fatalf("厂商面模型没有套上能力基线: %+v", a)
	}
	if got[GrokVideoModel].ReasonCode != ReasonBackendUnavailable {
		t.Fatalf("订阅后端没接时预设 = %+v", got[GrokVideoModel])
	}
	catalog, err := s.Catalog(ctx)
	if err != nil || len(catalog) != len(Presets())+1 || catalog[len(catalog)-1].ID != "hailuo-video" {
		t.Fatalf("Catalog = %+v / %v", catalog, err)
	}

	// 受理：按基线校验（必填参数），任务记按量后端、不钉订阅账号。
	sub := Submission{Model: "hailuo-video", Prompt: "p", KeyID: key.ID, Params: map[string]any{"resolution": "768P", "duration": 6}}
	_, err = s.Check(ctx, Submission{Model: "hailuo-video", Prompt: "p", KeyID: key.ID})
	wantInvalid(t, err, "缺少参数 resolution")
	job := waitTerminal(t, s, submit(t, s, sub)[0].ID)
	if job.Backend != BackendMinimaxVideo || job.Provider != BackendMinimaxVideo || job.AccountID != 0 || job.Kind != store.ModelKindVideo || job.Model != "hailuo-video" {
		t.Fatalf("按量任务 = %+v", job)
	}
	if reqs := backend.generated(); len(reqs) != 1 || reqs[0].Model.Billing != BillingMetered || reqs[0].Params.String("resolution") != "768P" {
		t.Fatalf("按量后端收到 = %+v", reqs)
	}

	// 三层启停：任一层停用都是 model_not_found（仍列在表里、标为不可用），恢复后重新可用。
	layers := []struct {
		name string
		set  func(disabled bool) error
	}{
		{"来源", func(d bool) error { return st.SetModelSourceDisabled(ctx, src.ID, d) }},
		{"上游", func(d bool) error { return st.SetUpstreamDisabled(ctx, up.ID, d) }},
		{"模型", func(d bool) error { return st.SetModelDisabled(ctx, model.ID, d) }},
	}
	for _, layer := range layers {
		if err := layer.set(true); err != nil {
			t.Fatalf("停用%s: %v", layer.name, err)
		}
		if a := availabilityOf(t, s, key.ID)["hailuo-video"]; a.Available || a.ReasonCode != ReasonModelNotFound || a.Backend != BackendMinimaxVideo {
			t.Fatalf("停用%s后 = %+v", layer.name, a)
		}
		wantUnavailable(t, s, sub, ReasonModelNotFound, http.StatusNotFound)
		if err := layer.set(false); err != nil {
			t.Fatalf("恢复%s: %v", layer.name, err)
		}
		if a := availabilityOf(t, s, key.ID)["hailuo-video"]; !a.Available {
			t.Fatalf("恢复%s后 = %+v", layer.name, a)
		}
	}

	// 可用模型范围：restricted 且不含它 → 与不存在一样；含它 → 可用；别的 Key 不受影响。
	if _, _, err := st.ReplaceKeyAPIModelConfig(ctx, store.KeyAPIModelConfig{KeyID: key.ID, Restricted: true, ModelIDs: []int64{chat.ID}}); err != nil {
		t.Fatalf("ReplaceKeyAPIModelConfig: %v", err)
	}
	if _, listed := availabilityOf(t, s, key.ID)["hailuo-video"]; listed {
		t.Fatal("范围外的模型不该出现在 Available 里")
	}
	_, err = s.Check(ctx, sub)
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("范围外的模型 = %v，期望 ErrModelNotFound", err)
	}
	if status, code, _, _, _ := HTTPError(err); status != http.StatusNotFound || code != ReasonModelNotFound {
		t.Fatalf("范围外的模型 HTTPError = %d %s", status, code)
	}
	other, err := st.CreateAPIKey(ctx, "other", "digest-mediagen-vendor-2", "sk_efgh", "stuv", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if a := availabilityOf(t, s, other.ID)["hailuo-video"]; !a.Available {
		t.Fatalf("不限制的 Key = %+v", a)
	}
	if _, _, err := st.ReplaceKeyAPIModelConfig(ctx, store.KeyAPIModelConfig{KeyID: key.ID, Restricted: true, ModelIDs: []int64{chat.ID, model.ID}}); err != nil {
		t.Fatalf("ReplaceKeyAPIModelConfig: %v", err)
	}
	if a := availabilityOf(t, s, key.ID)["hailuo-video"]; !a.Available {
		t.Fatalf("范围内的模型 = %+v", a)
	}

	// 后端没注册的厂商面：列出但不可用（503）。
	bare := New(st, slog.New(slog.DiscardHandler), nil, "")
	bare.SetBackends(&fakeBackend{name: BackendGrok})
	wantUnavailable(t, bare, sub, ReasonBackendUnavailable, http.StatusServiceUnavailable)
}
