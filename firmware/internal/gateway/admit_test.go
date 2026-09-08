// admit_test.go 是 iteration-9 Phase 4 的可执行验收（网关这一半）：预算准入
// 在四个消费入口上的 429 形态、Retry-After 头、账本里的 rejected 记号，以及
// 「哪些路不过准入」这条边界。
//
// 分工：拒绝的**数值**（检查序、滑窗、Retry-After 到底几秒）钉在
// internal/usage/admit_test.go——那里时钟可注入；这里钉的是 HTTP 形态与接线。
package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// denyAll 把假计量出口切成"一律拒绝"，并给出一个可断言的 Retry-After。
func denyAll(fm *fakeMeter, reason string, retryAfter int) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.deny = &usage.Decision{Reason: reason, RetryAfterSec: retryAfter}
}

// forbiddenInErrorText 是准入错误文案里绝不允许出现的词：金额、限额数值与
// 上游身份。客户端只该知道"被配额挡住了、什么时候能再来"。
var forbiddenInErrorText = []string{
	meterUpstream, "budget_day", "budget_month", "micro", "微元", "元",
	"upstream", "deepseek", "ark", "minimax",
}

func assertNoLeak(t *testing.T, where, text string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, w := range forbiddenInErrorText {
		if strings.Contains(low, strings.ToLower(w)) {
			t.Errorf("%s 的 429 文案泄露了 %q: %s", where, w, text)
		}
	}
}

// 四个消费入口在被预算拦下时的形态：429 + Retry-After + 各自入口风格的
// 错误体；一个字节都没打到上游；账本里记一笔 rejected（不是 error）。
func TestAdmitRejectionShapes(t *testing.T) {
	t.Run("chat 走 OpenAI 形", func(t *testing.T) {
		e, stub, fm := newMeterEnv(t, chatReply(meterUpModel))
		denyAll(fm, usage.RejectKeyBudgetDay, 3600)

		w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
			fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("状态码 = %d，期望 429；body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "3600" {
			t.Errorf("Retry-After = %q，期望 3600", got)
		}
		// 429 的 error.type 是 rate_limit_error（外审遗留：归进
		// invalid_request_error 会让按 type 分支的客户端把「你超额了」读成
		// 永久性请求错误，从而丢掉 Retry-After 指示的那次重试）。
		typ, code, msg := decodeError(t, w)
		if typ != "rate_limit_error" || code != "budget_exceeded" {
			t.Errorf("错误体 type=%q code=%q，期望 rate_limit_error/budget_exceeded", typ, code)
		}
		assertNoLeak(t, "chat", msg)
		if stub.count() != 0 {
			t.Errorf("被拒的请求打了上游 %d 次", stub.count())
		}
		assertRejectedSample(t, fm, usage.EntryChat, usage.RejectKeyBudgetDay)
	})

	t.Run("messages 走 Anthropic 形", func(t *testing.T) {
		e, stub, fm := newMeterEnv(t, chatReply(meterUpModel))
		denyAll(fm, usage.RejectRPM, 17)

		w := do(e.h, "POST", "/v1/messages", messagesAuth,
			fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("状态码 = %d，期望 429；body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "17" {
			t.Errorf("Retry-After = %q，期望 17", got)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
		}
		if body.Type != "error" || body.Error.Type != "rate_limit_error" {
			t.Errorf("Anthropic 错误体 = %+v，期望 type=error error.type=rate_limit_error", body)
		}
		// Anthropic 形没有 code 字段：档位不外显。
		if strings.Contains(w.Body.String(), `"code"`) {
			t.Errorf("Anthropic 错误体不该带 code: %s", w.Body.String())
		}
		assertNoLeak(t, "messages", body.Error.Message)
		if stub.count() != 0 {
			t.Errorf("被拒的请求打了上游 %d 次", stub.count())
		}
		assertRejectedSample(t, fm, usage.EntryMessages, usage.RejectRPM)
	})

	t.Run("video 提交走 MiniMax v2 形", func(t *testing.T) {
		e := newVideoEnv(t)
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		denyAll(fm, usage.RejectKeyBudgetMonth, 900)

		w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
			fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}],"resolution":"768P","duration":4}`, videoModel))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("状态码 = %d，期望 429；body: %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "900" {
			t.Errorf("Retry-After = %q，期望 900", got)
		}
		errType, msg := decodeMinimaxError(t, w)
		if errType != "budget_exceeded" {
			t.Errorf("error.type = %q，期望 budget_exceeded", errType)
		}
		assertNoLeak(t, "video", msg)
		if creates, _, _ := e.mm.counts(); creates != 0 {
			t.Errorf("被拒的提交打了厂商 %d 次——那会产生一个既扣费又无主的任务", creates)
		}
		assertRejectedSample(t, fm, usage.EntryVideo, usage.RejectKeyBudgetMonth)
	})
}

// assertRejectedSample 断言账本里恰好记了一笔被拒的样本：模型维度在（准入
// 排在 beginEntry 之后），rejected 记号在，token 与上游名为空。
func assertRejectedSample(t *testing.T, fm *fakeMeter, entry, reason string) {
	t.Helper()
	s := fm.only(t)
	if !s.Rejected {
		t.Errorf("被拒的请求应打 rejected 记号: %+v", s)
	}
	if s.RejectReason != reason {
		t.Errorf("拒绝档位 = %q，期望 %q", s.RejectReason, reason)
	}
	if s.Entry != entry {
		t.Errorf("入口 = %q，期望 %q", s.Entry, entry)
	}
	if s.ModelName == "" {
		t.Error("被拒的样本应带模型维度（准入排在 beginEntry 之后）")
	}
	if s.Status != http.StatusTooManyRequests {
		t.Errorf("状态码 = %d，期望 429", s.Status)
	}
	if (s.Tokens != usage.Tokens{}) {
		t.Errorf("被拒的请求没到上游，不该有 token: %+v", s.Tokens)
	}
	if s.UpstreamName != "" {
		t.Errorf("被拒的请求没走到选路，上游名应为空，实际 %q", s.UpstreamName)
	}
}

// 「只有新消费过闸」这条边界：count_tokens、/v1/models 与任务的查询/列表/
// 下载/取消在计量出口一律拒绝的情况下照常工作——查已经花掉的钱不该被预算拦。
func TestAdmitSkipsReadPaths(t *testing.T) {
	t.Run("count_tokens 与 models", func(t *testing.T) {
		e, _, fm := newMeterEnv(t, jsonReply(http.StatusOK, `{"input_tokens":7}`))
		denyAll(fm, usage.RejectKeyBudgetDay, 60)

		w := do(e.h, "POST", "/v1/messages/count_tokens", messagesAuth,
			fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel))
		if w.Code != http.StatusOK {
			t.Errorf("count_tokens 状态码 = %d，期望 200（不过准入）；body: %s", w.Code, w.Body.String())
		}
		w = do(e.h, "GET", "/v1/models", chatAuth, "")
		if w.Code != http.StatusOK {
			t.Errorf("/v1/models 状态码 = %d，期望 200（不过准入）", w.Code)
		}
	})

	t.Run("任务查询/取消", func(t *testing.T) {
		e := newVideoEnv(t)
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		// 先在放行状态下提交一个任务，再把闸门关死。
		id := submitVideo(t, e)
		denyAll(fm, usage.RejectKeyBudgetDay, 60)

		for _, tc := range []struct{ method, path string }{
			{"GET", "/minimax/v2/query/video_generation/" + id},
			{"DELETE", "/minimax/v2/video_generation/" + id},
		} {
			w := do(e.h, tc.method, tc.path, chatAuth, "")
			if w.Code == http.StatusTooManyRequests {
				t.Errorf("%s %s 被预算拦了——已提交的任务必须照常查/取消；body: %s",
					tc.method, tc.path, w.Body.String())
			}
		}
	})
}

// 真 Meter 端到端：日预算为 0 时新请求立刻 429；增加按量额度后继续放行并按
// 最终金额扣减，额度耗尽再次 429；清除预算则恢复不限。预算与按量额度都随
// 鉴权点查带回，无缓存失效问题。
func TestAdmitRealMeterBudgetTakesEffectImmediately(t *testing.T) {
	e, stub, _ := newMeterEnv(t, jsonReply(http.StatusOK, fmt.Sprintf(
		`{"id":"chatcmpl-1","model":%q,"choices":[],"usage":{"prompt_tokens":1000000,"completion_tokens":0,"total_tokens":1000000}}`,
		meterUpModel)))
	m := usage.NewMeter(e.st, 31, nil, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)
	ctx := t.Context()

	keys, err := e.st.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (%d 条)", err, len(keys))
	}
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel)

	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body); w.Code != http.StatusOK {
		t.Fatalf("未设限额时应放行，状态码 = %d", w.Code)
	}
	zero := int64(0)
	if err := e.st.SetAPIKeyLimits(ctx, keys[0].ID, &zero, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits: %v", err)
	}
	before := stub.count()
	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("零额度后应 429，状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if ra, err := strconv.Atoi(w.Header().Get("Retry-After")); err != nil || ra < 1 || ra > 86400 {
		t.Errorf("Retry-After = %q，期望 1..86400 秒内的整数", w.Header().Get("Retry-After"))
	}
	if stub.count() != before {
		t.Error("被拒的请求不该打上游")
	}

	// 每次请求 100 万输入 token，目录价 3 元；4 元按量额度可放两笔，第二笔
	// 整笔扣穿到 0，第三笔在准入处被拒。
	if remaining, err := e.st.AdjustAPIKeyMeteredAllowance(ctx, keys[0].ID, 4_000_000); err != nil || remaining != 4_000_000 {
		t.Fatalf("增加按量额度 = (%d, %v)，期望 4000000", remaining, err)
	}
	for i := 0; i < 2; i++ {
		if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body); w.Code != http.StatusOK {
			t.Fatalf("按量额度第 %d 笔应放行，状态码 = %d；body: %s", i+1, w.Code, w.Body.String())
		}
	}
	if pending := m.Spend(keys[0].ID).MeteredAllowancePendingMicro; pending != 6_000_000 {
		t.Fatalf("按量额度待扣 = %d，期望 6000000", pending)
	}
	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("按量额度耗尽后应再次 429，状态码 = %d；body: %s", w.Code, w.Body.String())
	}

	// 放开限额：下一个请求就通过（无缓存层）。
	if err := e.st.SetAPIKeyLimits(ctx, keys[0].ID, nil, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits(清除): %v", err)
	}
	if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body); w.Code != http.StatusOK {
		t.Fatalf("清除限额后应立即放行，状态码 = %d；body: %s", w.Code, w.Body.String())
	}
}

// 真 Meter 端到端：RPM 上限。第 N+1 个请求 429 且带 rate_limited。
func TestAdmitRealMeterRPM(t *testing.T) {
	e, _, _ := newMeterEnv(t, chatReply(meterUpModel))
	m := usage.NewMeter(e.st, 31, nil, nil, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)
	ctx := t.Context()

	keys, err := e.st.ListAPIKeys(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListAPIKeys: %v (%d 条)", err, len(keys))
	}
	rpm := int64(2)
	if err := e.st.SetAPIKeyLimits(ctx, keys[0].ID, nil, nil, nil, &rpm); err != nil {
		t.Fatalf("SetAPIKeyLimits: %v", err)
	}
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, meterModel)
	for i := 0; i < 2; i++ {
		if w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body); w.Code != http.StatusOK {
			t.Fatalf("第 %d 个请求应放行，状态码 = %d", i+1, w.Code)
		}
	}
	w := do(e.h, "POST", "/v1/chat/completions", chatAuth, body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超 RPM 应 429，状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "rate_limited" {
		t.Errorf("错误码 = %q，期望 rate_limited", code)
	}
	if ra := w.Header().Get("Retry-After"); ra == "" {
		t.Error("RPM 拒绝必须带 Retry-After——客户端要靠它知道什么时候能再来")
	}
}

// 未装配计量出口时准入整条跳过：计量是旁路能力，它没起来不该让网关少转发
// 一个字节（也不该凭空拦请求）。
func TestAdmitSkippedWithoutMeter(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, chatReply(""))
	up := dbUpstream(t, e.st, "no-meter-up", config.UpstreamMock, "sk-not-real", stub.url)
	dbSource(t, e.st, dbModel(t, e.st, "no-meter-model"), up, "", 100)

	w := do(e.h, "POST", "/v1/chat/completions", chatAuth,
		`{"model":"no-meter-model","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("未装配计量时应照常转发，状态码 = %d；body: %s", w.Code, w.Body.String())
	}
}

// ---- 图片入口的准入环境 ----

const admitImageModel = "admit-image-model"

// newImageAdmitEnv 装配一个 kind=image 的模型（ark 型上游 + base_url 覆盖：
// mock 类型不服务图片协议）。
func newImageAdmitEnv(t *testing.T) (*routeEnv, *stubUpstream, *fakeMeter) {
	t.Helper()
	e := newRouteEnv(t)
	stub := newStub(t, jsonReply(http.StatusOK, `{"model":"x","data":[{"url":"http://x/1.png"}]}`))
	up := dbUpstream(t, e.st, "ark-image-admit", config.UpstreamArk, "sk-ark-not-real", stub.url)
	dbSource(t, e.st, dbKindModel(t, e.st, admitImageModel, store.ModelKindImage), up, "seedream", 100)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	return e, stub, fm
}
