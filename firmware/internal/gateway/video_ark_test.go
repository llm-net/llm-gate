// video_ark_test.go 是方舟 Seedance 官方协议面（2026-08-10 接入）的可执行
// 验收。它与 video_test.go 是同一条任务面流程的两个厂商实例，所以这里只钉
// **方舟特有**的那几件，不重复验收 minimax 已覆盖的通用语义（归属 404、
// 计费特征列、保留期清理…）：
//
//   - 客户端路径是 /api/v3/…（厂商站点根之后的官方那一段），而设备→厂商那
//     一跳的路径不含 /api/v3——那一段在端点根里；
//   - 受理响应形态是 {"id":"cgt-…"}（minimax 是 {"task_id":…}），透传原样；
//   - 查询响应无 {"task":…} 信封，model 在顶层，回显改写到客户端可见名；
//   - **跨族的任务 id 一律 404**：方舟 id 走 minimax 路径、minimax id 走方舟
//     路径都拿不到东西，否则客户端会收到一份自己那族文档解释不了的响应；
//   - 按量与订阅同族多来源：订阅优先、429 时切到按量（一个模型两条通道正是
//     这次接入要支撑的形态）；
//   - 刻意不挂载的路径（任务列表、方舟自己的 chat）落 404。
package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	arkVideoModel     = "doubao-seedance-2.0"
	arkVideoUpModelID = "doubao-seedance-2-0-260128" // 来源侧 ID 与模型名不同，改写可证
	arkVideoKey       = "sk-ark-video-not-real"
	arkVendorTaskID   = "cgt-20260810120000-abcde"

	// arkTasksPath 是设备→厂商那一跳的路径（端点根由 base_url 覆盖，故不含
	// /api/v3）；arkClientTasksPath 是客户端那一跳的路径。
	arkTasksPath       = "/contents/generations/tasks"
	arkClientTasksPath = "/ark/api/v3/contents/generations/tasks"
)

// fakeArk 是方舟视频任务面的假上游：受理 / 查询 / 取消三类路由，响应可编程。
type fakeArk struct {
	url string

	mu       sync.Mutex
	create   fakeReply
	query    fakeReply
	del      fakeReply
	creates  int
	queries  int
	deletes  int
	bodies   [][]byte
	headers  []http.Header
	taskIDs  []string
	lastPath string
}

func newFakeArk(t *testing.T) *fakeArk {
	t.Helper()
	f := &fakeArk{
		create: fakeReply{http.StatusOK, fmt.Sprintf(`{"id":%q}`, arkVendorTaskID)},
		query: fakeReply{http.StatusOK, fmt.Sprintf(
			`{"id":%q,"model":%q,"status":"succeeded",`+
				`"content":{"video_url":"https://ark-cdn.example.com/v.mp4?sig=1"},`+
				`"usage":{"completion_tokens":128000,"total_tokens":128000},"duration":5}`,
			arkVendorTaskID, arkVideoUpModelID)},
		// 方舟的 DELETE 无返回参数（文档 §7）：空体 200。
		del: fakeReply{http.StatusOK, ""},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+arkTasksPath, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.creates++
		f.bodies = append(f.bodies, b)
		f.headers = append(f.headers, r.Header.Clone())
		f.lastPath = r.URL.Path
		reply := f.create
		f.mu.Unlock()
		writeReply(w, reply)
	})
	mux.HandleFunc("GET "+arkTasksPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries++
		f.taskIDs = append(f.taskIDs, r.PathValue("id"))
		reply := f.query
		f.mu.Unlock()
		writeReply(w, reply)
	})
	mux.HandleFunc("DELETE "+arkTasksPath+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletes++
		f.taskIDs = append(f.taskIDs, r.PathValue("id"))
		reply := f.del
		f.mu.Unlock()
		writeReply(w, reply)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *fakeArk) counts() (creates, queries, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.queries, f.deletes
}

func (f *fakeArk) sentCreate(i int) ([]byte, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		return nil, nil
	}
	return f.bodies[i], f.headers[i]
}

// arkEnv 是方舟任务面用例环境：routeEnv + 一个 ark_plan（订阅）型假上游与
// kind=video 模型。订阅型是刻意的——本次接入的验收路径就是它。
type arkEnv struct {
	*routeEnv
	ark     *fakeArk
	modelID int64
}

func newArkVideoEnv(t *testing.T) *arkEnv {
	t.Helper()
	e := newRouteEnv(t)
	f := newFakeArk(t)
	up := dbUpstream(t, e.st, "ark-plan-main", config.UpstreamArkPlan, arkVideoKey, f.url)
	mid := dbKindModel(t, e.st, arkVideoModel, store.ModelKindVideo)
	dbSource(t, e.st, mid, up, arkVideoUpModelID, 100)
	return &arkEnv{routeEnv: e, ark: f, modelID: mid}
}

// submitArkVideo 提交一个方舟视频任务并返回客户端拿到的任务 id。
func submitArkVideo(t *testing.T, e *arkEnv) string {
	t.Helper()
	w := do(e.h, "POST", arkClientTasksPath, chatAuth, fmt.Sprintf(
		`{"model":%q,"content":[{"type":"text","text":"一只猫"}],"resolution":"720p","duration":5}`,
		arkVideoModel))
	if w.Code != http.StatusOK {
		t.Fatalf("受理状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("受理响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if v.ID != arkVendorTaskID {
		t.Fatalf("客户端可见任务 id = %q，期望厂商原值 %q", v.ID, arkVendorTaskID)
	}
	return v.ID
}

// TestArkVideoFullPath：提交 → 查询 → 取消的方舟全链路。钉住两跳路径不同、
// 受理响应逐字透传、请求体 model 改写为来源侧 ID、凭证换成上游 Key、
// 查询响应顶层 model 回显改写、观测落任务行。
func TestArkVideoFullPath(t *testing.T) {
	e := newArkVideoEnv(t)
	id := submitArkVideo(t, e)

	body, hdr := e.ark.sentCreate(0)
	if got := hdr.Get("Authorization"); got != "Bearer "+arkVideoKey {
		t.Errorf("上游凭证 = %q，期望上游 Key", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("厂商收到的体不是 JSON: %v", err)
	}
	if sent["model"] != arkVideoUpModelID {
		t.Errorf("厂商收到的 model = %v，期望来源侧 ID %q", sent["model"], arkVideoUpModelID)
	}
	if sent["resolution"] != "720p" {
		t.Errorf("请求体未字节保真透传: %v", sent)
	}

	// 任务行：厂商 id 映射回受理它的上游与归属用户，计费特征已固化。
	task := taskByVendorIDIn(t, e.routeEnv, arkVendorTaskID)
	if task.ModelName != arkVideoModel || task.Kind != store.ModelKindVideo {
		t.Errorf("任务行维度不对: model=%q kind=%q", task.ModelName, task.Kind)
	}
	if task.ReqResolution != "720p" || task.ReqDuration != "5" || task.HasVideoInput {
		t.Errorf("计费特征未固化: %+v", task)
	}

	// 查询：响应透传，但顶层 model 回显改写回客户端可见名。
	w := do(e.h, "GET", arkClientTasksPath+"/"+id, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("查询响应不是 JSON: %v", err)
	}
	if got["model"] != arkVideoModel {
		t.Errorf("查询响应 model = %v，期望回写为 %q", got["model"], arkVideoModel)
	}
	if strings.Contains(w.Body.String(), arkVideoUpModelID) {
		t.Errorf("响应泄露来源侧模型 ID: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ark-plan-main") {
		t.Errorf("响应泄露上游账户名: %s", w.Body.String())
	}
	// usage 与产物 URL 进任务行（账单事实 + 排障副本），状态转终态。
	task = taskByVendorIDIn(t, e.routeEnv, arkVendorTaskID)
	if task.Status != "succeeded" || !strings.Contains(task.UsageJSON, `"completion_tokens":128000`) {
		t.Errorf("观测未落行: status=%q usage=%q", task.Status, task.UsageJSON)
	}

	// 取消：方舟 DELETE 无返回体，照样透传；取消前对非终态行的回查此处不触发
	// （行已是终态）。
	w = do(e.h, "DELETE", arkClientTasksPath+"/"+id, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("取消状态码 = %d，期望 200", w.Code)
	}
	if _, queries, deletes := e.ark.counts(); deletes != 1 || queries != 1 {
		t.Errorf("厂商侧调用次数不对: queries=%d deletes=%d（终态行不该再回查）", queries, deletes)
	}
}

// TestArkVideoCrossFamily404：跨协议面的任务 id 一律 404 invalid_task_id，
// 与「不存在」同响应。这是「一个模型固定一种 API 格式」的边界——放行就意味着
// 客户端可能拿回一份自己那族文档解释不了的响应。
func TestArkVideoCrossFamily404(t *testing.T) {
	e := newArkVideoEnv(t)
	id := submitArkVideo(t, e)

	// 错误形态随**客户端所用入口路径**走，不随任务所属的协议面走：这两条是
	// /minimax 段的路径，所以回的是 MiniMax v2 形（错误码在 error.type 上）。
	for _, tc := range []struct{ method, path string }{
		{"GET", "/minimax/v2/query/video_generation/" + id},
		{"DELETE", "/minimax/v2/video_generation/" + id},
	} {
		w := do(e.h, tc.method, tc.path, chatAuth, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s 状态码 = %d，期望 404", tc.method, tc.path, w.Code)
		}
		if typ, _ := decodeMinimaxError(t, w); typ != "invalid_task_id" {
			t.Errorf("%s %s error.type = %q，期望 invalid_task_id", tc.method, tc.path, typ)
		}
	}
	// 反向：minimax 的任务 id 走方舟路径同样 404（另一套环境里的 id，这里用
	// 一个不存在的 id 表达同一口径——两种情况本就不该区分）。
	w := do(e.h, "GET", arkClientTasksPath+"/no-such-task", chatAuth, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("未知任务 id 状态码 = %d，期望 404", w.Code)
	}
	if _, queries, deletes := e.ark.counts(); queries != 0 || deletes != 0 {
		t.Errorf("跨族/未知 id 不该打到厂商: queries=%d deletes=%d", queries, deletes)
	}
}

// TestArkVideoSubscriptionThenUsage：同一视频模型挂订阅（100）与按量（200）
// 两条来源——订阅先试，429 时切到按量。这正是「火山方舟（订阅）+（用量）」
// 这次要支撑的形态，两条来源同族所以管理侧也放行。
func TestArkVideoSubscriptionThenUsage(t *testing.T) {
	e := newArkVideoEnv(t)
	// 订阅那条改成恒 429（配额用尽）；按量那条是另一台假上游，正常受理。
	e.ark.mu.Lock()
	e.ark.create = fakeReply{http.StatusTooManyRequests, `{"error":{"code":"QuotaExceeded"}}`}
	e.ark.mu.Unlock()

	payg := newFakeArk(t)
	up := dbUpstream(t, e.st, "ark-payg", config.UpstreamArk, "sk-ark-payg-not-real", payg.url)
	dbSource(t, e.st, e.modelID, up, arkVideoUpModelID, 200)

	id := submitArkVideo(t, &arkEnv{routeEnv: e.routeEnv, ark: payg, modelID: e.modelID})
	if plan, _, _ := e.ark.counts(); plan != 1 {
		t.Errorf("订阅来源受理次数 = %d，期望先试一次", plan)
	}
	if usage, _, _ := payg.counts(); usage != 1 {
		t.Errorf("按量来源受理次数 = %d，期望切换过去一次", usage)
	}
	// 任务钉死在真正受理它的那条来源上：查询必须打到按量那台。
	if w := do(e.h, "GET", arkClientTasksPath+"/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d，期望 200", w.Code)
	}
	if _, q, _ := payg.counts(); q != 1 {
		t.Errorf("查询没打到受理任务的那条来源: queries=%d", q)
	}
	if _, q, _ := e.ark.counts(); q != 0 {
		t.Errorf("查询打到了没受理这个任务的来源: queries=%d", q)
	}
}

// TestArkUnmountedPaths：刻意不挂载的方舟路径落 404（认证之后）。任务列表是
// 厂商侧的全账户视角，转发会把别人的任务泄给同设备任何 Key；方舟自己的 chat
// 路径不挂是因为文本入口恒为 /v1/chat/completions，一个协议只留一个入口。
func TestArkUnmountedPaths(t *testing.T) {
	e := newArkVideoEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", arkClientTasksPath},                      // 任务列表
		{"POST", "/ark/api/v3/chat/completions"},         // 方舟文本
		{"POST", "/ark/api/v3/embeddings"},               // 其余方舟端点
		{"POST", "/ark/api/v3/contents/generations/xyz"}, // 相邻未知路径
	} {
		w := do(e.h, tc.method, tc.path, chatAuth, `{}`)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d，期望 404", tc.method, tc.path, w.Code)
		}
	}
	// 无凭证一律 401（与 /v1、/v2 同一口径：先认证再 404）。
	if w := do(e.h, "GET", arkClientTasksPath, nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证状态码 = %d，期望 401", w.Code)
	}
}

// TestArkFaceSegmentRequired：火山方舟协议面同样只认带 /ark 段的路径。无段的
// /api/v3/… 不是数据面入口，落进根路由的兜底，且一个字节都不到厂商。
func TestArkFaceSegmentRequired(t *testing.T) {
	e := newArkVideoEnv(t)
	body := fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, arkVideoModel)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/v3/contents/generations/tasks"},
		{"GET", "/api/v3/contents/generations/tasks/" + arkVendorTaskID},
		{"DELETE", "/api/v3/contents/generations/tasks/" + arkVendorTaskID},
		{"POST", "/api/v3/images/generations"},
	} {
		if w := do(e.h, tc.method, tc.path, chatAuth, body); w.Code != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d，期望 404（无厂商段不是入口）", tc.method, tc.path, w.Code)
		}
	}
	if c, q, d := e.ark.counts(); c != 0 || q != 0 || d != 0 {
		t.Errorf("无段路径打到了厂商: creates=%d queries=%d deletes=%d", c, q, d)
	}
}
