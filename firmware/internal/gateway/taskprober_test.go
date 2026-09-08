// taskprober_test.go 是 iteration-9 Phase 5 的验收：懒对账的探针接线。
//
// 三件事：①「假上游 + 从不轮询的任务 → 一轮对账后金额进账本」的端到端（这条
// 链路漏接是个静默故障——客户端弃轮询的视频任务永远不入账）；②探针尊重每任务
// 的可取消 ctx（不然队头一个挂死的厂商就能吃光整轮预算，后面的任务轮轮饿死）；
// ③取消路径观测到终态同样就地清算（此前只有查询路径做了）。
package gateway_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 第二条连接：改 store 不开写口的观测时刻

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// minimaxVideoPricing 是 H3 768P 档的目录价：0.5 元 / 秒。
const minimaxVideoPricing = `{"minimax_video_sec_768p":500000}`

// backdateTask 把任务行的 updated_at 改早，好让它落进懒对账的陈旧视野
// （ListUnsettledAIGCTasks 判的是 updated_at < now − settleStaleness）。
//
// 直改库是有意的：那一列的语义是「最近一次观测厂商」，store 不该、也确实没有
// 提供把它改早的写口——这里要的是一台时间机器，不是一个生产接口。
func backdateTask(t *testing.T, dir, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, store.DBFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开第二条连接: %v", err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE aigc_tasks SET updated_at = ? WHERE vendor_task_id = ?`, "2020-01-01T00:00:00.000Z", id)
	if err != nil {
		t.Fatalf("改任务观测时刻: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("改任务观测时刻影响 %d 行，期望 1", n)
	}
}

// backdateCreated 把任务行的 created_at 改早（保留期清理用例的时间机器；
// ts 是 store 的固定宽度 UTC 毫秒文本）。
func backdateCreated(t *testing.T, dir, vendorID, ts string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, store.DBFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开第二条连接: %v", err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE aigc_tasks SET created_at = ? WHERE vendor_task_id = ?`, ts, vendorID)
	if err != nil {
		t.Fatalf("改任务创建时刻: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("改任务创建时刻影响 %d 行，期望 1", n)
	}
}

// TestLazySettleProbesNeverPolledTask：客户端提交完就再也不来轮询，懒对账仍
// 要现查厂商、算出金额、写进任务行与账本，并喂进预算计数器。
func TestLazySettleProbesNeverPolledTask(t *testing.T) {
	e := newVideoEnv(t)
	setModelPricing(t, e.routeEnv, videoModel, minimaxVideoPricing)
	// 探针就是 *gateway.Server（cmd/llmgate 的装配同款）。
	m := usage.NewMeter(e.st, 31, time.UTC, e.srv, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)

	id := submitVideo(t, e) // 提交之后一次都不查（resolution 768P、duration 4）
	backdateTask(t, e.dir, id)
	e.mm.setQuery(http.StatusOK, fmt.Sprintf(
		`{"task":{"id":%q,"status":"succeeded","content":{"url":"https://cdn.example.net/h3.mp4"},"usage":{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}}}`,
		id))
	_, queriesBefore, _ := e.mm.counts()

	m.SettleOnce(t.Context())

	// 5 输出秒 × 0.5 元/秒 = 2.5 元 = 2_500_000 微元。
	const want = 2_500_000
	uid := testKeyID(t, e.st)
	task, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), id, uid)
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.CostMicro == nil {
		t.Fatal("一轮对账后任务行仍未清算（cost_micro 为 NULL）——探针没接上，弃轮询的任务永远不入账")
	}
	if *task.CostMicro != want {
		t.Errorf("清算金额 = %d 微元，期望 %d", *task.CostMicro, want)
	}
	if task.Estimated {
		t.Error("厂商 usage 拿到了，不该打估算标")
	}
	if task.Status != "succeeded" {
		t.Errorf("对账应把观测写回任务行，status = %q", task.Status)
	}
	if _, queries, _ := e.mm.counts(); queries != queriesBefore+1 {
		t.Errorf("厂商回查次数 = %d，期望恰好比对账前多 1 次", queries-queriesBefore)
	}

	// 账本里有这笔钱（清算与入账在同一个事务里落库）。
	rep, err := m.Report(t.Context(), time.Now().Add(-2*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Total.CostMicro != want {
		t.Errorf("账本区间合计 = %d 微元，期望 %d", rep.Total.CostMicro, want)
	}
	// 清算增量不计 requests（提交那一刻访问日志已记过一笔）。
	if rep.Total.Requests != 1 {
		t.Errorf("账本请求数 = %d，期望 1（提交记的那一笔，清算不再记）", rep.Total.Requests)
	}
	// 预算计数器同步吃到（重启后的准入才判得准）。
	if sp := m.Spend(testKeyID(t, e.st)); sp.DayMicro != want {
		t.Errorf("预算计数器 = %+v，期望日额 %d 微元", sp, want)
	}

	// 再跑一轮不重复入账（一次性语义压在条件更新上）。
	m.SettleOnce(t.Context())
	rep2, err := m.Report(t.Context(), time.Now().Add(-2*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Report(第二轮): %v", err)
	}
	if rep2.Total.CostMicro != want {
		t.Errorf("第二轮对账后账本合计 = %d 微元，期望仍是 %d（不得重复入账）", rep2.Total.CostMicro, want)
	}
}

// TestTaskProberHonorsContext：厂商挂死时，探针必须被调用方给的 ctx 打断。
// 没有这一条，队头一个坏行就能耗光整轮对账预算，排在后面的任务轮轮饿死
// （懒对账给每个任务 30s，Phase 3.5 定的）。
func TestTaskProberHonorsContext(t *testing.T) {
	e := newVideoEnv(t)
	// 永不回应的假上游：只在请求被取消时才返回。
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	upID := dbUpstream(t, e.st, "mm-hang", config.UpstreamMinimax, "sk-hang-not-real", hang.URL)
	task, err := e.st.CreateAIGCTask(t.Context(), store.NewAIGCTask{
		ID: "agt-hang-1", VendorTaskID: "424010985738888",
		UpstreamID: upID, UpstreamName: "mm-hang",
		ModelName: videoModel, Kind: store.ModelKindVideo,
		KeyID: 1, KeyDisplay: "sk_hanghangha…hang", Status: "running",
	})
	if err != nil {
		t.Fatalf("CreateAIGCTask: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := e.srv.ProbeTask(ctx, *task)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("厂商挂死时探针应报错返回，而不是当作成功观测")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("探针没有被 ctx 打断（挂死的厂商会拖垮整轮对账）")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("探针返回耗时 %v，远超 ctx 的 150ms 预算", el)
	}
}

// TestVideoCancelSettlesTerminalTask：取消把任务推进终态，账要就地清算——
// 不然这一行要等懒对账那一轮才落定（金额通常为 0，但「已清算」本身决定了
// 对账协程要不要每轮再扫它一遍）。
func TestVideoCancelSettlesTerminalTask(t *testing.T) {
	e := newVideoEnv(t)
	setModelPricing(t, e.routeEnv, videoModel, minimaxVideoPricing)
	m := usage.NewMeter(e.st, 31, time.UTC, e.srv, logging.New(io.Discard, slog.LevelDebug))
	e.srv.EnableMetering(m)

	id := submitVideo(t, e)
	if w := do(e.h, "DELETE", "/minimax/v2/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("取消状态码 = %d；body: %s", w.Code, w.Body.String())
	}

	task, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), id, testKeyID(t, e.st))
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey: %v", err)
	}
	if task.Status != "cancelled" {
		t.Fatalf("取消后 status = %q，期望 cancelled", task.Status)
	}
	if task.CostMicro == nil {
		t.Fatal("取消到终态后任务行应已清算（cost_micro 非 NULL），否则要等懒对账才落定")
	}
	if *task.CostMicro != 0 {
		t.Errorf("排队期取消不计费，金额 = %d 微元，期望 0", *task.CostMicro)
	}
	if task.Estimated {
		t.Error("取消的 0 元是已知值，不该打估算标")
	}
}
