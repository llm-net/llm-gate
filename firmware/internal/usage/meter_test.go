package usage

// iteration-9 Phase 2 的可执行验收：聚合器内存态与落盘。

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// shanghai 是目标部署时区（整点偏移，本地零点恰好落在小时桶边界上）。
var shanghai = time.FixedZone("CST", 8*3600)

// kathmandu 是 +5:45 的半小时（准确说是三刻钟）偏移时区：本地零点落在桶中间，
// 用来钉死播种的取整方向。
var kathmandu = time.FixedZone("NPT", 5*3600+45*60)

// quietLogger 是不输出的 logger（测试里不想被告警刷屏）。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestMeter 建一个挂在临时库上的 Meter，时钟固定在 now。
func newTestMeter(t *testing.T, loc *time.Location, now time.Time) (*Meter, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := NewMeter(st, 90, loc, nil, quietLogger())
	m.now = func() time.Time { return now }
	return m, st
}

// textSample 造一条维度齐全的文本样本。
func textSample(at time.Time) Sample {
	return Sample{
		At: at, KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: "deepseek-chat", UpstreamName: "ds-main", UpstreamType: config.UpstreamDeepseek,
		Entry: EntryChat, Pricing: `{"in":2000000,"out":8000000}`,
		Tokens: Tokens{Prompt: 1_000_000, Completion: 500_000},
		Status: 200, Attempts: 1, DurationMs: 850,
	}
}

// Record → 内存增量 → 冲刷 → 库里的行与样本逐字段对得上；同一维度多次
// Record 在一个桶里相加。
func TestRecordFlushAccumulates(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)
	ctx := context.Background()

	m.Record(textSample(at))
	m.Record(textSample(at.Add(20 * time.Minute))) // 同一小时桶
	m.flush(ctx)

	bucket := at.Unix() / 3600
	rows, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("落盘 %d 行, 期望 1（同维度同桶应合并）", len(rows))
	}
	r := rows[0]
	// 2 元/百万 × 100 万 + 8 元/百万 × 50 万 = 6 元 = 6e6 微元，两次即 12e6。
	want := store.UsageRow{
		BucketHour: bucket,
		KeyID:      7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: "deepseek-chat", UpstreamName: "ds-main",
		Entry: EntryChat, Kind: store.ModelKindText,
		Requests: 2, PromptTokens: 2_000_000, CompletionTokens: 1_000_000,
		TotalTokens: 3_000_000, CostMicro: 12_000_000, DurationMsSum: 1700,
	}
	if r != want {
		t.Errorf("落盘行 =\n%+v\n期望\n%+v", r, want)
	}
	// 冲刷成功后内存增量必须清空——重放等于重复计数。
	if len(m.deltas) != 0 {
		t.Errorf("冲刷成功后仍有 %d 条内存增量", len(m.deltas))
	}
}

// 冲刷失败时增量必须留在内存等下轮，绝不丢账；期间新到的样本与回填的增量
// 合并而不是二选一。
func TestFlushFailureKeepsDeltas(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)

	m.Record(textSample(at))
	st.Close() // 让落盘必然失败

	m.flush(context.Background())
	if len(m.deltas) != 1 {
		t.Fatalf("落盘失败后内存增量 = %d 条, 期望 1（丢了就是丢账）", len(m.deltas))
	}
	for _, d := range m.deltas {
		if d.Requests != 1 || d.CostMicro != 6_000_000 {
			t.Errorf("回填的增量失真: %+v", d)
		}
	}
}

// 被准入拒绝的样本：单列进 rejected_requests，**不进 errors**——混进去会把
// 「用户超限」误读成「上游故障」。
func TestRecordRejectedIsNotAnError(t *testing.T) {
	at := time.Date(2026, 8, 8, 3, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	m.Record(Sample{
		At: at, KeyID: 7,
		ModelName: "deepseek-chat", Entry: EntryChat,
		Status: 429, Rejected: true, RejectReason: "budget_day",
	})
	m.Record(Sample{ // 对照：真正的错误
		At: at, KeyID: 7,
		ModelName: "deepseek-chat", UpstreamName: "ds-main", Entry: EntryChat,
		Status: 502,
	})
	var rejected, errs, requests int64
	for _, d := range m.deltas {
		rejected += d.RejectedRequests
		errs += d.Errors
		requests += d.Requests
	}
	if rejected != 1 || errs != 1 || requests != 2 {
		t.Errorf("rejected=%d errors=%d requests=%d, 期望 1/1/2", rejected, errs, requests)
	}
	// 被拒的行没到上游，维度里的 upstream_name 必须是空串（它与真正转发的
	// 行分属两个聚合键，否则「按上游分解」会把超限算到某个上游头上）。
	var sawEmptyUpstream bool
	for k := range m.deltas {
		if k.upstream == "" {
			sawEmptyUpstream = true
		}
	}
	if !sawEmptyUpstream {
		t.Error("被拒样本应落在 upstream_name 为空的维度上")
	}
}

// 预算计数器：跨日只清日、跨周清周、跨月全清。
// 2026-08-08 是周六（所在周 = 8-03 周一 … 8-09 周日），跨到周日不清周、
// 跨到下周一才清。
func TestSpendCountersRollOver(t *testing.T) {
	base := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, base)

	m.Record(textSample(base))
	if s := m.Spend(7); s.DayMicro != 6_000_000 ||
		s.WeekMicro != 6_000_000 || s.MonthMicro != 6_000_000 {
		t.Fatalf("首笔后 Spend = %+v", s)
	}

	// 跨到次日（周日，仍在同一周）：日清零，周、月保留。
	m.now = func() time.Time { return base.AddDate(0, 0, 1) }
	if s := m.Spend(7); s.DayMicro != 0 || s.WeekMicro != 6_000_000 ||
		s.MonthMicro != 6_000_000 {
		t.Errorf("跨日后 Spend = %+v, 期望日清零、周月保留", s)
	}

	// 跨到下周一：日、周清零，月保留。
	m.now = func() time.Time { return base.AddDate(0, 0, 2) }
	if s := m.Spend(7); s.DayMicro != 0 || s.WeekMicro != 0 ||
		s.MonthMicro != 6_000_000 {
		t.Errorf("跨周后 Spend = %+v, 期望日周清零、月保留", s)
	}

	// 跨到次月：全清零。
	m.now = func() time.Time { return base.AddDate(0, 1, 0) }
	if s := m.Spend(7); s.DayMicro != 0 || s.WeekMicro != 0 || s.MonthMicro != 0 {
		t.Errorf("跨月后 Spend = %+v, 期望全清零", s)
	}
}

// AddProvisional 只动预算计数器（长流 30s 中间结算）；不进小时桶、不进环——
// 它是临时值，流终由 Record 的真值取代。冲正走 ReverseProvisional，并带回
// 并入时的那一对窗口键。
func TestAddProvisionalOnlyMovesBudget(t *testing.T) {
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, at)

	win := m.AddProvisional(7, 1_500_000)
	if win.Day != "2026-08-08" || win.Week != "2026-08-03" || win.Month != "2026-08" {
		t.Fatalf("并入窗口 = %+v，期望本地日/周（周一日期）/月键", win)
	}
	if s := m.Spend(7); s.DayMicro != 1_500_000 || s.WeekMicro != 1_500_000 {
		t.Fatalf("provisional 后 Spend = %+v", s)
	}
	if len(m.deltas) != 0 || len(m.ring) != 0 {
		t.Fatalf("provisional 不该进小时桶(%d)或环(%d)", len(m.deltas), len(m.ring))
	}
	// 冲正：流终先减掉已记的 provisional，再走 Record 记真值。
	m.ReverseProvisional(7, 1_500_000, 1_500_000, 1_500_000, win)
	if s := m.Spend(7); s.DayMicro != 0 || s.WeekMicro != 0 || s.MonthMicro != 0 {
		t.Fatalf("冲正后 Spend = %+v, 期望 0", s)
	}
}

// 播种：把 UTC 小时桶折进本地当日/当月窗口，重启后预算不清零；
// **重复调用是空操作**（sync.Once），否则装配层多调一次就把账翻倍。
func TestSeedFoldsBucketsIntoLocalWindows(t *testing.T) {
	ctx := context.Background()
	// 本地 2026-08-08 12:00 (+8) = UTC 04:00。
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, st := newTestMeter(t, shanghai, now)

	monthStart := time.Date(2026, 8, 1, 0, 0, 0, 0, shanghai) // UTC 7-31 16:00
	dayStart := time.Date(2026, 8, 8, 0, 0, 0, 0, shanghai)   // UTC 8-7 16:00
	mk := func(at time.Time, cost int64) store.UsageDelta {
		return store.UsageDelta{
			BucketHour: at.UTC().Unix() / 3600, KeyID: 7,
			ModelName: "m", Entry: EntryChat, CostMicro: cost,
		}
	}
	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{
		mk(monthStart.AddDate(0, 0, -1), 100), // 上月，两个窗口都不算
		mk(monthStart.Add(2*time.Hour), 200),  // 本月非今日
		mk(dayStart.Add(3*time.Hour), 400),    // 今日
	}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}

	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s := m.Spend(7)
	// 2026-08-08 是周六，本周从 8-03（周一）起：8-01 的那笔只进月窗口。
	if s.MonthMicro != 600 || s.WeekMicro != 400 || s.DayMicro != 400 {
		t.Errorf("播种后 Spend = %+v, 期望 month=600 week=400 day=400", s)
	}
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed（第二次）: %v", err)
	}
	if s2 := m.Spend(7); s2 != s {
		t.Errorf("重复播种改变了计数器: %+v → %+v", s, s2)
	}
}

// 播种的周窗口可以早于月初：本周周一还在上个月里时，上月末、本周内的消费
// 要进周计数器（但不进月计数器）——播种起点必须取 min(周一, 月初)。
// 2026-08-01 是周六，所在周从 7-27（周一）起算。
func TestSeedWeekWindowCrossesMonthStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, shanghai)
	m, st := newTestMeter(t, shanghai, now)

	mk := func(at time.Time, cost int64) store.UsageDelta {
		return store.UsageDelta{
			BucketHour: at.UTC().Unix() / 3600, KeyID: 7,
			ModelName: "m", Entry: EntryChat, CostMicro: cost,
		}
	}
	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{
		mk(time.Date(2026, 7, 26, 10, 0, 0, 0, shanghai), 1000), // 上周日：周月都不算
		mk(time.Date(2026, 7, 30, 10, 0, 0, 0, shanghai), 200),  // 本周、上月：只进周
		mk(time.Date(2026, 8, 1, 2, 0, 0, 0, shanghai), 40),     // 本周、本月、今日
	}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s := m.Spend(7)
	if s.WeekMicro != 240 || s.MonthMicro != 40 || s.DayMicro != 40 {
		t.Errorf("播种后 Spend = %+v, 期望 week=240 month=40 day=40", s)
	}
}

// 播种的取整方向（包头裁决）：半小时/三刻钟偏移时区的本地零点落在桶中间，
// 跨界那只桶整只算给**较早**的一天——当日计数偏少（预算变松），不偏多。
func TestSeedRoundingDirectionForFractionalZones(t *testing.T) {
	ctx := context.Background()
	// 本地 2026-08-08 12:00 (+5:45)。
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, kathmandu)
	m, st := newTestMeter(t, kathmandu, now)

	dayStart := time.Date(2026, 8, 8, 0, 0, 0, 0, kathmandu) // UTC 8-7 18:15
	// 落在跨界桶（UTC 18:00–19:00）里、但本地已属今天的一笔消费。
	straddling := dayStart.Add(30 * time.Minute) // 本地 00:30，UTC 18:45
	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{{
		BucketHour: straddling.UTC().Unix() / 3600, KeyID: 7,
		ModelName: "m", Entry: EntryChat, CostMicro: 500,
	}}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s := m.Spend(7)
	if s.DayMicro != 0 {
		t.Errorf("当日已用 = %d, 期望 0（跨界桶整只归较早的一天，宁少勿多）", s.DayMicro)
	}
	if s.MonthMicro != 500 {
		t.Errorf("当月已用 = %d, 期望 500（月界不受此桶影响）", s.MonthMicro)
	}
}

// 环形缓冲：容量 256、新的在前；本人视图只留自己的行且剥掉上游账户名。
func TestRingBufferKeepsNewestFirst(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	for i := 0; i < ringSize+20; i++ {
		s := textSample(at)
		s.ModelName = "m-" + string(rune('a'+i%26))
		m.Record(s)
	}
	// 另一把密钥的行，验证它照常进环（没有按归属过滤这回事了）。
	other := textSample(at)
	other.KeyID, other.KeyDisplay = 9, "sk_bbbbbbbbbbbb…zzzz"
	m.Record(other)

	all := m.recent()
	if len(all) != ringSize {
		t.Fatalf("环长度 = %d, 期望 %d", len(all), ringSize)
	}
	if !all[0].At.Equal(at) || all[0].KeyID != 9 {
		t.Errorf("环首条应是最新的那条，得 %+v", all[0])
	}
	if all[0].Upstream != "ds-main" {
		t.Errorf("环应保留上游账户名，得 %q", all[0].Upstream)
	}
}

// 读数 = 已落盘的行 + 尚未冲刷的内存增量：冲刷节奏对 UI 不可见。
func TestReportMergesFlushedAndPending(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	m.Record(textSample(at))
	m.flush(ctx) // 第一笔落盘
	m.Record(textSample(at.Add(time.Hour)))
	// 另一把密钥的一笔，验证按密钥维度分得开。
	other := textSample(at)
	other.KeyID, other.KeyDisplay = 9, "sk_bbbbbbbbbbbb…zzzz"
	m.Record(other)

	from, to := at.Add(-time.Hour), at.Add(3*time.Hour)

	rep, err := m.Report(ctx, from, to)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Total.Requests != 3 || rep.Total.CostMicro != 18_000_000 {
		t.Errorf("合计 = %+v, 期望 3 次 / 18e6 微元", rep.Total)
	}
	if len(rep.ByKey) != 2 || len(rep.ByUpstream) != 1 {
		t.Errorf("维度数 by_key=%d by_upstream=%d, 期望 2/1", len(rep.ByKey), len(rep.ByUpstream))
	}
	// 按日序列按本地日期分组：UTC 10:00/11:00 (+8) 都落在 2026-08-08。
	if len(rep.Days) != 1 || rep.Days[0].Day != "2026-08-08" {
		t.Errorf("按日序列 = %+v", rep.Days)
	}

	filtered, err := m.ReportForKey(ctx, from, to, 7)
	if err != nil {
		t.Fatalf("ReportForKey: %v", err)
	}
	if filtered.Total.Requests != 2 || filtered.Total.CostMicro != 12_000_000 {
		t.Errorf("单 Key 合计 = %+v，期望同时包含已落盘与待冲刷的两笔", filtered.Total)
	}
	if len(filtered.ByKey) != 1 || filtered.ByKey[0].ID != 7 {
		t.Errorf("单 Key 的 by_key = %+v", filtered.ByKey)
	}
	if len(filtered.Keys) != 2 {
		t.Errorf("单 Key 报表的选择项 = %+v，期望仍保留区间内两把密钥", filtered.Keys)
	}
}

// Cursor 透明面不解析协议帧：只有 Run/RunSSE 是模型消费，其他 RPC
// 都是协议辅助流量。报表要从总计与所有维度里排除辅助行，同时把
// 可确认的对话行归成一个稳定展示名。
func TestCursorReportOnlyIncludesAgentRuns(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)

	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{
		{
			BucketHour: at.Unix() / 3600, KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
			ModelName: "aiserver.v1.AiService/AvailableModels", UpstreamName: "Cursor 订阅",
			Entry: EntryCursorAgent, Kind: store.ModelKindText, Requests: 26,
		},
		{
			BucketHour: at.Unix() / 3600, KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
			ModelName: "agent.v1.AgentService/RunSSE", UpstreamName: "Cursor 订阅",
			Entry: EntryCursorAgent, Kind: store.ModelKindText, Requests: 5,
		},
		{
			// 不能分辨真实对话与辅助 RPC 的含混维度也宁可不展示。
			BucketHour: at.Unix() / 3600, KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
			ModelName: "Cursor Agent", UpstreamName: "Cursor 订阅",
			Entry: EntryCursorAgent, Kind: store.ModelKindText, Requests: 20,
		},
	}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}

	// 当前新样本只有真正对话才会进 Meter，账本与最近请求都用稳定维度。
	m.Record(Sample{
		At: at.Add(time.Minute), KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: CursorAgentRunRPC, UpstreamName: "Cursor 订阅",
		Entry: EntryCursorAgent, Status: 200, Attempts: 1,
	})

	rep, err := m.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.ByModel) != 1 || rep.ByModel[0].Key != CursorAgentModelDimension ||
		rep.ByModel[0].Requests != 6 || rep.Total.Requests != 6 {
		t.Fatalf("Cursor 对话归并 = %+v total=%+v，期望 6 次", rep.ByModel, rep.Total)
	}
	if rep.ByModel[0].Priced != nil {
		t.Errorf("Cursor Agent 是订阅服务维度，不应冒充目录模型: priced=%v", rep.ByModel[0].Priced)
	}
	if len(rep.Recent) != 1 || rep.Recent[0].ModelName != CursorAgentModelDimension {
		t.Errorf("Cursor 最近请求维度 = %+v", rep.Recent)
	}
	for k := range m.deltas {
		if k.entry == EntryCursorAgent && k.model != CursorAgentModelDimension {
			t.Errorf("新 Cursor 小时增量仍使用 RPC 名: %+v", k)
		}
	}
}

// 保留清理：只删截止桶之前的行，且每天最多跑一次。
func TestPruneRunsOncePerDay(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, now)
	m.days = 31

	old := now.AddDate(0, 0, -40)
	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{
		{BucketHour: old.Unix() / 3600, KeyID: 7, ModelName: "m", Entry: EntryChat, Requests: 1},
		{BucketHour: now.Unix() / 3600, KeyID: 7, ModelName: "m", Entry: EntryChat, Requests: 1},
	}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}
	m.prune(ctx)
	rows, err := st.QueryUsageRange(ctx, 0, now.Unix()/3600+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("清理后剩 %d 行, 期望 1", len(rows))
	}
	// 同一天内再调不重复扫全表。
	before := m.lastPruneDay
	m.prune(ctx)
	if m.lastPruneDay != before {
		t.Error("同一天内 prune 不该再跑")
	}
}

// Run 的收尾：ctx 取消后必须把内存里的账落完再退出（父 ctx 已取消，
// 冲刷要走 WithoutCancel 的有界 ctx，否则最后一个窗口的账凭空消失）。
func TestRunFlushesOnShutdown(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	m.Record(textSample(at))
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在取消后退出")
	}

	bucket := at.Unix() / 3600
	rows, err := st.QueryUsageRange(context.Background(), bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("停机冲刷后库里 = %+v, 期望 1 行 1 次请求", rows)
	}
}

// 坏掉的目录价（库被手工改过）按未定价处理并告警，绝不把请求路径带崩。
func TestBadPricingFallsBackToZero(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	s := textSample(at)
	s.Pricing = `{"in":"1000"}` // 带引号的数字：形态非法
	m.Record(s)
	for _, d := range m.deltas {
		if d.CostMicro != 0 {
			t.Errorf("坏价目表下金额 = %d, 期望 0", d.CostMicro)
		}
		if d.Requests != 1 {
			t.Errorf("坏价目表不该影响用量计数: %+v", d)
		}
	}
}

// 全程无浮点（根 AGENTS.md 硬约束「金额与配额恒为定点整数，全链路禁止浮点」）：
// 本包的非测试源码里不得出现 float 类型或浮点数学。这条是绊线，别删——
// 一次 float64 中转就够让金额在第 15 位有效数字上开始漂。
func TestNoFloatingPointInPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	banned := []string{"float32", "float64", "math.Round", "math.Floor", "math.Ceil"}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				t.Errorf("%s 出现了 %q——金额与配额必须全程定点整数", name, b)
			}
		}
	}
}

// 密钥维度按**主键**分组、展示串只作展示：同一把密钥的历史行里展示串快照
// 未必处处都在（早期行、调用方漏填），按名字分组会把同一把密钥劈成两行。
func TestReportGroupsSubjectsByID(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	noName := textSample(at)
	noName.KeyDisplay = "" // 展示串快照缺失的那一行
	m.Record(noName)
	m.Record(textSample(at.Add(time.Minute))) // 同一 key_id，带展示串

	rep, err := m.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(rep.ByKey) != 1 || rep.ByKey[0].ID != 7 || rep.ByKey[0].Requests != 2 {
		t.Errorf("by_key = %+v, 期望一行 id=7 / 2 次请求", rep.ByKey)
	}
	if rep.ByKey[0].Key != "sk_prefix12…wxyz" {
		t.Errorf("by_key 展示串 = %q, 期望取最后一个非空快照", rep.ByKey[0].Key)
	}
}

// 环绕回之后仍保留**最新的** ringSize 条，且顺序恒是新→旧（写指针绕回的
// 实现最容易在这里出错：读序错位表现为「最近请求」里混着几小时前的行）。
func TestRingKeepsNewestAfterWrap(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	const total = ringSize + 37
	for i := 0; i < total; i++ {
		s := textSample(at)
		s.TaskID = "seq-" + strconv.Itoa(i)
		m.Record(s)
	}
	evs := m.recent()
	if len(evs) != ringSize {
		t.Fatalf("环长度 = %d, 期望 %d", len(evs), ringSize)
	}
	for i, ev := range evs {
		want := "seq-" + strconv.Itoa(total-1-i)
		if ev.TaskID != want {
			t.Fatalf("第 %d 条 = %s, 期望 %s（新→旧）", i, ev.TaskID, want)
		}
	}
}

// 外审遗留（Phase 2/3.5）：长流的中间结算跨过本地零点时，「+X 记在昨天、
// −X 落在今天」会从**新一天的合法消费**里扣钱——等于给新一天白送 X 微元额度。
// 预算机制正是为了「长流不能绕开限额」而存在，这个方向的漏是唯一不能接受的
// 那一种。修法：冲正必须带回并入时的窗口键，窗口已翻转就整个丢弃。
func TestProvisionalReversalRespectsWindow(t *testing.T) {
	before := time.Date(2026, 8, 8, 23, 58, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, before)

	win := m.AddProvisional(7, 3_000_000) // 流中：+3 元记在 8 日
	if s := m.Spend(7); s.DayMicro != 3_000_000 {
		t.Fatalf("跨零点前 = %+v", s)
	}

	// 时钟跨过本地零点；新的一天里先有一笔别人的合法消费。
	m.now = func() time.Time { return time.Date(2026, 8, 9, 0, 5, 0, 0, shanghai) }
	s := textSample(m.now())
	s.KeyID = 7
	s.Pricing = `{"in":1000000,"out":1000000}` // 该样本 1000+500 token = 1_500_000 微元
	m.Record(s)
	spentToday := m.Spend(7).DayMicro
	if spentToday == 0 {
		t.Fatal("新一天的对照消费没记上，用例前提不成立")
	}

	// 流终冲正：那 3 元记在 8 日，9 日的计数器不该被它动一分钱。
	m.ReverseProvisional(7, 3_000_000, 3_000_000, 3_000_000, win)
	got := m.Spend(7)
	if got.DayMicro != spentToday {
		t.Errorf("跨零点冲正动了新一天的日额: %+v，期望仍是 %d", got, spentToday)
	}
	// 周、月窗口都没翻（8 日周六与 9 日周日同周、同属 8 月）：那 3 元确实还
	// 记在这两个计数器上，冲正照常生效。
	if got.WeekMicro != spentToday || got.MonthMicro != spentToday {
		t.Errorf("周/月额 = %d/%d，期望冲掉 3 元后只剩 %d",
			got.WeekMicro, got.MonthMicro, spentToday)
	}

	// 同窗口内的成对操作照常回到 0。
	win2 := m.AddProvisional(8, 1_000_000)
	m.ReverseProvisional(8, 1_000_000, 1_000_000, 1_000_000, win2)
	if s := m.Spend(8); s.DayMicro != 0 || s.WeekMicro != 0 || s.MonthMicro != 0 {
		t.Errorf("同窗口成对操作 = %+v, 期望 0", s)
	}
}

// 跨周的那一半同理：周日深夜的预扣不该从下周一的周额度里扣。
// 2026-08-09 是周日（当周最后一天），8-10 起算新的一周。
func TestProvisionalReversalRespectsWeekWindow(t *testing.T) {
	m, _ := newTestMeter(t, shanghai, time.Date(2026, 8, 9, 23, 55, 0, 0, shanghai))
	win := m.AddProvisional(7, 2_000_000)

	m.now = func() time.Time { return time.Date(2026, 8, 10, 0, 10, 0, 0, shanghai) }
	s := textSample(m.now())
	s.KeyID = 7
	s.Pricing = `{"in":1000000,"out":1000000}` // 1000+500 token = 1_500_000 微元
	m.Record(s)
	spent := m.Spend(7).WeekMicro

	m.ReverseProvisional(7, 2_000_000, 2_000_000, 2_000_000, win)
	got := m.Spend(7)
	if got.WeekMicro != spent || got.DayMicro != spent {
		t.Errorf("跨周冲正动了新一周的额度: %+v，期望仍是 %d", got, spent)
	}
	// 月窗口没翻（9 日与 10 日同属 8 月）：月计数器上的 2 元照常冲掉。
	if got.MonthMicro != spent {
		t.Errorf("月额 = %d，期望冲掉 2 元后只剩 %d", got.MonthMicro, spent)
	}
}

// 跨月的那一半同理：8 月 31 日的预扣不该从 9 月的额度里扣。
// 2026-08-31 是周一，9-01 周二同属一周：周计数器上的预扣照常冲掉。
func TestProvisionalReversalRespectsMonthWindow(t *testing.T) {
	m, _ := newTestMeter(t, shanghai, time.Date(2026, 8, 31, 23, 50, 0, 0, shanghai))
	win := m.AddProvisional(7, 2_000_000)

	m.now = func() time.Time { return time.Date(2026, 9, 1, 0, 10, 0, 0, shanghai) }
	s := textSample(m.now())
	s.KeyID = 7
	s.Pricing = `{"in":1000000,"out":1000000}`
	m.Record(s)
	spent := m.Spend(7).MonthMicro

	m.ReverseProvisional(7, 2_000_000, 2_000_000, 2_000_000, win)
	got := m.Spend(7)
	if got.MonthMicro != spent || got.DayMicro != spent {
		t.Errorf("跨月冲正动了新月份的额度: %+v，期望仍是 %d", got, spent)
	}
	if got.WeekMicro != spent {
		t.Errorf("周额 = %d，期望同周冲正生效后只剩 %d", got.WeekMicro, spent)
	}
}

// 按量额度待扣随冲刷落库、扣穿钳 0；落库失败原样放回内存等待下一轮。
func TestFlushMeteredAllowancePersistsAndRefillsOnFailure(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, st := newTestMeter(t, shanghai, at)
	k, err := st.CreateAPIKey(ctx, "metered", "digest-metered", "sk_metered", "ered", "plain-metered")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if _, err := st.AdjustAPIKeyMeteredAllowance(ctx, k.ID, 5_000_000); err != nil {
		t.Fatalf("增加按量额度: %v", err)
	}
	zero := int64(0)
	ka := store.KeyAuth{KeyID: k.ID, KeyBudgetDayMicro: &zero, KeyMeteredAllowanceMicro: 5_000_000}
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("有按量额度应放行: %+v", d)
	}
	sample := textSample(at)
	sample.KeyID = k.ID
	m.Record(sample) // 6 元，超过 5 元剩余，落库时钳 0。
	m.flush(ctx)
	if got, _ := st.GetAPIKeyByID(ctx, k.ID); got.MeteredAllowanceMicro != 0 {
		t.Errorf("落库后按量额度应钳 0，实际 %d", got.MeteredAllowanceMicro)
	}
	if pending := m.Spend(k.ID).MeteredAllowancePendingMicro; pending != 0 {
		t.Errorf("落库成功后待扣应清零，实际 %d", pending)
	}

	if _, err := st.AdjustAPIKeyMeteredAllowance(ctx, k.ID, 5_000_000); err != nil {
		t.Fatalf("再次增加按量额度: %v", err)
	}
	if d := m.Admit(ka); !d.Allowed {
		t.Fatalf("再次增加后应放行: %+v", d)
	}
	m.Record(sample)
	st.Close()
	m.flushMeteredAllowance(ctx)
	if pending := m.Spend(k.ID).MeteredAllowancePendingMicro; pending != 6_000_000 {
		t.Errorf("落库失败后待扣应回填 6000000，实际 %d", pending)
	}
}

// 外审遗留（Phase 2）：清理失败**不打日戳**，下一轮（5 分钟）重试。
// 打了日戳就要等到下一个 UTC 日才重来——一次瞬时的库锁争用能让保留期整整
// 跳过一天，而现场只剩一行 ERROR 作证。
func TestPruneRetriesAfterFailure(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, now)
	m.days = 31
	st.Close() // 让清理必然失败

	m.prune(context.Background())
	if m.lastPruneDay != "" {
		t.Errorf("清理失败后打了日戳 %q——本 UTC 日不会再重试了", m.lastPruneDay)
	}
}

// 缺陷③（Phase 3.5）：**失败的播种绝不能被 latch 住**。启动那一刻库瞬时
// 不可用（首启初始化、锁争用），旧写法会让整个进程生命周期的预算都从 0 起算
// ——等于当天预算凭空翻倍。只有成功才封印，失败必须可重试。
func TestSeedFailureIsNotLatched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.AddUsageDeltas(ctx, []store.UsageDelta{{
		BucketHour: now.UTC().Unix() / 3600, KeyID: 7,
		ModelName: "m", Entry: EntryChat, CostMicro: 400,
	}}); err != nil {
		t.Fatalf("AddUsageDeltas: %v", err)
	}

	m := NewMeter(st, 90, shanghai, nil, quietLogger())
	m.now = func() time.Time { return now }
	st.Close() // 启动那一刻库不可用

	if err := m.Seed(ctx); err == nil {
		t.Fatal("库不可用时播种应报错")
	}

	// 库恢复后重试必须真的再播一次。
	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("store.Open（恢复后）: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	m.st = st2
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("重试播种: %v", err)
	}
	if s := m.Spend(7); s.DayMicro != 400 {
		t.Errorf("重试播种后当日已用 = %d, 期望 400（失败被 latch 住了，本进程预算从 0 起算）", s.DayMicro)
	}
}

// 播种的重试窗口只到**第一次成功冲刷**为止：那之后库里混进了本进程自己记过的
// 增量，再播种就是把这部分账加第二遍（计数器偏高 = 预算凭空变紧）。
func TestSeedStopsAfterFirstFlush(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, shanghai)
	m, _ := newTestMeter(t, shanghai, now)

	m.Record(textSample(now)) // 6e6 微元，只在内存计数器上
	m.flush(ctx)              // 落盘：这一笔现在也在库里了
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if s := m.Spend(7); s.DayMicro != 6_000_000 {
		t.Errorf("冲刷之后播种把自己记过的账又加了一遍 = %d, 期望 6000000", s.DayMicro)
	}
}

// 外审遗留（Phase 2）：冲刷失败回填时，**dst 是新的、src 是旧的**——
// 只补 dst 缺的快照列，绝不用旧值覆盖新值。
func TestMergeDeltaKeepsNewerSnapshots(t *testing.T) {
	newer := store.UsageDelta{KeyDisplay: "sk_p…z", Kind: "", Requests: 1}
	older := store.UsageDelta{KeyDisplay: "sk_OLD…zzzz", Kind: store.ModelKindText, Requests: 2}
	mergeDelta(&newer, older)
	if newer.KeyDisplay != "sk_p…z" {
		t.Errorf("旧快照覆盖了新的密钥展示串: %q", newer.KeyDisplay)
	}
	if newer.Kind != store.ModelKindText {
		t.Errorf("未用旧快照补上缺失的 kind: %q", newer.Kind)
	}
	if newer.Requests != 3 {
		t.Errorf("计数未相加: %d", newer.Requests)
	}
}

// ---- 账本维度基数封顶（Phase 5） ----

// unknownSample 造一条「目录里没有这个模型」的样本（选路失败，或准入在选路
// 之前就把请求拦下了）。
func unknownSample(at time.Time, model string) Sample {
	s := textSample(at)
	s.ModelName, s.ModelKnown = model, false
	s.UpstreamName = "" // 没走到上游
	s.Tokens, s.Pricing = Tokens{}, ""
	s.Status = 404
	return s
}

// 目录里没有的模型名，一个冲刷窗口内至多放行 unknownModelsPerFlush 个，
// 其余并进哨兵维度：一把有效密钥连发不存在的模型名，不该在 SD 卡上长出
// 无界多行 usage_hourly。
func TestUnknownModelDimCapped(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	m, _ := newTestMeter(t, shanghai, at)

	for i := 0; i < unknownModelsPerFlush+20; i++ {
		m.Record(unknownSample(at, "ghost-"+strconv.Itoa(i)))
	}
	// 50 个真名 + 1 个哨兵 = 51 条增量，20 次超额调用全并进哨兵那一条。
	if len(m.deltas) != unknownModelsPerFlush+1 {
		t.Fatalf("内存增量 = %d 条, 期望 %d（未知模型名没有被封顶）",
			len(m.deltas), unknownModelsPerFlush+1)
	}
	var sentinel *store.UsageDelta
	for k, d := range m.deltas {
		if k.model == UnknownModelDim {
			sentinel = d
		}
	}
	if sentinel == nil {
		t.Fatalf("超额的未知模型名没有并进哨兵维度 %q", UnknownModelDim)
	}
	if sentinel.Requests != 20 {
		t.Errorf("哨兵维度请求数 = %d, 期望 20", sentinel.Requests)
	}

	// 目录里存在的模型不受这道闸限制——它们的基数由管理台约束，是真要看的账。
	known := textSample(at)
	known.ModelName, known.ModelKnown = "deepseek-chat", true
	m.Record(known)
	if _, ok := m.deltas[deltaKey{bucket: at.Unix() / 3600, keyID: 7,
		model: "deepseek-chat", upstream: "ds-main", entry: EntryChat}]; !ok {
		t.Error("目录里存在的模型被基数闸拦掉了")
	}

	// 配额随冲刷重置：下一个窗口重新给 50 个名额。
	m.flush(context.Background())
	if len(m.unknownModels) != 0 {
		t.Fatalf("冲刷后未知模型名配额未重置，仍有 %d 个", len(m.unknownModels))
	}
	m.Record(unknownSample(at, "ghost-brand-new"))
	if _, ok := m.deltas[deltaKey{bucket: at.Unix() / 3600, keyID: 7,
		model: "ghost-brand-new", entry: EntryChat}]; !ok {
		t.Error("新窗口里的未知模型名应重新按真名记账")
	}
}

// ---- iteration-9 Phase 7：收口走查点出的两处读数层缺口 ----

// 「未定价」徽章的真值来自**目录**，不从金额反推。收口走查把反推的两个方向
// 都复现过一次，这里各钉一条：
//
//	①已定价模型在本区间内只有 count_tokens 流量（刻意计 1 次请求、0 token、
//	  0 元）——按金额反推会误标「未定价」；
//	②真未定价模型窗口内留着一笔改价前的消费（cost_micro > 0）——按金额反推
//	  会漏标。
//
// 目录里压根没有的名字（客户端拼错的、并进哨兵的）保持"无从判断"：那不是
// 未定价，是没有这个模型。
func TestReportPricedReadsCatalogNotAmount(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)
	ctx := context.Background()
	for _, mm := range [...]struct{ name, pricing string }{
		{"count-tokens-only", `{"in":2000000,"out":8000000}`}, // 已定价
		{"was-priced", ""}, // 未定价（管理员刚清掉价签）
	} {
		if _, err := st.CreateModel(ctx, mm.name, store.ModelKindText, mm.pricing); err != nil {
			t.Fatalf("CreateModel(%s): %v", mm.name, err)
		}
	}

	// ① 已定价模型，本区间只有 count_tokens：计一次请求，0 token、0 元。
	ct := textSample(at)
	ct.ModelName, ct.ModelKnown = "count-tokens-only", true
	ct.Tokens = Tokens{}
	m.Record(ct)
	// ② 未定价模型，账上却留着一笔改价前按老价记的消费。
	old := textSample(at)
	old.ModelName, old.ModelKnown = "was-priced", true
	m.Record(old)
	// ③ 客户端随口给的名字：目录里没有。
	ghost := textSample(at)
	ghost.ModelName, ghost.ModelKnown = "typo-model", false
	m.Record(ghost)

	rep, err := m.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	byModel := map[string]DimRow{}
	for _, r := range rep.ByModel {
		byModel[r.Key] = r
	}
	priced, ok := byModel["count-tokens-only"]
	if !ok {
		t.Fatalf("按模型维度缺 count-tokens-only: %+v", rep.ByModel)
	}
	if priced.CostMicro != 0 {
		t.Fatalf("前置条件不成立：这一行本该是 0 元, got %d", priced.CostMicro)
	}
	if priced.Priced == nil || !*priced.Priced {
		t.Errorf("已定价模型被标成未定价（金额 0 只是因为这一区间只有 count_tokens）: %v", priced.Priced)
	}
	unpriced, ok := byModel["was-priced"]
	if !ok {
		t.Fatalf("按模型维度缺 was-priced: %+v", rep.ByModel)
	}
	if unpriced.CostMicro == 0 {
		t.Fatalf("前置条件不成立：这一行本该有改价前的金额, got 0")
	}
	if unpriced.Priced == nil || *unpriced.Priced {
		t.Errorf("未定价模型漏标（窗口里有一笔改价前的消费就把徽章顶掉了）: %v", unpriced.Priced)
	}
	if row, ok := byModel["typo-model"]; !ok || row.Priced != nil {
		t.Errorf("目录里没有的名字应保持「无从判断」(nil): ok=%v priced=%v", ok, row.Priced)
	}
	// 其余维度不带这个字段——它只对模型有意义。
	for _, dim := range [...][]DimRow{rep.ByKey, rep.ByEntry, rep.ByKind, rep.ByUpstream} {
		for _, r := range dim {
			if r.Priced != nil {
				t.Errorf("非模型维度带上了 priced: %+v", r)
			}
		}
	}
}

// 非 token 形态的计费量要进账本：H3 的秒与参考图张数、Seedream 的出图张数。
// 缺了它们，页面上每一行视频/图片消费都是「Token 0」——金额可审、金额背后的
// 量不可审（收口走查点名的缺口）。
func TestLedgerCarriesSecondsAndImages(t *testing.T) {
	at := time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC)
	m, st := newTestMeter(t, shanghai, at)
	ctx := context.Background()

	// 同步出图（走 Record 的请求路径）：张数与 token 都落位。
	img := Sample{
		At: at, KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
		ModelName: "doubao-seedream", ModelKnown: true, UpstreamName: "ark-main",
		UpstreamType: config.UpstreamArk, Entry: EntryImage, Kind: store.ModelKindImage,
		Pricing:   `{"ark_image_each":200000}`,
		TaskUsage: TaskUsage{GeneratedImages: 2, OutputTokens: 32928},
		Status:    200, Attempts: 1, DurationMs: 4200,
	}
	m.Record(img)
	m.flush(ctx)

	bucket := at.Unix() / 3600
	rows, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("落盘 %d 行 (err=%v)", len(rows), err)
	}
	r := rows[0]
	if r.ImageCount != 2 || r.CompletionTokens != 32928 || r.TotalTokens != 32928 {
		t.Errorf("出图的量没进账本: %+v（张数与 output_tokens 都只活在厂商原文里）", r)
	}
	if r.VideoSeconds != 0 {
		t.Errorf("出图不该有秒数: %+v", r)
	}
	if r.CostMicro != 400_000 {
		t.Errorf("金额 = %d, 期望 400000（2 张 × 0.2 元）", r.CostMicro)
	}

	// H3 视频任务（走清算路径）：输出秒 + 输入秒进 video_seconds，参考图进 image_count。
	m2, st2 := newTestMeter(t, shanghai, settleNow())
	m2.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st2, store.ModelKindVideo, config.UpstreamMinimax,
		`{"minimax_video_sec_768p":300000,"minimax_video_image_extra":100000}`,
		"succeeded", `{"output_seconds":6,"input_seconds":4,"input_image_count":7}`, false, "768P")
	m2.settle(ctx)

	b2 := m2.now().UTC().Unix() / 3600
	rows2, err := st2.QueryUsageRange(ctx, b2, b2+1)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("清算落盘 %d 行 (err=%v)", len(rows2), err)
	}
	s2 := rows2[0]
	if s2.VideoSeconds != 10 || s2.ImageCount != 7 {
		t.Errorf("H3 的秒与张没进账本: %+v（那是它的权威计费量，只活在 usage_json 里就不可审）", s2)
	}
	// 10 秒 × 0.3 元 + (7−5) 张 × 0.1 元 = 3.2 元。
	if s2.CostMicro != 3_200_000 {
		t.Errorf("金额 = %d, 期望 3200000", s2.CostMicro)
	}
	if s2.Requests != 0 {
		t.Errorf("清算增量不该计 requests: %+v", s2)
	}
	// 明细环的清算行同样带量（页面上那一行不再只能显示「—」）。
	ring := m2.recent()
	if len(ring) != 1 || ring[0].VideoSeconds != 10 || ring[0].ImageCount != 7 {
		t.Errorf("明细环的清算行没带计费量: %+v", ring)
	}

	// 方舟 Seedance：计价量是 token，落进 completion_tokens（同一个「量」列，
	// 形态由 kind + 上游账户名区分）。
	m3, st3 := newTestMeter(t, shanghai, settleNow())
	m3.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st3, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":246840}`, false, "")
	m3.settle(ctx)
	b3 := m3.now().UTC().Unix() / 3600
	rows3, err := st3.QueryUsageRange(ctx, b3, b3+1)
	if err != nil || len(rows3) != 1 {
		t.Fatalf("清算落盘 %d 行 (err=%v)", len(rows3), err)
	}
	if rows3[0].CompletionTokens != 246840 || rows3[0].TotalTokens != 246840 {
		t.Errorf("Seedance 的计价 token 没进账本: %+v", rows3[0])
	}
	if rows3[0].VideoSeconds != 0 || rows3[0].ImageCount != 0 {
		t.Errorf("Seedance 不按秒/张计费，两列应为 0: %+v", rows3[0])
	}
}

// 形态对不上时（ark 的价签遇 minimax 的秒数原文）金额记 0 且打估算标——
// **量也一并留零**：那几个数字属于别家的计价口径，记进这一行等于给一笔
// 「不知道多少钱」的账配上一份看着很确定的量。
func TestLedgerShapeMismatchRecordsNoQuantities(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded",
		`{"output_seconds":5,"input_seconds":0,"input_image_count":0}`, false, "")

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("落盘 %d 行 (err=%v)", len(rows), err)
	}
	if rows[0].VideoSeconds != 0 || rows[0].ImageCount != 0 || rows[0].TotalTokens != 0 {
		t.Errorf("形态错配却记了量: %+v", rows[0])
	}
	if rows[0].EstimatedRequests != 1 || rows[0].CostMicro != 0 {
		t.Errorf("形态错配应是 0 元 + 估算标: %+v", rows[0])
	}
}
