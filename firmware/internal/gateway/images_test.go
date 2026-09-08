// images_test.go 是 iteration-8 Phase 5 的验收：图片同步入口
// POST /api/v3/images/generations 的假上游出图全链路（请求体透传只改 model、响应
// 顶层 model 回写、data/usage 原样透出、上游身份不泄露）、kind 闸门双向 404、
// /v1/models 文本口径不受图片模型影响、上游错误体保真透传、种类内协议过滤
// （minimax 型来源不服务 ark_image）、SSE 透传与请求体上限。
//
// 编排方式沿 video_test.go：httptest 假上游以真实类型 ark + base_url 覆盖挂进
// 目录（mock 类型不服务图片协议），断言"客户端看到什么"与"厂商收到什么"。
package gateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	imageModel       = "seedream-img"
	imageUpstreamID  = "doubao-seedream-4-0-250828"
	imageUpstreamKey = "sk-ark-image-not-real"

	// imageOKBody 按方舟官方文档的非流式成功响应形态构造（顶层 model 是
	// 来源侧 ID——回写可证；usage 三字段是账单事实）。
	imageOKBody = `{"model":"doubao-seedream-4-0-250828","created":1765250822,` +
		`"data":[{"url":"https://ark-cdn.example.com/tos/img-1.png","size":"2048x2048"}],` +
		`"usage":{"generated_images":1,"output_tokens":16464,"total_tokens":16464}}`
)

// newImageEnv 装配图片入口用例环境：空目录网关 + 一个只认
// POST {base}/images/generations 的假方舟上游与 kind=image 模型。
// 客户端入口是 /api/v3/images/generations（方舟站点根之后的官方那一段），
// 而设备→厂商这一跳的路径不含 /api/v3——那一段在端点根里，用例的 base_url
// 覆盖把它换成了 stub 地址。两者不同正是本环境要钉住的东西。
func newImageEnv(t *testing.T, respond http.HandlerFunc) (*routeEnv, *stubUpstream) {
	t.Helper()
	e := newRouteEnv(t)
	stub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		// 路径钉死：base_url 覆盖后网关应打 {base}/images/generations
		// （newStub 的 base 以 /v1 结尾，故此处是 /v1/images/generations）。
		// 打错路径回 404，主断言的 200 会当场揭穿。
		if r.Method != http.MethodPost || r.URL.Path != "/v1/images/generations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(w, r)
	})
	up := dbUpstream(t, e.st, "ark-image-main", config.UpstreamArk, imageUpstreamKey, stub.url)
	dbSource(t, e.st, dbKindModel(t, e.st, imageModel, store.ModelKindImage), up, imageUpstreamID, 100)
	return e, stub
}

// TestImagesGenerateFullPath：出图全链路。请求体透传只改 model 为来源侧 ID、
// 凭证换成上游 Key；响应顶层 model 回写为客户端请求名，data/usage 原样透出；
// 来源侧 ID 与上游账户名绝不出现在响应。
func TestImagesGenerateFullPath(t *testing.T) {
	e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))

	w := do(e.h, "POST", "/ark/api/v3/images/generations", chatAuth,
		fmt.Sprintf(`{"model":%q,"prompt":"一只赛博朋克猫","size":"2K","watermark":false}`, imageModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Data    []struct {
			URL  string `json:"url"`
			Size string `json:"size"`
		} `json:"data"`
		Usage map[string]json.Number `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, w.Body.String())
	}
	if resp.Model != imageModel {
		t.Errorf("响应 model = %q，期望回写为 %q", resp.Model, imageModel)
	}
	if resp.Created != 1765250822 {
		t.Errorf("created = %d，期望原样透传 1765250822", resp.Created)
	}
	if len(resp.Data) != 1 || resp.Data[0].URL != "https://ark-cdn.example.com/tos/img-1.png" ||
		resp.Data[0].Size != "2048x2048" {
		t.Errorf("data 未原样透传: %s", w.Body.String())
	}
	if resp.Usage["generated_images"].String() != "1" || resp.Usage["output_tokens"].String() != "16464" {
		t.Errorf("usage 未原样透传: %s", w.Body.String())
	}
	for _, leak := range []string{imageUpstreamID, "ark-image-main", imageUpstreamKey} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("响应泄露了上游侧信息 %q: %s", leak, w.Body.String())
		}
	}

	if stub.count() != 1 {
		t.Fatalf("上游收到 %d 次请求，期望 1", stub.count())
	}
	if got := stub.sentModel(0); got != imageUpstreamID {
		t.Errorf("上游收到 model = %q，期望改写为来源侧 ID %q", got, imageUpstreamID)
	}
	if got := stub.sentAuth(0); got != "Bearer "+imageUpstreamKey {
		t.Errorf("上游收到 Authorization = %q，期望注入上游 Key", got)
	}
}

// TestImagesKindGate：kind 闸门双向——图片模型进文本双入口、文本/视频模型与
// 未知名字进图片入口，一律 404，且与「不存在」同响应（不解释内部原因）。
func TestImagesKindGate(t *testing.T) {
	e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
	// 文本与视频模型各一（上游可达性无所谓：kind 闸门在拨号之前裁决）。
	upDead := dbUpstream(t, e.st, "text-up", config.UpstreamMock, "k-text", deadURL)
	dbSource(t, e.st, dbModel(t, e.st, "plain-text"), upDead, "", 100)
	upArk := dbUpstream(t, e.st, "ark-video-side", config.UpstreamArk, "k-video", deadURL)
	dbSource(t, e.st, dbKindModel(t, e.st, "seedance-side", store.ModelKindVideo), upArk, "", 100)

	cases := []struct {
		name, method, path, body string
	}{
		{"图片模型进chat", "POST", "/v1/chat/completions",
			fmt.Sprintf(`{"model":%q,"messages":[]}`, imageModel)},
		{"图片模型进messages", "POST", "/v1/messages",
			fmt.Sprintf(`{"model":%q,"messages":[],"max_tokens":8}`, imageModel)},
		{"文本模型进图片入口", "POST", "/ark/api/v3/images/generations",
			`{"model":"plain-text","prompt":"p"}`},
		{"视频模型进图片入口", "POST", "/ark/api/v3/images/generations",
			`{"model":"seedance-side","prompt":"p"}`},
		{"未知模型进图片入口", "POST", "/ark/api/v3/images/generations",
			`{"model":"no-such","prompt":"p"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(e.h, c.method, c.path, chatAuth, c.body)
			if w.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404；body: %s", w.Code, w.Body.String())
			}
			if c.path != "/v1/messages" { // messages 侧 Anthropic 形无 code 字段
				if _, code, _ := decodeError(t, w); code != "model_not_found" {
					t.Errorf("error.code = %q，期望 model_not_found", code)
				}
			}
			if strings.Contains(w.Body.String(), "kind") || strings.Contains(w.Body.String(), "image") {
				t.Errorf("404 响应泄露了种类信息: %s", w.Body.String())
			}
		})
	}
	if stub.count() != 0 {
		t.Errorf("闸门用例打到了上游 %d 次", stub.count())
	}
}

// TestImagesModelsUnaffected：/v1/models 文本口径不变——可服务的图片模型不
// 出现，既有文本模型照常在列。
func TestImagesModelsUnaffected(t *testing.T) {
	e, _ := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
	upDead := dbUpstream(t, e.st, "text-up", config.UpstreamMock, "k-text", deadURL)
	dbSource(t, e.st, dbModel(t, e.st, "plain-text"), upDead, "", 100)

	w := do(e.h, "GET", "/v1/models", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if len(ids) != 1 || ids[0] != "plain-text" {
		t.Errorf("/v1/models = %v，期望只有 [plain-text]（图片模型不入列）", ids)
	}
}

// TestImagesUpstreamErrorPassthrough：上游请求级错误（OpenAI 形
// {error:{code,message}}）原样透传——状态码与错误体逐字节不改写。
func TestImagesUpstreamErrorPassthrough(t *testing.T) {
	const errBody = `{"error":{"code":"InvalidParameter.SensitiveContent","message":"输入文本含敏感信息"}}`
	e, _ := newImageEnv(t, jsonReply(http.StatusBadRequest, errBody))

	w := do(e.h, "POST", "/ark/api/v3/images/generations", chatAuth,
		fmt.Sprintf(`{"model":%q,"prompt":"x"}`, imageModel))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400 原样透传；body: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != errBody {
		t.Errorf("错误体被改写:\n got  %s\n want %s", w.Body.String(), errBody)
	}
}

// TestImagesProtocolFilterWithinKind：种类内协议过滤——图片模型同时挂
// minimax 与 ark 来源时，minimax（不服务 ark_image，优先级还更高）被静默
// 跳过，请求落到 ark 来源。
func TestImagesProtocolFilterWithinKind(t *testing.T) {
	e := newRouteEnv(t)
	stub := newStub(t, jsonReply(http.StatusOK, imageOKBody))
	mID := dbKindModel(t, e.st, "seedream-mixed", store.ModelKindImage)
	// minimax 来源优先级 50 压过 ark 的 100——若协议过滤失效它会先被拨号。
	upMM := dbUpstream(t, e.st, "minimax-side", config.UpstreamMinimax, "k-mm", "")
	dbSource(t, e.st, mID, upMM, "MiniMax-Image", 50)
	upArk := dbUpstream(t, e.st, "ark-image-b", config.UpstreamArk, imageUpstreamKey, stub.url)
	dbSource(t, e.st, mID, upArk, imageUpstreamID, 100)

	w := do(e.h, "POST", "/ark/api/v3/images/generations", chatAuth,
		`{"model":"seedream-mixed","prompt":"p"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（minimax 来源应被协议过滤跳过）；body: %s", w.Code, w.Body.String())
	}
	if stub.count() != 1 {
		t.Errorf("ark 上游收到 %d 次请求，期望 1", stub.count())
	}
}

// TestImagesSSEPassthrough：stream:true 的 SSE 事件流逐行透传，带 model 的
// data 行回写为客户端请求名，事件形态与其余字段不动。
func TestImagesSSEPassthrough(t *testing.T) {
	sse := "data: {\"type\":\"image_generation.partial_succeeded\",\"model\":\"" + imageUpstreamID +
		"\",\"image_index\":0,\"url\":\"https://ark-cdn.example.com/tos/img-1.png\",\"size\":\"2048x2048\"}\n\n" +
		"data: {\"type\":\"image_generation.completed\",\"model\":\"" + imageUpstreamID +
		"\",\"usage\":{\"generated_images\":1,\"output_tokens\":16464,\"total_tokens\":16464}}\n\n"
	e, _ := newImageEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse)
	})

	w := do(e.h, "POST", "/ark/api/v3/images/generations", chatAuth,
		fmt.Sprintf(`{"model":%q,"prompt":"p","stream":true}`, imageModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, imageUpstreamID) {
		t.Errorf("SSE 流泄露了来源侧模型 ID: %s", body)
	}
	if got := strings.Count(body, fmt.Sprintf("%q:%q", "model", imageModel)); got != 2 {
		t.Errorf("SSE 流 model 回写次数 = %d，期望 2；body: %s", got, body)
	}
	for _, keep := range []string{"image_generation.partial_succeeded", "image_generation.completed",
		"https://ark-cdn.example.com/tos/img-1.png", "\"generated_images\":1"} {
		if !strings.Contains(body, keep) {
			t.Errorf("SSE 流缺少原样字段 %q: %s", keep, body)
		}
	}
}

// repeatReader 产出 n 个重复字节：超大请求体流式构造，不在内存攒一个 80 MiB
// 的字符串。
type repeatReader struct {
	b byte
	n int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = r.b
	}
	r.n -= int64(len(p))
	return len(p), nil
}

// TestImagesBodyLimit：图片入口与视频入口同一 80 MiB 上限（超限 413，不打
// 厂商）。上调 videoSubmitBodyLimit 时同步这里。
func TestImagesBodyLimit(t *testing.T) {
	e, stub := newImageEnv(t, jsonReply(http.StatusOK, imageOKBody))
	const limit = 80 << 20
	body := io.MultiReader(
		strings.NewReader(fmt.Sprintf(`{"model":%q,"prompt":"`, imageModel)),
		&repeatReader{b: 'a', n: limit},
		strings.NewReader(`"}`),
	)
	r := httptest.NewRequest("POST", "/ark/api/v3/images/generations", body)
	for k, v := range chatAuth {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413；body: %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "request_too_large" {
		t.Errorf("error.code = %q，期望 request_too_large", code)
	}
	if stub.count() != 0 {
		t.Errorf("超限请求打到了上游 %d 次", stub.count())
	}
}
