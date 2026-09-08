// agentaccounts_test.go 是迭代 11 Phase 4 的可执行验收：Agents 订阅账号管理面
// 的权限边界（非 admin 一律 403）、授权码+PKCE 登录的两步一次 POST（start 出
// authorize URL、callback 核 state 换码落库，全程无后台协程、无读态 GET）、
// 三种粘错各回明确 4xx、重复 start 顶掉旧会话、粘贴 auth.json 兜底、
// 改名/启停/删除/自检，以及 §15.1——响应体、审计 detail 与日志里一个令牌字节
// 都不许出现。
package admin_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/admin"
	"github.com/llm-net/llm-gate/firmware/internal/codexauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 夹具令牌。**测试断言的核心之一就是这三个串一个都不许出现在响应/审计/日志里**，
// 所以取成一眼能 grep 出来的形状。
const (
	agentAccess  = "codex-access-token-fixture"
	agentRefresh = "codex-refresh-token-fixture"
	agentIDToken = "codex-id-token-fixture"
	agentAcctID  = "acct-codex-0123456789"
)

// agentAuthJSON 造一份形如 ~/.codex/auth.json 的句柄。
func agentAuthJSON(access, refresh, accountID string) string {
	return fmt.Sprintf(`{"OPENAI_API_KEY":null,"tokens":{"access_token":%q,"refresh_token":%q,`+
		`"id_token":%q,"account_id":%q},"last_refresh":"2026-08-11T00:00:00Z"}`,
		access, refresh, agentIDToken, accountID)
}

// ---- 假签发方（auth.openai.com 的令牌端点） ----

type agentIssuer struct {
	url string
	mu  sync.Mutex
	// forms 是每次换码/刷新提交的表单（断言 grant_type/code_verifier 用）。
	forms []url.Values
	reply http.HandlerFunc
}

// newAgentIssuer 起一个只认 POST /oauth/token 的假签发方。
func newAgentIssuer(t *testing.T, reply http.HandlerFunc) *agentIssuer {
	t.Helper()
	iss := &agentIssuer{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		iss.mu.Lock()
		iss.forms = append(iss.forms, r.PostForm)
		iss.mu.Unlock()
		iss.reply(w, r)
	}))
	t.Cleanup(srv.Close)
	iss.url = srv.URL
	return iss
}

func (i *agentIssuer) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.forms)
}

func (i *agentIssuer) form(n int) url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	if n >= len(i.forms) {
		return nil
	}
	return i.forms[n]
}

// issuerGrants 回一份完整句柄（换码成功）。
func issuerGrants(access, refresh string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":%q,`+
			`"expires_in":3600,"token_type":"Bearer"}`, access, refresh, agentIDToken)
	}
}

// issuerRejects 回一个 OAuth 确定性拒绝（授权码过期/已用过就是这个形状）。
func issuerRejects() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"authorization code expired"}`))
	}
}

// ---- 假令牌持有方（生产实现是 gateway.Server） ----

type fakeAgentTokens struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeAgentTokens) RefreshAgent(_ context.Context, provider string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, provider)
	return f.err
}

func (f *fakeAgentTokens) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// ---- 用例环境 ----

type agentDTO struct {
	ID            int64   `json:"id"`
	Provider      string  `json:"provider"`
	Label         string  `json:"label"`
	AccountID     string  `json:"account_id"`
	DefaultModel  string  `json:"default_model"`
	Status        string  `json:"status"`
	LastRefreshAt *string `json:"last_refresh_at"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

// agentEnv 是「管理员已登录 + 假签发方已接线」的环境。
type agentEnv struct {
	*env
	cookie string
	issuer *agentIssuer
}

func newAgentAdminEnv(t *testing.T, reply http.HandlerFunc) *agentEnv {
	t.Helper()
	e := newEnv(t)
	cookie := e.rootSession()
	iss := newAgentIssuer(t, reply)
	e.srv.SetAgentIssuer(iss.url)
	return &agentEnv{env: e, cookie: cookie, issuer: iss}
}

// startLogin 发起登录并返回 authorize URL 里的 state（管理员的浏览器看得到的
// 就是这条 URL，测试从中取 state 与真人从地址栏取回调地址是同一份信息）。
func (a *agentEnv) startLogin() (authorizeURL, state string) {
	a.t.Helper()
	resp := a.do("POST", "/admin/v1/agent-accounts/login/start", a.cookie, "{}")
	wantStatus(a.t, resp, http.StatusOK)
	var out struct {
		AuthorizeURL string `json:"authorize_url"`
		ExpiresIn    int    `json:"expires_in"`
		RedirectURI  string `json:"redirect_uri"`
	}
	decodeInto(a.t, resp, &out)
	u, err := url.Parse(out.AuthorizeURL)
	if err != nil {
		a.t.Fatalf("authorize_url 不是合法 URL: %v", err)
	}
	q := u.Query()
	for _, key := range []string{"client_id", "redirect_uri", "code_challenge", "state", "scope"} {
		if q.Get(key) == "" {
			a.t.Fatalf("authorize_url 缺少参数 %s：%s", key, out.AuthorizeURL)
		}
	}
	if q.Get("code_challenge_method") != "S256" {
		a.t.Fatalf("code_challenge_method = %q，期望 S256", q.Get("code_challenge_method"))
	}
	if out.RedirectURI != codexauth.RedirectURI {
		a.t.Fatalf("redirect_uri = %q，期望 %q", out.RedirectURI, codexauth.RedirectURI)
	}
	if out.ExpiresIn <= 0 {
		a.t.Fatalf("expires_in = %d，期望正数", out.ExpiresIn)
	}
	return out.AuthorizeURL, q.Get("state")
}

// callback 把一条回调地址粘回面板。
func (a *agentEnv) callback(callbackURL string) *http.Response {
	a.t.Helper()
	body := fmt.Sprintf(`{"callback_url":%q}`, callbackURL)
	return a.do("POST", "/admin/v1/agent-accounts/login/callback", a.cookie, body)
}

// callbackURLFor 造一条浏览器会停在的回调地址。
func callbackURLFor(code, state string) string {
	return codexauth.RedirectURI + "?code=" + url.QueryEscape(code) + "&state=" + url.QueryEscape(state)
}

func (a *agentEnv) list() []agentDTO {
	a.t.Helper()
	resp := a.do("GET", "/admin/v1/agent-accounts", a.cookie, "")
	wantStatus(a.t, resp, http.StatusOK)
	var out struct {
		Accounts []agentDTO `json:"accounts"`
	}
	decodeInto(a.t, resp, &out)
	return out.Accounts
}

// importAuth 走「粘贴 auth.json」兜底入口。
func (a *agentEnv) importAuth(blob, label, model string) *http.Response {
	a.t.Helper()
	body := fmt.Sprintf(`{"auth_json":%q,"label":%q,"default_model":%q}`, blob, label, model)
	return a.do("POST", "/admin/v1/agent-accounts/import", a.cookie, body)
}

// agentAuditRow / auditRows 直读审计表（与板上走查的 sqlite3 同视角）。
type agentAuditRow struct{ event, entity, detail string }

func auditRows(t *testing.T, dir string) []agentAuditRow {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, store.DBFileName))
	if err != nil {
		t.Fatalf("打开审计视角连接: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT event, entity, detail FROM audit_events ORDER BY id`)
	if err != nil {
		t.Fatalf("查询审计表: %v", err)
	}
	defer rows.Close()
	var out []agentAuditRow
	for rows.Next() {
		var a agentAuditRow
		if err := rows.Scan(&a.event, &a.entity, &a.detail); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历审计行: %v", err)
	}
	return out
}

// ---- 权限边界 ----

// TestAgentAccountsRequireSession 钉住「/admin/v1/* 除登录外一律要会话」：
// Agents 那一族每一条路由无会话都是 401，而不是 404 或者半路才发现没权限。
func TestAgentAccountsRequireSession(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))

	cases := []struct{ method, path, body string }{
		{"GET", "/admin/v1/agent-accounts", ""},
		{"POST", "/admin/v1/agent-accounts/login/start", "{}"},
		{"POST", "/admin/v1/agent-accounts/login/callback", `{"callback_url":"x"}`},
		{"POST", "/admin/v1/agent-accounts/import", `{"auth_json":"{}"}`},
		{"POST", "/admin/v1/agent-accounts/claude/setup-token", `{"setup_token":"fake"}`},
		{"POST", "/admin/v1/agent-accounts/claude/login/start", `{}`},
		{"POST", "/admin/v1/agent-accounts/claude/login/callback", `{"code":"fake"}`},
		{"POST", "/admin/v1/agent-accounts/cursor/api-key", `{"api_key":"fake"}`},
		{"PATCH", "/admin/v1/agent-accounts/1", `{"label":"x"}`},
		{"DELETE", "/admin/v1/agent-accounts/1", ""},
		{"POST", "/admin/v1/agent-accounts/1/refresh", "{}"},
	}
	for _, c := range cases {
		for _, cookie := range []string{"", "forged-session-token"} {
			resp := e.do(c.method, c.path, cookie, c.body)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s（cookie=%q）状态 = %d，期望 401", c.method, c.path, cookie, resp.StatusCode)
			}
			if code := errCode(t, resp); code != "unauthorized" {
				t.Fatalf("%s %s code = %q，期望 unauthorized", c.method, c.path, code)
			}
		}
	}
}

// ---- 登录：两步一次 POST ----

// TestAgentLoginFlow 是主路径：start 出 authorize URL → 管理员粘回回调地址 →
// 这一次 POST 里换码 + 密封落库 + 审计全部走完，列表立刻有一行。
func TestAgentLoginFlow(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	if got := e.list(); len(got) != 0 {
		t.Fatalf("初始列表 = %d 行，期望 0", len(got))
	}

	_, state := e.startLogin()
	if e.issuer.count() != 0 {
		t.Fatal("start 不该出网：authorize 端点是给管理员的浏览器打开的")
	}

	resp := e.callback(callbackURLFor("auth-code-1", state))
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Provider != store.AgentProviderCodex || out.Account.Status != store.AgentStatusActive {
		t.Fatalf("连接后 provider=%q status=%q，期望 codex/active", out.Account.Provider, out.Account.Status)
	}

	// 换码请求的形态：授权码流 + PKCE，redirect_uri 原样再报一次。
	form := e.issuer.form(0)
	if form.Get("grant_type") != "authorization_code" {
		t.Fatalf("grant_type = %q，期望 authorization_code", form.Get("grant_type"))
	}
	if form.Get("code") != "auth-code-1" {
		t.Fatalf("code = %q，期望 auth-code-1", form.Get("code"))
	}
	if form.Get("code_verifier") == "" {
		t.Fatal("换码没有带 code_verifier：PKCE 形同虚设")
	}
	if form.Get("redirect_uri") != codexauth.RedirectURI {
		t.Fatalf("redirect_uri = %q，期望 %q", form.Get("redirect_uri"), codexauth.RedirectURI)
	}

	// 落库的是**能解回来的**句柄：取令牌视图解封后拿到的就是刚才那一代。
	acct, authJSON, err := e.st.GetAgentCredential(context.Background(), store.AgentProviderCodex)
	if err != nil {
		t.Fatalf("GetAgentCredential: %v", err)
	}
	if !strings.Contains(authJSON, agentAccess) || !strings.Contains(authJSON, agentRefresh) {
		t.Fatal("落库的句柄里没有刚换来的那一代令牌")
	}
	if acct.ID != out.Account.ID {
		t.Fatalf("落库行 id=%d，响应 id=%d", acct.ID, out.Account.ID)
	}

	// 会话已被消费：同一条回调地址再粘一次不该还能换码。
	again := e.callback(callbackURLFor("auth-code-1", state))
	wantStatus(t, again, http.StatusConflict)
	if code := errCode(t, again); code != "agent_login_not_started" {
		t.Fatalf("重复粘贴 code = %q，期望 agent_login_not_started", code)
	}
	if e.issuer.count() != 1 {
		t.Fatalf("换码次数 = %d，期望 1（授权码单次消费，不该再打一次）", e.issuer.count())
	}
}

// TestAgentLoginCallbackErrors 是三种「粘错了」各自的明确 4xx：还没发起登录、
// state 对不上、回调地址自己带 error=。都不出网——错在这条 URL 上，去打令牌
// 端点只会换回一句更难懂的话。
//
// 第四种「会话过期」不在这里：它要拨快时钟，而管理面没有可注入的时钟。判定本身
// 是 codexauth.Login.Expired 一个方法，那边用假时钟测过（login_test.go 的
// 「会话过期」子用例），本层只是照着它分岔。
func TestAgentLoginCallbackErrors(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))

	resp := e.callback(callbackURLFor("c", "s"))
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_login_not_started" {
		t.Fatalf("未 start 就 callback code = %q", code)
	}

	_, state := e.startLogin()

	resp = e.callback(callbackURLFor("c", state+"-tampered"))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "agent_login_failed" {
		t.Fatalf("state 不符 code = %q，期望 agent_login_failed", code)
	}

	resp = e.callback(codexauth.RedirectURI + "?error=access_denied&state=" + state)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "agent_login_failed" {
		t.Fatalf("回调带 error= 时 code = %q", code)
	}

	resp = e.callback("这不是一条地址")
	wantStatus(t, resp, http.StatusBadRequest)

	if e.issuer.count() != 0 {
		t.Fatalf("换码次数 = %d，期望 0：这四种都错在粘回来的地址上，不该出网", e.issuer.count())
	}
	// 会话仍在：改一处再粘一次就该过，不必重新发起登录。
	ok := e.callback(callbackURLFor("auth-code-2", state))
	wantStatus(t, ok, http.StatusOK)
}

// TestAgentLoginRejected 是 OpenAI 明确拒绝换码（授权码过期/已用过）：机读码
// 与上面那组分开，因为下一步不同——只能重新发起登录。
func TestAgentLoginRejected(t *testing.T) {
	e := newAgentAdminEnv(t, issuerRejects())
	_, state := e.startLogin()
	resp := e.callback(callbackURLFor("stale-code", state))
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "agent_login_rejected" {
		t.Fatalf("code = %q，期望 agent_login_rejected", code)
	}
	if got := e.list(); len(got) != 0 {
		t.Fatalf("换码失败却落了 %d 行", len(got))
	}
}

// TestAgentLoginRestartSupersedes 钉住「重复 start 顶掉旧会话」：拿第一次的
// state 去粘必须失败，拿第二次的必须成功。
func TestAgentLoginRestartSupersedes(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	_, first := e.startLogin()
	_, second := e.startLogin()
	if first == second {
		t.Fatal("两次 start 拿到同一个 state：会话没有重新生成")
	}
	resp := e.callback(callbackURLFor("c", first))
	wantStatus(t, resp, http.StatusBadRequest)

	resp = e.callback(callbackURLFor("c", second))
	wantStatus(t, resp, http.StatusOK)
}

// ---- 粘贴 auth.json 兜底 ----

func TestAgentImport(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))

	resp := e.importAuth(agentAuthJSON(agentAccess, agentRefresh, agentAcctID), "我的 Codex", "gpt-5-codex")
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Label != "我的 Codex" || out.Account.DefaultModel != "gpt-5-codex" {
		t.Fatalf("导入后 label=%q default_model=%q", out.Account.Label, out.Account.DefaultModel)
	}
	if out.Account.AccountID != agentAcctID {
		t.Fatalf("account_id = %q，期望 %q", out.Account.AccountID, agentAcctID)
	}
	if out.Account.LastRefreshAt != nil {
		t.Fatal("刚导入的账号不该有 last_refresh_at（从未由本盒刷新过）")
	}
	if e.issuer.count() != 0 {
		t.Fatal("导入不该出网")
	}

	// 形态不合的一律 400，且错误文本**不回显原文**（§15.1）。
	bad := []string{`{"tokens":{"access_token":"a"}}`, `{}`, `not json`, ``}
	for _, blob := range bad {
		resp := e.importAuth(blob, "", "")
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != "invalid_auth_json" {
			t.Fatalf("blob %q 的 code = %q，期望 invalid_auth_json", blob, code)
		}
	}

	// 单账户语义：再导入一次是覆盖而不是增行，且**不带 label 时保持原名**。
	resp = e.importAuth(agentAuthJSON("access-2", "refresh-2", agentAcctID), "", "")
	wantStatus(t, resp, http.StatusOK)
	accounts := e.list()
	if len(accounts) != 1 {
		t.Fatalf("重复连接后 = %d 行，期望 1（同 provider 覆盖）", len(accounts))
	}
	if accounts[0].Label != "我的 Codex" || accounts[0].DefaultModel != "gpt-5-codex" {
		t.Fatalf("重连抹掉了管理员的配置：label=%q default_model=%q",
			accounts[0].Label, accounts[0].DefaultModel)
	}
	_, authJSON, err := e.st.GetAgentCredential(context.Background(), store.AgentProviderCodex)
	if err != nil {
		t.Fatalf("GetAgentCredential: %v", err)
	}
	if !strings.Contains(authJSON, "refresh-2") {
		t.Fatal("重复连接没有换掉凭据")
	}
}

// TestAgentProfileValidation 钉住默认模型的形态：它会被写进用户电脑的
// ~/.codex/config.toml，一个引号或换行就让 codex 起不来。
func TestAgentProfileValidation(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	blob := agentAuthJSON(agentAccess, agentRefresh, agentAcctID)
	for _, model := range []string{"gpt 5", "gpt\n5", "gpt\"5", "gpt$(id)5", "gpt`id`5"} {
		resp := e.importAuth(blob, "", model)
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != "invalid_default_model" {
			t.Fatalf("default_model %q 的 code = %q", model, code)
		}
	}
	resp := e.importAuth(blob, strings.Repeat("名", 200), "")
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_label" {
		t.Fatalf("超长 label 的 code = %q", code)
	}
}

// ---- PATCH / DELETE ----

func TestAgentPatch(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, e.importAuth(agentAuthJSON(agentAccess, agentRefresh, agentAcctID), "初名", "gpt-5-codex"),
		http.StatusOK)
	id := e.list()[0].ID
	path := fmt.Sprintf("/admin/v1/agent-accounts/%d", id)

	patch := func(body string) agentDTO {
		t.Helper()
		resp := e.do("PATCH", path, e.cookie, body)
		wantStatus(t, resp, http.StatusOK)
		var out struct {
			Account agentDTO `json:"account"`
		}
		decodeInto(t, resp, &out)
		return out.Account
	}

	// 缺省 = 不变（只改一项时另一项必须原样保留）。
	got := patch(`{"label":"新名"}`)
	if got.Label != "新名" || got.DefaultModel != "gpt-5-codex" {
		t.Fatalf("只改 label 却动了别的：label=%q default_model=%q", got.Label, got.DefaultModel)
	}
	// null 与空串都清空——Upsert 的「空即保持」在这条路上表达不出清空。
	got = patch(`{"label":null}`)
	if got.Label != "" {
		t.Fatalf("null 没有清空 label：%q", got.Label)
	}
	got = patch(`{"default_model":""}`)
	if got.DefaultModel != "" {
		t.Fatalf("空串没有清空 default_model：%q", got.DefaultModel)
	}
	// 启停走同一条 PATCH。
	got = patch(`{"disabled":true}`)
	if got.Status != store.AgentStatusDisabled {
		t.Fatalf("停用后 status = %q", got.Status)
	}
	got = patch(`{"disabled":false}`)
	if got.Status != store.AgentStatusActive {
		t.Fatalf("启用后 status = %q", got.Status)
	}
	// 类型错拒绝。
	resp := e.do("PATCH", path, e.cookie, `{"label":123}`)
	wantStatus(t, resp, http.StatusBadRequest)

	// auth_expired 不许被启停覆盖：先改 disabled 再启用会丢掉失效闩，
	// 让管理台显示正常、而每个请求照旧撞上游 401。
	if err := e.st.SetAgentStatus(context.Background(), id, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp = e.do("PATCH", path, e.cookie, `{"disabled":false}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_auth_expired" {
		t.Fatalf("启用失效行 code = %q，期望 agent_auth_expired", code)
	}
	resp = e.do("PATCH", path, e.cookie, `{"disabled":true}`)
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_auth_expired" {
		t.Fatalf("停用失效行 code = %q，期望 agent_auth_expired", code)
	}

	// 不存在的 id 一律 404。
	resp = e.do("PATCH", "/admin/v1/agent-accounts/9999", e.cookie, `{"label":"x"}`)
	wantStatus(t, resp, http.StatusNotFound)
}

// TestAgentDelete 删掉订阅之后，取令牌那条路（/v1/responses 每请求走的正是它）
// 立刻回 ErrNotFound——数据面据此回 agent_not_configured，没有缓存要失效。
func TestAgentDelete(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, e.importAuth(agentAuthJSON(agentAccess, agentRefresh, agentAcctID), "x", ""), http.StatusOK)
	id := e.list()[0].ID

	resp := e.do("DELETE", fmt.Sprintf("/admin/v1/agent-accounts/%d", id), e.cookie, "")
	wantStatus(t, resp, http.StatusNoContent)
	if got := e.list(); len(got) != 0 {
		t.Fatalf("删除后仍有 %d 行", len(got))
	}
	if _, _, err := e.st.GetAgentCredential(context.Background(), store.AgentProviderCodex); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后 GetAgentCredential err = %v，期望 ErrNotFound", err)
	}
	resp = e.do("DELETE", fmt.Sprintf("/admin/v1/agent-accounts/%d", id), e.cookie, "")
	wantStatus(t, resp, http.StatusNotFound)
}

// ---- 自检 ----

// TestAgentRefresh 钉住三件事：刷新委托给数据面（本包不自己造 Provider——
// 那会成为第二个刷新者，与数据面互废世代）、成功时把 auth_expired 带回 active、
// 两类失败两种反应。
func TestAgentRefresh(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	wantStatus(t, e.importAuth(agentAuthJSON(agentAccess, agentRefresh, agentAcctID), "x", ""), http.StatusOK)
	id := e.list()[0].ID
	path := fmt.Sprintf("/admin/v1/agent-accounts/%d/refresh", id)

	// 未接线：说实话（503），不假装刷过。
	resp := e.do("POST", path, e.cookie, "{}")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if code := errCode(t, resp); code != "agent_refresh_unavailable" {
		t.Fatalf("未接线 code = %q", code)
	}

	tokens := &fakeAgentTokens{}
	e.srv.SetAgentTokens(tokens)

	// 成功：委托了一次，且 auth_expired 回 active。
	if err := e.st.SetAgentStatus(context.Background(), id, store.AgentStatusAuthExpired); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp = e.do("POST", path, e.cookie, "{}")
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Status != store.AgentStatusActive {
		t.Fatalf("自检成功后 status = %q，期望 active", out.Account.Status)
	}
	if tokens.count() != 1 {
		t.Fatalf("委托给数据面的次数 = %d，期望 1", tokens.count())
	}
	if e.issuer.count() != 0 {
		t.Fatal("管理面自己打了令牌端点：那就是第二个刷新者，正是决策 1 要消灭的形态")
	}

	// 停用行不因一次自检被推翻。
	if err := e.st.SetAgentStatus(context.Background(), id, store.AgentStatusDisabled); err != nil {
		t.Fatalf("SetAgentStatus: %v", err)
	}
	resp = e.do("POST", path, e.cookie, "{}")
	wantStatus(t, resp, http.StatusOK)
	decodeInto(t, resp, &out)
	if out.Account.Status != store.AgentStatusDisabled {
		t.Fatalf("自检把停用行改成了 %q", out.Account.Status)
	}

	// 确定性拒绝是**状态**：409 指向重新连接。
	tokens.err = fmt.Errorf("包一层：%w", codexauth.ErrAuthExpired)
	resp = e.do("POST", path, e.cookie, "{}")
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_auth_expired" {
		t.Fatalf("确定性拒绝 code = %q", code)
	}

	// 出网失败是**故障**：502 并说清是连不上（决策 9，受限现场要能看懂）。
	tokens.err = errors.New("刷新 Codex 订阅凭据失败：连接 OpenAI 超时")
	resp = e.do("POST", path, e.cookie, "{}")
	wantStatus(t, resp, http.StatusBadGateway)
	if code := errCode(t, resp); code != "agent_upstream_unreachable" {
		t.Fatalf("出网失败 code = %q", code)
	}
}

// ---- §15.1 ----

// TestAgentNoSecretLeak 是本 phase 的脱敏绊线：响应体、审计 detail 与日志
// （debug 级别——脱敏在最宽松级别也必须成立）里一个令牌字节都不许出现。
func TestAgentNoSecretLeak(t *testing.T) {
	e := newAgentAdminEnv(t, issuerGrants(agentAccess, agentRefresh))
	_, state := e.startLogin()
	resp := e.callback(callbackURLFor("auth-code-1", state))
	wantStatus(t, resp, http.StatusOK)
	connected := readAll(t, resp)

	blob := agentAuthJSON("imported-access-token", "imported-refresh-token", agentAcctID)
	imported := readAll(t, e.importAuth(blob, "我的 Codex", "gpt-5-codex"))
	listed := readAll(t, e.do("GET", "/admin/v1/agent-accounts", e.cookie, ""))

	secrets := []string{
		agentAccess, agentRefresh, agentIDToken,
		"imported-access-token", "imported-refresh-token",
		"auth_json", "auth_json_sealed",
	}
	for _, body := range []string{connected, imported, listed} {
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Fatalf("响应体泄露了 %q：%s", s, body)
			}
		}
	}

	// 审计：连接事件在，detail 只有 provider/名称/账号末段/状态。
	rows := auditRows(t, e.dir)
	var connects int
	for _, row := range rows {
		if row.event != admin.EventAgentConnect {
			continue
		}
		connects++
		for _, s := range secrets {
			if strings.Contains(row.detail, s) {
				t.Fatalf("审计 detail 泄露了 %q：%s", s, row.detail)
			}
		}
		if strings.Contains(row.detail, agentAcctID) {
			t.Fatalf("审计 detail 记了完整 account_id（应只记末段）：%s", row.detail)
		}
		if !strings.Contains(row.detail, "provider=codex") {
			t.Fatalf("审计 detail 少了 provider：%s", row.detail)
		}
	}
	if connects != 2 {
		t.Fatalf("agent.connect 审计 = %d 条，期望 2（登录一次 + 导入一次）", connects)
	}

	logs := e.buf.String()
	for _, s := range secrets[:5] {
		if strings.Contains(logs, s) {
			t.Fatalf("日志泄露了 %q", s)
		}
	}
	// code_verifier 也是密钥物料：它只该活在 codexauth.Login 的未导出字段里。
	if strings.Contains(logs, "code_verifier") || strings.Contains(connected, "code_verifier") {
		t.Fatal("code_verifier 出现在日志或响应里")
	}
}
