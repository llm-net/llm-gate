// imagine_grok_test.go 钉住 Grok Build 订阅接入面的 Grok Imagine 门
// （imagine_grok.go）：鉴权替换的逐字节透传、401 换代重试、闸门与 404、
// 画图门按张记账（0 元真值 + 名义价旋钮只借 image 行）、视频提交记 1 次而
// 轮询不计量。夹具全部是假凭据。
package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/usage"
)

const (
	imagineImageModel = "grok-imagine-image-2.0"
	imagineVideoModel = "grok-imagine-video-1.5"
	imagineImageBody  = `{"data":[{"url":"https://imgen.x.ai/one.png"},{"url":"https://imgen.x.ai/two.png"}]}`
	imagineVideoBody  = `{"request_id":"req_1"}`
	imaginePollBody   = `{"status":"done","video":{"url":"https://videos.x.ai/req_1.mp4","duration":6}}`
)

// grokImagineEnv 装配「已连接的 Grok Build 订阅 + 记录方法与路径的假 xAI 宿主
// + 假签发方」。假宿主同时充当 Responses 后端与 Imagine 端点根（真地址两者
// 同宿主）。
type grokImagineEnv struct {
	*routeEnv
	backend *stubUpstream
	issuer  *grokStubIssuer
	mu      sync.Mutex
	calls   []string // "METHOD /path"
}

func (e *grokImagineEnv) call(i int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i >= len(e.calls) {
		return ""
	}
	return e.calls[i]
}

func newGrokImagineEnv(t *testing.T, respond, refresh http.HandlerFunc) *grokImagineEnv {
	t.Helper()
	e := &grokImagineEnv{routeEnv: newRouteEnv(t)}
	e.backend = newStub(t, func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.calls = append(e.calls, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		respond(w, r)
	})
	e.issuer = newGrokStubIssuer(t, refresh)
	e.srv.SetGrokEndpoints(e.issuer.url, e.backend.url+"/responses")
	e.srv.SetGrokImagineEndpoint(e.backend.url)
	if _, err := e.st.UpsertAgentAccount(t.Context(), store.NewAgentAccount{
		Provider:     store.AgentProviderGrok,
		Label:        "Grok 订阅",
		AccountID:    "admin@example.invalid",
		DefaultModel: grokModel,
		AuthJSON:     grokAuthJSONFixture(grokAccess1, grokRefresh1),
	}); err != nil {
		t.Fatalf("UpsertAgentAccount: %v", err)
	}
	return e
}

// imagineReply 按路径回放各门的成功体。
func imagineReply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/images/generations"), strings.HasSuffix(r.URL.Path, "/images/edits"):
		fmt.Fprint(w, imagineImageBody)
	case strings.HasSuffix(r.URL.Path, "/videos/generations"):
		fmt.Fprint(w, imagineVideoBody)
	case strings.Contains(r.URL.Path, "/videos/"):
		fmt.Fprint(w, imaginePollBody)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func imagineImageReq(model string) string {
	return fmt.Sprintf(`{"model":%q,"prompt":"a red square","n":2,"response_format":"url"}`, model)
}

// imaginePollHeaders 是不带 Content-Type 的轮询请求头（GET 无体）。
func imaginePollHeaders() map[string]string {
	h := map[string]string{}
	for k, v := range grokClientHeaders {
		if k != "Content-Type" {
			h[k] = v
		}
	}
	return h
}

// sameJSON 按语义比较两份 JSON（设备重序列化请求体，键序不保证）。
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("解析 %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("解析 %q: %v", b, err)
	}
	return reflect.DeepEqual(x, y)
}

func sampleCost(t *testing.T, s usage.Sample) int64 {
	t.Helper()
	p, err := usage.ParsePricing(s.Pricing)
	if err != nil {
		t.Fatalf("ParsePricing(%q): %v", s.Pricing, err)
	}
	return usage.Cost(p, usage.Measure{Kind: usage.EntryKind(s.Entry), Entry: s.Entry, TaskUsage: s.TaskUsage})
}

func TestGrokImagineImagesFullPath(t *testing.T) {
	e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	body := imagineImageReq(imagineImageModel)
	w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, body)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != imagineImageBody {
		t.Errorf("响应体未原样透传:\n%s", got)
	}
	if got := e.call(0); got != "POST /v1/images/generations" {
		t.Errorf("上游调用 = %q，期望 POST /v1/images/generations", got)
	}
	if got := e.backend.sentAuth(0); got != "Bearer "+grokAccess1 {
		t.Errorf("上游 Authorization = %q，期望订阅令牌", got)
	}
	if got := e.backend.sentHeaderValues(0, grokTokenAuthName); len(got) != 1 || got[0] != "xai-grok-cli" {
		t.Errorf("%s = %v，期望恰好一个设备侧的值", grokTokenAuthName, got)
	}
	if got := e.backend.sentHeaderValue(0, "x-grok-client-version"); got != "9.9.9-test" {
		t.Errorf("grok 身份头未原样过: %q", got)
	}
	if sent := e.backend.sentBody(0); !sameJSON(t, sent, body) {
		t.Errorf("请求体应语义等同透传（model 不改写）:\n%s", sent)
	}
	if strings.Contains(e.backend.sentBody(0), testKey) || strings.Contains(e.backend.sentAuth(0), testKey) {
		t.Errorf("客户端 sk_ Key 外传到了上游")
	}
	sample := fm.only(t)
	if sample.Entry != usage.EntryImagineImage || sample.ModelName != imagineImageModel {
		t.Fatalf("记账入口/模型 = %q/%q", sample.Entry, sample.ModelName)
	}
	if sample.TaskUsage.GeneratedImages != 2 || sample.Estimated || sample.Status != http.StatusOK {
		t.Fatalf("画图门样本形态不对: %+v", sample)
	}
	if got := sampleCost(t, sample); got != 0 {
		t.Fatalf("未定价时金额 = %d，期望 0（订阅无边际成本）", got)
	}
	if !strings.Contains(sample.UpstreamName, "Grok Build 订阅") {
		t.Errorf("上游维度 = %q，期望记在订阅账号头上", sample.UpstreamName)
	}
}

func TestGrokImagineEditsForwardReferenceImage(t *testing.T) {
	e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
	body := fmt.Sprintf(`{"model":%q,"prompt":"make it blue","image":{"type":"image_url","url":"https://example.invalid/in.png"}}`, imagineImageModel)
	w := do(e.h, http.MethodPost, "/agents/grok/v1/images/edits", grokClientHeaders, body)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if got := e.call(0); got != "POST /v1/images/edits" {
		t.Errorf("上游调用 = %q", got)
	}
	if sent := e.backend.sentBody(0); !sameJSON(t, sent, body) {
		t.Errorf("参考图对象应语义等同透传:\n%s", sent)
	}
}

func TestGrokImagineVideoSubmitAndUnmeteredPoll(t *testing.T) {
	e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
	fm := &fakeMeter{}
	e.srv.EnableMetering(fm)

	body := fmt.Sprintf(`{"model":%q,"prompt":"a cat","duration":6}`, imagineVideoModel)
	w := do(e.h, http.MethodPost, "/agents/grok/v1/videos/generations", grokClientHeaders, body)
	if w.Code != http.StatusOK || w.Body.String() != imagineVideoBody {
		t.Fatalf("提交：状态码 = %d，body: %s", w.Code, w.Body.String())
	}
	if got := e.call(0); got != "POST /v1/videos/generations" {
		t.Errorf("上游调用 = %q", got)
	}
	sample := fm.only(t)
	if sample.Entry != usage.EntryImagineVideo || sample.ModelName != imagineVideoModel || sample.Estimated {
		t.Fatalf("视频提交样本形态不对: %+v", sample)
	}
	if got := sampleCost(t, sample); got != 0 {
		t.Fatalf("视频提交金额 = %d，期望 0", got)
	}

	w = do(e.h, http.MethodGet, "/agents/grok/v1/videos/req_1", imaginePollHeaders(), "")
	if w.Code != http.StatusOK || w.Body.String() != imaginePollBody {
		t.Fatalf("轮询：状态码 = %d，body: %s", w.Code, w.Body.String())
	}
	if got := e.call(1); got != "GET /v1/videos/req_1" {
		t.Errorf("轮询上游调用 = %q，期望 GET /v1/videos/req_1", got)
	}
	if e.backend.sentBody(1) != "" || e.backend.sentHeaderValue(1, "Content-Type") != "" {
		t.Errorf("GET 轮询不该带体或 Content-Type")
	}
	if got := e.backend.sentAuth(1); got != "Bearer "+grokAccess1 {
		t.Errorf("轮询 Authorization = %q，期望订阅令牌", got)
	}
	if fm.count() != 1 {
		t.Fatalf("轮询后记账笔数 = %d，期望仍为 1（轮询不计量）", fm.count())
	}
}

func TestGrokImagineRefreshOn401(t *testing.T) {
	var backendCalls int
	var mu sync.Mutex
	e := newGrokImagineEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		backendCalls++
		n := backendCalls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_token"}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+grokAccess2 {
			t.Errorf("重试请求 Authorization = %q，期望新一代令牌", got)
		}
		imagineReply(w, r)
	}, issuerRotates(grokAccess2, grokRefresh2))

	w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, imagineImageReq(imagineImageModel))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 401 后刷新重试成功；body: %s", w.Code, w.Body.String())
	}
	if e.issuer.count() != 1 {
		t.Fatalf("刷新 %d 次，期望 1", e.issuer.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if backendCalls != 2 {
		t.Errorf("后端收到 %d 次请求，期望 2（401 + 重试）", backendCalls)
	}
}

func TestGrokImagineRelaysUpstreamErrorVerbatim(t *testing.T) {
	e := newGrokImagineEnv(t, jsonReply(http.StatusUnprocessableEntity, `{"error":"prompt rejected","code":"content_policy"}`), issuerNever(t))
	w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, imagineImageReq(imagineImageModel))
	if w.Code != http.StatusUnprocessableEntity || w.Body.String() != `{"error":"prompt rejected","code":"content_policy"}` {
		t.Fatalf("上游错误未原样转发：%d %s", w.Code, w.Body.String())
	}
}

func TestGrokImagineGatesAndNotFound(t *testing.T) {
	t.Run("未勾 grok 整棵子树 403", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		overrideDevToolConfig(t, e.st, func(*store.DevToolConfig) {})
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodPost, "/agents/grok/v1/images/generations", imagineImageReq(imagineImageModel)},
			{http.MethodPost, "/agents/grok/v1/images/edits", imagineImageReq(imagineImageModel)},
			{http.MethodPost, "/agents/grok/v1/videos/generations", imagineImageReq(imagineVideoModel)},
			{http.MethodGet, "/agents/grok/v1/videos/req_1", ""},
		} {
			w := do(e.h, tc.method, tc.path, grokClientHeaders, tc.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s 状态码 = %d，期望 403", tc.method, tc.path, w.Code)
				continue
			}
			if _, code, _ := decodeError(t, w); code != "devtool_not_allowed" {
				t.Errorf("%s 错误 code = %q，期望 devtool_not_allowed", tc.path, code)
			}
		}
		if e.backend.count() != 0 {
			t.Fatalf("被闸门拦下的请求不该出网，后端调用 = %d", e.backend.count())
		}
	})
	t.Run("勾了 grok 但设备没连订阅 409", func(t *testing.T) {
		e := newRouteEnv(t)
		w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, imagineImageReq(imagineImageModel))
		if w.Code != http.StatusConflict {
			t.Fatalf("状态码 = %d，期望 409；body: %s", w.Code, w.Body.String())
		}
		if _, code, _ := decodeError(t, w); code != "agent_not_configured" {
			t.Fatalf("错误 code = %q，期望 agent_not_configured", code)
		}
		w = do(e.h, http.MethodGet, "/agents/grok/v1/videos/req_1", grokClientHeaders, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("轮询状态码 = %d，期望 409", w.Code)
		}
	})
	t.Run("无 Key 401、未知子路径 404、缺 model 400", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		if w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", nil, imagineImageReq(imagineImageModel)); w.Code != http.StatusUnauthorized {
			t.Errorf("无 Key 状态码 = %d，期望 401", w.Code)
		}
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/agents/grok/v1/images/other"},
			{http.MethodGet, "/agents/grok/v1/videos/"},
			{http.MethodGet, "/agents/grok/v1/images/generations"},
		} {
			w := do(e.h, tc.method, tc.path, grokClientHeaders, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s 状态码 = %d，期望 404", tc.method, tc.path, w.Code)
			}
		}
		w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, `{"prompt":"no model"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("缺 model 状态码 = %d，期望 400", w.Code)
		}
		if _, code, _ := decodeError(t, w); code != "missing_model" {
			t.Fatalf("错误 code = %q，期望 missing_model", code)
		}
		if e.backend.count() != 0 {
			t.Fatalf("以上请求都不该出网，后端调用 = %d", e.backend.count())
		}
	})
}

func TestGrokImagineNominalPriceKnobOnlyBorrowsImageRows(t *testing.T) {
	t.Run("同名 image 行按张记名义金额", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		id := dbKindModel(t, e.st, imagineImageModel, store.ModelKindImage)
		if err := e.st.SetModelPricing(t.Context(), id, `{"ark_image_each":100000}`); err != nil {
			t.Fatalf("SetModelPricing: %v", err)
		}
		w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, imagineImageReq(imagineImageModel))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
		}
		sample := fm.only(t)
		if !sample.ModelKnown || sample.Pricing == "" {
			t.Fatalf("同名 image 行应被借价: %+v", sample)
		}
		if got := sampleCost(t, sample); got != 2*100000 {
			t.Fatalf("名义金额 = %d，期望 2 张 × 100000", got)
		}
	})
	t.Run("同名 text 行不借价", func(t *testing.T) {
		e := newGrokImagineEnv(t, imagineReply, issuerNever(t))
		fm := &fakeMeter{}
		e.srv.EnableMetering(fm)
		id := dbModel(t, e.st, imagineImageModel)
		if err := e.st.SetModelPricing(t.Context(), id, `{"in":1000000,"out":1000000}`); err != nil {
			t.Fatalf("SetModelPricing: %v", err)
		}
		w := do(e.h, http.MethodPost, "/agents/grok/v1/images/generations", grokClientHeaders, imagineImageReq(imagineImageModel))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
		}
		sample := fm.only(t)
		if !sample.ModelKnown || sample.Pricing != "" {
			t.Fatalf("同名 text 行只算目录里有名字、不借价: %+v", sample)
		}
	})
}
