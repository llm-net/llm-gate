package gateway_test

// API模型策略在数据面的可执行验收（管理端契约见 internal/admin/apimodels_test.go）：
//
//   - 缺省不限制：没配过策略的 Key 照旧调得到目录里的模型。
//   - restricted=1 时范围外的模型与「不存在」完全同响应（404 model_not_found，
//     Anthropic 入口是同状态的 not_found_error），且**一个上游都不打**。
//   - /v1/models 按同一份策略收窄：调不到的名字不出现在自己的清单里。
//   - 改策略下一个请求即时生效（无缓存层）。
//   - 订阅/开发工具面不受本策略约束：Claude 混合面按开发工具勾选照常分流到
//     目录模型，即使该模型不在 API模型范围内。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// restrictAPIModels 把 testKey 那把密钥的 API模型策略设成「只许这些」。
// 按摘要认 Key，不按「第一行」认——有的装配会另外签一把 Key 进来。
func restrictAPIModels(t *testing.T, st *store.Store, modelIDs ...int64) {
	t.Helper()
	if _, _, err := st.ReplaceKeyAPIModelConfig(t.Context(), store.KeyAPIModelConfig{
		KeyID: testKeyID(t, st), Restricted: true, ModelIDs: modelIDs,
	}); err != nil {
		t.Fatalf("ReplaceKeyAPIModelConfig: %v", err)
	}
}

func TestAPIModelScopeNarrowsTextEntriesAndModelList(t *testing.T) {
	e := newRouteEnv(t)
	allowedUp := newStub(t, chatReply("allowed"))
	deniedUp := newStub(t, chatReply("denied"))
	allowed := dbModel(t, e.st, "allowed-model")
	denied := dbModel(t, e.st, "denied-model")
	dbSource(t, e.st, allowed, dbUpstream(t, e.st, "a", config.UpstreamMock, "k1", allowedUp.url), "", 100)
	dbSource(t, e.st, denied, dbUpstream(t, e.st, "b", config.UpstreamMock, "k2", deniedUp.url), "", 100)

	chat := func(model string) *httptest.ResponseRecorder {
		return do(e.h, http.MethodPost, "/v1/chat/completions", chatAuth,
			`{"model":"`+model+`","messages":[]}`)
	}

	// 缺省不限制：两个都调得到。
	if w := chat("allowed-model"); w.Code != http.StatusOK {
		t.Fatalf("缺省 allowed=%d body=%s", w.Code, w.Body.String())
	}
	if w := chat("denied-model"); w.Code != http.StatusOK {
		t.Fatalf("缺省 denied=%d body=%s", w.Code, w.Body.String())
	}
	if names := listedModels(t, e.h); len(names) != 2 {
		t.Fatalf("缺省 /v1/models = %v，期望两个都在", names)
	}

	// 收窄后：范围外的模型 404，且上游一次都没被打。
	restrictAPIModels(t, e.st, allowed)
	before := deniedUp.count()
	w := chat("denied-model")
	if w.Code != http.StatusNotFound {
		t.Fatalf("范围外模型 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w.Body.Bytes()); code != "model_not_found" {
		t.Errorf("错误 code = %q，期望与「不存在」同响应 model_not_found", code)
	}
	if deniedUp.count() != before {
		t.Errorf("范围外模型不该打上游：count %d → %d", before, deniedUp.count())
	}
	if w := chat("allowed-model"); w.Code != http.StatusOK {
		t.Fatalf("范围内模型 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}

	// /v1/models 同一份策略收窄。
	if names := listedModels(t, e.h); len(names) != 1 || names[0] != "allowed-model" {
		t.Fatalf("/v1/models = %v，期望只剩 allowed-model", names)
	}

	// Anthropic 入口与 count_tokens 同闸门。
	w = do(e.h, http.MethodPost, "/v1/messages", messagesAuth,
		`{"model":"denied-model","max_tokens":1,"messages":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("messages 范围外 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}
	w = do(e.h, http.MethodPost, "/v1/messages/count_tokens", messagesAuth,
		`{"model":"denied-model","messages":[]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("count_tokens 范围外 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}

	// 改策略即时生效：放开之后同一个名字立刻调得通。
	restrictAPIModels(t, e.st, allowed, denied)
	if w := chat("denied-model"); w.Code != http.StatusOK {
		t.Fatalf("放开后 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
	// 空选择 = 一个都不能调。
	restrictAPIModels(t, e.st)
	if w := chat("allowed-model"); w.Code != http.StatusNotFound {
		t.Fatalf("空选择 = %d，期望 404", w.Code)
	}
	if names := listedModels(t, e.h); len(names) != 0 {
		t.Fatalf("空选择 /v1/models = %v，期望空清单", names)
	}
}

// AIGC 官方路径也是 API 面：范围外的视频模型同样 404，且厂商一次都不打。
func TestAPIModelScopeNarrowsVideoEntry(t *testing.T) {
	e := newVideoEnv(t)
	submit := `{"model":"` + videoModel + `","content":[{"type":"text","text":"小猫打哈欠"}],"resolution":"768P","duration":4}`

	restrictAPIModels(t, e.st) // 空选择：一个 API 模型都不许调
	if w := do(e.h, http.MethodPost, "/minimax/v2/video_generation", chatAuth, submit); w.Code != http.StatusNotFound {
		t.Fatalf("范围外视频模型 = %d，期望 404；body: %s", w.Code, w.Body.String())
	}
	if got := len(e.mm.sentPaths()); got != 0 {
		t.Fatalf("范围外模型不该打厂商：%d 次", got)
	}

	restrictAPIModels(t, e.st, devToolModelID(t, e.st, videoModel))
	if w := do(e.h, http.MethodPost, "/minimax/v2/video_generation", chatAuth, submit); w.Code != http.StatusOK {
		t.Fatalf("范围内视频模型 = %d，期望 200；body: %s", w.Code, w.Body.String())
	}
}

// 订阅/开发工具面不套用 API模型策略：Claude 混合面按开发工具勾选分流到目录
// 模型，即便该模型被 API模型策略排除在外。
func TestAPIModelScopeDoesNotNarrowClaudeMixedSurface(t *testing.T) {
	e := newRouteEnv(t)
	backend := newStub(t, jsonReply(http.StatusOK, `{"model":"deepseek-v4-flash","usage":{"input_tokens":1,"output_tokens":1}}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", backend.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 10)
	selectDevToolModels(t, e.st, modelID)
	// API模型策略把它排除在外：直连 API 入口从此 404。
	restrictAPIModels(t, e.st)
	if w := do(e.h, http.MethodPost, "/v1/chat/completions", chatAuth,
		`{"model":"deepseek-v4-flash","messages":[]}`); w.Code != http.StatusNotFound {
		t.Fatalf("直连入口 = %d，期望 404", w.Code)
	}

	models := serveClaude(e, http.MethodGet, "/agents/claude/v1/models", claudeHeaders(), "", true)
	var discovered struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(models.Body.Bytes(), &discovered); err != nil || len(discovered.Data) != 1 {
		t.Fatalf("catalog discovery=%s err=%v", models.Body.String(), err)
	}
	body, err := json.Marshal(map[string]any{
		"model": discovered.Data[0].ID, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := serveClaude(e, http.MethodPost, "/agents/claude/v1/messages", claudeHeaders(), string(body), true)
	if w.Code != http.StatusOK || backend.count() != 1 {
		t.Fatalf("混合面应不受 API模型策略约束：code=%d hits=%d body=%s", w.Code, backend.count(), w.Body.String())
	}
}

// listedModels 取 GET /v1/models 的名字集合。
func listedModels(t *testing.T, h http.Handler) []string {
	t.Helper()
	w := do(h, http.MethodGet, "/v1/models", chatAuth, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d body=%s", w.Code, w.Body.String())
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("/v1/models 不是 JSON: %v", err)
	}
	out := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		out = append(out, m.ID)
	}
	return out
}

// errorCode 取 OpenAI 风格错误体的 error.code。
func errorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("错误体不是 JSON: %v\n%s", err, raw)
	}
	return body.Error.Code
}
