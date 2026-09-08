// upstreams_qwen_test.go 是 qwen_plan（阿里云百炼 通义千问 Token Plan，
// 2026-08-09 新增的内置端点类型）在管理面的可执行验收，复用 server_test.go /
// catalog_test.go 的装置：建行须 api_key 且**不得**带 base_url（特化产品上游，
// 端点固定取自内置端点表——openai_compat 的自由地址例外不外溢到它）；挂来源
// 的 protocols 是文本双入口；平台余额恒不支持（套餐余量在阿里云账号层）。
package admin_test

import (
	"fmt"
	"net/http"
	"testing"
)

// TestQwenPlanCreateValidation：建行校验、双入口 protocols、不支持余额查询。
func TestQwenPlanCreateValidation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	resp := e.do("POST", "/admin/v1/upstreams", root, `{"name":"qp","type":"qwen_plan"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "api_key_required" {
		t.Errorf("缺 api_key 错误码 = %q", code)
	}

	resp = e.do("POST", "/admin/v1/upstreams", root,
		fmt.Sprintf(`{"name":"qp","type":"qwen_plan","api_key":"%s","base_url":"http://evil.example.com/v1"}`,
			upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_not_allowed" {
		t.Errorf("带 base_url 建行错误码 = %q", code)
	}

	qp := e.createUpstream(root, fmt.Sprintf(`{"name":"qp","type":"qwen_plan","api_key":"%s"}`, upstreamKeyPlaintext))
	if qp.Type != "qwen_plan" || qp.BaseURL != "" ||
		qp.APIKeyLast4 != upstreamKeyPlaintext[len(upstreamKeyPlaintext)-4:] {
		t.Errorf("qwen_plan 行异常: %+v", qp)
	}
	if qp.BalanceSupported {
		t.Error("qwen_plan 不应支持余额查询（套餐余量在阿里云账号层，Key 够不着）")
	}

	// 文本模型下的来源：chat 与 messages 双入口（端点根不同段，但都在表里）。
	m := e.createModel(root, "qwen-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, qp.ID))
	if len(src.Protocols) != 3 || src.Protocols[0] != "openai_chat" ||
		src.Protocols[1] != "openai_responses" || src.Protocols[2] != "anthropic_messages" {
		t.Errorf("qwen_plan 来源 protocols = %v，期望 [openai_chat openai_responses anthropic_messages]", src.Protocols)
	}

	// 建后改址照旧被拒：内置端点表的类型不接受任何 base_url。
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", qp.ID), root,
		fmt.Sprintf(`{"base_url":"http://evil.example.com/v1","api_key":"%s"}`, upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_not_allowed" {
		t.Errorf("qwen_plan 改址错误码 = %q", code)
	}

	// 余额查询对不支持的类型是 400 balance_not_supported（不去猜端点、不发请求）。
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", qp.ID), root, "")
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "balance_not_supported" {
		t.Errorf("qwen_plan 余额错误码 = %q", code)
	}
}
