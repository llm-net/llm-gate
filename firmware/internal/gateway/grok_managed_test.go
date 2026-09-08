// grok_managed_test.go：GET /grok-helper/managed-config（受管区渲染面）——
// 全量渲染（目录透传、过滤、密钥回填、头注入）、model 参数覆盖、401 刷新
// 重试、四类降级（未连接/停用/上游 5xx/体不成形）与降级字节一致性。
// 夹具全部是假凭据；CCP 由本机 httptest 扮演。
package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/grokhelper"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// managedPath 是受管区渲染面的路径；testHost 是 httptest.NewRequest 的缺省
// Host——渲染出的 base_url 必须指回请求到达的地址。
const (
	managedPath     = "/grok-helper/managed-config"
	managedTestBase = "http://example.com"
)

// grokCatalogBody 是假 CCP 目录：两行真模型（形态按 2026-08-13 真机转储裁剪）
// + 一行非 grok 前缀（必须被过滤）+ 一行 hidden（必须被过滤）。
const grokCatalogBody = `{"object":"list","data":[
  {"id":"grok-4.6","object":"model","owned_by":"xAI","model":"grok-4.6","name":"Grok 4.6",
   "description":"SpaceXAI's latest frontier model","context_window":500000,
   "auto_compact_threshold_percent":80,"system_prompt_label":"Grok 4.6",
   "api_backend":"responses","reasoning_effort":"high","supports_reasoning_effort":true,
   "reasoning_efforts":[
     {"id":"xhigh","value":"xhigh","label":"Extra High Effort","description":"Highest effort and reasoning level","default":true},
     {"id":"low","value":"low","label":"Low Effort","default":false}],
   "supports_backend_search":true,
   "base_url":"https://cli-chat-proxy.grok.com/v1","api_key":"leak-me-not"},
  {"id":"grok-4.5","object":"model","model":"grok-4.5","name":"Grok 4.5","context_window":500000},
  {"id":"vision-x","object":"model","model":"vision-x","name":"Not A Grok"},
  {"id":"grok-secret","object":"model","model":"grok-secret","hidden":true}
]}`

// withGrokCatalog 给 grokEnv 接上一个假 CCP 目录端点（GET /v1/models）。
func withGrokCatalog(t *testing.T, e *grokEnv, respond http.HandlerFunc) *stubUpstream {
	t.Helper()
	stub := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("打到了意料之外的目录路径 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		respond(w, r)
	})
	e.srv.SetGrokModelsEndpoint(stub.url + "/models")
	return stub
}

// managedHeaders 是安装脚本形态的请求头（含一个客户端伪造的令牌标记头，
// 不许跟到 CCP）。
var managedHeaders = map[string]string{
	"Authorization":    "Bearer " + testKey,
	"X-XAI-Token-Auth": "fake-client-forged-value",
}

func TestManagedConfigFullRegion(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
	catalog := withGrokCatalog(t, e, jsonReply(http.StatusOK, grokCatalogBody))

	w := do(e.h, "GET", managedPath, managedHeaders, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	body := w.Body.String()
	for _, want := range []string{
		grokhelper.RegionBegin + "\n",
		grokhelper.RegionEnd + "\n",
		"model = \"" + grokModel + "\"\n", // 默认条目吃管理员设的 default_model
		"[model.\"llmgate-grok-4.6\"]\n",
		"name = \"LLM Gate · Grok 4.6\"\n",
		"[model.\"llmgate-grok-4.5\"]\n",
		"base_url = \"" + managedTestBase + "/agents/grok/v1\"\n",
		"api_key = \"" + testKey + "\"\n", // 属主自己的密钥回填
		"context_window = 500000\n",
		"auto_compact_threshold_percent = 80\n",
		"system_prompt_label = \"Grok 4.6\"\n",
		"supports_backend_search = true\n",
		`{ id = "xhigh", value = "xhigh", label = "Extra High Effort", description = "Highest effort and reasoning level", default = true },`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("受管区缺少 %q：\n%s", want, body)
		}
	}
	for _, ban := range []string{
		"vision-x",                     // 非 grok 前缀过滤
		"grok-secret",                  // hidden 过滤
		"leak-me-not",                  // 上游 api_key 绝不落区
		"cli-chat-proxy.grok.com",      // 上游 base_url 换盒子的
		"supports_reasoning_effort = ", // 效力菜单在场就不写 legacy 标量
		"reasoning_effort = ",
	} {
		if strings.Contains(body, ban) {
			t.Errorf("受管区不该含 %q：\n%s", ban, body)
		}
	}
	// 4.5 那行目录元数据只有 context_window：确认最小条目也成形。
	if !strings.Contains(body, "name = \"LLM Gate · Grok 4.5\"\n") {
		t.Errorf("缺 4.5 目录条目：\n%s", body)
	}
	// 不带模型名的别名条目已取消；管理员设的 default_model（grok-4.5）那条
	// 恒排在最前，安装脚本据此推 `grok -m llmgate-grok-4.5`。
	if strings.Contains(body, "[model.llmgate]") {
		t.Errorf("受管区不该再有 [model.llmgate] 别名条目：\n%s", body)
	}
	if idx, other := strings.Index(body, `[model."llmgate-`+grokModel+`"]`),
		strings.Index(body, `[model."llmgate-grok-4.6"]`); idx < 0 || idx > other {
		t.Errorf("default_model 那条未排在最前（idx=%d, 4.6=%d）：\n%s", idx, other, body)
	}

	// CCP 侧：订阅令牌 + 设备的令牌标记头（客户端伪造值不得跟去），零刷新。
	if catalog.count() != 1 {
		t.Fatalf("目录上游收到 %d 次请求，期望 1", catalog.count())
	}
	if got := catalog.sentAuth(0); got != "Bearer "+grokAccess1 {
		t.Errorf("目录请求 Authorization = %q，期望注入订阅令牌", got)
	}
	if got := catalog.sentHeaderValues(0, grokTokenAuthName); len(got) != 1 || got[0] != "xai-grok-cli" {
		t.Errorf("目录请求 %s = %v，期望恰好一个设备值", grokTokenAuthName, got)
	}
	if e.issuer.count() != 0 {
		t.Errorf("happy path 不该刷新，实际 %d 次", e.issuer.count())
	}
}

func TestManagedConfigForwardedProto(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
	withGrokCatalog(t, e, jsonReply(http.StatusOK, grokCatalogBody))

	hdr := map[string]string{}
	for k, v := range managedHeaders {
		hdr[k] = v
	}
	hdr["X-Forwarded-Proto"] = "https"
	w := do(e.h, "GET", managedPath, hdr, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `base_url = "https://example.com/agents/grok/v1"`) {
		t.Fatalf("X-Forwarded-Proto=https 时应写 https base_url：\n%s", w.Body.String())
	}

	hdr["X-Forwarded-Proto"] = "ftp"
	w = do(e.h, "GET", managedPath, hdr, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `base_url = "http://example.com/agents/grok/v1"`) {
		t.Fatalf("非法 X-Forwarded-Proto 应忽略：\n%s", w.Body.String())
	}
}

func TestManagedConfigModelParam(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
	withGrokCatalog(t, e, jsonReply(http.StatusOK, grokCatalogBody))

	// 显式 model 覆盖 default_model，且正好在目录里 → 默认条目补齐元数据。
	w := do(e.h, "GET", managedPath+"?model=grok-4.6", managedHeaders, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// 默认条目段 = 第一个条目表头到下一个条目表头之间。
	firstHdr := strings.Index(body, "[model.\"")
	defaultEntry := body[firstHdr:]
	if next := strings.Index(defaultEntry[1:], "\n[model.\""); next >= 0 {
		defaultEntry = defaultEntry[:next+1]
	}
	for _, want := range []string{"model = \"grok-4.6\"\n", "context_window = 500000\n"} {
		if !strings.Contains(defaultEntry, want) {
			t.Errorf("默认条目缺 %q：\n%s", want, defaultEntry)
		}
	}

	// 非法字符 → 400，一个目录请求都不发。
	w = do(e.h, "GET", managedPath+"?model=bad%3Bmodel", managedHeaders, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 model 参数状态码 = %d，期望 400", w.Code)
	}
}

func TestManagedConfigRefreshOn401(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerRotates(grokAccess2, grokRefresh2))
	first := true
	catalog := withGrokCatalog(t, e, func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"expired"}`))
			return
		}
		jsonReply(http.StatusOK, grokCatalogBody)(w, r)
	})

	w := do(e.h, "GET", managedPath, managedHeaders, "")
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "[model.\"llmgate-grok-4.6\"]") {
		t.Errorf("刷新重试后仍未取到目录：\n%s", w.Body.String())
	}
	if catalog.count() != 2 {
		t.Errorf("目录上游收到 %d 次请求，期望 401 后重试共 2", catalog.count())
	}
	if e.issuer.count() != 1 {
		t.Errorf("期望恰好一次刷新，实际 %d", e.issuer.count())
	}
	if got := catalog.sentAuth(1); got != "Bearer "+grokAccess2 {
		t.Errorf("重试请求 Authorization = %q，期望新一代令牌", got)
	}
}

// TestManagedConfigDegraded：订阅不可用时直接拒绝；已授权且在线时，目录故障
// 仍返回仅含默认条目的可用配置。
func TestManagedConfigDegraded(t *testing.T) {
	minimal := func(defModel string) string {
		return grokhelper.RenderManagedRegion(managedTestBase, testKey, defModel, nil)
	}

	t.Run("未连接订阅", func(t *testing.T) {
		e := newRouteEnv(t) // 没有 agent_accounts 行
		w := do(e.h, "GET", managedPath, managedHeaders, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("状态码 = %d，期望 409；body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("订阅已停用", func(t *testing.T) {
		e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
		catalog := withGrokCatalog(t, e, jsonReply(http.StatusOK, grokCatalogBody))
		if err := e.st.SetAgentStatus(t.Context(), e.acctID, store.AgentStatusDisabled); err != nil {
			t.Fatalf("SetAgentStatus: %v", err)
		}
		w := do(e.h, "GET", managedPath, managedHeaders, "")
		if w.Code != http.StatusConflict {
			t.Fatalf("状态码 = %d，期望 409；body: %s", w.Code, w.Body.String())
		}
		if catalog.count() != 0 {
			t.Errorf("停用状态不该打目录上游，实际 %d 次", catalog.count())
		}
	})

	t.Run("目录上游5xx", func(t *testing.T) {
		e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
		withGrokCatalog(t, e, jsonReply(http.StatusInternalServerError, `{"error":"boom"}`))
		w := do(e.h, "GET", managedPath, managedHeaders, "")
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
		}
		if got := w.Body.String(); got != minimal(grokModel) {
			t.Errorf("降级区与渲染器输出不一致：\n got: %s\nwant: %s", got, minimal(grokModel))
		}
	})

	t.Run("目录体不成形", func(t *testing.T) {
		e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
		withGrokCatalog(t, e, jsonReply(http.StatusOK, `not json at all`))
		w := do(e.h, "GET", managedPath, managedHeaders, "")
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d；body: %s", w.Code, w.Body.String())
		}
		if got := w.Body.String(); got != minimal(grokModel) {
			t.Errorf("降级区与渲染器输出不一致：\n got: %s\nwant: %s", got, minimal(grokModel))
		}
	})
}

// TestManagedConfigRequiresAuth：无凭证 401——这是安装脚本「密钥被拒即中止」
// 语义的服务器半边。
func TestManagedConfigRequiresAuth(t *testing.T) {
	e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
	withGrokCatalog(t, e, jsonReply(http.StatusOK, grokCatalogBody))
	w := do(e.h, "GET", managedPath, nil, "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("无凭证状态码 = %d，期望 401", w.Code)
	}
	w = do(e.h, "GET", managedPath, map[string]string{"Authorization": "Bearer sk_wrong"}, "")
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
		t.Errorf("坏密钥状态码 = %d，期望 401/403", w.Code)
	}
}
