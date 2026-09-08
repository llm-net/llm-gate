// catalog_test.go 是 iteration-5 Phase 4 的可执行验收：上游/模型/来源三组
// 管理端点的全流程 CRUD、上游 Key 明文的 §15.1 全链路不外泄（响应只给 last4、
// 审计 detail 与日志扫描无明文）、引用完整性的 409 口径（被引用上游不可删、
// (模型, 上游) 不可重复挂）、type 不可改，以及类型联动必填——mock 必须有
// base_url（否则两个入口都 404 却仍被 /v1/models 列出，Phase 3 口径缺口）、
// 非 mock 建行必须有 api_key。会话与 CSRF 两道防线对新端点同等生效。
//
// 复用 server_test.go 的 env/req/do 等装置（同属 admin_test 包）。
package admin_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// upstreamKeyPlaintext 是全套用例共用的上游凭证明文：足够长（last4 需 ≥8 位）
// 且形态独特，便于在响应体、审计表与日志里做全文扫描断言。
const upstreamKeyPlaintext = "sk-upstream-secret-abcd9731"

type upstreamDTO struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	CatalogID        string `json:"catalog_id"`
	PlatformLabel    string `json:"platform_label"`
	BillingMode      string `json:"billing_mode"`
	APIKeyLast4      string `json:"api_key_last4"`
	BaseURL          string `json:"base_url"`
	Disabled         bool   `json:"disabled"`
	EgressMode       string `json:"egress_mode"`
	BalanceSupported bool   `json:"balance_supported"`
}

type sourceDTO struct {
	ID                    int64    `json:"id"`
	ModelID               int64    `json:"model_id"`
	UpstreamID            int64    `json:"upstream_id"`
	UpstreamName          string   `json:"upstream_name"`
	UpstreamType          string   `json:"upstream_type"`
	UpstreamCatalogID     string   `json:"upstream_catalog_id"`
	UpstreamPlatformLabel string   `json:"upstream_platform_label"`
	UpstreamBillingMode   string   `json:"upstream_billing_mode"`
	UpstreamDisabled      bool     `json:"upstream_disabled"`
	UpstreamModelID       string   `json:"upstream_model_id"`
	Priority              int64    `json:"priority"`
	Disabled              bool     `json:"disabled"`
	Protocols             []string `json:"protocols"`
}

type modelDTO struct {
	ID             int64            `json:"id"`
	Name           string           `json:"name"`
	Kind           string           `json:"kind"`
	Family         string           `json:"family"` // 0014：协议面声明，text 恒空
	Agent          string           `json:"agent"`  // 2026-08-15：订阅计价行注记，按已存官方价文件的 agent 标记逐次算出
	Disabled       bool             `json:"disabled"`
	EntryResponses bool             `json:"entry_responses"`
	EntryOpenAI    bool             `json:"entry_openai"`
	EntryAnthropic bool             `json:"entry_anthropic"`
	Pricing        map[string]int64 `json:"pricing"` // iteration-9：null = 未定价
	Sources        []sourceDTO      `json:"sources"`
}

// createUpstream 经 API 建上游（期望 201）。
func (e *env) createUpstream(cookie, body string) upstreamDTO {
	e.t.Helper()
	resp := e.do("POST", "/admin/v1/upstreams", cookie, body)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("创建上游状态 = %d，期望 201；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var out struct {
		Upstream upstreamDTO `json:"upstream"`
	}
	decodeInto(e.t, resp, &out)
	return out.Upstream
}

// createModel 经 API 建模型（期望 201；不带 kind 字段 = 既有前端形态，缺省 text）。
func (e *env) createModel(cookie, name string) modelDTO {
	e.t.Helper()
	return e.createModelBody(cookie, `{"name":"`+name+`"}`)
}

// createModelKind 经 API 建指定 kind 的模型（期望 201）。
func (e *env) createModelKind(cookie, name, kind string) modelDTO {
	e.t.Helper()
	return e.createModelBody(cookie, fmt.Sprintf(`{"name":"%s","kind":"%s"}`, name, kind))
}

func (e *env) createModelBody(cookie, body string) modelDTO {
	e.t.Helper()
	resp := e.do("POST", "/admin/v1/models", cookie, body)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("创建模型状态 = %d，期望 201；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var out struct {
		Model modelDTO `json:"model"`
	}
	decodeInto(e.t, resp, &out)
	return out.Model
}

// createSource 给模型挂来源（期望 201）。
func (e *env) createSource(cookie string, modelID int64, body string) sourceDTO {
	e.t.Helper()
	resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", modelID), cookie, body)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("挂来源状态 = %d，期望 201；body: %s", resp.StatusCode, readAll(e.t, resp))
	}
	var out struct {
		Source sourceDTO `json:"source"`
	}
	decodeInto(e.t, resp, &out)
	return out.Source
}

func (e *env) listUpstreams(cookie string) []upstreamDTO {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/upstreams", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out struct {
		Upstreams []upstreamDTO `json:"upstreams"`
	}
	decodeInto(e.t, resp, &out)
	return out.Upstreams
}

func (e *env) listModels(cookie string) []modelDTO {
	e.t.Helper()
	resp := e.do("GET", "/admin/v1/models", cookie, "")
	wantStatus(e.t, resp, http.StatusOK)
	var out struct {
		Models []modelDTO `json:"models"`
	}
	decodeInto(e.t, resp, &out)
	return out.Models
}

// mockUpstreamBody 是一条合法的 mock 上游请求体（mock 必须带 base_url）。
func mockUpstreamBody(name, baseURL string) string {
	return fmt.Sprintf(`{"name":"%s","type":"mock","base_url":"%s"}`, name, baseURL)
}

// TestCatalogCRUDFlow：建上游 → 建模型 → 挂两条来源 → 改优先级/来源侧 ID →
// 启停三层 → 删来源 → 删模型 → 删上游，全流程与嵌套列表口径。
func TestCatalogCRUDFlow(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	ds := e.createUpstream(root, fmt.Sprintf(
		`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	if ds.Type != "deepseek" || ds.APIKeyLast4 != upstreamKeyPlaintext[len(upstreamKeyPlaintext)-4:] {
		t.Fatalf("创建上游响应异常: %+v", ds)
	}
	mock := e.createUpstream(root, mockUpstreamBody("local-mock", "http://127.0.0.1:18080/v1"))

	m := e.createModel(root, "deepseek-chat")
	if m.Name != "deepseek-chat" || len(m.Sources) != 0 {
		t.Fatalf("新建模型响应异常: %+v", m)
	}

	// 缺省优先级 100、缺省来源侧 ID 为空串（= 与模型名相同）。
	primary := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, ds.ID))
	if primary.Priority != 100 || primary.UpstreamModelID != "" || primary.UpstreamName != "ds" {
		t.Fatalf("缺省来源响应异常: %+v", primary)
	}
	backup := e.createSource(root, m.ID, fmt.Sprintf(
		`{"upstream_id":%d,"upstream_model_id":"mock-chat","priority":200}`, mock.ID))

	// 嵌套列表：来源按 (priority, id) 排序，带上游展示字段与可服务入口。
	models := e.listModels(root)
	if len(models) != 1 || len(models[0].Sources) != 2 {
		t.Fatalf("模型列表异常: %+v", models)
	}
	got := models[0].Sources
	if got[0].ID != primary.ID || got[1].ID != backup.ID {
		t.Errorf("来源未按 (priority, id) 排序: %+v", got)
	}
	if got[0].UpstreamType != "deepseek" || got[1].UpstreamModelID != "mock-chat" {
		t.Errorf("来源上游展示字段异常: %+v", got)
	}

	// 改优先级 + 改来源侧 ID：备用来源提到最前。
	resp := e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", backup.ID), root,
		`{"priority":50,"upstream_model_id":"mock-chat-v2"}`)
	wantStatus(t, resp, http.StatusOK)
	var patched struct {
		Source sourceDTO `json:"source"`
	}
	decodeInto(t, resp, &patched)
	if patched.Source.Priority != 50 || patched.Source.UpstreamModelID != "mock-chat-v2" {
		t.Errorf("来源 PATCH 响应异常: %+v", patched.Source)
	}
	models = e.listModels(root)
	if models[0].Sources[0].ID != backup.ID {
		t.Errorf("改优先级后排序未生效: %+v", models[0].Sources)
	}

	// 三层启停各自独立可切换。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", backup.ID), root, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", ds.ID), root, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"disabled":true}`), http.StatusOK)
	models = e.listModels(root)
	if !models[0].Disabled || !models[0].Sources[0].Disabled || !models[0].Sources[1].UpstreamDisabled {
		t.Errorf("三层禁用位未如实回显: %+v", models[0])
	}
	// 管理台停用后模型退出数据面 /v1/models（无缓存，下一个请求即生效）。
	// 三个禁用位各自的口径由 store_test 逐位覆盖，此处只验管理 API → 数据面这条联动。
	servable, err := e.st.ListServableModels(t.Context())
	if err != nil {
		t.Fatalf("ListServableModels: %v", err)
	}
	if len(servable) != 0 {
		t.Errorf("模型已停用仍出现在可服务列表: %+v", servable)
	}

	// 改名（改的就是客户端可见名）。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root,
		`{"name":"deepseek-chat-v2","disabled":false}`), http.StatusOK)
	if models = e.listModels(root); models[0].Name != "deepseek-chat-v2" || models[0].Disabled {
		t.Errorf("模型改名/启用未生效: %+v", models[0])
	}

	// 删来源 → 删模型（级联剩余来源）→ 删上游（不再被引用）。
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/sources/%d", backup.ID), root, ""), http.StatusNoContent)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/sources/%d", backup.ID), root, ""), http.StatusNotFound)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, ""), http.StatusNoContent)
	if models = e.listModels(root); len(models) != 0 {
		t.Errorf("删模型后列表应为空: %+v", models)
	}
	if _, err := e.st.GetModelSourceByID(t.Context(), primary.ID); err == nil {
		t.Error("删模型应级联删除其来源")
	}
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/upstreams/%d", ds.ID), root, ""), http.StatusNoContent)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/upstreams/%d", mock.ID), root, ""), http.StatusNoContent)
	if ups := e.listUpstreams(root); len(ups) != 0 {
		t.Errorf("删上游后列表应为空: %+v", ups)
	}
}

// TestUpstreamKeyNeverLeaks：api_key 明文在创建/列表/PATCH 响应、审计 detail
// 与日志里全都不出现，只有 last4 可见；换 Key 后 last4 跟着变（§15.1）。
func TestUpstreamKeyNeverLeaks(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	const rotated = "sk-upstream-rotated-xyz42088"
	created := e.do("POST", "/admin/v1/upstreams", root, fmt.Sprintf(
		`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	wantStatus(t, created, http.StatusCreated)
	createBody := readAll(t, created)
	if strings.Contains(createBody, upstreamKeyPlaintext) {
		t.Error("创建上游的响应泄露了 api_key 明文")
	}
	if !strings.Contains(createBody, `"api_key_last4":"9731"`) {
		t.Errorf("创建响应应含 api_key_last4；body: %s", createBody)
	}
	var out struct {
		Upstream upstreamDTO `json:"upstream"`
	}
	if err := json.Unmarshal([]byte(createBody), &out); err != nil {
		t.Fatalf("解析创建响应 %q: %v", createBody, err)
	}

	// 列表响应无明文。
	listResp := e.do("GET", "/admin/v1/upstreams", root, "")
	wantStatus(t, listResp, http.StatusOK)
	if raw := readAll(t, listResp); strings.Contains(raw, upstreamKeyPlaintext) {
		t.Error("上游列表响应泄露了 api_key 明文")
	}

	// 换 Key：留空不动，非空则换；两种响应都不含明文。
	keep := e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", out.Upstream.ID), root, `{"api_key":""}`)
	wantStatus(t, keep, http.StatusOK)
	var kept struct {
		Upstream upstreamDTO `json:"upstream"`
	}
	decodeInto(t, keep, &kept)
	if kept.Upstream.APIKeyLast4 != "9731" {
		t.Errorf("api_key 留空应保留原凭证，last4 = %q", kept.Upstream.APIKeyLast4)
	}
	rot := e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", out.Upstream.ID), root,
		fmt.Sprintf(`{"api_key":"%s"}`, rotated))
	wantStatus(t, rot, http.StatusOK)
	rotBody := readAll(t, rot)
	if strings.Contains(rotBody, rotated) || strings.Contains(rotBody, upstreamKeyPlaintext) {
		t.Error("换 Key 的响应泄露了 api_key 明文")
	}
	if !strings.Contains(rotBody, `"api_key_last4":"2088"`) {
		t.Errorf("换 Key 后 last4 应更新；body: %s", rotBody)
	}

	// 审计表全文扫描：任何一列都不得出现明文。
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	var events []string
	for rows.Next() {
		var event, entity, detail string
		if err := rows.Scan(&event, &entity, &detail); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		events = append(events, event)
		for _, secret := range []string{upstreamKeyPlaintext, rotated} {
			if strings.Contains(entity+detail, secret) {
				t.Errorf("审计行泄露 api_key 明文（event=%s）", event)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计行: %v", err)
	}
	for _, want := range []string{"upstream.create", "upstream.rotate_key"} {
		if !containsString(events, want) {
			t.Errorf("审计表缺少事件 %s；已有: %v", want, events)
		}
	}

	// 日志（debug 级别）同样无明文。
	logs := e.buf.String()
	for _, secret := range []string{upstreamKeyPlaintext, rotated} {
		if strings.Contains(logs, secret) {
			t.Error("管理面日志泄露 api_key 明文（§15.1）")
		}
	}
}

// TestCatalogAuditEvents：模型与来源的每类变更都留下审计事件，
// detail 只含名称/优先级等非敏感字段。
func TestCatalogAuditEvents(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	up := e.createUpstream(root, mockUpstreamBody("m1", "http://127.0.0.1:18080/v1"))
	m := e.createModel(root, "mock-chat")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", src.ID), root, `{"priority":10}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"name":"mock-chat-2"}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"disabled":false}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"disabled":true}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"disabled":false}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"name":"m1-renamed"}`), http.StatusOK)

	// 无变更的 PATCH 不写审计（回传现值、空 body 都算），否则审计表会被
	// 前端的整表回填淹没，"谁改了什么"就查不出来了。
	before := auditCount(t, e)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root,
		`{"name":"m1-renamed","disabled":false,"api_key":""}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"name":"mock-chat-2"}`), http.StatusOK)
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/sources/%d", src.ID), root, `{}`), http.StatusOK)
	if after := auditCount(t, e); after != before {
		t.Errorf("无变更的 PATCH 写了 %d 条审计，期望 0", after-before)
	}

	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/sources/%d", src.ID), root, ""), http.StatusNoContent)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, ""), http.StatusNoContent)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, ""), http.StatusNoContent)

	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	seen := map[string]string{}
	details := map[string]string{}
	for rows.Next() {
		var event, entity, detail string
		if err := rows.Scan(&event, &entity, &detail); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		seen[event] = entity
		details[event] = detail
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计行: %v", err)
	}
	want := map[string]string{
		"upstream.create":  fmt.Sprintf("upstream:%d", up.ID),
		"upstream.update":  fmt.Sprintf("upstream:%d", up.ID),
		"upstream.disable": fmt.Sprintf("upstream:%d", up.ID),
		"upstream.enable":  fmt.Sprintf("upstream:%d", up.ID),
		"upstream.delete":  fmt.Sprintf("upstream:%d", up.ID),
		"model.create":     fmt.Sprintf("model:%d", m.ID),
		"model.rename":     fmt.Sprintf("model:%d", m.ID),
		"model.disable":    fmt.Sprintf("model:%d", m.ID),
		"model.enable":     fmt.Sprintf("model:%d", m.ID),
		"model.delete":     fmt.Sprintf("model:%d", m.ID),
		"source.create":    fmt.Sprintf("source:%d", src.ID),
		"source.update":    fmt.Sprintf("source:%d", src.ID),
		"source.delete":    fmt.Sprintf("source:%d", src.ID),
	}
	for event, entity := range want {
		if seen[event] != entity {
			t.Errorf("审计事件 %s 的 entity = %q，期望 %q", event, seen[event], entity)
		}
		// detail 是 key=value 形式的非敏感字段（名称/优先级），不能是空串——
		// 空 detail 的审计行等于没记。
		if d := details[event]; !strings.Contains(d, "=") {
			t.Errorf("审计事件 %s 的 detail = %q，期望 key=value 形式的非敏感字段", event, d)
		}
	}
}

// auditCount 读当前审计行数（只读 SQL 视角，与板上走查一致）。
func auditCount(t *testing.T, e *env) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatalf("统计审计行: %v", err)
	}
	return n
}

// TestUpstreamInUseCannotBeDeleted：被模型来源引用的上游 DELETE → 409
// upstream_in_use；解除引用后可删。
func TestUpstreamInUseCannotBeDeleted(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, mockUpstreamBody("m1", "http://127.0.0.1:18080/v1"))
	m := e.createModel(root, "mock-chat")
	src := e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))

	resp := e.do("DELETE", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, "")
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "upstream_in_use" {
		t.Errorf("error.code = %q，期望 upstream_in_use", got)
	}

	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/sources/%d", src.ID), root, ""), http.StatusNoContent)
	wantStatus(t, e.do("DELETE", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, ""), http.StatusNoContent)
}

// TestDuplicateSourceRejected：同一 (模型, 上游) 只允许一条来源 → 409
// source_exists；不同模型挂同一上游则允许。
func TestDuplicateSourceRejected(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, mockUpstreamBody("m1", "http://127.0.0.1:18080/v1"))
	m := e.createModel(root, "mock-chat")
	e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))

	resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", m.ID), root,
		fmt.Sprintf(`{"upstream_id":%d,"priority":200}`, up.ID))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "source_exists" {
		t.Errorf("error.code = %q，期望 source_exists", got)
	}

	other := e.createModel(root, "mock-mini")
	e.createSource(root, other.ID, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))

	// 引用不存在的上游/模型分别给出可读错误，不落到 500。
	resp = e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", m.ID), root, `{"upstream_id":9999}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "upstream_not_found" {
		t.Errorf("error.code = %q，期望 upstream_not_found", got)
	}
	resp = e.do("POST", "/admin/v1/models/9999/sources", root, fmt.Sprintf(`{"upstream_id":%d}`, up.ID))
	wantStatus(t, resp, http.StatusNotFound)
}

// TestUpstreamTypeImmutable：type 不可改（建错重建，决策 8），改同值放行。
func TestUpstreamTypeImmutable(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()
	up := e.createUpstream(root, fmt.Sprintf(
		`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))

	resp := e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"type":"ark"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "type_immutable" {
		t.Errorf("error.code = %q，期望 type_immutable", got)
	}
	// 同值不算修改：type 与其他可改字段同时回传时不该被拒。
	// （decodeJSON 开了 DisallowUnknownFields，前端只能回传这五个可改字段——
	// name/type/api_key/base_url/disabled，不能把整个响应对象原样 PATCH 回来。）
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root,
		`{"type":"deepseek","name":"ds-2"}`), http.StatusOK)
	if ups := e.listUpstreams(root); ups[0].Name != "ds-2" || ups[0].Type != "deepseek" {
		t.Errorf("同值 type 的 PATCH 未生效: %+v", ups)
	}
}

// TestUpstreamShapeValidation：类型联动必填——mock 必须有 base_url
// （否则两个入口都不服务，模型会被 /v1/models 列出却处处 404），
// 非 mock 建行必须有 api_key；type 枚举与 base_url 形态同样在写入侧拦。
func TestUpstreamShapeValidation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	cases := []struct {
		name, body, code string
		status           int
	}{
		{"mock 缺 base_url", `{"name":"m","type":"mock"}`, "base_url_required", http.StatusBadRequest},
		{"deepseek 缺 api_key", `{"name":"d","type":"deepseek"}`, "api_key_required", http.StatusBadRequest},
		{"ark 缺 api_key", `{"name":"a","type":"ark"}`, "api_key_required", http.StatusBadRequest},
		{"未知 type", `{"name":"x","type":"openai","api_key":"k123456789"}`, "invalid_type", http.StatusBadRequest},
		{"空名", `{"name":"","type":"mock","base_url":"http://127.0.0.1:1/v1"}`, "invalid_name", http.StatusBadRequest},
		{"名含空白", `{"name":"a b","type":"mock","base_url":"http://127.0.0.1:1/v1"}`, "invalid_name", http.StatusBadRequest},
		{"base_url 非法", `{"name":"m","type":"mock","base_url":"ftp://x"}`, "invalid_base_url", http.StatusBadRequest},
		// URL 内嵌凭证会绕过"只回 last4"的脱敏链路，写入侧直接拒（§15.1）。
		{"base_url 内嵌凭证", `{"name":"m","type":"mock","base_url":"http://u:p@127.0.0.1:1/v1"}`, "invalid_base_url", http.StatusBadRequest},
		// 产品上游不给写 base_url：能写就能把上游指向任意主机，而数据面会把
		// 解密后的 Key 发过去——"管理员也读不回 Key"的承诺会被这个写接口绕开。
		{"deepseek 带 base_url", `{"name":"d","type":"deepseek","api_key":"sk-1234567890","base_url":"http://evil.example/v1"}`,
			"base_url_not_allowed", http.StatusBadRequest},
		{"api_key 过短", `{"name":"d","type":"deepseek","api_key":"sk-123"}`, "invalid_api_key", http.StatusBadRequest},
		{"api_key 含空白", `{"name":"d","type":"deepseek","api_key":"sk-123 456789"}`, "invalid_api_key", http.StatusBadRequest},
		{"api_key 含换行", "{\"name\":\"d\",\"type\":\"deepseek\",\"api_key\":\"sk-1234\\n56789\"}", "invalid_api_key", http.StatusBadRequest},
		// minimax（迭代 8）：api_key 必填；base_url 是站点选择，站点白名单外
		// 的地址 400——放开就等于把解密后的 Key 发往任意主机。
		{"minimax 缺 api_key", `{"name":"mm","type":"minimax"}`, "api_key_required", http.StatusBadRequest},
		{"minimax 站点外地址", `{"name":"mm","type":"minimax","api_key":"sk-1234567890","base_url":"https://evil.example"}`,
			"invalid_base_url", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp := e.do("POST", "/admin/v1/upstreams", root, c.body)
		wantStatus(t, resp, c.status)
		if got := errCode(t, resp); got != c.code {
			t.Errorf("%s: error.code = %q，期望 %q", c.name, got, c.code)
		}
	}

	// 重名 409。
	e.createUpstream(root, mockUpstreamBody("m1", "http://127.0.0.1:18080/v1"))
	resp := e.do("POST", "/admin/v1/upstreams", root, mockUpstreamBody("m1", "http://127.0.0.1:18080/v1"))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "upstream_name_taken" {
		t.Errorf("重名 error.code = %q，期望 upstream_name_taken", got)
	}

	// PATCH 同样守住 mock 的 base_url 必填。
	up := e.listUpstreams(root)[0]
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", up.ID), root, `{"base_url":""}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "base_url_required" {
		t.Errorf("PATCH 清空 mock base_url 的 error.code = %q，期望 base_url_required", got)
	}

	// PATCH 同样拦住"把产品上游指向新地址"与不合形态的换 Key。
	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", ds.ID), root, `{"base_url":"http://evil.example/v1"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "base_url_not_allowed" {
		t.Errorf("PATCH 给 deepseek 写 base_url 的 error.code = %q，期望 base_url_not_allowed", got)
	}
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", ds.ID), root, `{"api_key":"short"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "invalid_api_key" {
		t.Errorf("PATCH 换短 Key 的 error.code = %q，期望 invalid_api_key", got)
	}
	// 被拒的 PATCH 一个字段都不许落库（校验全部先于写入）。
	if after := e.listUpstreams(root); after[1].BaseURL != "" || after[1].APIKeyLast4 != "9731" {
		t.Errorf("被拒的 PATCH 不应改动任何字段: %+v", after[1])
	}
}

// TestModelShapeValidation：模型名形态与重名口径。
func TestModelShapeValidation(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	for _, body := range []string{`{"name":""}`, `{"name":"a b"}`, `{"name":"a\tb"}`} {
		resp := e.do("POST", "/admin/v1/models", root, body)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "invalid_name" {
			t.Errorf("模型名 %s 的 error.code = %q，期望 invalid_name", body, got)
		}
	}
	m := e.createModel(root, "deepseek-chat")
	resp := e.do("POST", "/admin/v1/models", root, `{"name":"deepseek-chat"}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "model_name_taken" {
		t.Errorf("重名 error.code = %q，期望 model_name_taken", got)
	}
	e.createModel(root, "other")
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"name":"other"}`)
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "model_name_taken" {
		t.Errorf("改重名 error.code = %q，期望 model_name_taken", got)
	}
	wantStatus(t, e.do("PATCH", "/admin/v1/models/9999", root, `{"disabled":true}`), http.StatusNotFound)
	wantStatus(t, e.do("PATCH", "/admin/v1/sources/9999", root, `{"priority":1}`), http.StatusNotFound)
	wantStatus(t, e.do("PATCH", "/admin/v1/upstreams/9999", root, `{"disabled":true}`), http.StatusNotFound)
	wantStatus(t, e.do("DELETE", "/admin/v1/upstreams/9999", root, ""), http.StatusNotFound)
	wantStatus(t, e.do("DELETE", "/admin/v1/models/9999", root, ""), http.StatusNotFound)
}

// TestSourceProtocols：每条来源标注的可服务入口 = 运行时 (上游 type, 入口协议)
// 过滤口径，且按所属模型的 kind 圈定——文本模型下 deepseek 双入口、ark 仅
// chat、带 base_url 的 mock 双入口；视频模型下 minimax 是 minimax_video、
// 方舟按量与订阅都是 ark_video；图片模型下方舟两型都是 ark_image。
// kind≠text 的来源守卫（2026-08-09 厂商官方接口改版）：不服务该 kind 协议面
// 的上游类型在**写入时**被拒（source_kind_unservable，文本模型不设此闸——
// 协议在入口层各自过滤），已有来源是别家协议面的被拒为 source_family_mismatch。
// **同族内不同 type 放行**：方舟按量 + 订阅是一个模型的正常多来源形态
// （订阅先用满、按量兜底）。
func TestSourceProtocols(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	ds := e.createUpstream(root, fmt.Sprintf(`{"name":"ds","type":"deepseek","api_key":"%s"}`, upstreamKeyPlaintext))
	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))
	arkPlan := e.createUpstream(root, fmt.Sprintf(`{"name":"arkplan","type":"ark_plan","api_key":"%s"}`, upstreamKeyPlaintext))
	mock := e.createUpstream(root, mockUpstreamBody("mk", "http://127.0.0.1:18080/v1"))
	mm := e.createUpstream(root, fmt.Sprintf(`{"name":"mm","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))

	m := e.createModel(root, "deepseek-chat")
	for i, id := range []int64{ds.ID, ark.ID, mock.ID} {
		e.createSource(root, m.ID, fmt.Sprintf(`{"upstream_id":%d,"priority":%d}`, id, (i+1)*100))
	}
	vm := e.createModelKind(root, "MiniMax-H3", "video")
	e.createSource(root, vm.ID, fmt.Sprintf(`{"upstream_id":%d,"priority":100}`, mm.ID))
	// 不服务视频协议面的类型在写入时即拒：mock（说不清自己该被当哪家厂商）
	// 与 deepseek（压根没有 AIGC 端点）。
	for _, id := range []int64{mock.ID, ds.ID} {
		resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", vm.ID), root,
			fmt.Sprintf(`{"upstream_id":%d,"priority":300}`, id))
		wantStatus(t, resp, http.StatusBadRequest)
		if got := errCode(t, resp); got != "source_kind_unservable" {
			t.Errorf("视频模型挂不服务类型的 error.code = %q，期望 source_kind_unservable", got)
		}
	}
	// 服务视频、但走另一家协议面的类型：协议面冲突（一个模型固定一种 API 格式）。
	resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", vm.ID), root,
		fmt.Sprintf(`{"upstream_id":%d,"priority":300}`, ark.ID))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "source_family_mismatch" {
		t.Errorf("视频模型挂异面来源的 error.code = %q，期望 source_family_mismatch", got)
	}
	// 方舟视频模型：按量与订阅同族，两条来源并存（订阅 100、按量 200）。
	av := e.createModelKind(root, "doubao-seedance-2.0", "video")
	e.createSource(root, av.ID, fmt.Sprintf(`{"upstream_id":%d,"priority":100}`, arkPlan.ID))
	e.createSource(root, av.ID, fmt.Sprintf(`{"upstream_id":%d,"priority":200}`, ark.ID))
	// 图片模型：方舟两型都服务 ark_image；minimax 不服务图片。
	im := e.createModelKind(root, "doubao-seedream-5.0-lite", "image")
	e.createSource(root, im.ID, fmt.Sprintf(`{"upstream_id":%d,"priority":100}`, arkPlan.ID))
	resp = e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", im.ID), root,
		fmt.Sprintf(`{"upstream_id":%d,"priority":300}`, mm.ID))
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "source_kind_unservable" {
		t.Errorf("图片模型挂 minimax 来源的 error.code = %q，期望 source_kind_unservable", got)
	}

	byName := map[string]modelDTO{}
	for _, md := range e.listModels(root) {
		byName[md.Name] = md
	}
	assertProtocols := func(model string, want [][]string) {
		t.Helper()
		sources := byName[model].Sources
		if len(sources) != len(want) {
			t.Fatalf("模型 %s 的来源数 = %d，期望 %d", model, len(sources), len(want))
		}
		for i, w := range want {
			if strings.Join(sources[i].Protocols, ",") != strings.Join(w, ",") {
				t.Errorf("模型 %s 来源 %d（上游 %s）protocols = %v，期望 %v",
					model, i, sources[i].UpstreamName, sources[i].Protocols, w)
			}
		}
	}
	assertProtocols("deepseek-chat", [][]string{
		{"openai_chat", "openai_responses", "anthropic_messages"},
		{"openai_chat", "openai_responses"},
		{"openai_chat", "openai_responses", "anthropic_messages"},
	})
	assertProtocols("MiniMax-H3", [][]string{
		{"minimax_video"},
	})
	assertProtocols("doubao-seedance-2.0", [][]string{
		{"ark_video"}, // 订阅（priority 100）
		{"ark_video"}, // 按量（priority 200）
	})
	assertProtocols("doubao-seedream-5.0-lite", [][]string{
		{"ark_image"},
	})
}

// TestModelKindAdmin（迭代 8）：POST /admin/v1/models 收 kind（缺省 text 兼容
// 既有前端、非法值 400 invalid_kind）；建后不可改（kind_immutable，同值回传
// 不算改）；创建响应与列表都带 kind；model.create 审计 detail 带 kind。
func TestModelKindAdmin(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	legacy := e.createModel(root, "deepseek-chat")
	if legacy.Kind != "text" {
		t.Errorf("缺省 kind = %q，期望 text", legacy.Kind)
	}
	video := e.createModelKind(root, "doubao-seedance", "video")
	if video.Kind != "video" {
		t.Errorf("创建响应 kind = %q，期望 video", video.Kind)
	}
	resp := e.do("POST", "/admin/v1/models", root, `{"name":"bad","kind":"audio"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "invalid_kind" {
		t.Errorf("非法 kind 的 error.code = %q，期望 invalid_kind", got)
	}

	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", video.ID), root, `{"kind":"image"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "kind_immutable" {
		t.Errorf("改 kind 的 error.code = %q，期望 kind_immutable", got)
	}
	// 同值不算修改：kind 与其他可改字段同时回传时不该被拒。
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", video.ID), root,
		`{"kind":"video","name":"doubao-seedance-2"}`), http.StatusOK)

	kinds := map[string]string{}
	for _, md := range e.listModels(root) {
		kinds[md.Name] = md.Kind
	}
	if kinds["deepseek-chat"] != "text" || kinds["doubao-seedance-2"] != "video" {
		t.Errorf("列表 kind 失真: %v", kinds)
	}

	// model.create 审计 detail 带 kind（既有事件、不加新事件名）。
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	var detail string
	if err := db.QueryRow(`SELECT detail FROM audit_events WHERE event = 'model.create' AND entity = ?`,
		fmt.Sprintf("model:%d", video.ID)).Scan(&detail); err != nil {
		t.Fatalf("查询 model.create 审计行: %v", err)
	}
	if !strings.Contains(detail, "kind=video") {
		t.Errorf("model.create 审计 detail = %q，期望含 kind=video", detail)
	}
}

// TestMinimaxUpstreamSites（迭代 8）：minimax 上游的 base_url 是站点选择，
// 只在内置双值里选——空 = 国内缺省、国际站可选；PATCH 换站同规则（站点外
// 地址 400、清空回国内缺省）。
func TestMinimaxUpstreamSites(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	cn := e.createUpstream(root, fmt.Sprintf(`{"name":"mm-cn","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))
	if cn.Type != "minimax" || cn.BaseURL != "" || cn.APIKeyLast4 != "9731" {
		t.Errorf("minimax 国内缺省行失真: %+v", cn)
	}
	intl := e.createUpstream(root, fmt.Sprintf(
		`{"name":"mm-intl","type":"minimax","api_key":"%s","base_url":"https://api.minimax.io"}`, upstreamKeyPlaintext))
	if intl.BaseURL != "https://api.minimax.io" {
		t.Errorf("国际站 base_url 未存: %+v", intl)
	}

	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", cn.ID), root,
		`{"base_url":"https://api.minimax.io"}`), http.StatusOK)
	resp := e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", cn.ID), root,
		`{"base_url":"https://evil.example"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "invalid_base_url" {
		t.Errorf("站点外地址的 error.code = %q，期望 invalid_base_url", got)
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/upstreams/%d", cn.ID), root, `{"base_url":""}`), http.StatusOK)
	if ups := e.listUpstreams(root); ups[0].BaseURL != "" {
		t.Errorf("清空站点未回国内缺省: %+v", ups[0])
	}
}

// TestCatalogAuthGuards：会话与 CSRF 两道防线对新端点同等生效
// （未登录 401、缺 X-LlmGate-CSRF 头 403、非 JSON Content-Type 415）。
func TestCatalogAuthGuards(t *testing.T) {
	e := newEnv(t)
	cookie := e.rootSession()

	endpoints := []struct{ method, path string }{
		{"GET", "/admin/v1/upstreams"},
		{"POST", "/admin/v1/upstreams"},
		{"PATCH", "/admin/v1/upstreams/1"},
		{"DELETE", "/admin/v1/upstreams/1"},
		{"GET", "/admin/v1/models"},
		{"POST", "/admin/v1/models"},
		{"PATCH", "/admin/v1/models/1"},
		{"DELETE", "/admin/v1/models/1"},
		{"POST", "/admin/v1/models/1/sources"},
		{"PATCH", "/admin/v1/sources/1"},
		{"DELETE", "/admin/v1/sources/1"},
	}
	for _, ep := range endpoints {
		for _, c := range []string{"", "forged-session-token"} {
			resp := e.do(ep.method, ep.path, c, "{}")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s（cookie=%q）状态 = %d，期望 401", ep.method, ep.path, c, resp.StatusCode)
			}
			if got := errCode(t, resp); got != "unauthorized" {
				t.Fatalf("%s %s error.code = %q，期望 unauthorized", ep.method, ep.path, got)
			}
		}
		if ep.method == http.MethodGet {
			continue
		}
		r := e.req(ep.method, ep.path, cookie, "{}")
		r.Header.Del("X-LlmGate-CSRF")
		resp := e.send(r)
		wantStatus(t, resp, http.StatusForbidden)
		if got := errCode(t, resp); got != "csrf_required" {
			t.Errorf("%s %s error.code = %q，期望 csrf_required", ep.method, ep.path, got)
		}

		r = e.req(ep.method, ep.path, cookie, "{}")
		r.Header.Set("Content-Type", "text/plain")
		resp = e.send(r)
		wantStatus(t, resp, http.StatusUnsupportedMediaType)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestModelEntrySwitchesAPI（0013 调用入口开关）：建时缺省双开、可只开一个；
// PATCH 三态改写；text 双关与 AIGC 带开关都在动任何字段前 400；审计记
// model.entries。
func TestModelEntrySwitchesAPI(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	m := e.createModel(root, "entry-default")
	if !m.EntryOpenAI || !m.EntryResponses || !m.EntryAnthropic {
		t.Fatalf("新模型应缺省双开: %+v", m)
	}

	only := e.createModelBody(root, `{"name":"entry-anthropic-only","entry_openai":false}`)
	if only.EntryOpenAI || !only.EntryAnthropic {
		t.Fatalf("建时关 openai 未生效: %+v", only)
	}

	resp := e.do("POST", "/admin/v1/models", root, `{"name":"entry-none","entry_openai":false,"entry_anthropic":false}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "entries_required" {
		t.Errorf("error.code = %q，期望 entries_required", got)
	}

	resp = e.do("POST", "/admin/v1/models", root, `{"name":"entry-video","kind":"video","entry_openai":false}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "entries_text_only" {
		t.Errorf("error.code = %q，期望 entries_text_only", got)
	}

	// PATCH 三态：只传要改的一侧；把仅剩的入口也关掉必须 400 且现值不动。
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"entry_openai":false}`)
	wantStatus(t, resp, http.StatusOK)
	var patched struct {
		Model modelDTO `json:"model"`
	}
	decodeInto(t, resp, &patched)
	if patched.Model.EntryOpenAI || !patched.Model.EntryAnthropic {
		t.Fatalf("PATCH 后开关不符: %+v", patched.Model)
	}
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", m.ID), root, `{"entry_responses":false,"entry_anthropic":false}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "entries_required" {
		t.Errorf("error.code = %q，期望 entries_required", got)
	}
	list := e.listModels(root)
	for _, lm := range list {
		if lm.ID == m.ID && (lm.EntryOpenAI || !lm.EntryAnthropic) {
			t.Fatalf("被拒的 PATCH 不该改动现值: %+v", lm)
		}
	}

	video := e.createModelKind(root, "entry-video-ok", "video")
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", video.ID), root, `{"entry_openai":false}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "entries_text_only" {
		t.Errorf("error.code = %q，期望 entries_text_only", got)
	}

	// 审计：入口开关变更单独记 model.entries，detail 带组合。
	db, err := sql.Open("sqlite", filepath.Join(e.dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	var detail string
	if err := db.QueryRow(`SELECT detail FROM audit_events WHERE event = 'model.entries' ORDER BY id DESC LIMIT 1`).
		Scan(&detail); err != nil {
		t.Fatalf("未见 model.entries 审计事件: %v", err)
	}
	if !strings.Contains(detail, "entries=openai_responses+anthropic_messages") {
		t.Errorf("model.entries detail = %q，期望含 entries=openai_responses+anthropic_messages", detail)
	}
}

// TestModelFamilyDeclaration：协议面建模声明（0014）的契约。族是模型对客户端
// 的 API 格式承诺（H3 这类开源模型「模型名 → 厂商」推不出来），只能声明不能
// 推断：
//   - video 声明后，挂异面来源在**还没有任何来源**时就被拒——旧「首源钉族」
//     做不到这一点，第一条挂错的来源会安静地钉错整个模型；
//   - image 缺省自动声明唯一一族；text 传族 400 family_text_only；
//     与 kind 不符 400 family_invalid；
//   - PATCH 改族 400 family_immutable，回传现值放行（同 kind 先例）；
//   - 不带 family 的 video（兼容旧调用方）保持懒钉：首条来源定族并落库，
//     此后异面同样 409。
func TestModelFamilyDeclaration(t *testing.T) {
	e := newEnv(t)
	root := e.rootSession()

	ark := e.createUpstream(root, fmt.Sprintf(`{"name":"ark","type":"ark","api_key":"%s"}`, upstreamKeyPlaintext))
	mm := e.createUpstream(root, fmt.Sprintf(`{"name":"mm","type":"minimax","api_key":"%s"}`, upstreamKeyPlaintext))

	// 声明 minimax_video：族先于任何来源存在，挂方舟来源当场协议面冲突。
	vm := e.createModelBody(root, `{"name":"MiniMax-H3","kind":"video","family":"minimax_video"}`)
	if vm.Family != "minimax_video" {
		t.Fatalf("family = %q，期望声明的 minimax_video", vm.Family)
	}
	resp := e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", vm.ID), root,
		fmt.Sprintf(`{"upstream_id":%d}`, ark.ID))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "source_family_mismatch" {
		t.Errorf("零来源模型挂异面 error.code = %q，期望 source_family_mismatch", got)
	}
	e.createSource(root, vm.ID, fmt.Sprintf(`{"upstream_id":%d}`, mm.ID)) // 声明的同族放行

	// image 缺省自动声明 ark_image（当前唯一一族）；text 恒空。
	im := e.createModelKind(root, "doubao-seedream-5.0-lite", "image")
	if im.Family != "ark_image" {
		t.Errorf("image 缺省 family = %q，期望自动补 ark_image", im.Family)
	}
	if tm := e.createModel(root, "deepseek-chat"); tm.Family != "" {
		t.Errorf("text 模型 family = %q，期望空串", tm.Family)
	}

	// text 不收族；族与 kind 不符也拒。
	resp = e.do("POST", "/admin/v1/models", root, `{"name":"t-fam","family":"ark_video"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "family_text_only" {
		t.Errorf("text 带族 error.code = %q，期望 family_text_only", got)
	}
	resp = e.do("POST", "/admin/v1/models", root, `{"name":"v-bad","kind":"video","family":"ark_image"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "family_invalid" {
		t.Errorf("video 配 image 族 error.code = %q，期望 family_invalid", got)
	}

	// 族建后不可改；回传现值放行（同 kind 先例）。
	resp = e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", vm.ID), root, `{"family":"ark_video"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := errCode(t, resp); got != "family_immutable" {
		t.Errorf("改族 error.code = %q，期望 family_immutable", got)
	}
	wantStatus(t, e.do("PATCH", fmt.Sprintf("/admin/v1/models/%d", vm.ID), root,
		`{"family":"minimax_video","disabled":true}`), http.StatusOK)

	// 兼容口径：不带 family 的 video 未声明，首条来源懒钉并落库，此后异面即拒。
	legacy := e.createModelKind(root, "seedance-legacy", "video")
	if legacy.Family != "" {
		t.Fatalf("未声明 video 的 family = %q，期望空串（未声明）", legacy.Family)
	}
	e.createSource(root, legacy.ID, fmt.Sprintf(`{"upstream_id":%d}`, ark.ID))
	found := false
	for _, m := range e.listModels(root) {
		if m.ID == legacy.ID {
			found = true
			if m.Family != "ark_video" {
				t.Errorf("懒钉后 family = %q，期望 ark_video", m.Family)
			}
		}
	}
	if !found {
		t.Errorf("列表中未见懒钉后的模型行")
	}
	resp = e.do("POST", fmt.Sprintf("/admin/v1/models/%d/sources", legacy.ID), root,
		fmt.Sprintf(`{"upstream_id":%d}`, mm.ID))
	wantStatus(t, resp, http.StatusConflict)
	if got := errCode(t, resp); got != "source_family_mismatch" {
		t.Errorf("懒钉后挂异面 error.code = %q，期望 source_family_mismatch", got)
	}
}
