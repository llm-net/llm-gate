// agents_faces_test.go 钉住开发工具接入面 /agents/<tool>/ 的三件事：正式路径接到
// 各自的处理器且 provider 由路径钉死；子树闸 withDevTool 按这把 Key 的开发工具
// 策略放行或 403 devtool_not_allowed（错误体风格随路径）；兼容别名接同一组处理器、
// 过同一道闸。
package gateway_test

import (
	"net/http"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// overrideDevToolConfig 覆写唯一测试 Key 的开发工具策略（routeEnv 缺省四个订阅全勾、
// 没有目录模型；mutate 从一份全空策略起改）。
func overrideDevToolConfig(t *testing.T, st *store.Store, mutate func(*store.DevToolConfig)) {
	t.Helper()
	keys, err := st.ListAPIKeys(t.Context())
	if err != nil || len(keys) == 0 {
		t.Fatalf("ListAPIKeys: %v (%d)", err, len(keys))
	}
	cfg := store.DevToolConfig{KeyID: keys[0].ID}
	mutate(&cfg)
	if _, _, err := st.ReplaceDevToolConfig(t.Context(), cfg); err != nil {
		t.Fatalf("ReplaceDevToolConfig: %v", err)
	}
}

func TestAgentFacesRouteByPath(t *testing.T) {
	t.Run("codex 正式面转到 Codex 订阅后端", func(t *testing.T) {
		e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
		w := do(e.h, http.MethodPost, "/agents/codex/v1/responses", codexAuth, codexReq(false))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		if e.backend.count() != 1 {
			t.Fatalf("订阅后端调用次数 = %d，期望 1", e.backend.count())
		}
		if got := bodyModel(t, w); got != codexModel {
			t.Errorf("响应 model = %q，期望回写为 %q", got, codexModel)
		}
	})
	t.Run("grok 正式面 provider 由路径钉死", func(t *testing.T) {
		e := newGrokEnv(t, jsonReply(http.StatusOK, grokNonStreamBody), issuerNever(t))
		w := do(e.h, http.MethodPost, "/agents/grok/v1/responses", grokClientHeaders, grokReq(grokModel))
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；body: %s", w.Code, w.Body.String())
		}
		// 名字不以 grok 开头也归这份 Grok 订阅——正式面不猜前缀（别名才猜）。
		w = do(e.h, http.MethodPost, "/agents/grok/v1/responses", grokClientHeaders, grokReq(codexModel))
		if w.Code != http.StatusOK || e.backend.count() != 2 {
			t.Fatalf("非 grok 前缀的模型名：状态码 = %d，后端调用 = %d；body: %s", w.Code, e.backend.count(), w.Body.String())
		}
	})
	t.Run("子树兜底先鉴权再 404", func(t *testing.T) {
		e := newRouteEnv(t)
		for _, path := range []string{"/agents/codex/v1/other", "/agents/grok/v1/models", "/agents/codex/", "/agents/grok/"} {
			if w := do(e.h, http.MethodGet, path, nil, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("%s 无凭证状态码 = %d，期望 401", path, w.Code)
			}
			w := do(e.h, http.MethodGet, path, chatAuth, "")
			if w.Code != http.StatusNotFound {
				t.Errorf("%s 状态码 = %d，期望 404", path, w.Code)
				continue
			}
			if _, code, _ := decodeError(t, w); code == "" {
				t.Errorf("%s 应是 OpenAI 风格错误体: %s", path, w.Body.String())
			}
		}
	})
}

func TestAgentFacesGateByKeyPolicy(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	// 四个订阅都不勾、也没有目录模型：整棵子树 403 devtool_not_allowed，错误体
	// 风格随路径；别名过同一道闸。
	overrideDevToolConfig(t, e.st, func(*store.DevToolConfig) {})
	for _, tc := range []struct {
		method, path string
		anthropic    bool
	}{
		{http.MethodPost, "/agents/codex/v1/responses", false},
		{http.MethodGet, "/agents/codex/v1/models", false},
		{http.MethodGet, "/agents/codex/v1/model-catalog", false},
		{http.MethodPost, "/agents/grok/v1/responses", false},
		{http.MethodPost, "/agents/claude/v1/messages", true},
		{http.MethodGet, "/agents/claude/v1/models", true},
		{http.MethodHead, "/agents/claude/api/hello", true},
		{http.MethodPost, "/codex/v1/responses", false},
		{http.MethodGet, "/codex/v1/models", false},
		{http.MethodPost, "/claude/v1/messages", true},
		{http.MethodHead, "/claude/api/hello", true},
	} {
		body := ""
		if tc.method == http.MethodPost {
			body = codexReq(false)
		}
		w := do(e.h, tc.method, tc.path, codexAuth, body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s 状态码 = %d，期望 403；body: %s", tc.method, tc.path, w.Code, w.Body.String())
			continue
		}
		switch {
		case tc.method == http.MethodHead:
			// HEAD 没有响应体，只看状态码。
		case tc.anthropic:
			if got := decodeAnthropicType(t, w); got != "permission_error" {
				t.Errorf("%s Anthropic 错误类型 = %q，期望 permission_error", tc.path, got)
			}
		default:
			if _, code, _ := decodeError(t, w); code != "devtool_not_allowed" {
				t.Errorf("%s 错误 code = %q，期望 devtool_not_allowed", tc.path, code)
			}
		}
	}
	if e.backend.count() != 0 {
		t.Fatalf("被闸门拦下的请求不该出网，后端调用 = %d", e.backend.count())
	}

	// 只勾目录模型、不勾订阅：codex 子树开放——目录模型可用，订阅模型 403
	// subscription_not_allowed；grok 没有目录投影，子树仍然关着。
	stub := newStub(t, chatJSONReply(`{"id":"chatcmpl-gate","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	upstreamID := dbUpstream(t, e.st, "deepseek", config.UpstreamDeepseek, "sk-not-real", stub.url)
	modelID := dbModel(t, e.st, "deepseek-v4-flash")
	dbSource(t, e.st, modelID, upstreamID, "deepseek-v4-flash", 100)
	overrideDevToolConfig(t, e.st, func(c *store.DevToolConfig) { c.CatalogModelIDs = []int64{modelID} })

	got := decodeModelList(t, do(e.h, http.MethodGet, "/agents/codex/v1/models", chatAuth, ""))
	if len(got) != 1 || got[0].ID != "deepseek-v4-flash" || got[0].OwnedBy != "llmgate" {
		t.Fatalf("只勾目录模型时的 codex 目录 = %+v", got)
	}
	w := do(e.h, http.MethodPost, "/agents/codex/v1/responses", chatAuth,
		`{"model":"deepseek-v4-flash","input":"hi","store":false}`)
	if w.Code != http.StatusOK || stub.count() != 1 {
		t.Fatalf("目录模型：状态码 = %d，目录上游调用 = %d；body: %s", w.Code, stub.count(), w.Body.String())
	}
	w = do(e.h, http.MethodPost, "/agents/codex/v1/responses", codexAuth, codexReq(false))
	if w.Code != http.StatusForbidden {
		t.Fatalf("未勾订阅时订阅模型状态码 = %d，期望 403；body: %s", w.Code, w.Body.String())
	}
	if _, code, _ := decodeError(t, w); code != "subscription_not_allowed" {
		t.Fatalf("错误 code = %q，期望 subscription_not_allowed", code)
	}
	if e.backend.count() != 0 {
		t.Fatalf("未勾订阅的请求不该出网，后端调用 = %d", e.backend.count())
	}
	w = do(e.h, http.MethodPost, "/agents/grok/v1/responses", codexAuth, grokReq(grokModel))
	if w.Code != http.StatusForbidden {
		t.Fatalf("grok 子树状态码 = %d，期望 403", w.Code)
	}
	if _, code, _ := decodeError(t, w); code != "devtool_not_allowed" {
		t.Fatalf("grok 子树错误 code = %q，期望 devtool_not_allowed", code)
	}
}

func TestAgentFacesAliasesMatchCanonical(t *testing.T) {
	e := newAgentEnv(t, jsonReply(http.StatusOK, codexNonStreamBody))
	canonical := do(e.h, http.MethodGet, "/agents/codex/v1/models", chatAuth, "")
	alias := do(e.h, http.MethodGet, "/codex/v1/models", chatAuth, "")
	if canonical.Code != http.StatusOK || alias.Code != http.StatusOK || alias.Body.String() != canonical.Body.String() {
		t.Fatalf("别名与正式面读数不一致：%d %s / %d %s", canonical.Code, canonical.Body.String(), alias.Code, alias.Body.String())
	}
	for _, path := range []string{"/agents/codex/v1/responses", "/codex/v1/responses", "/agents/v1/responses"} {
		w := do(e.h, http.MethodPost, path, codexAuth, codexReq(false))
		if w.Code != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200；body: %s", path, w.Code, w.Body.String())
		}
	}
	if e.backend.count() != 3 {
		t.Fatalf("三条路径都该到达订阅后端，调用 = %d", e.backend.count())
	}
	connectClaudeCredential(t, e.routeEnv, claudeOAuthFake)
	for _, path := range []string{"/agents/claude/api/hello", "/claude/api/hello"} {
		w := serveClaude(e.routeEnv, http.MethodHead, path, claudeHeaders(), "", true)
		if w.Code != http.StatusNoContent {
			t.Errorf("%s 状态码 = %d，期望 204", path, w.Code)
		}
	}
}
