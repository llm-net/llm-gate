// upstreams_opencode_test.go 是 opencode_go（OpenCode Go 包月套餐，内置端点类型）
// 在管理面的可执行验收，复用 server_test.go / catalog_test.go 的装置：建行须
// api_key 且**不得**带 base_url（特化产品上游，端点固定取自内置端点表——
// openai_compat 的自由地址例外不外溢到它）；来源 protocols 按型号声明收窄；
// 平台余额恒不支持（套餐额度在 OpenCode Zen 控制台）；目录选单只收套餐清单里的
// 文本型号，自定义照走。
package admin_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestOpenCodeGoCreateValidation：建行校验、三协议面 protocols、不支持余额查询。
func TestOpenCodeGoCreateValidation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	resp := e.do("POST", "/admin/v1/upstreams", root, `{"name":"ocg","type":"opencode_go"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "api_key_required" {
		t.Errorf("缺 api_key 错误码 = %q", code)
	}

	resp = e.do("POST", "/admin/v1/upstreams", root,
		fmt.Sprintf(`{"name":"ocg","type":"opencode_go","api_key":"%s","base_url":"http://evil.example.com/v1"}`,
			upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_not_allowed" {
		t.Errorf("带 base_url 建行错误码 = %q", code)
	}

	ocg := e.createUpstream(root, fmt.Sprintf(`{"name":"ocg","type":"opencode_go","api_key":"%s"}`, upstreamKeyPlaintext))
	if ocg.Type != "opencode_go" || ocg.BaseURL != "" ||
		ocg.APIKeyLast4 != upstreamKeyPlaintext[len(upstreamKeyPlaintext)-4:] {
		t.Errorf("opencode_go 行异常: %+v", ocg)
	}
	if ocg.BillingMode != "subscription" {
		t.Errorf("opencode_go 的 billing_mode = %q，期望 subscription（套餐先用满）", ocg.BillingMode)
	}
	if ocg.BalanceSupported {
		t.Error("opencode_go 不应支持余额查询（套餐额度在 Zen 控制台，Key 够不着）")
	}

	// 文本模型下的来源：chat、responses（经 Chat 承载）与 messages 三个协议面。
	m := e.createModel(root, "ocg-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, ocg.ID))
	if len(src.Protocols) != 3 || src.Protocols[0] != "openai_chat" ||
		src.Protocols[1] != "openai_responses" || src.Protocols[2] != "anthropic_messages" {
		t.Errorf("opencode_go 来源 protocols = %v，期望 [openai_chat openai_responses anthropic_messages]", src.Protocols)
	}

	// 建后改址照旧被拒：内置端点表的类型不接受任何 base_url。
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", ocg.ID), root,
		fmt.Sprintf(`{"base_url":"http://evil.example.com/v1","api_key":"%s"}`, upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_not_allowed" {
		t.Errorf("opencode_go 改址错误码 = %q", code)
	}

	// 余额查询对不支持的类型是 400 balance_not_supported（不去猜端点、不发请求）。
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", ocg.ID), root, "")
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "balance_not_supported" {
		t.Errorf("opencode_go 余额错误码 = %q", code)
	}
}

// TestUpstreamModelsOpenCodeGoCatalog：Go 套餐清单里的文本型号形成选单，型号名是
// 套餐接口 GET /models 返回的裸名字（不带 opencode-go/ 前缀）；选单不是白名单，
// 套餐上新但数据目录尚未收录时，自定义仍然照走；订阅型缺省优先级 100。
func TestUpstreamModelsOpenCodeGoCatalog(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(`{"name":"ocg","type":"opencode_go","api_key":"%s"}`, upstreamKeyPlaintext))

	got := e.upstreamModels(root, up.ID)
	if len(got.Models) == 0 {
		t.Fatal("opencode_go 选单为空")
	}
	mustHave := map[string]bool{"kimi-k3": false, "glm-5.3": false, "deepseek-v4-pro": false, "qwen3.8-max": false, "minimax-m3": false}
	for _, m := range got.Models {
		if m.Name == "gpt-5.6-luna" || m.Name == "grok-4.6" || m.Name == "grok-4.5" {
			t.Errorf("仅原生 Responses 的型号不应作为可添加选项: %s", m.Name)
		}
		if m.Kind != "text" || m.Family != "" {
			t.Errorf("opencode_go 选单含非文本条目：%+v", m)
		}
		if len(m.Name) > 12 && m.Name[:12] == "opencode-go/" {
			t.Errorf("opencode_go 选单型号 %q 带了 CLI 的 provider 前缀，请求体里应是裸名字", m.Name)
		}
		if _, ok := mustHave[m.Name]; ok {
			mustHave[m.Name] = true
		}
	}
	for name, seen := range mustHave {
		if !seen {
			t.Errorf("opencode_go 选单缺少套餐清单里的型号 %q", name)
		}
	}
	if got.Priority != 100 {
		t.Errorf("缺省优先级 = %d，期望 100", got.Priority)
	}
	added := e.addUpstreamModels(root, up.ID, `{"models":[{"name":"ocg-custom-model","kind":"text"}]}`)
	if added.Created != 1 || added.Added != 1 {
		t.Fatalf("created=%d added=%d，期望 1/1", added.Created, added.Added)
	}
	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/models", up.ID), root,
		`{"models":[{"name":"blocked-alias","kind":"text","upstream_model_id":"gpt-5.6-luna"}]}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "source_protocol_unservable" {
		t.Fatalf("不可承载型号的错误码: %s", code)
	}
}
