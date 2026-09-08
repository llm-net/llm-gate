package usage

// iteration-9 Phase 2 的可执行验收：懒对账（客户端弃轮询也要入账）与
// 「清算 + 入账」的原子性。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// fakeProber 是 gateway 侧探针的替身：terminal 决定哪些状态算终态，
// probe 把任务翻成给定的观测（nil = 报错，模拟厂商不可达）。
type fakeProber struct {
	terminal map[string]bool
	probe    func(store.AIGCTask) (store.AIGCTask, error)
	calls    int
	// byID 是每个任务被回查的次数（坏行降级要看的是这个，不是总数）。
	byID map[string]int
	// sawCtx 观察回查拿到的 ctx（断言每个任务有自己的时限）。
	sawCtx func(context.Context)
}

func (p *fakeProber) TaskTerminal(status string) bool { return p.terminal[status] }

func (p *fakeProber) ProbeTask(ctx context.Context, task store.AIGCTask) (store.AIGCTask, error) {
	p.calls++
	if p.byID == nil {
		p.byID = map[string]int{}
	}
	p.byID[task.ID]++
	if p.sawCtx != nil {
		p.sawCtx(ctx)
	}
	if p.probe == nil {
		return store.AIGCTask{}, errors.New("厂商不可达")
	}
	return p.probe(task)
}

// settleNow 是对账测试的时钟：真实当前时间之后一小时。
//
// 任务行的 updated_at 由 store 按**真实**时钟写入（CreateAIGCTask 与
// UpdateAIGCTaskObserved 都用 time.Now），而 ListUnsettledAIGCTasks 的谓词是
// 「updated_at 早于 now − settleStaleness」——Meter 的时钟必须走在真实时钟
// 前面，行才够「陈旧」到进对账视野。固定一个 2026 年的字面时刻是行不通的：
// 它与机器的真实时钟没有确定的先后关系。
func settleNow() time.Time { return time.Now().Add(time.Hour).In(shanghai) }

func defaultTerminal() map[string]bool {
	return map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "expired": true}
}

// seedTask 建一条「模型 + 上游 + 未清算任务行」，任务的 updated_at 足够陈旧。
func seedTask(t *testing.T, st *store.Store, kind, upType, pricing, status, usageJSON string, hasVideoInput bool, resolution string) store.AIGCTask {
	t.Helper()
	ctx := context.Background()
	up, err := st.CreateUpstream(ctx, "acct-"+upType, upType, "sk-not-a-real-key", "")
	if err != nil {
		t.Fatalf("CreateUpstream: %v", err)
	}
	if _, err := st.CreateModel(ctx, "vid-model", kind, pricing); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	task, err := st.CreateAIGCTask(ctx, store.NewAIGCTask{
		ID: "agt-test", VendorTaskID: "cgt-vendor", UpstreamID: up.ID, UpstreamName: up.Name,
		ModelName: "vid-model", Kind: kind,
		KeyID: 7, KeyDisplay: "sk_prefix12…wxyz",
		Status: status, HasVideoInput: hasVideoInput, ReqResolution: resolution,
		CreatedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateAIGCTask: %v", err)
	}
	if usageJSON != "" {
		if _, err := st.UpdateAIGCTaskObserved(ctx, task.ID, store.AIGCObservation{
			Status: status, UsageJSON: usageJSON,
		}); err != nil {
			t.Fatalf("UpdateAIGCTaskObserved: %v", err)
		}
		task.UsageJSON, task.Status = usageJSON, status
	}
	return *task
}

// 终态但未清算的任务：一轮对账把它按清算时点价入账，落库 + 预算 + 环三处
// 一致，且**不重复计 requests**（提交那一刻已经记过一笔）。
func TestSettleTerminalTask(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}

	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000,"ark_video_token_ref":46000000}`,
		"succeeded", `{"completion_tokens":246840,"total_tokens":246840}`, false, "")

	m.settle(ctx)

	wantCost := int64(246840 * 28)
	bucket := now.UTC().Unix() / 3600
	rows, err := st.QueryUsageRange(ctx, bucket, bucket+1)
	if err != nil {
		t.Fatalf("QueryUsageRange: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("清算落盘 %d 行, 期望 1", len(rows))
	}
	r := rows[0]
	if r.CostMicro != wantCost {
		t.Errorf("入账金额 = %d, 期望 %d", r.CostMicro, wantCost)
	}
	if r.Requests != 0 {
		t.Errorf("清算增量 requests = %d, 期望 0（提交时已计过一次，再计就是把一个任务数成两次调用）", r.Requests)
	}
	if r.EstimatedRequests != 0 {
		t.Errorf("有真值 usage 不该打估算标: %+v", r)
	}
	if r.Entry != EntryVideo || r.Kind != store.ModelKindVideo {
		t.Errorf("维度失真: entry=%q kind=%q", r.Entry, r.Kind)
	}
	if s := m.Spend(7); s.DayMicro != wantCost {
		t.Errorf("预算计数器 = %+v, 期望日额 %d", s, wantCost)
	}
	if evs := m.recent(); len(evs) != 1 || evs[0].CostMicro != wantCost || evs[0].TaskID != "agt-test" {
		t.Errorf("明细环 = %+v", evs)
	}

	// 二次对账是空操作：条件更新（cost_micro IS NULL）落空，绝不重复入账。
	m.settle(ctx)
	rows, _ = st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != wantCost {
		t.Errorf("二次对账后 = %+v, 期望金额不变", rows)
	}
	if s := m.Spend(7); s.DayMicro != wantCost {
		t.Errorf("二次对账后预算计数器 = %+v, 期望不变", s)
	}
}

// 开放态任务：先回查厂商，翻成终态才清算；仍未终结就等下一轮。
func TestSettleProbesOpenTask(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)

	task := seedTask(t, st, store.ModelKindVideo, config.UpstreamMinimax,
		`{"minimax_video_sec_2k":800000,"minimax_video_sec_768p":500000}`,
		"running", "", false, "2K")
	_ = task

	still := &fakeProber{
		terminal: defaultTerminal(),
		probe: func(tk store.AIGCTask) (store.AIGCTask, error) {
			return tk, nil // 还在跑
		},
	}
	m.prober = still
	m.settle(ctx)
	if still.calls != 1 {
		t.Fatalf("回查次数 = %d, 期望 1", still.calls)
	}
	bucket := now.UTC().Unix() / 3600
	if rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1); len(rows) != 0 {
		t.Fatalf("未终结的任务不该入账: %+v", rows)
	}

	// 下一轮厂商回了终态 + usage。
	m.prober = &fakeProber{
		terminal: defaultTerminal(),
		probe: func(tk store.AIGCTask) (store.AIGCTask, error) {
			tk.Status = "succeeded"
			tk.UsageJSON = `{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}`
			return tk, nil
		},
	}
	m.settle(ctx)
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != 5*800_000 {
		t.Fatalf("清算结果 = %+v, 期望 4e6 微元", rows)
	}
}

// 厂商回查失败：本轮跳过，行留着下轮再来（绝不当成 0 元入账）。
func TestSettleSkipsWhenProbeFails(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"running", "", false, "")
	m.prober = &fakeProber{terminal: defaultTerminal()} // probe 为 nil → 报错

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	if rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1); len(rows) != 0 {
		t.Fatalf("回查失败不该入账: %+v", rows)
	}
	left, err := st.ListUnsettledAIGCTasks(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListUnsettledAIGCTasks: %v", err)
	}
	if len(left) != 1 {
		t.Fatalf("回查失败后未清算行数 = %d, 期望 1（留着下轮）", len(left))
	}
}

// estimated 标记的四种归宿：真值 / 过期 / 完成但无 usage / 失败取消。
func TestSettleEstimatedFlag(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		usageJSON     string
		wantCost      int64
		wantEstimated bool
	}{
		{"有真值 usage", "succeeded", `{"completion_tokens":1000000}`, 28_000_000, false},
		{"厂商记录已过保留窗口 → 记 0 并标估算", "expired", "", 0, true},
		{"完成却没回 usage → 记 0 并标估算", "succeeded", "", 0, true},
		{"失败任务 → 记 0 但**不**标估算（厂商本就不计费，0 是已知真值）", "failed", "", 0, false},
		{"排队期取消 → 同上", "cancelled", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := settleNow()
			m, st := newTestMeter(t, shanghai, now)
			m.prober = &fakeProber{terminal: defaultTerminal()}
			seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
				`{"ark_video_token":28000000}`, tc.status, tc.usageJSON, false, "")

			m.settle(ctx)

			bucket := now.UTC().Unix() / 3600
			rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
			if len(rows) != 1 {
				t.Fatalf("落盘 %d 行, 期望 1", len(rows))
			}
			if rows[0].CostMicro != tc.wantCost {
				t.Errorf("金额 = %d, 期望 %d", rows[0].CostMicro, tc.wantCost)
			}
			gotEstimated := rows[0].EstimatedRequests == 1
			if gotEstimated != tc.wantEstimated {
				t.Errorf("estimated = %v, 期望 %v", gotEstimated, tc.wantEstimated)
			}
		})
	}
}

// 上游行被删（本表无外键，账单事实照留）：认不出计价形态，记 0 + 估算标，
// 绝不按别家的价目表瞎算一笔。
func TestSettleUnknownUpstreamRecordsZeroEstimated(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}

	task := seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":1000000}`, false, "")
	if err := st.DeleteUpstream(ctx, task.UpstreamID); err != nil {
		t.Fatalf("DeleteUpstream: %v", err)
	}

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != 0 || rows[0].EstimatedRequests != 1 {
		t.Errorf("上游已删时应记 0 元 + 估算标, 得 %+v", rows)
	}
}

// 未接线（prober 为 nil）时整个对账环节跳过——不去打厂商，也不拿一份复制的
// 状态词汇表自作主张。
func TestSettleNoopWithoutProber(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"succeeded", `{"completion_tokens":1000000}`, false, "")

	m.settle(ctx) // prober == nil

	bucket := now.UTC().Unix() / 3600
	if rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1); len(rows) != 0 {
		t.Errorf("未接线时不该有任何入账: %+v", rows)
	}
}

// 只有「多久没变过」超过 settleStaleness 的行才进对账视野——刚被客户端观测过
// 的任务留给查询路径，避免与它抢着打厂商。
func TestSettleSkipsFreshTasks(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	// CreateAIGCTask 的 updated_at 取真实当前时间，而 Meter 的时钟停在
	// 2026-08-08——把时钟推到「刚刚」，这一行就是新鲜的。
	m.now = func() time.Time { return time.Now() }

	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"succeeded", `{"completion_tokens":1000000}`, false, "")

	m.settle(ctx)

	left, _ := st.ListUnsettledAIGCTasks(ctx, time.Now(), 10)
	if len(left) != 1 {
		t.Errorf("新鲜的行不该被本轮清算，剩余未清算 = %d", len(left))
	}
}

// 「清算成功但进程随即退出」：因为清算与入账在**同一个事务**里，那笔钱已经
// 在账本里了——重启后的播种会把它读回预算计数器，一分不多一分不少。
//
// 这正是把两个动作合进一个事务要买的东西：分成两个事务时，进程在窗口里退出
// 会让这笔钱既不在账本上、行又已标清算，从此没有任何一条路径会回来补账。
func TestSettleSurvivesProcessExit(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":1000000}`, false, "")

	m.settle(ctx)
	wantCost := int64(28_000_000)
	if s := m.Spend(7); s.DayMicro != wantCost {
		t.Fatalf("清算后计数器 = %+v, 期望日额 %d", s, wantCost)
	}

	// 「进程退出」= 丢掉整个 Meter 的内存态（未冲刷的 delta、计数器、环），
	// 换一个新的挂到同一个库上重新播种。
	restarted := NewMeter(st, 90, shanghai, nil, quietLogger())
	restarted.now = func() time.Time { return now }
	if err := restarted.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if s := restarted.Spend(7); s.DayMicro != wantCost {
		t.Errorf("重启播种后 = %+v, 期望日额 %d（钱在事务里已落账本）", s, wantCost)
	}

	// 而且这笔账不会被再次清算：任务行上的 cost_micro 已非 NULL。
	left, err := st.ListUnsettledAIGCTasks(ctx, now, 10)
	if err != nil {
		t.Fatalf("ListUnsettledAIGCTasks: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("已清算的任务仍在未清算清单里: %+v", left)
	}
}

// 外审遗留（Phase 2）：模型行不在了（删除，或**改名**——aigc_tasks.model_name
// 是提交时的快照）应记 0 元 + 估算标 + 计入本轮告警，而不是当成「未定价」
// 悄悄以 0 元非估算一次性清算掉：改名是一次正常的管理动作，不该让窗口内的
// 所有任务在账面上无声消失。
func TestSettleRenamedModelIsMarkedEstimated(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}

	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":1000000}`, false, "")
	models, err := st.ListModelsWithSources(ctx)
	if err != nil || len(models) != 1 {
		t.Fatalf("ListModelsWithSources: %v (%d)", err, len(models))
	}
	if err := st.RenameModel(ctx, models[0].Model.ID, "vid-model-v2"); err != nil {
		t.Fatalf("RenameModel: %v", err)
	}

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 {
		t.Fatalf("落盘 %d 行, 期望 1", len(rows))
	}
	if rows[0].CostMicro != 0 || rows[0].EstimatedRequests != 1 {
		t.Errorf("改名后应记 0 元 + 估算标, 得 %+v", rows[0])
	}
	// 而「模型还在、只是没定价」仍是已知的 0 元，不打估算标（对照组）。
	m2, st2 := newTestMeter(t, shanghai, now)
	m2.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st2, store.ModelKindVideo, config.UpstreamArk, "", "succeeded",
		`{"completion_tokens":1000000}`, false, "")
	m2.settle(ctx)
	rows, _ = st2.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 || rows[0].CostMicro != 0 || rows[0].EstimatedRequests != 0 {
		t.Errorf("未定价模型应记 0 元且**不**打估算标, 得 %+v", rows)
	}
}

// ---- Phase 3.5：计价内核加固（外审在 Phase 3 查出的 5 处缺陷） ----

// 缺陷①：**临时性**的点查失败绝不能被当成「行已不存在」。清算是一次性语义，
// 按 0 元清算掉的任务再也扫不回来——库瞬时不可用时必须整笔跳过、留给下一轮，
// 而且不许把这个错误结论缓存进本轮（一缓存，同上游/同模型的整批任务跟着按
// 0 元清算，一次库抖动就能让一轮 50 笔钱集体消失）。
func TestTaskCostSkipsOnTransientLookupFailure(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	task := seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":1000000}`, false, "")
	st.Close() // 库瞬时不可用：点查报错，但**不是** ErrNotFound

	rd := newSettleRound()
	cost, estimated, tu, ok := m.taskCost(ctx, task, rd)
	if ok {
		t.Fatalf("临时性点查失败仍判定了金额（cost=%d estimated=%v）——一次性清算会让这笔钱永久消失", cost, estimated)
	}
	if (tu != TaskUsage{}) {
		t.Errorf("判不出金额却回了用量 %+v——那会给一笔「不知道多少钱」的账配上看着很确定的量", tu)
	}
	if len(rd.types) != 0 {
		t.Errorf("把临时故障缓存成「上游行已删」: %v（同上游的整批任务会跟着按 0 元清算）", rd.types)
	}
	if len(rd.prices) != 0 {
		t.Errorf("把临时故障缓存成「模型行已删」: %v", rd.prices)
	}
	if !rd.failed {
		t.Error("临时故障未标记本轮失败：告警压缩会在下一轮谎报「已恢复」")
	}
}

// 缺陷②：上游族与 usage 形态错配（ark 价签遇 minimax 秒数原文）时，
// 「见到任一认得的键就算可得」会解析出一份空测量值，任务以 0 元、**非估算**
// 被一次性清算——钱永久消失，而账面上一点异常都看不出来。
// 正确处置：判成「usage 不可得」，走 0 元 + 估算标 + 告警。
func TestSettleShapeMismatchIsEstimatedNotSilentZero(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	// ark（Seedance）的任务，厂商 usage 却是 minimax 的秒数形态。
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded",
		`{"output_seconds":5,"input_seconds":0,"input_image_count":0}`, false, "")

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 {
		t.Fatalf("落盘 %d 行, 期望 1", len(rows))
	}
	if rows[0].CostMicro != 0 {
		t.Errorf("形态对不上却算出了金额 %d——那是按别家的量套本家的价", rows[0].CostMicro)
	}
	if rows[0].EstimatedRequests != 1 {
		t.Errorf("形态错配应打估算标（这笔钱是**不知道**而不是 0），得 %+v", rows[0])
	}
}

// Phase 4 外审遗留：**价签这一侧**的同类失效。一个模型可以同时挂 ark 与
// minimax 两条来源，管理员只填了其中一家的价时 `Priced()` 仍为真——模型页
// 不挂「未定价」徽章，而另一家服务的任务会被 `Cost` 的档位回退算成 0 元、
// 以**非估算**一次性清算掉：钱永久消失，账面上看不出任何异常。
//
// 正确处置与 usage 形态错配同款：0 元 + 估算标 + 告警。
func TestSettlePriceTableWithoutThisVendorIsEstimated(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	// ark（Seedance）服务的任务，价目表里却只有 minimax 的秒价。
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"minimax_video_sec_2k":800000}`, "succeeded",
		`{"completion_tokens":246840,"total_tokens":246840}`, false, "")

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 {
		t.Fatalf("落盘 %d 行, 期望 1", len(rows))
	}
	if rows[0].CostMicro != 0 {
		t.Errorf("价目表里没有这家上游的字段，却算出了金额 %d", rows[0].CostMicro)
	}
	if rows[0].EstimatedRequests != 1 {
		t.Errorf("「有价但没这家的字段」的 0 元是**不知道**，必须打估算标，得 %+v", rows[0])
	}
}

// 对照：真·未定价（空表）的 0 元是**已知**的（政策如此，模型页有警示徽章），
// 不该打估算标——上一条的加固不能把这条也带偏。
func TestSettleUnpricedModelIsNotEstimated(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		"", "succeeded", `{"completion_tokens":246840,"total_tokens":246840}`, false, "")

	m.settle(ctx)

	bucket := now.UTC().Unix() / 3600
	rows, _ := st.QueryUsageRange(ctx, bucket, bucket+1)
	if len(rows) != 1 {
		t.Fatalf("落盘 %d 行, 期望 1", len(rows))
	}
	if rows[0].CostMicro != 0 || rows[0].EstimatedRequests != 0 {
		t.Errorf("未定价应记 0 元且**不**打估算标，得 %+v", rows[0])
	}
}

// 缺陷⑤之一：单个任务的回查有自己的时限。没有它，队头几个挂死的探针就能把
// 整轮 2 分钟预算耗光，后面的任务轮轮饿死。
func TestSettleProbeDeadlineIsPerTask(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"running", "", false, "")

	var deadlines []time.Duration
	m.prober = &fakeProber{
		terminal: defaultTerminal(),
		sawCtx: func(c context.Context) {
			dl, ok := c.Deadline()
			if !ok {
				t.Error("回查 ctx 没有时限：一个挂死的探针能吃掉整轮")
				return
			}
			deadlines = append(deadlines, time.Until(dl))
		},
	}
	m.settle(ctx)

	if len(deadlines) != 1 {
		t.Fatalf("回查次数 = %d, 期望 1", len(deadlines))
	}
	if deadlines[0] > settleProbeTimeout {
		t.Errorf("单次回查时限 = %v, 期望 ≤ %v（整轮预算不能被一个任务独吞）", deadlines[0], settleProbeTimeout)
	}
}

// 缺陷⑤之二：探针**永久失败**的坏行要降级跳过，否则它每轮都占着回查预算，
// 后面的任务跟着饿死。降级不是放弃——冷却到期后重新给它机会。
func TestSettleQuarantinesPermanentlyFailingProbe(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"running", "", false, "")
	p := &fakeProber{terminal: defaultTerminal()} // probe 恒失败
	m.prober = p

	for i := 0; i < settleProbeFailLimit; i++ {
		m.settle(ctx)
	}
	if p.byID["agt-test"] != settleProbeFailLimit {
		t.Fatalf("前 %d 轮的回查次数 = %d, 期望每轮各一次", settleProbeFailLimit, p.byID["agt-test"])
	}

	// 连败到阈值：进冷却，之后几轮一次都不该再打厂商。
	m.settle(ctx)
	m.settle(ctx)
	if p.byID["agt-test"] != settleProbeFailLimit {
		t.Errorf("坏行进冷却后仍被回查 %d 次——它会一直占着整轮的回查预算", p.byID["agt-test"])
	}

	// 冷却到期后重新尝试：跳过是降级，不是永久放弃（上游故障总会恢复）。
	m.now = func() time.Time { return now.Add(settleQuarantine + time.Minute) }
	m.settle(ctx)
	if p.byID["agt-test"] != settleProbeFailLimit+1 {
		t.Errorf("冷却到期后回查次数 = %d, 期望 %d（坏行被永久放弃了）",
			p.byID["agt-test"], settleProbeFailLimit+1)
	}
}

// Phase 3.5 外审：懒对账的清算**也是本进程往账本里写**（同一事务里落 delta，
// 不经过 flush），播种的重试窗口同样到此为止。少标这一处，下一轮播种会把刚
// 清算的这笔钱从库里读回来再加一遍——预算凭空变紧，拒掉本该放行的请求。
func TestSeedStopsAfterSettleWritesLedger(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "succeeded", `{"completion_tokens":1000000}`, false, "")

	m.settle(ctx) // 清算 28e6 微元，同一事务里已落进账本
	const want = int64(28_000_000)
	if s := m.Spend(7); s.DayMicro != want {
		t.Fatalf("清算后计数器 = %+v, 期望日额 %d", s, want)
	}

	// 此时才轮到播种重试（启动时那次失败了）：必须是空操作。
	if err := m.Seed(ctx); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if s := m.Spend(7); s.DayMicro != want {
		t.Errorf("清算写账本之后播种把这笔钱又加了一遍 = %d, 期望 %d", s.DayMicro, want)
	}
}

// Phase 3.5 外审：清算增量不是一次 HTTP 调用。失败/取消/过期的任务在明细环里
// 挂着 200 + 0 元，正是排查「这笔怎么没记钱」时最容易把人带偏的一行。
func TestSettleRingDoesNotClaimHTTPSuccess(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	m.prober = &fakeProber{terminal: defaultTerminal()}
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk,
		`{"ark_video_token":28000000}`, "failed", "", false, "")

	m.settle(ctx)

	evs := m.recent()
	if len(evs) != 1 {
		t.Fatalf("环里 %d 条, 期望 1", len(evs))
	}
	if evs[0].Status != 0 || evs[0].Attempts != 0 {
		t.Errorf("厂商侧失败的任务在环里报成 HTTP %d / %d 次尝试——清算行不是一次调用",
			evs[0].Status, evs[0].Attempts)
	}
	if evs[0].TaskID != "agt-test" {
		t.Errorf("清算行必须带 TaskID（读数侧据它认出这是清算而不是调用），得 %q", evs[0].TaskID)
	}
}

// 外审遗留（Phase 2）：本轮出过故障就不许清告警态——否则「WARN 之后紧跟一条
// 已恢复」会在每一轮重演，告警压缩形同虚设，而那条 INFO 还在撒谎。
func TestSettleWarnCompressionSurvivesFailedRound(t *testing.T) {
	ctx := context.Background()
	now := settleNow()
	m, st := newTestMeter(t, shanghai, now)
	seedTask(t, st, store.ModelKindVideo, config.UpstreamArk, `{"ark_video_token":28000000}`,
		"running", "", false, "")
	m.prober = &fakeProber{terminal: defaultTerminal()} // probe 恒失败

	m.settle(ctx)
	if !m.settleWarned {
		t.Fatal("回查失败后应处于告警态")
	}
	m.settle(ctx)
	if !m.settleWarned {
		t.Error("连续失败的第二轮把告警态清掉了——下一次失败会再刷一条 WARN，且中间还多了一条假的「已恢复」")
	}

	// 真的恢复了才清：让回查成功并把任务翻成终态。
	m.prober = &fakeProber{
		terminal: defaultTerminal(),
		probe: func(tk store.AIGCTask) (store.AIGCTask, error) {
			tk.Status = "succeeded"
			tk.UsageJSON = `{"completion_tokens":1000000}`
			return tk, nil
		},
	}
	m.settle(ctx)
	if m.settleWarned {
		t.Error("恢复后仍处于告警态")
	}
}
