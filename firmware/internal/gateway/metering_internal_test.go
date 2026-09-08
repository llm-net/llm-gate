// metering_internal_test.go 直测两件从 HTTP 层不好驱动的包内口径：
// 长流的 30s 中间结算（等半分钟不现实，用例直接把上次结算时刻拨回去），
// 以及收尾时对临时金额的原额冲正。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

func TestCanceledCommittedResponsePreservesUsage(t *testing.T) {
	for _, commit := range []string{"header", "write", "flush"} {
		for _, observed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/observed=%t", commit, observed), func(t *testing.T) {
				s, spy := spyServer(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				info := streamingInfo()
				info.attempts = 1
				info.bill.usageSeen = observed
				info.bill.tokens = usage.Tokens{Prompt: 100, Completion: 20}
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(context.WithValue(ctx, reqInfoKey{}, info))
				h := s.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					switch commit {
					case "header":
						w.WriteHeader(http.StatusOK)
					case "write":
						w.Write([]byte("data: {}\n\n"))
					case "flush":
						w.(http.Flusher).Flush()
					}
					cancel()
				}))
				h.ServeHTTP(httptest.NewRecorder(), r)
				if len(spy.samples) != 1 {
					t.Fatalf("samples = %d", len(spy.samples))
				}
				sample := spy.samples[0]
				if sample.Status != 200 || sample.Estimated == observed {
					t.Fatalf("committed stream accounting changed: %+v", sample)
				}
				if observed && sample.Tokens != info.bill.tokens || !observed && sample.Tokens.Prompt != 1000 {
					t.Fatalf("partial stream usage lost: %+v", sample)
				}
			})
		}
	}
}

// provCall 是一次 AddProvisional 调用。
type provCall struct{ keyID, delta int64 }

// revCall 是一次 ReverseProvisional 调用（收尾冲正，分日/周/月三个金额）。
type revCall struct {
	keyID                           int64
	dayMicro, weekMicro, monthMicro int64
	win                             usage.ProvisionalWindow
}

// meterSpy 是包内用例的 Metering 实现。
type meterSpy struct {
	samples []usage.Sample
	prov    []provCall
	rev     []revCall
	settled []string
	// win 是 AddProvisional 回给网关的窗口；用例改它即模拟"流中间跨过零点"。
	win usage.ProvisionalWindow
	// deny 非 nil 时 Admit 一律按它拒绝（nil = 放行）。
	deny *usage.Decision
}

func (m *meterSpy) Record(s usage.Sample) { m.samples = append(m.samples, s) }

func (m *meterSpy) Admit(store.KeyAuth) usage.Decision {
	if m.deny != nil {
		return *m.deny
	}
	return usage.Decision{Allowed: true}
}

func (m *meterSpy) AddProvisional(keyID, deltaMicro int64) usage.ProvisionalWindow {
	m.prov = append(m.prov, provCall{keyID, deltaMicro})
	return m.win
}

func (m *meterSpy) ReverseProvisional(keyID, dayMicro, weekMicro, monthMicro int64, win usage.ProvisionalWindow) {
	m.rev = append(m.rev, revCall{keyID, dayMicro, weekMicro, monthMicro, win})
}

func (m *meterSpy) SettleTask(_ context.Context, task store.AIGCTask) {
	m.settled = append(m.settled, task.ID)
}

// spyServer 是一个只装了计量出口的 Server（不碰库、不起监听）。
func spyServer(t *testing.T) (*Server, *meterSpy) {
	t.Helper()
	s := New(&config.Config{Listen: "127.0.0.1:0", DataDir: t.TempDir()},
		logging.New(io.Discard, slog.LevelDebug), nil, nil, nil, nil)
	spy := &meterSpy{win: usage.ProvisionalWindow{Day: "2026-08-08", Week: "2026-08-03", Month: "2026-08"}}
	s.EnableMetering(spy)
	return s, spy
}

// streamingInfo 造一条"正在跑的长流"：文本入口、有价、输入估算 1000 token。
func streamingInfo() *reqInfo {
	info := &reqInfo{id: "prov", keyID: 9}
	info.bill = billState{
		entry:      usage.EntryChat,
		kind:       "text",
		model:      "m",
		keyDisplay: "sk_xxxx…abcd",
		// 输入 1000 token × 3 元/百万 = 3000 微元；输出按估算逐帧增长。
		pricing:       `{"in":3000000,"out":12000000}`,
		sse:           true,
		estimateInput: func() int64 { return 1000 },
	}
	return info
}

// TestProvisionalTickAndReversal：长流每 30s 把当前估算并进预算计数器，
// 收尾时**原额冲平**再由 Record 记真值——中间估高估低都不该在账上留痕。
func TestProvisionalTickAndReversal(t *testing.T) {
	s, spy := spyServer(t)
	info := streamingInfo()

	// 首帧只起表不结算：没有"上一次"就谈不上间隔。
	s.tickProvisional(info)
	if len(spy.prov) != 0 {
		t.Fatalf("首帧不该结算: %+v", spy.prov)
	}
	if info.bill.provisionalAt.IsZero() {
		t.Fatal("首帧应把结算时刻起表")
	}

	// 未到 30s：不结算。
	info.bill.text.Add(strings1k())
	s.tickProvisional(info)
	if len(spy.prov) != 0 {
		t.Fatalf("窗口内不该结算: %+v", spy.prov)
	}

	// 把上次结算时刻拨回 31s 前 = 窗口已过。
	info.bill.provisionalAt = time.Now().Add(-31 * time.Second)
	s.tickProvisional(info)
	if len(spy.prov) != 1 {
		t.Fatalf("过窗应结算一次: %+v", spy.prov)
	}
	// 1000 输入 × 3 元/M + 250 输出（1000 个 ASCII / 4）× 12 元/M
	//   = 3000 + 3000 = 6000 微元
	const wantFirst = 6000
	if spy.prov[0] != (provCall{9, wantFirst}) {
		t.Errorf("中间结算 = %+v，期望 {9 %d}", spy.prov[0], wantFirst)
	}
	if info.bill.provisional != wantFirst {
		t.Errorf("已记临时金额 = %d，期望 %d", info.bill.provisional, wantFirst)
	}

	// 又跑了一段：只补差额，不重复记全额。
	info.bill.text.Add(strings1k())
	info.bill.provisionalAt = time.Now().Add(-31 * time.Second)
	s.tickProvisional(info)
	if len(spy.prov) != 2 {
		t.Fatalf("第二轮应再结算一次: %+v", spy.prov)
	}
	if spy.prov[1].delta != 3000 {
		t.Errorf("第二轮增量 = %d，期望只补新增的 3000 微元", spy.prov[1].delta)
	}

	// 收尾：先把 9000 原额冲平，再记一笔真值（上游报了 usage 就按真值）。
	info.bill.mergeChatUsage(map[string]any{"prompt_tokens": jsonNum("120"), "completion_tokens": jsonNum("60")})
	s.recordUsage(info, http.StatusOK, 90*time.Second)
	if len(spy.prov) != 2 {
		t.Fatalf("收尾不该再走 AddProvisional: %+v", spy.prov)
	}
	if len(spy.rev) != 1 || spy.rev[0].dayMicro != 9000 ||
		spy.rev[0].weekMicro != 9000 || spy.rev[0].monthMicro != 9000 {
		t.Fatalf("收尾应原额冲正 9000/9000/9000: %+v", spy.rev)
	}
	if spy.rev[0].win != spy.win {
		t.Errorf("冲正应带上并入时的窗口 %+v，实际 %+v", spy.win, spy.rev[0].win)
	}
	if info.bill.provisional != 0 || info.bill.provDayMicro != 0 ||
		info.bill.provWeekMicro != 0 || info.bill.provMonthMicro != 0 {
		t.Errorf("冲正后已记临时金额应归零，实际 %d/%d/%d/%d",
			info.bill.provisional, info.bill.provDayMicro, info.bill.provWeekMicro, info.bill.provMonthMicro)
	}
	if len(spy.samples) != 1 {
		t.Fatalf("收尾应记一笔: %+v", spy.samples)
	}
	got := spy.samples[0]
	if (got.Tokens != usage.Tokens{Prompt: 120, Completion: 60}) {
		t.Errorf("收尾 tokens = %+v，期望上游真值 {120,60}", got.Tokens)
	}
	if got.Estimated {
		t.Error("上游报了 usage，收尾不该打估算标")
	}
	if got.DurationMs != 90_000 {
		t.Errorf("duration_ms = %d，期望 90000", got.DurationMs)
	}
}

// TestProvisionalOnlyOnStreams：非流式请求不做中间结算（只观测一次，
// "中间"无从谈起）。
func TestProvisionalOnlyOnStreams(t *testing.T) {
	s, spy := spyServer(t)
	info := streamingInfo()
	info.bill.sse = false
	info.bill.provisionalAt = time.Now().Add(-10 * time.Minute)
	info.bill.text.Add(strings1k())
	s.tickProvisional(info)
	if len(spy.prov) != 0 {
		t.Errorf("非流式不该有中间结算: %+v", spy.prov)
	}
}

// TestProvisionalCrossMidnightSplitsWindows（Phase 3.5 外审遗留之二）：
// 长流在中途跨过本地零点时，收尾的冲正必须只回退**落在新窗口里**的那部分。
// 记在昨天的预扣已随日计数器清零蒸发，硬减回来就是从今天别人的合法消费里
// 扣钱——等于给新的一天白送额度。日翻月不翻（月内跨日）是常态，所以两个
// 分窗口金额必须分开算。
func TestProvisionalCrossMidnightSplitsWindows(t *testing.T) {
	s, spy := spyServer(t)
	info := streamingInfo()
	spy.win = usage.ProvisionalWindow{Day: "2026-08-08", Week: "2026-08-03", Month: "2026-08"}

	// 零点之前：并入 6000（输入 3000 + 输出 3000）。
	info.bill.text.Add(strings1k())
	info.bill.provisionalAt = time.Now().Add(-31 * time.Second)
	s.tickProvisional(info)
	if info.bill.provDayMicro != 6000 || info.bill.provWeekMicro != 6000 || info.bill.provMonthMicro != 6000 {
		t.Fatalf("零点前分窗口累计 = %d/%d/%d，期望 6000/6000/6000",
			info.bill.provDayMicro, info.bill.provWeekMicro, info.bill.provMonthMicro)
	}

	// 时钟跨过本地零点（周六→周日：周内、月内跨日）：这一轮补的 3000 落在
	// 新的一天，周与月窗口都没翻。
	spy.win = usage.ProvisionalWindow{Day: "2026-08-09", Week: "2026-08-03", Month: "2026-08"}
	info.bill.text.Add(strings1k())
	info.bill.provisionalAt = time.Now().Add(-31 * time.Second)
	s.tickProvisional(info)
	if info.bill.provDayMicro != 3000 {
		t.Errorf("跨日后日窗口累计 = %d，期望只剩新一天的 3000", info.bill.provDayMicro)
	}
	if info.bill.provWeekMicro != 9000 || info.bill.provMonthMicro != 9000 {
		t.Errorf("周/月窗口没翻，累计应仍是 9000/9000，实际 %d/%d",
			info.bill.provWeekMicro, info.bill.provMonthMicro)
	}

	s.recordUsage(info, http.StatusOK, 90*time.Second)
	if len(spy.rev) != 1 {
		t.Fatalf("收尾应冲正一次: %+v", spy.rev)
	}
	if spy.rev[0].dayMicro != 3000 || spy.rev[0].weekMicro != 9000 || spy.rev[0].monthMicro != 9000 {
		t.Errorf("冲正 = 日 %d / 周 %d / 月 %d，期望 3000 / 9000 / 9000",
			spy.rev[0].dayMicro, spy.rev[0].weekMicro, spy.rev[0].monthMicro)
	}
}

// TestMessagesTruncatedStreamIsEstimated（Phase 3.5 外审遗留之一）：
// Anthropic 的 message_start 自带 usage（output_tokens 恒为占位的 1），
// 流断在 message_delta 之前时不能按「Completion=1、非估算」入账——那是把
// 一整条流的输出记成一个 token 且不留任何痕迹。输入侧是真值，留着。
func TestMessagesTruncatedStreamIsEstimated(t *testing.T) {
	s, spy := spyServer(t)
	info := streamingInfo()
	info.bill.entry = usage.EntryMessages

	// message_start：输入真值 + 占位的 output_tokens=1。
	info.bill.mergeMessagesUsage(map[string]any{
		"input_tokens": jsonNum("500"), "output_tokens": jsonNum("1"),
	}, false)
	// 收到了一段增量正文（1000 个 ASCII = 250 token 估算），然后流断了。
	info.bill.text.Add(strings1k())

	s.recordUsage(info, http.StatusOK, time.Second)
	got := spy.samples[0]
	if got.Tokens.Prompt != 500 {
		t.Errorf("输入 = %d，期望留用 message_start 的真值 500", got.Tokens.Prompt)
	}
	if got.Tokens.Completion != 250 {
		t.Errorf("输出 = %d，期望按已收到的增量估算 250（而不是占位的 1）", got.Tokens.Completion)
	}
	if !got.Estimated {
		t.Error("断流的 messages 请求必须打估算标")
	}

	// 对照：message_delta 到齐时是真值，不打标。
	s2, spy2 := spyServer(t)
	info2 := streamingInfo()
	info2.bill.entry = usage.EntryMessages
	info2.bill.mergeMessagesUsage(map[string]any{
		"input_tokens": jsonNum("500"), "output_tokens": jsonNum("1"),
	}, false)
	info2.bill.text.Add(strings1k())
	info2.bill.mergeMessagesUsage(map[string]any{"output_tokens": jsonNum("321")}, true)
	s2.recordUsage(info2, http.StatusOK, time.Second)
	if got := spy2.samples[0]; got.Tokens.Completion != 321 || got.Estimated {
		t.Errorf("usage 到齐时 = %+v estimated=%v，期望真值 321 且不打标",
			got.Tokens, got.Estimated)
	}
}

// TestRecordSkippedWithoutEntry：没标记入口的请求（/v1/models、任务查询等）
// 一笔都不记。
func TestRecordSkippedWithoutEntry(t *testing.T) {
	s, spy := spyServer(t)
	s.recordUsage(&reqInfo{id: "x", keyID: 2}, http.StatusOK, time.Second)
	if len(spy.samples) != 0 {
		t.Errorf("未标记入口不该入账: %+v", spy.samples)
	}
}

// strings1k 返回 1000 个 ASCII 字符（估算公式下正好 250 token）。
func strings1k() string {
	b := make([]byte, 1000)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}

// jsonNum 造一个 json.Number——观察器只认它（UseNumber 解析出来的形态）。
func jsonNum(s string) any { return json.Number(s) }
