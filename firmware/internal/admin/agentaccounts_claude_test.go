package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/claudeauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const claudeSetupFake = "fake-claude-setup-credential-never-real-0123456789"

func connectClaude(t *testing.T, e *agentEnv, token, label string) *http.Response {
	t.Helper()
	body := fmt.Sprintf(`{"setup_token":%q,"label":%q}`, token, label)
	r := e.req(http.MethodPost, "/admin/v1/agent-accounts/claude/setup-token", e.cookie, body)
	return e.send(r)
}

func TestClaudeSetupTokenAcceptsProxyHTTPOriginAndIsSealed(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	body := fmt.Sprintf(`{"setup_token":%q,"label":"Claude 主订阅"}`, claudeSetupFake)

	resp := e.do(http.MethodPost, "/admin/v1/agent-accounts/claude/setup-token", e.cookie, body)
	wantStatus(t, resp, http.StatusOK)
	responseBody := readAll(t, resp)
	if strings.Contains(responseBody, claudeSetupFake) || strings.Contains(responseBody, `"setup_token":`) {
		t.Fatal("连接响应泄露 setup-token")
	}
	acct, blob, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Provider != store.AgentProviderClaude || acct.Status != store.AgentStatusActive || acct.AccountID != "" {
		t.Fatalf("Claude 管理行不符: provider=%q status=%q account=%q", acct.Provider, acct.Status, acct.AccountID)
	}
	cred, err := claudeauth.Parse(blob)
	if err != nil || cred.Token() != claudeSetupFake {
		t.Fatalf("封存凭据无法还原: err=%v", err)
	}
	for _, haystack := range []string{e.buf.String(), fmt.Sprint(auditRows(t, e.dir))} {
		if strings.Contains(haystack, claudeSetupFake) {
			t.Fatal("日志或审计泄露 setup-token")
		}
	}
}

func TestClaudeReconnectReplacesCredentialAndRefreshIsUnsupported(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, connectClaude(t, e, claudeSetupFake, "Claude"), http.StatusOK)
	const replacement = "fake-claude-replacement-credential-never-real-9876543210"
	wantStatus(t, connectClaude(t, e, replacement, ""), http.StatusOK)

	acct, blob, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderClaude)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := claudeauth.Parse(blob)
	if err != nil || cred.Token() != replacement {
		t.Fatalf("重连后不是新凭据: err=%v", err)
	}
	if acct.Label != "Claude" {
		t.Fatalf("空名称应保留旧值，得到 %q", acct.Label)
	}

	tokens := &fakeAgentTokens{}
	e.srv.SetAgentTokens(tokens)
	refresh := e.do(http.MethodPost, fmt.Sprintf("/admin/v1/agent-accounts/%d/refresh", acct.ID), e.cookie, `{}`)
	wantStatus(t, refresh, http.StatusConflict)
	if code := errCode(t, refresh); code != "agent_refresh_unsupported" {
		t.Fatalf("refresh code=%q", code)
	}
	if tokens.count() != 0 {
		t.Fatal("Claude setup-token 不应进入 Codex/Grok 刷新器")
	}
}

func TestClaudeSetupTokenValidationNeverEchoesCredential(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	const bad = "fake credential with spaces"
	resp := connectClaude(t, e, bad, "")
	wantStatus(t, resp, http.StatusBadRequest)
	body := readAll(t, resp)
	var decoded struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, bad) || decoded.Error.Code != "invalid_setup_token" {
		t.Fatalf("非法凭据错误不安全: %s", body)
	}
}

// TestClaudeVisibleModelPersists 钉住连接页新给出的「对成员可见的模型」这条路：
// 它复用 default_model 落库（Claude 一路读作可见模型，收窄模型发现，
// 见 docs/firmware-agents-claude.md），重连保持既有值，PATCH 空串清空 = 不再收窄。
func TestClaudeVisibleModelPersists(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	body := fmt.Sprintf(`{"setup_token":%q,"label":"Claude 主订阅","default_model":"claude-fable-5"}`, claudeSetupFake)
	r := e.req(http.MethodPost, "/admin/v1/agent-accounts/claude/setup-token", e.cookie, body)
	wantStatus(t, e.send(r), http.StatusOK)

	visible := func() *store.AgentAccount {
		t.Helper()
		acct, _, err := e.st.GetAgentCredential(t.Context(), store.AgentProviderClaude)
		if err != nil {
			t.Fatal(err)
		}
		return acct
	}
	if got := visible().DefaultModel; got != "claude-fable-5" {
		t.Fatalf("可见模型未落库：%q", got)
	}

	// 重连不带该字段 = 保持既有值（Upsert 的「空即保持」）。
	wantStatus(t, connectClaude(t, e, claudeSetupFake, "Claude 主订阅"), http.StatusOK)
	acct := visible()
	if acct.DefaultModel != "claude-fable-5" {
		t.Fatalf("重连抹掉了可见模型：%q", acct.DefaultModel)
	}

	// PATCH 空串清空 = 回到「成员看得到全部」。
	path := fmt.Sprintf("/admin/v1/agent-accounts/%d", acct.ID)
	wantStatus(t, e.do(http.MethodPatch, path, e.cookie, `{"default_model":""}`), http.StatusOK)
	if got := visible().DefaultModel; got != "" {
		t.Fatalf("清空可见模型失败：%q", got)
	}
}
