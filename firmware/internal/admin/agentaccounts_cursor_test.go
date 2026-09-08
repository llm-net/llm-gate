// agentaccounts_cursor_test.go 是迭代 3 Phase 4 的可执行验收：Cursor 订阅在
// 管理面的粘贴连接（离线封存，不出网验证）、重复连接整体替换、非法 API Key
// 形态的拒绝与不回显、自检对 cursor 放行并委托数据面（成功/被拒两路）、
// auth_expired 行启停拒绝的 cursor 专属文案，以及 §15.1——响应体、审计 detail
// 与日志里一个 Key 字节都不许出现。
package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/agentauth"
	"github.com/llm-net/llm-gate/firmware/internal/cursorauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const cursorKeyFake = "fake-cursor-dashboard-api-key-never-real-0123456789"

func connectCursor(t *testing.T, e *agentEnv, key, label string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"api_key":%q,"label":%q}`, key, label)
	return e.do(http.MethodPost, "/admin/v1/agent-accounts/cursor/api-key", e.cookie, body)
}

// providerAt 取第 n 次委托刷新时传给数据面的 provider（断言分流正确用）。
func (f *fakeAgentTokens) providerAt(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.calls) {
		return ""
	}
	return f.calls[n]
}

// TestCursorAPIKeyConnectIsSealedOffline 是主路径：粘贴 API Key → 校验、密封
// 落库、审计，全程**不出网**（同 claude setup-token 纪律，联网验证留给自检）。
func TestCursorAPIKeyConnectIsSealedOffline(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	body := fmt.Sprintf(`{"api_key":%q,"label":"Cursor 主订阅"}`, cursorKeyFake)

	resp := e.do(http.MethodPost, "/admin/v1/agent-accounts/cursor/api-key", e.cookie, body)
	wantStatus(t, resp, http.StatusOK)
	responseBody := readAll(t, resp)
	if strings.Contains(responseBody, cursorKeyFake) || strings.Contains(responseBody, "api_key") {
		t.Fatal("连接响应泄露 API Key")
	}
	if e.issuer.count() != 0 {
		t.Fatal("粘贴连接不该出网：exchange 验证是自检与数据面的事")
	}
	acct, blob, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderCursor)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Provider != store.AgentProviderCursor || acct.Status != store.AgentStatusActive || acct.AccountID != "" {
		t.Fatalf("Cursor 管理行不符: provider=%q status=%q account=%q", acct.Provider, acct.Status, acct.AccountID)
	}
	if acct.Label != "Cursor 主订阅" || acct.DefaultModel != "" {
		t.Fatalf("连接后 label=%q default_model=%q", acct.Label, acct.DefaultModel)
	}
	cred, err := cursorauth.Parse(blob)
	if err != nil || cred.Token() != cursorKeyFake {
		t.Fatalf("封存凭据无法还原: err=%v", err)
	}
	rows := auditRows(t, e.dir)
	var connects int
	for _, row := range rows {
		if row.event != admin.EventAgentConnect {
			continue
		}
		connects++
		if !strings.Contains(row.detail, "provider=cursor") || !strings.Contains(row.detail, "source=api_key") {
			t.Fatalf("连接审计 detail 不符: %s", row.detail)
		}
	}
	if connects != 1 {
		t.Fatalf("agent.connect 审计 = %d 条，期望 1", connects)
	}
	for _, haystack := range []string{e.buf.String(), fmt.Sprint(rows)} {
		if strings.Contains(haystack, cursorKeyFake) {
			t.Fatal("日志或审计泄露 API Key")
		}
	}
}

// Cursor 模型由 cursor-agent 自行发现；连接和通用编辑端点都不得重新造出
// 一个不会被代理执行的 default_model 配置。
func TestCursorRejectsDefaultModel(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	body := fmt.Sprintf(`{"api_key":%q,"label":"Cursor","default_model":"cursor-fake-model"}`, cursorKeyFake)
	resp := e.do(http.MethodPost, "/admin/v1/agent-accounts/cursor/api-key", e.cookie, body)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := readAll(t, resp); !strings.Contains(got, "invalid_default_model") || !strings.Contains(got, "自行发现") {
		t.Fatalf("连接端点的拒绝不符: %s", got)
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("拒绝 default_model 后仍落了 %d 行", len(got))
	}

	wantStatus(t, connectCursor(t, e, cursorKeyFake, "Cursor"), http.StatusOK)
	id := e.list()[0].ID
	resp = e.do(http.MethodPatch, fmt.Sprintf("/admin/v1/agent-accounts/%d", id), e.cookie,
		`{"default_model":"cursor-fake-model"}`)
	wantStatus(t, resp, http.StatusBadRequest)
	if got := readAll(t, resp); !strings.Contains(got, "invalid_default_model") {
		t.Fatalf("编辑端点的拒绝不符: %s", got)
	}
}

// TestCursorReconnectReplacesCredential 钉住单账户语义：重复连接是整体替换
// 而不是增行，且不带 label 时保持管理员配置（Upsert 的「空即保持」）。
func TestCursorReconnectReplacesCredential(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, connectCursor(t, e, cursorKeyFake, "Cursor"), http.StatusOK)
	const replacement = "fake-cursor-replacement-api-key-never-real-9876543210"
	wantStatus(t, connectCursor(t, e, replacement, ""), http.StatusOK)

	if accounts := e.list(); len(accounts) != 1 {
		t.Fatalf("重复连接后 = %d 行，期望 1（同 provider 覆盖）", len(accounts))
	}
	acct, blob, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderCursor)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := cursorauth.Parse(blob)
	if err != nil || cred.Token() != replacement {
		t.Fatalf("重连后不是新凭据: err=%v", err)
	}
	if acct.Label != "Cursor" {
		t.Fatalf("空名称应保留旧值，得到 %q", acct.Label)
	}
}

// TestCursorAPIKeyValidationNeverEchoesCredential 是形态校验的拒绝路：中间带
// 空白/控制字符/空串一律 400 invalid_api_key，且响应与日志都不回显原文（§15.1）。
func TestCursorAPIKeyValidationNeverEchoesCredential(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	for _, bad := range []string{"fake key with spaces", "fake\tkey-with-control", ""} {
		resp := connectCursor(t, e, bad, "")
		wantStatus(t, resp, http.StatusBadRequest)
		body := readAll(t, resp)
		if bad != "" && strings.Contains(body, bad) {
			t.Fatalf("非法凭据被回显: %s", body)
		}
		if !strings.Contains(body, "invalid_api_key") {
			t.Fatalf("错误码不符: %s", body)
		}
		if bad != "" && strings.Contains(e.buf.String(), bad) {
			t.Fatal("日志回显了非法凭据")
		}
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("校验失败却落了 %d 行", len(got))
	}
}

// TestCursorRefreshDelegatesToDataPlane 钉住自检对 cursor 放行：不像 claude
// 那样 409，而是委托数据面 RefreshAgent（那边 = 重新 exchange 一次）。三路
// 结论各自成形——成功把 auth_expired 带回 active、被上游确定性拒绝回 409、
// 出网失败回 502 且指名 api2.cursor.sh。
func TestCursorRefreshDelegatesToDataPlane(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, connectCursor(t, e, cursorKeyFake, "Cursor"), http.StatusOK)
	id := e.list()[0].ID
	path := fmt.Sprintf("/admin/v1/agent-accounts/%d/refresh", id)
	tokens := &fakeAgentTokens{}
	e.srv.SetAgentTokens(tokens)

	// 成功：委托一次、provider 传的是 cursor，且 auth_expired 回 active。
	if err := e.st.SetAgentStatus(context.Background(), id, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp := e.do(http.MethodPost, path, e.cookie, `{}`)
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Status != store.AgentStatusActive {
		t.Fatalf("自检成功后 status = %q，期望 active", out.Account.Status)
	}
	if tokens.count() != 1 || tokens.providerAt(0) != store.AgentProviderCursor {
		t.Fatalf("委托次数 = %d，provider = %q，期望 1 次 cursor", tokens.count(), tokens.providerAt(0))
	}

	// 被上游确定性拒绝（exchange 401/403 → agentauth.ErrAuthExpired 一族）：
	// 409 的恢复文案是 cursor 专属的「重新粘贴 API Key」——cursor 没有登录可失效。
	tokens.err = fmt.Errorf("包一层：%w", agentauth.ErrAuthExpired)
	resp = e.do(http.MethodPost, path, e.cookie, `{}`)
	wantStatus(t, resp, http.StatusConflict)
	body := readAll(t, resp)
	if !strings.Contains(body, "agent_auth_expired") || !strings.Contains(body, "Cursor") ||
		!strings.Contains(body, "API Key") {
		t.Fatalf("确定性拒绝的应答不符: %s", body)
	}

	// 出网失败是故障：502 并指名 api2.cursor.sh（不是 auth.openai.com）。
	tokens.err = fmt.Errorf("换发 Cursor 订阅 token 失败：连接超时")
	resp = e.do(http.MethodPost, path, e.cookie, `{}`)
	wantStatus(t, resp, http.StatusBadGateway)
	body = readAll(t, resp)
	if !strings.Contains(body, "agent_upstream_unreachable") || !strings.Contains(body, "api2.cursor.sh") {
		t.Fatalf("出网失败的应答不符: %s", body)
	}

	// 审计：三次自检各一条，detail 只有 provider 与结论词，无 Key。
	var results []string
	for _, row := range auditRows(t, e.dir) {
		if row.event != admin.EventAgentRefresh {
			continue
		}
		if !strings.Contains(row.detail, "provider=cursor") {
			t.Fatalf("自检审计 detail 少了 provider: %s", row.detail)
		}
		if strings.Contains(row.detail, cursorKeyFake) {
			t.Fatalf("自检审计 detail 泄露 Key: %s", row.detail)
		}
		results = append(results, row.detail)
	}
	if len(results) != 3 ||
		!strings.Contains(results[0], "result=ok") ||
		!strings.Contains(results[1], "result=auth_expired") ||
		!strings.Contains(results[2], "result=unreachable") {
		t.Fatalf("自检审计不符: %v", results)
	}
}

// TestCursorAuthExpiredToggleRefused 钉住启停拒绝的 cursor 专属文案：失效的
// 恢复路是重新粘贴 API Key 或自检（重新 exchange），不是重新登录。
func TestCursorAuthExpiredToggleRefused(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, connectCursor(t, e, cursorKeyFake, "Cursor"), http.StatusOK)
	id := e.list()[0].ID
	if err := e.st.SetAgentStatus(context.Background(), id, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	for _, body := range []string{`{"disabled":true}`, `{"disabled":false}`} {
		resp := e.do(http.MethodPatch, fmt.Sprintf("/admin/v1/agent-accounts/%d", id), e.cookie, body)
		wantStatus(t, resp, http.StatusConflict)
		got := readAll(t, resp)
		if !strings.Contains(got, "agent_auth_expired") || !strings.Contains(got, "API Key") {
			t.Fatalf("cursor 失效行启停应答不符: %s", got)
		}
	}
}
