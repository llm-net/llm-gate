package gateway_test

// mediagen_vendor_test.go 是媒体生成厂商面后端（mediagen_vendor.go：ark_video /
// minimax_video / ark_image）与受理准入闸（Server.AdmitMediaJob）的可执行验收，全程离线：
// 假厂商上游沿用厂商协议面用例的那几台（video_test.go / video_ark_test.go / images_test.go）。
//
// 钉住的是「设备自己作为调用方」这一跳与数据面是同一份代码：请求体按厂商官方形状拼装、
// model 改写成来源侧 ID、受理后落 aigc_tasks 行并钉死上游、用量记在归属密钥名下、这把 Key
// 的可用模型范围照样生效；以及准入只在受理时过一次，后端 Generate 不重复过闸。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/logging"
	"github.com/llm-net/llm-gate/firmware/internal/mediagen"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

// familyModel 给一个厂商面模型行套上能力基线（内核 Available 交给后端的就是这一项）。
func familyModel(t *testing.T, backend, id string) mediagen.Model {
	t.Helper()
	m, ok := mediagen.FamilyModel(backend, id)
	if !ok {
		t.Fatalf("能力表里没有厂商面后端 %q", backend)
	}
	return m
}

// ownedRequest 造一条归属 testKey 的生成请求。
func ownedRequest(t *testing.T, e *routeEnv, m mediagen.Model, prompt string) mediagen.Request {
	t.Helper()
	return mediagen.Request{JobID: "01JOB", Model: m, Operation: store.MediaOpGenerate, Prompt: prompt,
		KeyID: testKeyID(t, e.st), KeyDisplay: testKeyDisplay}
}

// vendorJob 造一条平台排队中的任务行（Refresh 的入参）。
func vendorJob(t *testing.T, e *routeEnv, backend, model, vendorID string) store.MediaJob {
	t.Helper()
	return store.MediaJob{ID: "01JOB", Origin: store.MediaOriginPage, Backend: backend, Provider: backend,
		Kind: store.ModelKindVideo, Model: model, Operation: store.MediaOpGenerate,
		KeyID: testKeyID(t, e.st), KeyDisplay: testKeyDisplay, VendorID: vendorID, Status: store.MediaStatusQueued}
}

func decodeObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("厂商收到的请求体不是 JSON: %v\n%s", err, raw)
	}
	return m
}

// assertVendorVideoContent 断言 content[]：一条 text，之后首帧 / 尾帧 / 参考图各是带 role 的
// image_url 元素，顺序即角色顺序。
func assertVendorVideoContent(t *testing.T, sent map[string]any, prompt string, images [][2]string) {
	t.Helper()
	content := jsonArrayAny(sent["content"])
	want := len(images)
	if prompt != "" {
		want++
	}
	if len(content) != want {
		t.Fatalf("content[] = %v，期望 %d 个元素", sent["content"], want)
	}
	i := 0
	if prompt != "" {
		el, _ := content[0].(map[string]any)
		if el["type"] != "text" || el["text"] != prompt {
			t.Fatalf("content[0] = %v，期望 text 元素", el)
		}
		i = 1
	}
	for _, img := range images {
		el, _ := content[i].(map[string]any)
		u, _ := el["image_url"].(map[string]any)
		if el["type"] != "image_url" || el["role"] != img[0] || u["url"] != img[1] {
			t.Fatalf("content[%d] = %v，期望 role=%s url=%s", i, el, img[0], img[1])
		}
		i++
	}
}

// ---- ark_video ----

func TestMediaArkVideoGenerate(t *testing.T) {
	const first, last, ref = "data:image/png;base64,iVBORw0KGgo=", "https://example.invalid/last.png", "https://example.invalid/ref.png"
	e := newArkVideoEnv(t)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	b := mediaBackend(t, e.srv, mediagen.BackendArkVideo)
	in := ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "海边日落")
	in.Inputs = mediagen.Inputs{FirstFrame: first, LastFrame: last, ReferenceImages: []string{ref}}
	in.Params = mediagen.Params{"resolution": "720p", "ratio": "16:9", "duration": int64(5), "generate_audio": false, "watermark": false, "seed": int64(-1)}

	res, err := b.Generate(t.Context(), in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != store.MediaStatusQueued || res.VendorID != arkVendorTaskID || res.MediaType != "video" || res.MediaURL != "" || res.Error != "" {
		t.Fatalf("结果 = %+v，期望 queued + 厂商任务 id", res)
	}
	// 厂商收到的请求体：官方形状，model 改写成来源侧 ID，参数是顶层字段。
	raw, header := e.ark.sentCreate(0)
	if creates, _, _ := e.ark.counts(); creates != 1 || e.ark.lastPath != arkTasksPath {
		t.Fatalf("受理次数 = %d，路径 = %q", creates, e.ark.lastPath)
	}
	if got := header.Get("Authorization"); got != "Bearer "+arkVideoKey {
		t.Fatalf("厂商收到的 Authorization 不是上游账户的凭据")
	}
	sent := decodeObject(t, raw)
	if sent["model"] != arkVideoUpModelID {
		t.Fatalf("model = %v，期望改写成来源侧 ID %q", sent["model"], arkVideoUpModelID)
	}
	assertVendorVideoContent(t, sent, "海边日落", [][2]string{{"first_frame", first}, {"last_frame", last}, {"reference_image", ref}})
	if sent["resolution"] != "720p" || sent["ratio"] != "16:9" || sent["duration"] != float64(5) ||
		sent["generate_audio"] != false || sent["watermark"] != false || sent["seed"] != float64(-1) {
		t.Fatalf("顶层参数 = %v", sent)
	}
	for _, k := range []string{"prompt", "inputs", "params", "operation", "first_frame", "camera_fixed"} {
		if _, ok := sent[k]; ok {
			t.Fatalf("请求体不该带 %s: %v", k, sent[k])
		}
	}
	// 受理后落 aigc_tasks 行：归属密钥、钉死上游、计费特征与数据面同一口径。
	task := taskByVendorIDIn(t, e.routeEnv, arkVendorTaskID)
	if task.KeyID != in.KeyID || task.KeyDisplay != testKeyDisplay || task.UpstreamName != "ark-plan-main" ||
		task.ModelName != arkVideoModel || task.Kind != store.ModelKindVideo || task.Status != "queued" {
		t.Fatalf("任务行 = %+v", task)
	}
	if task.ReqResolution != "720p" || task.ReqDuration != "5" || task.GenerateAudio {
		t.Fatalf("计费特征 = resolution %q / duration %q / audio %v，期望 720p / 5 / false", task.ReqResolution, task.ReqDuration, task.GenerateAudio)
	}
	// 提交这一笔记在归属密钥名下：video 入口、只计一次请求。
	s := fm.only(t)
	if s.Entry != usage.EntryVideo || s.KeyID != in.KeyID || s.KeyDisplay != testKeyDisplay || s.ModelName != arkVideoModel ||
		s.Status != http.StatusOK || s.UpstreamName != "ark-plan-main" || s.Rejected {
		t.Fatalf("记账 = %+v", s)
	}
	// §15.1：提示词与媒体不进日志。
	for _, leak := range []string{"海边日落", "iVBORw0KGgo", arkVideoKey} {
		if strings.Contains(e.logBuf.String(), leak) {
			t.Fatalf("日志泄露了 %q", leak)
		}
	}
}

// 只带首帧时提示词可省：content[] 里不出现空的 text 元素；没带的参数不带字段。
func TestMediaArkVideoGenerateWithoutPrompt(t *testing.T) {
	e := newArkVideoEnv(t)
	in := ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "")
	in.Inputs = mediagen.Inputs{FirstFrame: "https://example.invalid/first.png"}
	if res, err := mediaBackend(t, e.srv, mediagen.BackendArkVideo).Generate(t.Context(), in); err != nil || res.Status != store.MediaStatusQueued {
		t.Fatalf("Generate = %+v / %v", res, err)
	}
	raw, _ := e.ark.sentCreate(0)
	sent := decodeObject(t, raw)
	assertVendorVideoContent(t, sent, "", [][2]string{{"first_frame", "https://example.invalid/first.png"}})
	if len(sent) != 2 {
		t.Fatalf("请求体 = %v，期望只有 model 与 content", sent)
	}
}

// 受理后钉死上游：订阅来源 429、切到按量来源受理之后，查询只打受理它的那一台。
func TestMediaArkVideoPinsAcceptingUpstream(t *testing.T) {
	e := newArkVideoEnv(t)
	e.ark.mu.Lock()
	e.ark.create = fakeReply{http.StatusTooManyRequests, `{"error":{"code":"QuotaExceeded","message":"quota"}}`}
	e.ark.mu.Unlock()
	payg := newFakeArk(t)
	up := dbUpstream(t, e.st, "ark-payg", config.UpstreamArk, "sk-ark-payg-not-real", payg.url)
	dbSource(t, e.st, e.modelID, up, arkVideoUpModelID, 200)

	b := mediaBackend(t, e.srv, mediagen.BackendArkVideo)
	res, err := b.Generate(t.Context(), ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "p"))
	if err != nil || res.Status != store.MediaStatusQueued {
		t.Fatalf("Generate = %+v / %v", res, err)
	}
	if plan, _, _ := e.ark.counts(); plan != 1 {
		t.Fatalf("订阅来源受理次数 = %d，期望先试一次", plan)
	}
	if task := taskByVendorIDIn(t, e.routeEnv, res.VendorID); task.UpstreamName != "ark-payg" {
		t.Fatalf("任务行钉的上游 = %q，期望受理它的 ark-payg", task.UpstreamName)
	}
	got, err := b.Refresh(t.Context(), vendorJob(t, e.routeEnv, mediagen.BackendArkVideo, arkVideoModel, res.VendorID))
	if err != nil || got.Status != store.MediaStatusSucceeded {
		t.Fatalf("Refresh = %+v / %v", got, err)
	}
	if _, q, _ := payg.counts(); q != 1 {
		t.Fatalf("查询没打到受理任务的那条来源: queries=%d", q)
	}
	if _, q, _ := e.ark.counts(); q != 0 {
		t.Fatalf("查询打到了没受理这个任务的来源: queries=%d", q)
	}
}

// 厂商同步拒绝（未受理）：任务 failed，原因透出厂商文案与 code；不建 aigc 行。
func TestMediaArkVideoGenerateVendorRejection(t *testing.T) {
	e := newArkVideoEnv(t)
	e.ark.mu.Lock()
	e.ark.create = fakeReply{http.StatusBadRequest, `{"error":{"code":"InvalidParameter","message":"duration is out of range"}}`}
	e.ark.mu.Unlock()
	res, err := mediaBackend(t, e.srv, mediagen.BackendArkVideo).Generate(t.Context(),
		ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "p"))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "duration is out of range") || !strings.Contains(res.Error, "InvalidParameter") {
		t.Fatalf("结果 = %+v，期望 failed 且透出厂商文案与 code", res)
	}
	if _, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), arkVendorTaskID, testKeyID(t, e.st)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未受理不该建任务行: err=%v", err)
	}
	if strings.Contains(e.logBuf.String(), "duration is out of range") {
		t.Fatalf("日志不该带厂商错误正文: %s", e.logBuf.String())
	}
}

// 模型不在这把 Key 的可用范围（与不存在、已停用同一口径）：failed + model_not_found 文案，
// 一个字节不打到厂商，不建 aigc 行。
func TestMediaVendorVideoModelOutOfKeyScope(t *testing.T) {
	t.Run("ark_video", func(t *testing.T) {
		e := newArkVideoEnv(t)
		restrictAPIModels(t, e.st) // 收窄到空集合
		res, err := mediaBackend(t, e.srv, mediagen.BackendArkVideo).Generate(t.Context(),
			ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "p"))
		if err != nil || res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "does not exist or you do not have access") {
			t.Fatalf("结果 = %+v / %v，期望 failed + model_not_found 文案", res, err)
		}
		if creates, _, _ := e.ark.counts(); creates != 0 {
			t.Fatalf("范围外的模型不该打到厂商: creates=%d", creates)
		}
		if _, err := e.st.GetAIGCTaskByVendorIDForKey(t.Context(), arkVendorTaskID, testKeyID(t, e.st)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("不该建任务行: err=%v", err)
		}
	})
	t.Run("minimax_video", func(t *testing.T) {
		e := newVideoEnv(t)
		restrictAPIModels(t, e.st)
		in := ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendMinimaxVideo, videoModel), "p")
		in.Params = mediagen.Params{"resolution": "768P", "duration": int64(6)}
		res, err := mediaBackend(t, e.srv, mediagen.BackendMinimaxVideo).Generate(t.Context(), in)
		if err != nil || res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "does not exist or you do not have access") {
			t.Fatalf("结果 = %+v / %v，期望 failed + model_not_found 文案", res, err)
		}
		if creates, _, _ := e.mm.counts(); creates != 0 {
			t.Fatalf("范围外的模型不该打到厂商: creates=%d", creates)
		}
	})
	t.Run("协议面不符的模型", func(t *testing.T) {
		// 方舟后端拿到一个 minimax 模型：同面约束，按 model_not_found 处置。
		e := newVideoEnv(t)
		res, err := mediaBackend(t, e.srv, mediagen.BackendArkVideo).Generate(t.Context(),
			ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, videoModel), "p"))
		if err != nil || res.Status != store.MediaStatusFailed {
			t.Fatalf("结果 = %+v / %v，期望 failed", res, err)
		}
		if creates, _, _ := e.mm.counts(); creates != 0 {
			t.Fatalf("不该打到 minimax: creates=%d", creates)
		}
	})
}

func TestMediaArkVideoRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reply      fakeReply
		wantStatus string
		wantURL    string
		wantErr    bool
		wantText   []string
	}{
		{"running 折成 queued", fakeReply{http.StatusOK, fmt.Sprintf(`{"id":%q,"model":%q,"status":"running"}`, arkVendorTaskID, arkVideoUpModelID)}, store.MediaStatusQueued, "", false, nil},
		{"厂商 queued", fakeReply{http.StatusOK, fmt.Sprintf(`{"id":%q,"status":"queued"}`, arkVendorTaskID)}, store.MediaStatusQueued, "", false, nil},
		{"succeeded 带产物地址", fakeReply{}, store.MediaStatusSucceeded, "https://ark-cdn.example.com/v.mp4?sig=1", false, nil},
		{"succeeded 却没有产物", fakeReply{http.StatusOK, `{"status":"succeeded","content":{}}`}, store.MediaStatusFailed, "", false, []string{"未返回视频"}},
		{"任务级 failed", fakeReply{http.StatusOK, `{"status":"failed","error":{"code":"OutputVideoSensitiveContentDetected","message":"The output video may contain sensitive information."}}`},
			store.MediaStatusFailed, "", false, []string{"sensitive information", "OutputVideoSensitiveContentDetected"}},
		{"任务级 cancelled", fakeReply{http.StatusOK, `{"status":"cancelled"}`}, store.MediaStatusFailed, "", false, []string{"平台视频生成失败"}},
		{"厂商 404", fakeReply{http.StatusNotFound, `{"error":{"code":"ResourceNotFound","message":"task not found"}}`}, store.MediaStatusExpired, "", false, []string{"已过期", "HTTP 404"}},
		{"厂商 5xx", fakeReply{http.StatusBadGateway, `{"error":{"code":"InternalServiceError","message":"oops"}}`}, "", "", true, nil},
		{"厂商 429", fakeReply{http.StatusTooManyRequests, `{"error":{"code":"RateLimitExceeded","message":"slow down"}}`}, "", "", true, nil},
		{"响应解析不动", fakeReply{http.StatusOK, `{"id":"x"}`}, "", "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newArkVideoEnv(t)
			b := mediaBackend(t, e.srv, mediagen.BackendArkVideo)
			res, err := b.Generate(t.Context(), ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "p"))
			if err != nil || res.Status != store.MediaStatusQueued {
				t.Fatalf("Generate = %+v / %v", res, err)
			}
			// 计量出口在受理之后才接上：环里只可能有查询那一笔（如果有的话）。
			fm := &fakeMeter{}
			e.srv.EnableMetering(fm)
			if tc.reply.status != 0 {
				e.ark.mu.Lock()
				e.ark.query = tc.reply
				e.ark.mu.Unlock()
			}
			got, err := b.Refresh(t.Context(), vendorJob(t, e.routeEnv, mediagen.BackendArkVideo, arkVideoModel, res.VendorID))
			if _, queries, _ := e.ark.counts(); queries != 1 {
				t.Fatalf("厂商查询次数 = %d，期望 1", queries)
			}
			if n := fm.count(); n != 0 {
				t.Fatalf("查询不计模型消费：记账 %d 笔", n)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望返回 error 让下一轮再查，实得 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got.Status != tc.wantStatus || got.MediaURL != tc.wantURL || got.VendorID != res.VendorID || got.MediaType != "video" {
				t.Fatalf("结果 = %+v，期望 %q / %q", got, tc.wantStatus, tc.wantURL)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(got.Error, w) {
					t.Fatalf("失败原因 = %q，缺少 %q", got.Error, w)
				}
			}
			// 旁路观测与数据面同一份：任务行的状态跟着厂商走，终态就地清算。
			task := taskByVendorIDIn(t, e.routeEnv, res.VendorID)
			switch tc.name {
			case "succeeded 带产物地址":
				if task.Status != "succeeded" || len(fm.settled) != 1 {
					t.Fatalf("任务行 = %q，清算 %d 次；期望 succeeded 且清算一次", task.Status, len(fm.settled))
				}
			case "厂商 404":
				if task.Status != "expired" {
					t.Fatalf("任务行 = %q，期望归一成 expired", task.Status)
				}
			case "任务级 failed":
				if task.Status != "failed" {
					t.Fatalf("任务行 = %q，期望 failed", task.Status)
				}
			}
			if strings.Contains(e.logBuf.String(), "sensitive information") {
				t.Fatalf("日志不该带厂商错误正文: %s", e.logBuf.String())
			}
		})
	}
}

// 查询按归属密钥定位 aigc 行：别的 Key 名下的媒体任务拿这个厂商 id 查不到东西，也不打厂商；
// 没有平台任务 ID 的任务不支持刷新。
func TestMediaVendorVideoRefreshIsScopedToOwningKey(t *testing.T) {
	e := newArkVideoEnv(t)
	b := mediaBackend(t, e.srv, mediagen.BackendArkVideo)
	res, err := b.Generate(t.Context(), ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendArkVideo, arkVideoModel), "p"))
	if err != nil || res.Status != store.MediaStatusQueued {
		t.Fatalf("Generate = %+v / %v", res, err)
	}
	job := vendorJob(t, e.routeEnv, mediagen.BackendArkVideo, arkVideoModel, res.VendorID)
	job.KeyID = importOtherKey(t, e.routeEnv)
	got, err := b.Refresh(t.Context(), job)
	if err != nil || got.Status != store.MediaStatusExpired {
		t.Fatalf("别的 Key 查询 = %+v / %v，期望与「不存在」同一结局 expired", got, err)
	}
	// 跨协议面同样查不到：minimax 后端拿方舟的任务 id。
	job.KeyID = testKeyID(t, e.st)
	if got, err := mediaBackend(t, e.srv, mediagen.BackendMinimaxVideo).Refresh(t.Context(), job); err != nil || got.Status != store.MediaStatusExpired {
		t.Fatalf("跨协议面查询 = %+v / %v，期望 expired", got, err)
	}
	if _, queries, _ := e.ark.counts(); queries != 0 {
		t.Fatalf("查不到行的查询不该打到厂商: queries=%d", queries)
	}
	job.VendorID = ""
	if _, err := b.Refresh(t.Context(), job); err == nil {
		t.Fatal("没有平台任务 ID 的任务不该支持刷新")
	}
}

// ---- minimax_video ----

func TestMediaMinimaxVideoGenerate(t *testing.T) {
	const first, ref = "data:image/jpeg;base64,/9j/4AAQ", "https://example.invalid/ref.png"
	e := newVideoEnv(t)
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	b := mediaBackend(t, e.srv, mediagen.BackendMinimaxVideo)
	in := ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendMinimaxVideo, videoModel), "一只猫走过走廊")
	in.Inputs = mediagen.Inputs{FirstFrame: first, ReferenceImages: []string{ref, ref}}
	in.Params = mediagen.Params{"resolution": "768P", "duration": int64(6)}

	res, err := b.Generate(t.Context(), in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Status != store.MediaStatusQueued || res.VendorID != fakeVendorTaskID || res.MediaType != "video" {
		t.Fatalf("结果 = %+v，期望 queued + 厂商任务 id", res)
	}
	if paths := e.mm.sentPaths(); len(paths) != 1 || paths[0] != "/v2/video_generation" {
		t.Fatalf("受理路径 = %v，期望 /v2/video_generation", paths)
	}
	raw, header := e.mm.sentCreate(0)
	if got := header.Get("Authorization"); got != "Bearer "+videoUpstreamKey {
		t.Fatalf("厂商收到的 Authorization 不是上游账户的凭据")
	}
	sent := decodeObject(t, raw)
	if sent["model"] != videoUpModelID {
		t.Fatalf("model = %v，期望改写成来源侧 ID %q", sent["model"], videoUpModelID)
	}
	assertVendorVideoContent(t, sent, "一只猫走过走廊", [][2]string{{"first_frame", first}, {"reference_image", ref}, {"reference_image", ref}})
	if sent["resolution"] != "768P" || sent["duration"] != float64(6) || len(sent) != 4 {
		t.Fatalf("顶层参数 = %v，期望只有 model / content / resolution / duration", sent)
	}
	task := taskByVendorID(t, e, fakeVendorTaskID)
	if task.KeyID != in.KeyID || task.KeyDisplay != testKeyDisplay || task.UpstreamName != "mm-video-main" || task.ModelName != videoModel || task.Status != "queued" {
		t.Fatalf("任务行 = %+v", task)
	}
	if task.ReqResolution != "768P" || task.ReqDuration != "6" {
		t.Fatalf("计费特征 = resolution %q / duration %q，期望 768P / 6", task.ReqResolution, task.ReqDuration)
	}
	s := fm.only(t)
	if s.Entry != usage.EntryVideo || s.KeyID != in.KeyID || s.KeyDisplay != testKeyDisplay || s.ModelName != videoModel || s.UpstreamName != "mm-video-main" {
		t.Fatalf("记账 = %+v", s)
	}
}

func TestMediaMinimaxVideoRefresh(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reply      fakeReply
		wantStatus string
		wantURL    string
		wantErr    bool
		wantText   []string
	}{
		{"running 折成 queued", fakeReply{}, store.MediaStatusQueued, "", false, nil},
		{"succeeded 带产物地址", fakeReply{http.StatusOK, `{"task":{"status":"succeeded","content":{"url":"https://mm-cdn.example.com/out.mp4"},"usage":{"video_seconds":6}}}`},
			store.MediaStatusSucceeded, "https://mm-cdn.example.com/out.mp4", false, nil},
		{"任务级 failed（数字 code）", fakeReply{http.StatusOK, `{"task":{"status":"failed","code":1026,"message":"video description contains sensitive content"}}`},
			store.MediaStatusFailed, "", false, []string{"sensitive content", "1026"}},
		{"厂商 404", fakeReply{http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"task not found"}}`}, store.MediaStatusExpired, "", false, []string{"已过期", "HTTP 404"}},
		{"厂商 5xx", fakeReply{http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"oops"}}`}, "", "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newVideoEnv(t)
			b := mediaBackend(t, e.srv, mediagen.BackendMinimaxVideo)
			in := ownedRequest(t, e.routeEnv, familyModel(t, mediagen.BackendMinimaxVideo, videoModel), "p")
			in.Params = mediagen.Params{"resolution": "768P", "duration": int64(6)}
			res, err := b.Generate(t.Context(), in)
			if err != nil || res.Status != store.MediaStatusQueued {
				t.Fatalf("Generate = %+v / %v", res, err)
			}
			if tc.reply.status != 0 {
				e.mm.setQuery(tc.reply.status, tc.reply.body)
			}
			got, err := b.Refresh(t.Context(), vendorJob(t, e.routeEnv, mediagen.BackendMinimaxVideo, videoModel, res.VendorID))
			if ids := e.mm.sentTaskIDs(); len(ids) != 1 || ids[0] != res.VendorID {
				t.Fatalf("厂商查询命中的任务 id = %v，期望 [%s]", ids, res.VendorID)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望返回 error 让下一轮再查，实得 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if got.Status != tc.wantStatus || got.MediaURL != tc.wantURL || got.VendorID != res.VendorID || got.MediaType != "video" {
				t.Fatalf("结果 = %+v，期望 %q / %q", got, tc.wantStatus, tc.wantURL)
			}
			for _, w := range tc.wantText {
				if !strings.Contains(got.Error, w) {
					t.Fatalf("失败原因 = %q，缺少 %q", got.Error, w)
				}
			}
		})
	}
}

// ---- ark_image ----

func TestMediaArkImageGenerate(t *testing.T) {
	const ref1, ref2 = "data:image/png;base64,iVBORw0KGgo=", "https://example.invalid/ref2.png"
	for _, tc := range []struct {
		name      string
		refs      []string
		params    mediagen.Params
		wantImage any
	}{
		{"文生图：全部参数", nil, mediagen.Params{"size": "2K", "watermark": false, "seed": int64(42)}, nil},
		{"一张参考图是单值", []string{ref1}, nil, ref1},
		{"多张参考图是数组", []string{ref1, ref2}, mediagen.Params{"watermark": true}, []any{ref1, ref2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
			fm := &fakeMeter{}
			e.srv.EnableMetering(fm)
			in := ownedRequest(t, e, familyModel(t, mediagen.BackendArkImage, imageModel), "一只赛博朋克猫")
			in.Inputs.ReferenceImages, in.Params = tc.refs, tc.params
			res, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(), in)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if res.Status != store.MediaStatusSucceeded || res.MediaType != "image" || res.MediaURL != "https://ark-cdn.example.com/tos/img-1.png" || res.VendorID != "" {
				t.Fatalf("结果 = %+v，期望 succeeded + data[0].url", res)
			}
			if stub.count() != 1 || stub.sentAuth(0) != "Bearer "+imageUpstreamKey {
				t.Fatalf("上游调用 %d 次，凭据应是上游账户的", stub.count())
			}
			sent := decodeObject(t, []byte(stub.sentBody(0)))
			if sent["model"] != imageUpstreamID || sent["prompt"] != "一只赛博朋克猫" || sent["response_format"] != "url" {
				t.Fatalf("请求体 = %v，期望 model 改写成来源侧 ID、response_format=url", sent)
			}
			if got, want := fmt.Sprint(sent["image"]), fmt.Sprint(tc.wantImage); got != want {
				t.Fatalf("image = %v，期望 %v", sent["image"], tc.wantImage)
			}
			if _, has := sent["image"]; has != (tc.wantImage != nil) {
				t.Fatalf("image 字段在场 = %v: %v", has, sent)
			}
			for _, k := range []string{"size", "watermark", "seed"} {
				want, given := tc.params[k]
				got, has := sent[k]
				if has != given {
					t.Fatalf("%s 在场 = %v，期望 %v（没带的参数不带字段）: %v", k, has, given, sent)
				}
				if n, ok := want.(int64); ok {
					want = float64(n)
				}
				if given && got != want {
					t.Fatalf("%s = %v，期望 %v", k, got, want)
				}
			}
			for _, k := range []string{"inputs", "params", "reference_images", "operation", "n", "stream"} {
				if _, ok := sent[k]; ok {
					t.Fatalf("请求体不该带 %s: %v", k, sent[k])
				}
			}
			// 用量按 image 入口记在归属密钥名下，厂商 usage 与张数照数据面口径观察到。
			s := fm.only(t)
			if s.Entry != usage.EntryImage || s.Kind != store.ModelKindImage || s.KeyID != in.KeyID || s.KeyDisplay != testKeyDisplay ||
				s.ModelName != imageModel || s.UpstreamName != "ark-image-main" || s.Status != http.StatusOK {
				t.Fatalf("记账 = %+v", s)
			}
			if s.TaskUsage.GeneratedImages != 1 {
				t.Fatalf("出图张数 = %d，期望 1", s.TaskUsage.GeneratedImages)
			}
			if strings.Contains(e.logBuf.String(), "赛博朋克") || strings.Contains(e.logBuf.String(), "iVBORw0KGgo") {
				t.Fatalf("日志泄露了提示词或媒体")
			}
		})
	}
}

// 逐图失败挂在 data[].error、HTTP 仍 200：任务 failed 并带厂商 code / message；HTTP 层拒绝
// 同样 failed；范围外的模型按 model_not_found，不打上游。
func TestMediaArkImageFailures(t *testing.T) {
	t.Run("data[0].error", func(t *testing.T) {
		e, _ := newImageEnv(t, jsonReply(http.StatusOK,
			`{"model":"doubao-seedream-4-0-250828","data":[{"error":{"code":"OutputImageSensitiveContentDetected","message":"The output image may contain sensitive information."}}],"usage":{"generated_images":0}}`))
		res, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(),
			ownedRequest(t, e, familyModel(t, mediagen.BackendArkImage, imageModel), "p"))
		if err != nil || res.Status != store.MediaStatusFailed || res.MediaURL != "" ||
			!strings.Contains(res.Error, "sensitive information") || !strings.Contains(res.Error, "OutputImageSensitiveContentDetected") {
			t.Fatalf("结果 = %+v / %v，期望 failed 且带厂商 code / message", res, err)
		}
	})
	t.Run("HTTP 层拒绝", func(t *testing.T) {
		e, _ := newImageEnv(t, jsonReply(http.StatusBadRequest, `{"error":{"code":"InvalidParameter","message":"size is invalid"}}`))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		res, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(),
			ownedRequest(t, e, familyModel(t, mediagen.BackendArkImage, imageModel), "p"))
		if err != nil || res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "size is invalid") || !strings.Contains(res.Error, "InvalidParameter") {
			t.Fatalf("结果 = %+v / %v", res, err)
		}
		if s := fm.only(t); s.Entry != usage.EntryImage || s.Status != http.StatusBadRequest || s.KeyID != testKeyID(t, e.st) {
			t.Fatalf("记账 = %+v，期望 image 入口记下这次 400", s)
		}
	})
	t.Run("成功体里没有图", func(t *testing.T) {
		e, _ := newImageEnv(t, jsonReply(http.StatusOK, `{"data":[]}`))
		if _, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(),
			ownedRequest(t, e, familyModel(t, mediagen.BackendArkImage, imageModel), "p")); err == nil {
			t.Fatal("期望返回 error（内核判失败）")
		}
	})
	t.Run("模型在可用范围之外", func(t *testing.T) {
		e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
		restrictAPIModels(t, e.st)
		res, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(),
			ownedRequest(t, e, familyModel(t, mediagen.BackendArkImage, imageModel), "p"))
		if err != nil || res.Status != store.MediaStatusFailed || !strings.Contains(res.Error, "does not exist or you do not have access") {
			t.Fatalf("结果 = %+v / %v，期望 failed + model_not_found 文案", res, err)
		}
		if stub.count() != 0 {
			t.Fatalf("范围外的模型不该打到上游: %d 次", stub.count())
		}
	})
	t.Run("不支持刷新", func(t *testing.T) {
		e, _ := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
		if _, err := mediaBackend(t, e.srv, mediagen.BackendArkImage).Refresh(t.Context(), store.MediaJob{ID: "01JOB", VendorID: "x"}); err == nil {
			t.Fatal("图像任务不该支持刷新")
		}
	})
}

// 网关交出的后端名与能力表的后端名一一对应（内核按名字把模型派给后端）。
func TestMediaBackendsCoverCapabilityBackends(t *testing.T) {
	e := newRouteEnv(t)
	got := map[string]bool{}
	for _, b := range e.srv.MediaBackends() {
		if got[b.Name()] {
			t.Fatalf("后端名重复: %s", b.Name())
		}
		got[b.Name()] = true
	}
	want := append([]string{mediagen.BackendGrok, mediagen.BackendCodex}, mediagen.Families()...)
	for _, name := range want {
		if !got[name] {
			t.Fatalf("网关没有交出后端 %q（已有 %v）", name, got)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("后端 = %v，期望恰为 %v", got, want)
	}
}

// ---- 准入闸 ----

// recordingMeter 是真 Meter 外面套一层旁听：准入判据走真的（RPM 窗口、预算），每笔样本另记
// 一份供断言。
type recordingMeter struct {
	*usage.Meter
	mu      sync.Mutex
	samples []usage.Sample
}

func (m *recordingMeter) Record(s usage.Sample) {
	m.mu.Lock()
	m.samples = append(m.samples, s)
	m.mu.Unlock()
	m.Meter.Record(s)
}

func (m *recordingMeter) all() []usage.Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]usage.Sample(nil), m.samples...)
}

func (m *recordingMeter) rejected() []usage.Sample {
	var out []usage.Sample
	for _, s := range m.all() {
		if s.Rejected {
			out = append(out, s)
		}
	}
	return out
}

func enableRealMeter(t *testing.T, e *routeEnv) *recordingMeter {
	t.Helper()
	m := &recordingMeter{Meter: usage.NewMeter(e.st, 31, nil, nil, logging.New(io.Discard, slog.LevelDebug))}
	e.srv.EnableMetering(m)
	return m
}

func setKeyRPM(t *testing.T, e *routeEnv, rpm int64) {
	t.Helper()
	if err := e.st.SetAPIKeyLimits(t.Context(), testKeyID(t, e.st), nil, nil, nil, &rpm); err != nil {
		t.Fatalf("SetAPIKeyLimits: %v", err)
	}
}

func TestAdmitMediaJobRPM(t *testing.T) {
	e := newArkVideoEnv(t)
	m := enableRealMeter(t, e.routeEnv)
	setKeyRPM(t, e.routeEnv, 1)
	keyID := testKeyID(t, e.st)
	model := familyModel(t, mediagen.BackendArkVideo, arkVideoModel)

	if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); err != nil {
		t.Fatalf("第一次准入 = %v，期望放行", err)
	}
	if n := len(m.all()); n != 0 {
		t.Fatalf("放行不记样本：%d 笔", n)
	}
	err := e.srv.AdmitMediaJob(t.Context(), keyID, model)
	var rejected *mediagen.RejectedError
	if !errors.As(err, &rejected) || rejected.Code != "rate_limited" || rejected.RetryAfterSec < 1 || rejected.Msg == "" {
		t.Fatalf("第二次准入 = %v，期望 *RejectedError{rate_limited} 且带 Retry-After", err)
	}
	if status, code, _, retry, ok := mediagen.HTTPError(err); !ok || status != http.StatusTooManyRequests || code != "rate_limited" || retry != rejected.RetryAfterSec {
		t.Fatalf("HTTPError = %d %s retry=%d", status, code, retry)
	}
	// 被拒的那一笔进账本的 rejected 计数：入口随模型（视频 → video），归属这把 Key。
	samples := m.all()
	if len(samples) != 1 {
		t.Fatalf("样本 = %d 笔，期望恰一笔 rejected", len(samples))
	}
	s := samples[0]
	if !s.Rejected || s.RejectReason != usage.RejectRPM || s.Entry != usage.EntryVideo || s.ModelName != arkVideoModel ||
		s.KeyID != keyID || s.KeyDisplay != testKeyDisplay || s.Status != http.StatusTooManyRequests || s.UpstreamName != "" {
		t.Fatalf("rejected 样本 = %+v", s)
	}
	// 文案不泄露限额数值与上游身份（与数据面 429 同一口径）。
	assertNoLeak(t, "媒体生成准入", rejected.Msg)
	// 入口随后端走：订阅图像 imagine_image、Codex responses_agents、方舟图像 image。
	for _, tc := range []struct {
		model mediagen.Model
		entry string
	}{
		{mediaPreset(t, imagineImageModel), usage.EntryImagineImage},
		{mediaPreset(t, imagineVideoModel), usage.EntryImagineVideo},
		{mediaPreset(t, "gpt-image-2"), usage.EntryResponsesAgents},
		{familyModel(t, mediagen.BackendArkImage, imageModel), usage.EntryImage},
		{familyModel(t, mediagen.BackendMinimaxVideo, videoModel), usage.EntryVideo},
	} {
		before := len(m.all())
		if err := e.srv.AdmitMediaJob(t.Context(), keyID, tc.model); !errors.As(err, &rejected) {
			t.Fatalf("%s 准入 = %v，期望仍被 RPM 拒绝", tc.model.ID, err)
		}
		all := m.all()
		if len(all) != before+1 || all[before].Entry != tc.entry || all[before].ModelName != tc.model.ID {
			t.Fatalf("%s 的 rejected 样本入口 = %+v，期望 %s", tc.model.ID, all[len(all)-1], tc.entry)
		}
	}
}

func TestAdmitMediaJobBudget(t *testing.T) {
	e := newArkVideoEnv(t)
	m := enableRealMeter(t, e.routeEnv)
	keyID := testKeyID(t, e.st)
	model := familyModel(t, mediagen.BackendArkVideo, arkVideoModel)
	zero := int64(0)
	if err := e.st.SetAPIKeyLimits(t.Context(), keyID, &zero, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits: %v", err)
	}
	err := e.srv.AdmitMediaJob(t.Context(), keyID, model)
	var rejected *mediagen.RejectedError
	if !errors.As(err, &rejected) || rejected.Code != "budget_exceeded" {
		t.Fatalf("预算耗尽的准入 = %v，期望 *RejectedError{budget_exceeded}", err)
	}
	assertNoLeak(t, "媒体生成准入", rejected.Msg)
	if got := m.rejected(); len(got) != 1 || got[0].RejectReason != usage.RejectKeyBudgetDay || got[0].KeyID != keyID {
		t.Fatalf("rejected 样本 = %+v，期望一笔 key_budget_day", got)
	}
	// 预算满但按量额度尚余：放行（与数据面同一份判据）。
	if _, err := e.st.AdjustAPIKeyMeteredAllowance(t.Context(), keyID, 1_000_000); err != nil {
		t.Fatalf("AdjustAPIKeyMeteredAllowance: %v", err)
	}
	if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); err != nil {
		t.Fatalf("有按量额度时的准入 = %v，期望放行", err)
	}
	// 清除限额即时生效（按行 id 点查，无缓存）。
	if err := e.st.SetAPIKeyLimits(t.Context(), keyID, nil, nil, nil, nil); err != nil {
		t.Fatalf("SetAPIKeyLimits(清除): %v", err)
	}
	if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); err != nil {
		t.Fatalf("清除限额后的准入 = %v，期望放行", err)
	}
}

// 计量未装配时一律放行（计量是旁路能力）；Key 不存在是 not_found，不是放行。
func TestAdmitMediaJobWithoutMeterAndUnknownKey(t *testing.T) {
	e := newArkVideoEnv(t)
	model := familyModel(t, mediagen.BackendArkVideo, arkVideoModel)
	keyID := testKeyID(t, e.st)
	setKeyRPM(t, e.routeEnv, 1)
	for i := 0; i < 3; i++ {
		if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); err != nil {
			t.Fatalf("未装配计量时第 %d 次准入 = %v，期望放行", i+1, err)
		}
	}
	if err := e.srv.AdmitMediaJob(t.Context(), 999999, model); err != nil {
		t.Fatalf("未装配计量时不点查 Key：%v", err)
	}
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)
	err := e.srv.AdmitMediaJob(t.Context(), 999999, model)
	var unavailable *mediagen.UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Code != "not_found" {
		t.Fatalf("不存在的 Key = %v，期望 *UnavailableError{not_found}", err)
	}
	if fm.count() != 0 {
		t.Fatalf("不存在的 Key 不该进账本：%d 笔", fm.count())
	}
}

// 准入只在受理时过一次：RPM=1 的 Key，AdmitMediaJob 一次之后经后端 Generate 照常出门——
// Generate 内部不再调 admit，账本里没有第二次 RPM 拒绝。三个厂商面后端与 Grok 订阅后端同理。
func TestMediaBackendGenerateDoesNotReAdmit(t *testing.T) {
	assertNoSecondGate := func(t *testing.T, e *routeEnv, m *recordingMeter, model mediagen.Model, entry string, generate func() (mediagen.Result, error), wantStatus string) {
		t.Helper()
		keyID := testKeyID(t, e.st)
		if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); err != nil {
			t.Fatalf("受理准入 = %v，期望放行", err)
		}
		res, err := generate()
		if err != nil || res.Status != wantStatus {
			t.Fatalf("Generate = %+v / %v，期望 %s（不该被 RPM 挡在后端里）", res, err, wantStatus)
		}
		if got := m.rejected(); len(got) != 0 {
			t.Fatalf("Generate 之后账本里出现了 rejected 样本：%+v", got)
		}
		all := m.all()
		if len(all) != 1 || all[0].Entry != entry || all[0].KeyID != keyID || all[0].Status != http.StatusOK {
			t.Fatalf("样本 = %+v，期望恰一笔 %s 的 200", all, entry)
		}
		// 窗口确实已满：再来一次受理准入就被拒——上面那次 Generate 没被拒不是因为限额没生效。
		var rejected *mediagen.RejectedError
		if err := e.srv.AdmitMediaJob(t.Context(), keyID, model); !errors.As(err, &rejected) || rejected.Code != "rate_limited" {
			t.Fatalf("窗口已满时的准入 = %v，期望 rate_limited", err)
		}
	}
	t.Run("ark_video", func(t *testing.T) {
		e := newArkVideoEnv(t)
		m := enableRealMeter(t, e.routeEnv)
		setKeyRPM(t, e.routeEnv, 1)
		model := familyModel(t, mediagen.BackendArkVideo, arkVideoModel)
		assertNoSecondGate(t, e.routeEnv, m, model, usage.EntryVideo, func() (mediagen.Result, error) {
			return mediaBackend(t, e.srv, mediagen.BackendArkVideo).Generate(t.Context(), ownedRequest(t, e.routeEnv, model, "p"))
		}, store.MediaStatusQueued)
		if creates, _, _ := e.ark.counts(); creates != 1 {
			t.Fatalf("厂商受理次数 = %d，期望 1", creates)
		}
	})
	t.Run("minimax_video", func(t *testing.T) {
		e := newVideoEnv(t)
		m := enableRealMeter(t, e.routeEnv)
		setKeyRPM(t, e.routeEnv, 1)
		model := familyModel(t, mediagen.BackendMinimaxVideo, videoModel)
		assertNoSecondGate(t, e.routeEnv, m, model, usage.EntryVideo, func() (mediagen.Result, error) {
			in := ownedRequest(t, e.routeEnv, model, "p")
			in.Params = mediagen.Params{"resolution": "768P", "duration": int64(6)}
			return mediaBackend(t, e.srv, mediagen.BackendMinimaxVideo).Generate(t.Context(), in)
		}, store.MediaStatusQueued)
	})
	t.Run("ark_image", func(t *testing.T) {
		e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
		m := enableRealMeter(t, e)
		setKeyRPM(t, e, 1)
		model := familyModel(t, mediagen.BackendArkImage, imageModel)
		assertNoSecondGate(t, e, m, model, usage.EntryImage, func() (mediagen.Result, error) {
			return mediaBackend(t, e.srv, mediagen.BackendArkImage).Generate(t.Context(), ownedRequest(t, e, model, "p"))
		}, store.MediaStatusSucceeded)
		if stub.count() != 1 {
			t.Fatalf("上游调用 = %d 次，期望 1", stub.count())
		}
	})
	t.Run("grok", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		m := enableRealMeter(t, e.routeEnv)
		setKeyRPM(t, e.routeEnv, 1)
		model := mediaPreset(t, imagineImageModel)
		assertNoSecondGate(t, e.routeEnv, m, model, usage.EntryImagineImage, func() (mediagen.Result, error) {
			in := ownedRequest(t, e.routeEnv, model, "p")
			in.AccountID = e.acctID
			return mediaBackend(t, e.srv, mediagen.BackendGrok).Generate(t.Context(), in)
		}, store.MediaStatusSucceeded)
	})
}

// 端到端：内核挂上网关的准入闸之后，超 RPM 的提交在持有人端点答 429 rate_limited + Retry-After，
// 不建任何任务；候选数按个过闸，任一被拒则整批不建。
func TestKeyMediaSubmitPassesAdmission(t *testing.T) {
	e := newRouteEnv(t)
	svc, _ := wireFakeMedia(t, e, false)
	svc.SetAdmitter(e.srv)
	m := enableRealMeter(t, e)
	connectAgent(t, e.st, store.NewAgentAccount{Provider: store.AgentProviderGrok, AuthJSON: grokAuthJSONFixture(grokAccess1, grokRefresh1)})
	setKeyRPM(t, e, 2)

	// 窗口里还剩 2 次，但数据面请求（withAuth 之后的提交本身）不占名额——只有准入占。
	// count=3 要过三次闸：第三次被拒，整批不建。
	w := do(e.h, http.MethodPost, mediaJobsPath, chatAuth, `{"model":"grok-imagine-image-2.0","prompt":"p","count":3}`)
	if w.Code != http.StatusTooManyRequests || !bytes.Contains(w.Body.Bytes(), []byte("rate_limited")) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("超 RPM 的批量提交 = %d retry=%q %s，期望 429 rate_limited + Retry-After", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	if list := listKeyMediaJobs(t, e, chatAuth); len(list) != 0 {
		t.Fatalf("被准入闸拒绝的提交不该建任务：%+v", list)
	}
	if got := m.rejected(); len(got) != 1 || got[0].Entry != usage.EntryImagineImage || got[0].RejectReason != usage.RejectRPM {
		t.Fatalf("rejected 样本 = %+v，期望一笔 imagine_image 的 rpm", got)
	}
}
