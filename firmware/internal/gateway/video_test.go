// video_test.go 是厂商官方接口任务面（2026-08-09 改版）的可执行验收：MiniMax
// /v2 四条路由的**纯转发**契约——受理/查询/取消响应逐字节透传（客户端可见的
// 任务 id 就是厂商 task_id）、查询响应仅 task.model 回显改写、旁路观测落任务
// 行（状态/usage/产物 URL、终态就地清算由 metering_test 验收）、归属 404、
// kind 闸门、提交侧故障切换与最终错误透传、计费特征列落库、任务级失败
// （HTTP 200 + status=failed + code/message）入行、厂商记录过期（404 透传 +
// 内部 expired 归一）、Context-IR 变体路径、刻意不挂载的厂商端点 404。
//
// 编排方式：httptest 假上游以真实类型 minimax + base_url 覆盖挂进目录（store
// 直建等价 dev 路径；管理 API 的白名单不拦库层写入，测试走库是既有惯例），
// 断言"客户端看到什么"与"厂商收到什么"。
package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/gateway"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	otherKey  = "sk_neighbor_9876543210fedcba"
	otherUser = "neighbor"

	videoModel       = "MiniMax-H3"
	videoUpModelID   = "mm-side/MiniMax-H3" // 来源侧 ID 与模型名不同，改写可证
	videoUpstreamKey = "sk-mm-video-not-real"
	// fakeVendorTaskID 是假上游第一次受理签发的厂商 id（后续受理递增后缀——
	// 厂商 id 唯一，UNIQUE(vendor_task_id, upstream_id) 才立得住）。改版后它
	// 就是客户端可见的任务 id。
	fakeVendorTaskID = "424010985738001"
)

// otherAuth 是第二个用户（归属用例）的请求头。
var otherAuth = map[string]string{"Authorization": "Bearer " + otherKey, "Content-Type": "application/json"}

// fakeReply 是一次可编程响应。
type fakeReply struct {
	status int
	body   string
}

// fakeMinimax 是 MiniMax /v2 任务面的假上游：受理（video_generation 与
// h3_context_ir 两个创建端点）/查询/取消三类路由，各步响应可编程；同时记录
// 厂商侧收到的请求以供断言。
type fakeMinimax struct {
	url string

	mu          sync.Mutex
	create      fakeReply
	query       fakeReply
	queryQueue  []fakeReply // 一次性队列，先于 query 消费
	del         fakeReply
	creates     int
	queries     int
	deletes     int
	gotBodies   [][]byte      // 受理请求体
	gotHeaders  []http.Header // 受理请求头
	gotPaths    []string      // 受理请求路径（video_generation / h3_context_ir）
	gotTaskIDs  []string      // 查询/取消命中的厂商任务 id
	gotQueryHdr []http.Header // 查询请求头（透传断言用）
}

func newFakeMinimax(t *testing.T) *fakeMinimax {
	t.Helper()
	f := &fakeMinimax{
		query: fakeReply{http.StatusOK,
			fmt.Sprintf(`{"task":{"id":%q,"model":%q,"status":"running"}}`, fakeVendorTaskID, videoUpModelID)},
		del: fakeReply{http.StatusOK,
			fmt.Sprintf(`{"task_id":%q,"action":"cancelled","status":"cancelled"}`, fakeVendorTaskID)},
	}
	create := func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.creates++
		f.gotBodies = append(f.gotBodies, b)
		f.gotHeaders = append(f.gotHeaders, r.Header.Clone())
		f.gotPaths = append(f.gotPaths, r.URL.Path)
		reply := f.create
		if reply.status == 0 { // 未显式编程：按受理序签发唯一厂商 id（首个即 fakeVendorTaskID）
			reply = fakeReply{http.StatusOK, fmt.Sprintf(`{"task_id":"42401098573800%d"}`, f.creates)}
		}
		f.mu.Unlock()
		writeReply(w, reply)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/video_generation", create)
	mux.HandleFunc("POST /v2/h3_context_ir", create)
	mux.HandleFunc("GET /v2/query/video_generation/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries++
		f.gotTaskIDs = append(f.gotTaskIDs, r.PathValue("id"))
		f.gotQueryHdr = append(f.gotQueryHdr, r.Header.Clone())
		reply := f.query
		if len(f.queryQueue) > 0 {
			reply = f.queryQueue[0]
			f.queryQueue = f.queryQueue[1:]
		}
		f.mu.Unlock()
		writeReply(w, reply)
	})
	mux.HandleFunc("DELETE /v2/video_generation/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletes++
		f.gotTaskIDs = append(f.gotTaskIDs, r.PathValue("id"))
		reply := f.del
		f.mu.Unlock()
		writeReply(w, reply)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func writeReply(w http.ResponseWriter, r fakeReply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.status)
	w.Write([]byte(r.body))
}

func (f *fakeMinimax) setCreate(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.create = fakeReply{status, body}
}

func (f *fakeMinimax) setQuery(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.query = fakeReply{status, body}
}

func (f *fakeMinimax) setDelete(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.del = fakeReply{status, body}
}

func (f *fakeMinimax) counts() (creates, queries, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.queries, f.deletes
}

// sentCreate 返回第 i 次受理请求的（体, 头）。
func (f *fakeMinimax) sentCreate(i int) ([]byte, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.gotBodies) {
		return nil, nil
	}
	return f.gotBodies[i], f.gotHeaders[i]
}

func (f *fakeMinimax) sentPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotPaths...)
}

func (f *fakeMinimax) sentTaskIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotTaskIDs...)
}

// videoEnv 是任务面用例的环境：routeEnv + 一个 minimax 型假上游与 kind=video
// 模型（来源侧 ID 与模型名不同，改写可证）。
type videoEnv struct {
	*routeEnv
	mm *fakeMinimax
}

func newVideoEnv(t *testing.T) *videoEnv {
	t.Helper()
	e := newRouteEnv(t)
	if _, _, err := gateway.ImportConfigKeys(t.Context(), e.st,
		[]config.APIKey{{Key: otherKey}}, logging.New(io.Discard, slog.LevelDebug)); err != nil {
		t.Fatalf("ImportConfigKeys(other): %v", err)
	}
	f := newFakeMinimax(t)
	up := dbUpstream(t, e.st, "mm-video-main", config.UpstreamMinimax, videoUpstreamKey, f.url)
	dbSource(t, e.st, dbKindModel(t, e.st, videoModel, store.ModelKindVideo), up, videoUpModelID, 100)
	return &videoEnv{routeEnv: e, mm: f}
}

// testKeyID 取 testKey 那把密钥的主键（任务行归属断言用）。按摘要认，不按
// 「第一行」认——有的装配会另外签一把 Key 进来。
func testKeyID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ka, err := st.LookupKeyByDigest(t.Context(), digestOf(testKey))
	if err != nil {
		t.Fatalf("LookupKeyByDigest(testKey): %v", err)
	}
	return ka.KeyID
}

// taskByVendorID 取任务行（归属 testUser）。
func taskByVendorID(t *testing.T, e *videoEnv, vendorID string) *store.AIGCTask {
	t.Helper()
	return taskByVendorIDIn(t, e.routeEnv, vendorID)
}

// taskByVendorIDIn 是不绑定厂商环境的那一版（方舟用例共用，见 video_ark_test.go）。
func taskByVendorIDIn(t *testing.T, e *routeEnv, vendorID string) *store.AIGCTask {
	t.Helper()
	task, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), vendorID, testKeyID(t, e.st))
	if err != nil {
		t.Fatalf("GetAIGCTaskByVendorIDForKey(%s): %v", vendorID, err)
	}
	return task
}

// decodeTaskID 从受理响应取 task_id（厂商形 {"task_id":…}，字符串或数字）。
func decodeTaskID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct {
		TaskID any `json:"task_id"`
	}
	dec := json.NewDecoder(strings.NewReader(w.Body.String()))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("受理响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	switch x := v.TaskID.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	t.Fatalf("受理响应缺 task_id: %s", w.Body.String())
	return ""
}

// decodeMinimaxError 解析网关自产的 MiniMax v2 形错误体。
func decodeMinimaxError(t *testing.T, w *httptest.ResponseRecorder) (errType, msg string) {
	t.Helper()
	var v struct {
		Type  string `json:"type"`
		Error struct {
			Type     string `json:"type"`
			Message  string `json:"message"`
			HTTPCode int    `json:"http_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, w.Body.String())
	}
	if v.Type != "error" {
		t.Fatalf("错误体顶层 type = %q，期望 error（MiniMax v2 形）: %s", v.Type, w.Body.String())
	}
	if v.Error.HTTPCode != w.Code {
		t.Errorf("error.http_code = %d，期望与状态码 %d 一致", v.Error.HTTPCode, w.Code)
	}
	return v.Error.Type, v.Error.Message
}

// submitVideo 以缺省请求体提交一个任务并返回厂商任务 id（= 客户端可见 id）。
func submitVideo(t *testing.T, e *videoEnv) string {
	t.Helper()
	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"小猫打哈欠"}],"resolution":"768P","duration":4}`, videoModel))
	if w.Code != http.StatusOK {
		t.Fatalf("提交状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	id := decodeTaskID(t, w)
	if id == "" {
		t.Fatalf("受理响应缺任务 id: %s", w.Body.String())
	}
	return id
}

// —— 全链路纯转发 ——

// TestVideoSubmitQueryPassthrough：提交→轮询的全链路。受理响应逐字节透传
// （客户端拿到的就是厂商 task_id）；请求体透传只改 model 为来源侧 ID；查询
// 响应透传、仅 task.model 回显改写回客户端可见名；观测旁路落任务行（厂商
// id、usage 原文、产物 URL 时效副本）；上游账户名与 Key 不出现在任何响应。
func TestVideoSubmitQueryPassthrough(t *testing.T) {
	e := newVideoEnv(t)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		`{"model":"MiniMax-H3","content":[{"type":"text","text":"小猫打哈欠"}],"resolution":"768P","duration":4,"x_custom":{"k":1}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("提交状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	// 受理响应 = 厂商响应逐字节。
	if want := fmt.Sprintf(`{"task_id":%q}`, fakeVendorTaskID); w.Body.String() != want {
		t.Errorf("受理响应未逐字节透传：%q，期望 %q", w.Body.String(), want)
	}
	for _, leak := range []string{videoUpstreamKey, "mm-video-main"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("提交响应泄露上游信息 %q: %s", leak, w.Body.String())
		}
	}

	// 厂商收到的受理请求：路径命中、model 改写为来源侧 ID、其余字段透传、
	// 凭证是上游自己的 Key。
	body, header := e.mm.sentCreate(0)
	if body == nil {
		t.Fatal("厂商未收到受理请求")
	}
	var sent map[string]any
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("厂商收到的请求体不是 JSON: %v", err)
	}
	if got, _ := sent["model"].(string); got != videoUpModelID {
		t.Errorf("厂商收到 model = %q，期望改写为来源侧 ID", got)
	}
	if _, has := sent["x_custom"]; !has {
		t.Errorf("未知字段未透传: %s", body)
	}
	if got, _ := sent["resolution"].(string); got != "768P" {
		t.Errorf("resolution 未透传: %s", body)
	}
	if got := header.Get("Authorization"); got != "Bearer "+videoUpstreamKey {
		t.Errorf("厂商收到 Authorization = %q，期望上游自己的 Key", got)
	}
	if paths := e.mm.sentPaths(); len(paths) != 1 || paths[0] != "/v2/video_generation" {
		t.Errorf("厂商受理路径 = %v，期望 [/v2/video_generation]", paths)
	}

	// 轮询：running 响应透传，task.model 回显改写为客户端可见名，其余原文。
	w = do(e.h, "GET", "/minimax/v2/query/video_generation/"+fakeVendorTaskID, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	var q struct {
		Task struct {
			ID     string `json:"id"`
			Model  string `json:"model"`
			Status string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &q); err != nil {
		t.Fatalf("查询响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if q.Task.ID != fakeVendorTaskID || q.Task.Status != "running" {
		t.Errorf("查询响应未透传厂商原文: %s", w.Body.String())
	}
	if q.Task.Model != videoModel {
		t.Errorf("task.model = %q，期望回显改写为客户端可见名 %s", q.Task.Model, videoModel)
	}
	if strings.Contains(w.Body.String(), videoUpModelID) {
		t.Errorf("查询响应泄露来源侧模型 ID: %s", w.Body.String())
	}
	if ids := e.mm.sentTaskIDs(); len(ids) != 1 || ids[0] != fakeVendorTaskID {
		t.Errorf("厂商查询命中 id = %v，期望 [%s]", ids, fakeVendorTaskID)
	}

	// succeeded：厂商产物 URL **原样**到达客户端（纯转发，无设备下载口），
	// usage 原文透出；观测旁路把 usage 与产物 URL 落进任务行。
	artifact := "https://video-cdn.example.net/out/clip.mp4?sig=abc"
	e.mm.setQuery(http.StatusOK, fmt.Sprintf(
		`{"task":{"id":%q,"model":%q,"status":"succeeded","content":{"url":%q},"usage":{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0}}}`,
		fakeVendorTaskID, videoUpModelID, artifact))
	w = do(e.h, "GET", "/minimax/v2/query/video_generation/"+fakeVendorTaskID, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"status":"succeeded"`) ||
		!strings.Contains(w.Body.String(), `"output_seconds":5`) {
		t.Errorf("succeeded 响应未透传厂商原文: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"url":"`+artifact+`"`) {
		t.Errorf("厂商产物 URL 未原样透传: %s", w.Body.String())
	}

	// 任务行：厂商 id 就是行的检索键，usage 原文与产物 URL 时效副本都在库里。
	task := taskByVendorID(t, e, fakeVendorTaskID)
	if !strings.Contains(task.UsageJSON, `"output_seconds":5`) {
		t.Errorf("usage_json 未存原文: %q", task.UsageJSON)
	}
	if task.ContentURL != artifact {
		t.Errorf("content_url 时效副本 = %q，期望 %q", task.ContentURL, artifact)
	}
	if task.Status != "succeeded" {
		t.Errorf("行状态 = %q，期望 succeeded", task.Status)
	}
	if task.UpstreamName != "mm-video-main" || task.ModelName != videoModel || task.Kind != store.ModelKindVideo {
		t.Errorf("行快照异常: %+v", task)
	}
	if task.KeyID <= 0 || task.KeyDisplay == "" {
		t.Errorf("归属快照异常: keyID=%d key=%q", task.KeyID, task.KeyDisplay)
	}
	if !strings.HasPrefix(task.ID, "agt-") {
		t.Errorf("行内部主键 = %q，期望 agt- 前缀", task.ID)
	}

	// 日志不含上游凭证（§15.1）。
	if strings.Contains(e.logBuf.String(), videoUpstreamKey) {
		t.Errorf("日志泄露上游凭证:\n%s", e.logBuf.String())
	}
}

// TestVideoContextIRSubmit：POST /v2/h3_context_ir 与视频生成共用受理机制，
// 厂商收到的路径是 Context-IR 自己的；任务行照落（token 形 usage 的计价由
// usage 包按 prompt_tokens 在场识别）。
func TestVideoContextIRSubmit(t *testing.T) {
	e := newVideoEnv(t)
	w := do(e.h, "POST", "/minimax/v2/h3_context_ir", chatAuth,
		fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"深化这段描述"}],"duration":5}`, videoModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	id := decodeTaskID(t, w)
	if paths := e.mm.sentPaths(); len(paths) != 1 || paths[0] != "/v2/h3_context_ir" {
		t.Errorf("厂商受理路径 = %v，期望 [/v2/h3_context_ir]", paths)
	}
	task := taskByVendorID(t, e, id)
	if task.Kind != store.ModelKindVideo || task.ReqDuration != "5" || task.ReqResolution != "" {
		t.Errorf("Context-IR 任务行异常: kind=%q dur=%q res=%q", task.Kind, task.ReqDuration, task.ReqResolution)
	}
}

// —— 计费特征列 ——

// TestVideoBillingFacts：has_video_input 随 content[] 是否含 video_url 判定；
// generate_audio 恒 true（H3 原生出声、无请求参数）；service_tier 恒空
// （minimax 无此概念）；请求 resolution/duration 存字面量。
func TestVideoBillingFacts(t *testing.T) {
	e := newVideoEnv(t)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		`{"model":"MiniMax-H3","content":[{"type":"text","text":"编辑"},{"type":"video_url","video_url":{"url":"https://example.org/in.mp4"},"role":"reference_video"}],"resolution":"2K","duration":-1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("提交状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	task := taskByVendorID(t, e, decodeTaskID(t, w))
	if !task.HasVideoInput {
		t.Error("含 video_url 输入的任务 has_video_input 应为 true（参考视频按输入秒数计费）")
	}
	if !task.GenerateAudio || task.ServiceTier != "" {
		t.Errorf("特征列异常: audio=%v tier=%q（H3 恒出声、无 tier）", task.GenerateAudio, task.ServiceTier)
	}
	if task.ReqResolution != "2K" || task.ReqDuration != "-1" {
		t.Errorf("字面量特征异常: res=%q dur=%q", task.ReqResolution, task.ReqDuration)
	}

	// 纯文本任务。
	id2 := submitVideo(t, e)
	task2 := taskByVendorID(t, e, id2)
	if task2.HasVideoInput {
		t.Error("纯文本任务 has_video_input 应为 false")
	}
	if task2.ReqResolution != "768P" || task2.ReqDuration != "4" {
		t.Errorf("字面量特征异常: res=%q dur=%q", task2.ReqResolution, task2.ReqDuration)
	}
}

// —— 归属 404 ——

// TestVideoOwnership404：他人 Key 查/删一律 404，且与「任务不存在」的响应
// 逐字节相同（不给存在性 oracle）；错误形是 MiniMax v2 形。
func TestVideoOwnership404(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	var bodies []string
	for _, c := range []struct {
		name   string
		method string
		path   string
		auth   map[string]string
	}{
		{"他人查询", "GET", "/minimax/v2/query/video_generation/" + id, otherAuth},
		{"他人取消", "DELETE", "/minimax/v2/video_generation/" + id, otherAuth},
		{"不存在查询", "GET", "/minimax/v2/query/video_generation/999999999999999", chatAuth},
		{"不存在取消", "DELETE", "/minimax/v2/video_generation/999999999999999", chatAuth},
	} {
		w := do(e.h, c.method, c.path, c.auth, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404；body: %s", c.name, w.Code, w.Body.String())
		}
		if errType, _ := decodeMinimaxError(t, w); errType != "invalid_task_id" {
			t.Errorf("%s error.type = %q，期望 invalid_task_id", c.name, errType)
		}
		// 响应体里的 id 因请求而异，归一后再比逐字节。
		bodies = append(bodies, strings.ReplaceAll(strings.ReplaceAll(w.Body.String(), id, "<id>"), "999999999999999", "<id>"))
	}
	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("归属 404 与不存在 404 响应不一致（存在性 oracle）:\n%s\nvs\n%s", bodies[0], bodies[i])
		}
	}
	// 厂商一次都没被打到（行都没定位到，谈不上转发）。
	if _, queries, deletes := e.mm.counts(); queries != 0 || deletes != 0 {
		t.Errorf("归属不符的请求打到了厂商: queries=%d deletes=%d", queries, deletes)
	}
}

// —— kind 闸门与协议面 ——

// TestVideoEntryKindGate：文本模型进 /v2 视频入口一律 404 model_not_found
// （MiniMax v2 形），一个字节都不发往上游。
func TestVideoEntryKindGate(t *testing.T) {
	e := newVideoEnv(t)
	stub := newStub(t, chatReply("text-side-id"))
	upText := dbUpstream(t, e.st, "text-up", config.UpstreamMock, "sk-text-not-real", stub.url)
	dbSource(t, e.st, dbModel(t, e.st, "just-text"), upText, "", 100)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		`{"model":"just-text","content":[{"type":"text","text":"猫"}]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}
	if errType, _ := decodeMinimaxError(t, w); errType != "model_not_found" {
		t.Errorf("error.type = %q，期望 model_not_found", errType)
	}
	if stub.count() != 0 {
		t.Errorf("kind 不符的请求打到了上游 %d 次，期望 0", stub.count())
	}
}

// —— 提交侧故障切换 ——

// TestVideoSubmitFailover：连接失败/429/5xx 且还有下一候选时切换来源，任务行
// 钉在真正受理的上游上；最终失败时厂商错误原样透传。
func TestVideoSubmitFailover(t *testing.T) {
	t.Run("首选 5xx 切换到次选", func(t *testing.T) {
		e := newVideoEnv(t)
		e.mm.setCreate(http.StatusServiceUnavailable, `{"type":"error","error":{"type":"server_error"}}`)
		backup := newFakeMinimax(t)
		upBackup := dbUpstream(t, e.st, "mm-backup", config.UpstreamMinimax, "sk-mm-backup-not-real", backup.url)
		models, err := e.st.ListModelsWithSources(t.Context())
		if err != nil || len(models) != 1 {
			t.Fatalf("ListModelsWithSources: %v（%d 行）", err, len(models))
		}
		dbSource(t, e.st, models[0].ID, upBackup, "", 200)

		w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
			fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, videoModel))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望切换后 200；body: %s", w.Code, w.Body.String())
		}
		id := decodeTaskID(t, w)
		task := taskByVendorID(t, e, id)
		if task.UpstreamName != "mm-backup" {
			t.Errorf("任务钉在 %q，期望受理它的 mm-backup", task.UpstreamName)
		}
		// 次选收到的 model：来源侧 ID 留空 = 与模型名相同（按次选行重编码）。
		if b, _ := backup.sentCreate(0); b != nil {
			var sent map[string]any
			json.Unmarshal(b, &sent)
			if got, _ := sent["model"].(string); got != videoModel {
				t.Errorf("次选收到 model = %q，期望 %q", got, videoModel)
			}
		} else {
			t.Error("次选未收到受理请求")
		}
	})

	t.Run("末候选失败原样透传", func(t *testing.T) {
		e := newVideoEnv(t)
		vendorErr := `{"type":"error","error":{"type":"rate_limit_error","message":"concurrency limit"},"request_id":"req-1"}`
		e.mm.setCreate(http.StatusTooManyRequests, vendorErr)

		w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
			fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, videoModel))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("状态码 = %d，期望 429 原样；body: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != vendorErr {
			t.Errorf("厂商错误未逐字节透传：%q", w.Body.String())
		}
		// 没受理就没有任务行。
		if _, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), fakeVendorTaskID,
			testKeyID(t, e.st)); err == nil {
			t.Error("未受理的提交不该有任务行")
		}
	})
}

// TestVideoSubmitVendor4xxPassthrough：厂商同步 4xx（非 429）原样透传（含
// 状态码与体），不建任务行。
func TestVideoSubmitVendor4xxPassthrough(t *testing.T) {
	e := newVideoEnv(t)
	vendorErr := `{"type":"error","error":{"type":"unprocessable_entity_error","message":"content policy"},"request_id":"req-9"}`
	e.mm.setCreate(http.StatusUnprocessableEntity, vendorErr)

	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, videoModel))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422 原样；body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != vendorErr {
		t.Errorf("厂商 4xx 未逐字节透传：%q", w.Body.String())
	}
}

// —— 任务级失败与厂商记录过期 ——

// TestVideoAsyncFailure：创建 200 ≠ 任务成功——查询响应 HTTP 200 里的
// status=failed + code/message（审核拒绝 1026 类）原样透传，且旁路载进任务行
// （error_code/error_message，记账与排障依据）。
func TestVideoAsyncFailure(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	e.mm.setQuery(http.StatusOK, fmt.Sprintf(
		`{"task":{"id":%q,"status":"failed","code":"1026","message":"video description contains sensitive content"}}`, id))
	w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":"1026"`) ||
		!strings.Contains(w.Body.String(), "sensitive content") {
		t.Errorf("任务级失败未透传: %s", w.Body.String())
	}
	task := taskByVendorID(t, e, id)
	if task.Status != "failed" || task.ErrorCode != "1026" ||
		!strings.Contains(task.ErrorMessage, "sensitive content") {
		t.Errorf("任务级失败未入行: status=%q code=%q msg=%q", task.Status, task.ErrorCode, task.ErrorMessage)
	}
}

// TestVideoQueryVendorGone：厂商查不到任务（超 7 天/已删）→ 厂商 404 响应
// 原样透传，内部把行归一为 expired（0 元清算的依据；对账协程据此停查）。
func TestVideoQueryVendorGone(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	vendor404 := `{"type":"error","error":{"type":"invalid_params_error","message":"invalid task_id"},"request_id":"req-4"}`
	e.mm.setQuery(http.StatusNotFound, vendor404)
	w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("查询状态码 = %d，期望厂商 404 原样；body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != vendor404 {
		t.Errorf("厂商 404 未逐字节透传：%q", w.Body.String())
	}
	task := taskByVendorID(t, e, id)
	if task.Status != "expired" || task.ErrorCode != "task_record_expired" {
		t.Errorf("行未归一为 expired: status=%q code=%q", task.Status, task.ErrorCode)
	}
}

// —— 取消 ——

// TestVideoCancel：DELETE 语义直通厂商。queued → 厂商 action=cancelled →
// 行转 cancelled；响应原样透传；取消前先做一次设备自己的回查（账单保全）。
func TestVideoCancel(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	w := do(e.h, "DELETE", "/minimax/v2/video_generation/"+id, chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("取消状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"action":"cancelled"`) {
		t.Errorf("取消响应未透传厂商原文: %s", w.Body.String())
	}
	task := taskByVendorID(t, e, id)
	if task.Status != "cancelled" {
		t.Errorf("行状态 = %q，期望 cancelled", task.Status)
	}
	// 取消前的账单保全回查发生过（一次查询 + 一次删除）。
	if _, queries, deletes := e.mm.counts(); queries != 1 || deletes != 1 {
		t.Errorf("厂商调用次数 queries=%d deletes=%d，期望 1/1（取消前回查一次）", queries, deletes)
	}
}

// TestVideoCancelRunningRefused：running 任务厂商拒绝取消（4xx）→ 原样透传，
// 行保持厂商观测到的状态。
func TestVideoCancelRunningRefused(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)
	refuse := `{"type":"error","error":{"type":"invalid_params_error","message":"task is running"},"request_id":"req-7"}`
	e.mm.setDelete(http.StatusBadRequest, refuse)

	w := do(e.h, "DELETE", "/minimax/v2/video_generation/"+id, chatAuth, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望厂商 400 原样；body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != refuse {
		t.Errorf("厂商拒绝未逐字节透传：%q", w.Body.String())
	}
	if task := taskByVendorID(t, e, id); task.Status != "running" {
		t.Errorf("行状态 = %q，期望回查观测到的 running", task.Status)
	}
}

// TestVideoCancelTerminalDeletesRecordOnly：终态行的 DELETE 是删厂商记录
// （action=deleted），本地行的状态与账单事实不动，也不再做取消前回查。
func TestVideoCancelTerminalDeletesRecordOnly(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)
	// 先把行推进到 succeeded。
	e.mm.setQuery(http.StatusOK, fmt.Sprintf(
		`{"task":{"id":%q,"status":"succeeded","content":{"url":"https://cdn.example.net/a.mp4"},"usage":{"output_seconds":4}}}`, id))
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d", w.Code)
	}
	_, queriesBefore, _ := e.mm.counts()

	e.mm.setDelete(http.StatusOK, fmt.Sprintf(`{"task_id":%q,"action":"deleted","status":"succeeded"}`, id))
	w := do(e.h, "DELETE", "/minimax/v2/video_generation/"+id, chatAuth, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"action":"deleted"`) {
		t.Fatalf("终态删除响应异常: %d %s", w.Code, w.Body.String())
	}
	if _, queriesAfter, _ := e.mm.counts(); queriesAfter != queriesBefore {
		t.Errorf("终态行取消不该再回查厂商：%d → %d", queriesBefore, queriesAfter)
	}
	if task := taskByVendorID(t, e, id); task.Status != "succeeded" || !strings.Contains(task.UsageJSON, "output_seconds") {
		t.Errorf("终态行被取消改动了: status=%q usage=%q", task.Status, task.UsageJSON)
	}
}

// —— 来源锁定 ——

// TestVideoPinnedSurvivesCatalogChanges：任务创建后来源锁定——模型/来源/上游
// 全被停用后查询照常走钉死的上游（停用只挡新提交）。
func TestVideoPinnedSurvivesCatalogChanges(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	models, err := e.st.ListModelsWithSources(t.Context())
	if err != nil || len(models) != 1 {
		t.Fatalf("ListModelsWithSources: %v", err)
	}
	if err := e.st.SetModelDisabled(t.Context(), models[0].ID, true); err != nil {
		t.Fatalf("SetModelDisabled: %v", err)
	}
	if err := e.st.SetModelSourceDisabled(t.Context(), models[0].Sources[0].ID, true); err != nil {
		t.Fatalf("SetModelSourceDisabled: %v", err)
	}
	if err := e.st.SetUpstreamDisabled(t.Context(), models[0].Sources[0].UpstreamID, true); err != nil {
		t.Fatalf("SetUpstreamDisabled: %v", err)
	}

	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Errorf("目录停用后查询状态码 = %d，期望仍 200；body: %s", w.Code, w.Body.String())
	}
	// 新提交则被停用挡住。
	if w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth,
		fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, videoModel)); w.Code != http.StatusNotFound {
		t.Errorf("停用后新提交状态码 = %d，期望 404", w.Code)
	}
}

// —— 认证与未挂载路径 ——

// TestVideoAuthRequired：四条路由全部压在 withAuth 链上，无凭证一律 401
// （MiniMax v2 形错误）。
func TestVideoAuthRequired(t *testing.T) {
	e := newVideoEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/minimax/v2/video_generation"},
		{"POST", "/minimax/v2/h3_context_ir"},
		{"GET", "/minimax/v2/query/video_generation/" + fakeVendorTaskID},
		{"DELETE", "/minimax/v2/video_generation/" + fakeVendorTaskID},
	} {
		w := do(e.h, tc.method, tc.path, map[string]string{"Content-Type": "application/json"}, "{}")
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 状态码 = %d，期望 401", tc.method, tc.path, w.Code)
			continue
		}
		decodeMinimaxError(t, w) // 形状即断言（顶层 type=error + http_code 一致）
	}
}

// TestVideoUnmountedVendorPaths：刻意不挂载的厂商端点（任务列表、素材上传、
// 再生成）带凭证也是 404，形随路径（/v2 系 MiniMax 形、/v1 系 OpenAI 形）。
func TestVideoUnmountedVendorPaths(t *testing.T) {
	e := newVideoEnv(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/minimax/v2/query/video_generation"}, // 列表：厂商侧是全账户视角，转发会跨用户泄露
		{"POST", "/minimax/v2/video_regeneration"},    // 再生成：单价档目录价词汇未定
	} {
		w := do(e.h, tc.method, tc.path, chatAuth, "")
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d，期望 404", tc.method, tc.path, w.Code)
			continue
		}
		decodeMinimaxError(t, w)
	}
	// 素材上传在 /v1 命名空间：同样未挂载，形归 OpenAI 侧。
	if w := do(e.h, "POST", "/v1/files/upload", chatAuth, ""); w.Code != http.StatusNotFound {
		t.Errorf("/v1/files/upload 状态码 = %d，期望 404", w.Code)
	}
}

// TestVideoSubmitBodyLimit：视频提交体超上限 → 413，一个字节不发往厂商。
func TestVideoSubmitBodyLimit(t *testing.T) {
	e := newVideoEnv(t)
	big := `{"model":"` + videoModel + `","content":[{"type":"text","text":"` +
		strings.Repeat("a", (80<<20)+16) + `"}]}`
	w := do(e.h, "POST", "/minimax/v2/video_generation", chatAuth, big)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", w.Code)
	}
	if creates, _, _ := e.mm.counts(); creates != 0 {
		t.Errorf("超限请求打到了厂商 %d 次", creates)
	}
}

// —— 观测卫生 ——

// TestVideoVendorErrorSanitized：任务级错误信息入行前消毒（合法 UTF-8、单行、
// 截断）——客户端响应是厂商原文（透传），但**行**是设备自己的库，脏字节不落。
func TestVideoVendorErrorSanitized(t *testing.T) {
	e := newVideoEnv(t)
	id := submitVideo(t, e)

	dirty := "bad\tvalue\r\nwith ctl\x01bytes " + strings.Repeat("长", 600)
	body, err := json.Marshal(map[string]any{"task": map[string]any{
		"id": id, "status": "failed", "code": "1026", "message": dirty,
	}})
	if err != nil {
		t.Fatalf("构造响应: %v", err)
	}
	e.mm.setQuery(http.StatusOK, string(body))
	if w := do(e.h, "GET", "/minimax/v2/query/video_generation/"+id, chatAuth, ""); w.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d", w.Code)
	}
	task := taskByVendorID(t, e, id)
	if strings.ContainsAny(task.ErrorMessage, "\r\n\t\x01") {
		t.Errorf("行内错误信息未消毒: %q", task.ErrorMessage)
	}
	// CleanMessage 截断到上限后缀省略号：501 = 500 + "…"。
	if n := len([]rune(task.ErrorMessage)); n > 501 {
		t.Errorf("行内错误信息未截断: %d runes", n)
	}
}

// —— 保留期清理 ——

// TestRunAIGCTaskPrune：清理协程启动即清一次超过保留期（14 天）的任务行，
// 保留期内的行不动。
func TestRunAIGCTaskPrune(t *testing.T) {
	e := newVideoEnv(t)
	oldID := submitVideo(t, e)
	uid := testKeyID(t, e.st)
	// 直改 created_at 把行做旧（store 不提供写口，走第二条连接是既有惯例——
	// 固定宽度 UTC 毫秒文本，字符串比较即时间比较）。
	backdateCreated(t, e.dir, oldID, time.Now().Add(-15*24*time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"))
	freshID := submitVideo(t, e)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		gateway.RunAIGCTaskPrune(ctx, e.st, logging.New(io.Discard, slog.LevelDebug))
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), oldID, uid); err != nil {
			break // 已清理
		}
		if time.Now().After(deadline) {
			t.Fatal("过期任务行未被清理")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if _, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), freshID, uid); err != nil {
		t.Errorf("保留期内的任务行被误删: %v", err)
	}
}

// TestVideoFaceSegmentRequired：**厂商段是入口的一部分，不带就没有这条路**。
// 无段的路径 /v2/… 不是数据面入口：它落进根路由的兜底，对客户端就是「这台
// 设备上没有这个接口」。这条用例把「地址带厂商段」钉成可执行契约——删了它，
// 谁顺手把无段路由挂回来都没人发现，按路径首段分面也就白做了。
func TestVideoFaceSegmentRequired(t *testing.T) {
	e := newVideoEnv(t)
	body := fmt.Sprintf(`{"model":%q,"content":[{"type":"text","text":"猫"}]}`, videoModel)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v2/video_generation"},
		{"POST", "/v2/h3_context_ir"},
		{"GET", "/v2/query/video_generation/" + fakeVendorTaskID},
		{"DELETE", "/v2/video_generation/" + fakeVendorTaskID},
	} {
		if w := do(e.h, tc.method, tc.path, chatAuth, body); w.Code != http.StatusNotFound {
			t.Errorf("%s %s 状态码 = %d，期望 404（无厂商段不是入口）", tc.method, tc.path, w.Code)
		}
	}
	// 一个字节都不该到厂商：无段路径连选路都进不去。
	if creates, queries, deletes := e.mm.counts(); creates != 0 || queries != 0 || deletes != 0 {
		t.Errorf("无段路径打到了厂商: creates=%d queries=%d deletes=%d", creates, queries, deletes)
	}
}

// TestVideoFaceFallbackStyle：厂商段之下未挂载的路径先认证再 404，错误形按段
// 归该厂商（/minimax 段 → MiniMax v2 形）；无凭证一律 401。
func TestVideoFaceFallbackStyle(t *testing.T) {
	e := newVideoEnv(t)
	w := do(e.h, "POST", "/minimax/v1/files/upload", chatAuth, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
	if errType, _ := decodeMinimaxError(t, w); errType != "not_found" {
		t.Errorf("error.type = %q，期望 not_found（MiniMax v2 形）", errType)
	}
	if w := do(e.h, "POST", "/minimax/v1/files/upload", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证状态码 = %d，期望 401", w.Code)
	}
}
