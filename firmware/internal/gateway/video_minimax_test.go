// video_minimax_test.go 钉住 minimax 协议面里最容易坏在细节上的一条：受理
// 响应的 task_id 是**数字字面量**（文档示例 15 位长串，float64 解析会丢精度）
// 时，设备必须按原字面透传与保存——客户端拿到的 id、行的检索键、回查厂商的
// 路径三者逐字节一致。其余全链路契约在 video_test.go（同一协议面、字符串 id）。
package gateway_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// TestMinimaxNumericTaskID：厂商以 JSON 数字返回 task_id → 受理响应逐字节
// 透传（数字形态原样）、行按字面量检索得到、查询命中厂商路径的同一字面量。
func TestMinimaxNumericTaskID(t *testing.T) {
	const numericID = "424010985738629" // 15 位：float64 会舍入到 …630 附近
	e := newRouteEnv(t)
	f := newFakeMinimax(t)
	f.setCreate(http.StatusOK, fmt.Sprintf(`{"task_id":%s}`, numericID))
	up := dbUpstream(t, e.st, "mm-numeric", config.UpstreamMinimax, "sk-mm-numeric-not-real", f.url)
	dbSource(t, e.st, dbKindModel(t, e.st, "MiniMax-H3-num", store.ModelKindVideo), up, "", 100)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		`{"model":"MiniMax-H3-num","content":[{"type":"text","text":"猫"}],"resolution":"768P","duration":4}`)
	if w.Code != http.StatusOK {
		t.Fatalf("提交状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	// 数字字面量原样透传（不经 float64 往返）。
	if want := fmt.Sprintf(`{"task_id":%s}`, numericID); w.Body.String() != want {
		t.Errorf("受理响应 = %q，期望逐字节 %q", w.Body.String(), want)
	}
	// 行按字面量检索得到。
	task, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), numericID, testKeyID(t, e.st))
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey(%s): %v", numericID, err)
	}
	if task.VendorTaskID != numericID {
		t.Errorf("行内厂商 id = %q，期望字面量 %s", task.VendorTaskID, numericID)
	}
	// 查询命中厂商路径的同一字面量。
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+numericID, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if ids := f.sentTaskIDs(); len(ids) != 1 || ids[0] != numericID {
		t.Errorf("厂商查询命中 id = %v，期望 [%s]", ids, numericID)
	}
	if strings.Contains(w.Body.String(), "424010985738630") {
		t.Error("数字 id 经 float64 舍入泄露")
	}
}
