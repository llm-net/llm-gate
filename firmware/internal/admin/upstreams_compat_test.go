// upstreams_compat_test.go 是 2026-08-09 上游账户扩展的可执行验收（复用
// server_test.go / catalog_test.go / sourcetest_test.go 的装置）：
//
//   - openai_compat 类型：建行须 api_key + base_url；改址（PATCH base_url 为
//     不同值）须同请求重录 Key（400 base_url_requires_key），成功后地址与新
//     Key 原子生效；清空地址 400 base_url_required；其余产品上游照旧
//     base_url_not_allowed。挂来源的 protocols 只有 openai_chat。
//   - 平台余额查询（POST /admin/v1/upstreams/{id}/balance）：deepseek 快乐
//     路径（金额进响应）、上游 4xx 摘要、不支持类型 400、凭证解不开 409、
//     未认证/缺 CSRF/member/不存在四界；金额与摘要绝不落日志与审计 detail，
//     审计只记 name/type/status；balance_supported 事实随列表带出。
package admin_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// compatUpstreamBody 是一条合法的 openai_compat 上游请求体。
func compatUpstreamBody(name, baseURL string) string {
	return fmt.Sprintf(`{"name":"%s","type":"openai_compat","api_key":"%s","base_url":"%s"}`,
		name, upstreamKeyPlaintext, baseURL)
}

// TestOpenAICompatCreateValidation：建行两样都要；建成后 balance_supported
// 恒 false（通用适配没有平台能力），deepseek 恒 true。
func TestOpenAICompatCreateValidation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	resp := e.do("POST", "/admin/v1/upstreams", root,
		`{"name":"oc","type":"openai_compat","base_url":"http://10.0.0.5:8000/v1"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "api_key_required" {
		t.Errorf("缺 api_key 错误码 = %q", code)
	}

	resp = e.do("POST", "/admin/v1/upstreams", root,
		fmt.Sprintf(`{"name":"oc","type":"openai_compat","api_key":"%s"}`, upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_required" {
		t.Errorf("缺 base_url 错误码 = %q", code)
	}

	oc := e.createUpstream(root, compatUpstreamBody("oc", "http://10.0.0.5:8000/v1"))
	if oc.Type != "openai_compat" || oc.BaseURL != "http://10.0.0.5:8000/v1" ||
		oc.APIKeyLast4 != upstreamKeyPlaintext[len(upstreamKeyPlaintext)-4:] {
		t.Errorf("openai_compat 行异常: %+v", oc)
	}
	if oc.BalanceSupported {
		t.Error("openai_compat 不应支持余额查询（通用适配没有平台能力）")
	}
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	if !ds.BalanceSupported {
		t.Error("deepseek 应带 balance_supported=true")
	}
	for _, u := range e.listUpstreams(root) {
		if u.Name == "oc" && u.BalanceSupported {
			t.Error("列表里的 openai_compat 不应带 balance_supported")
		}
		if u.Name == "ds" && !u.BalanceSupported {
			t.Error("列表里的 deepseek 应带 balance_supported")
		}
	}

	// 挂来源：protocols 只有 openai_chat——anthropic 入口不进候选。
	m := e.createModel(root, "compat-model")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, oc.ID))
	if len(src.Protocols) != 2 || src.Protocols[0] != "openai_chat" || src.Protocols[1] != "openai_responses" {
		t.Errorf("openai_compat 来源 protocols = %v，期望 [openai_chat openai_responses]", src.Protocols)
	}

	ac := e.createUpstream(root, fmt.Sprintf(
		`{"name":"ac","type":"anthropic_compat","api_key":"%s","base_url":"https://api.example.invalid/v1"}`,
		upstreamKeyPlaintext))
	if ac.Type != "anthropic_compat" || ac.CatalogID != "anthropic_compat" || ac.BillingMode != "none" {
		t.Fatalf("anthropic_compat 行异常: %+v", ac)
	}
	am := e.createModel(root, "anthropic-compatible-model")
	asrc := e.createSource(root, am.ID, fmt.Sprintf(`{"upstream_id":%d}`, ac.ID))
	if len(asrc.Protocols) != 1 || asrc.Protocols[0] != "anthropic_messages" {
		t.Errorf("anthropic_compat 来源 protocols = %v，期望 [anthropic_messages]", asrc.Protocols)
	}
}

// TestOpenAICompatRepointRequiresKey：改址必须同请求重录 Key；成功后地址与
// 新 Key 原子生效；清空地址被拒；同值回传不算改址；产品上游照旧不可写。
func TestOpenAICompatRepointRequiresKey(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	oc := e.createUpstream(root, compatUpstreamBody("oc", "http://10.0.0.5:8000/v1"))
	path := fmt.Sprintf("/admin/v1/upstreams/%d", oc.ID)

	// 改址不带 Key：400，且行未被改动。
	resp := e.do("PATCH", path, root, `{"base_url":"http://evil.example.com/v1"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_requires_key" {
		t.Errorf("改址不带 Key 错误码 = %q", code)
	}
	if got := e.listUpstreams(root)[0].BaseURL; got != "http://10.0.0.5:8000/v1" {
		t.Errorf("被拒的改址不应生效: %q", got)
	}

	// 清空地址：openai_compat 没有内置端点，空地址什么都服务不了。
	resp = e.do("PATCH", path, root, `{"base_url":""}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_required" {
		t.Errorf("清空地址错误码 = %q", code)
	}

	// 同值回传不算改址：不带 Key 也放行（幂等 PATCH 不该被迫重录凭证）。
	wantStatus(t, e.do("PATCH", path, root, `{"base_url":"http://10.0.0.5:8000/v1"}`), http.StatusOK)

	// 改址 + 新 Key：原子生效，last4 立即换成新 Key 的。
	const newKey = "sk-compat-rotated-7788"
	resp = e.do("PATCH", path, root,
		fmt.Sprintf(`{"base_url":"http://10.0.0.6:8000/v1","api_key":"%s"}`, newKey))
	wantStatus(t, resp, http.StatusOK)
	u := e.listUpstreams(root)[0]
	if u.BaseURL != "http://10.0.0.6:8000/v1" || u.APIKeyLast4 != "7788" {
		t.Errorf("改址换 Key 后行异常: %+v", u)
	}

	// 特化产品上游照旧不可写地址（openai_compat 的例外不外溢）。
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", ds.ID), root,
		fmt.Sprintf(`{"base_url":"http://evil.example.com/v1","api_key":"%s"}`, upstreamKeyPlaintext))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "base_url_not_allowed" {
		t.Errorf("deepseek 改址错误码 = %q", code)
	}
}

// 与真实读数刻意不同的显眼金额：日志/审计的泄漏断言按它们全文扫描。
const (
	balTotal    = "3141.59"
	balGranted  = "27.18"
	balToppedUp = "3114.41"
)

// newBalanceUpstream 起假余额端点并 store 直建 deepseek 上游指向它（与
// sourcetest 的 base_url 覆盖同一能力）；返回上游 id 与收到的 Authorization。
func newBalanceUpstream(t *testing.T, e *env, name string, status int, body string) (int64, *string) {
	t.Helper()
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user/balance" {
			t.Errorf("余额请求形状异常: %s %s", r.Method, r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	up, err := e.st.CreateUpstream(context.Background(), name, "deepseek", upstreamKeyPlaintext, srv.URL)
	if err != nil {
		t.Fatalf("store.CreateUpstream: %v", err)
	}
	return up.ID, &auth
}

type balanceAmountDTO struct {
	Currency string `json:"currency"`
	Total    string `json:"total"`
	Granted  string `json:"granted"`
	ToppedUp string `json:"topped_up"`
}

type balanceReportDTO struct {
	UpstreamID int64              `json:"upstream_id"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	OK         bool               `json:"ok"`
	Status     int                `json:"status"`
	LatencyMS  int64              `json:"latency_ms"`
	Message    string             `json:"message"`
	Available  bool               `json:"available"`
	Balances   []balanceAmountDTO `json:"balances"`
}

// TestUpstreamBalanceHappyPath：deepseek 上游的余额查询——金额进响应、凭证
// 以 Bearer 到达平台；金额与 Key 明文在日志与审计里全不出现，审计只记状态。
func TestUpstreamBalanceHappyPath(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	id, auth := newBalanceUpstream(t, e, "ds-bal", http.StatusOK, fmt.Sprintf(
		`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"%s","granted_balance":"%s","topped_up_balance":"%s"}]}`,
		balTotal, balGranted, balToppedUp))

	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", id), root, "")
	wantStatus(t, resp, http.StatusOK)
	var out balanceReportDTO
	decodeInto(t, resp, &out)
	if !out.OK || out.Status != http.StatusOK || !out.Available || out.Name != "ds-bal" || out.Type != "deepseek" {
		t.Fatalf("余额报告异常: %+v", out)
	}
	if len(out.Balances) != 1 {
		t.Fatalf("Balances 条数 = %d，期望 1", len(out.Balances))
	}
	b := out.Balances[0]
	if b.Currency != "CNY" || b.Total != balTotal || b.Granted != balGranted || b.ToppedUp != balToppedUp {
		t.Errorf("金额读数异常: %+v", b)
	}
	if *auth != "Bearer "+upstreamKeyPlaintext {
		t.Error("余额请求未注入上游 Key")
	}

	// 金额只进响应：日志与审计 detail 全文无金额、无 Key 明文。
	logs := e.buf.String()
	details := e.auditDetails("upstream.balance")
	for _, amount := range []string{balTotal, balGranted, balToppedUp, upstreamKeyPlaintext} {
		if strings.Contains(logs, amount) {
			t.Errorf("金额/凭证泄入日志: %q", amount)
		}
		for _, d := range details {
			if strings.Contains(d, amount) {
				t.Errorf("金额/凭证泄入审计 detail: %q", amount)
			}
		}
	}
	if len(details) != 1 || details[0] != "name=ds-bal type=deepseek status=200" {
		t.Errorf("审计 detail = %v，期望只记名称/类型/状态", details)
	}
}

// TestUpstreamBalanceUpstreamError：平台 401 时报告 ok=false + 摘要；摘要只进
// 响应，不落日志与审计；审计记 status=401。
func TestUpstreamBalanceUpstreamError(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	const vendorMsg = "Authentication Fails, Your api key is invalid"
	id, _ := newBalanceUpstream(t, e, "ds-err", http.StatusUnauthorized,
		`{"error":{"message":"`+vendorMsg+`","type":"authentication_error"}}`)

	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", id), root, "")
	wantStatus(t, resp, http.StatusOK)
	var out balanceReportDTO
	decodeInto(t, resp, &out)
	if out.OK || out.Status != http.StatusUnauthorized || !strings.Contains(out.Message, "api key is invalid") {
		t.Fatalf("失败报告异常: %+v", out)
	}
	if strings.Contains(e.buf.String(), vendorMsg) {
		t.Error("上游错误摘要泄入日志")
	}
	details := e.auditDetails("upstream.balance")
	if len(details) != 1 || details[0] != "name=ds-err type=deepseek status=401" {
		t.Errorf("审计 detail = %v", details)
	}
	for _, d := range details {
		if strings.Contains(d, vendorMsg) {
			t.Error("上游错误摘要泄入审计 detail")
		}
	}
}

// TestUpstreamBalanceUnsupportedAndUnreadable：不支持的类型 400（一个请求都
// 不发）；密文损坏 409 upstream_key_unreadable。
func TestUpstreamBalanceUnsupportedAndUnreadable(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	oc := e.createUpstream(root, compatUpstreamBody("oc", "http://10.0.0.5:8000/v1"))
	resp := e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", oc.ID), root, "")
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "balance_not_supported" {
		t.Errorf("openai_compat 余额错误码 = %q", code)
	}
	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", ark.ID), root, "")
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "balance_not_supported" {
		t.Errorf("ark 余额错误码 = %q", code)
	}

	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开库连接: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE upstreams SET api_key_sealed = 'AAAA' WHERE id = ?`, ds.ID); err != nil {
		t.Fatalf("损坏密文: %v", err)
	}
	resp = e.do("POST", fmt.Sprintf("/admin/v1/upstreams/%d/balance", ds.ID), root, "")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "upstream_key_unreadable" {
		t.Errorf("密文损坏错误码 = %q", code)
	}
}

// TestUpstreamBalanceBoundaries：未认证 401、缺 CSRF 403、member 403（查询打
// 客户的上游账户，不属自助作用域）、上游不存在 404；被拒的请求不触达平台。
func TestUpstreamBalanceBoundaries(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	id, auth := newBalanceUpstream(t, e, "ds-bound", http.StatusOK,
		`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"1.00"}]}`)
	path := fmt.Sprintf("/admin/v1/upstreams/%d/balance", id)

	wantStatus(t, e.do("POST", path, "", ""), http.StatusUnauthorized)

	r := e.req("POST", path, root, "")
	r.Header.Del("X-LlmGate-CSRF")
	wantStatus(t, e.send(r), http.StatusForbidden)

	wantStatus(t, e.do("POST", "/admin/v1/upstreams/9999/balance", root, ""), http.StatusNotFound)

	if *auth != "" {
		t.Error("被拒的请求不应触达平台余额端点")
	}
}

// TestOpenAICompatServesNoVendorFace：泛型 openai_compat 只服务文本的
// openai_chat；视频 / 图像的厂商协议面都要求一条说得清自己是哪家厂商的上游，
// 泛型行挂不进任何 AIGC 模型（source_kind_unservable），也不会出现在视频 /
// 图像种类的可承载清单里。
func TestOpenAICompatServesNoVendorFace(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	oc := e.createUpstream(root, compatUpstreamBody("h3-local", "http://192.168.50.190:8000/v1"))

	for _, tc := range []struct{ name, body string }{
		{"video", `{"name":"MiniMax-H3-OpenAI","kind":"video","family":"minimax_video"}`},
		{"image", `{"name":"img-oc","kind":"image"}`},
	} {
		m := e.createModelBody(root, tc.body)
		resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", m.ID), root,
			fmt.Sprintf(`{"upstream_id":%d}`, oc.ID))
		if got := errCode(t, resp); got != "source_kind_unservable" {
			t.Errorf("openai_compat 挂 %s 模型的 error.code = %q，期望 source_kind_unservable", tc.name, got)
		}
	}
	// 文本圈里照旧是 OpenAI Chat 加经 Chat 承载的 OpenAI Responses，没有 Anthropic Messages。
	tm := e.createModelBody(root, `{"name":"oc-text"}`)
	src := e.createSource(root, tm.ID, fmt.Sprintf(`{"upstream_id":%d}`, oc.ID))
	if strings.Join(src.Protocols, ",") != "openai_chat,openai_responses" {
		t.Errorf("文本圈里的 openai_compat 来源 protocols = %v，期望 [openai_chat openai_responses]", src.Protocols)
	}
}
