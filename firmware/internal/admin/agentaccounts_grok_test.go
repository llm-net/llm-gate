// agentaccounts_grok_test.go：订阅接入管理面的 Grok Build 半边——设备码流的
// 三段（start 出 user_code / pending 时会话保留、可反复点 / 批准后落库）、
// 粘贴 ~/.grok/auth.json 导入、provider 校验、与 codex 行共存（一 provider
// 一行，互不相干）。
//
// codex 半边与共性（权限边界、PATCH/DELETE/refresh、审计泄露 grep）在
// agentaccounts_test.go，这里只测 grok 的分岔点。夹具全部是假凭据。
package admin_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/grokauth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	grokTestAccess  = "fake-grok-admin-access"
	grokTestRefresh = "fake-grok-admin-refresh"
	grokTestEmail   = "boss@example.invalid"
)

// grokAdminAuthJSON 造一份形如 ~/.grok/auth.json 的句柄（含一条要被拒收的
// API密钥 记录，验证导入只取订阅那条）。
func grokAdminAuthJSON(access, refresh string) string {
	return fmt.Sprintf(`{"https://auth.x.ai::%s":{"key":%q,"refresh_token":%q,`+
		`"auth_mode":"oidc","email":%q},"xai::api_key":{"key":"xai-fake","auth_mode":"api_key"}}`,
		grokauth.ClientID, access, refresh, grokTestEmail)
}

// grokIssuerStub 是假 auth.x.ai：设备码端点恒发码；令牌端点按 mode 应答。
type grokIssuerStub struct {
	url string
	mu  sync.Mutex
	// mode ∈ pending | slow_down | denied | grant
	mode       string
	tokenCalls int
}

func newGrokIssuerStub(t *testing.T) *grokIssuerStub {
	t.Helper()
	s := &grokIssuerStub{mode: "pending"}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth2/device/code", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if got := r.PostForm.Get("client_id"); got != grokauth.ClientID {
			t.Errorf("device/code client_id = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"device_code":"fake-admin-device-code","user_code":"FAKE-CODE",`+
			`"verification_uri":"https://accounts.x.ai/activate",`+
			`"verification_uri_complete":"https://accounts.x.ai/activate?user_code=FAKE-CODE",`+
			`"expires_in":600,"interval":5}`)
	})
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.tokenCalls++
		mode := s.mode
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "grant":
			id := grokTestIDToken()
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"id_token":%q,`+
				`"token_type":"Bearer","expires_in":3600}`, grokTestAccess, grokTestRefresh, id)
		case "denied":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"access_denied"}`)
		case "slow_down":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"slow_down"}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.url = srv.URL
	return s
}

func (s *grokIssuerStub) setMode(m string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = m
}

func grokTestIDToken() string {
	payload, _ := json.Marshal(map[string]any{"sub": "user-grok-admin", "email": grokTestEmail})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

type grokAdminEnv struct {
	*env
	cookie string
	issuer *grokIssuerStub
}

func newGrokAdminEnv(t *testing.T) *grokAdminEnv {
	t.Helper()
	e := newEnv(t)
	cookie := e.rootSession()
	iss := newGrokIssuerStub(t)
	e.srv.SetGrokIssuer(iss.url)
	return &grokAdminEnv{env: e, cookie: cookie, issuer: iss}
}

func (g *grokAdminEnv) startGrok() (userCode, verificationURI string) {
	g.t.Helper()
	resp := g.do("POST", "/admin/v1/agent-accounts/login/start", g.cookie, `{"provider":"grok"}`)
	wantStatus(g.t, resp, http.StatusOK)
	var out struct {
		Provider                string `json:"provider"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		AuthorizeURL            string `json:"authorize_url"`
	}
	decodeInto(g.t, resp, &out)
	if out.Provider != "grok" || out.UserCode == "" || out.VerificationURI == "" || out.ExpiresIn <= 0 {
		g.t.Fatalf("grok start 应答不全: %+v", out)
	}
	if out.AuthorizeURL != "" {
		g.t.Fatalf("grok start 不该有 authorize_url（那是 codex 的形态）")
	}
	return out.UserCode, out.VerificationURI
}

func (g *grokAdminEnv) completeGrok() *http.Response {
	g.t.Helper()
	return g.do("POST", "/admin/v1/agent-accounts/login/callback", g.cookie, `{"provider":"grok"}`)
}

// TestGrokAgentLoginFlow：设备码主路径。start 拿码（一次出网）→ 没批准时
// callback 回 409 pending 且**会话保留**（再点一次就是下一次尝试）→ slow_down
// 同样保留 → 批准后 callback 换出句柄、落库、审计；与 codex 行互不相干。
func TestGrokAgentLoginFlow(t *testing.T) {
	g := newGrokAdminEnv(t)
	userCode, _ := g.startGrok()
	if userCode != "FAKE-CODE" {
		t.Fatalf("user_code = %q", userCode)
	}

	// 还没批准：pending，可反复点。
	resp := g.completeGrok()
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_login_pending" {
		t.Fatalf("code = %q，期望 agent_login_pending", code)
	}
	g.issuer.setMode("slow_down")
	resp = g.completeGrok()
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_login_pending" {
		t.Fatalf("slow_down code = %q，期望 agent_login_pending", code)
	}

	// 批准之后：一次 POST 落库。
	g.issuer.setMode("grant")
	resp = g.completeGrok()
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Provider != store.AgentProviderGrok || out.Account.Status != "active" {
		t.Fatalf("落库行 = %+v", out.Account)
	}
	if out.Account.AccountID != grokTestEmail {
		t.Fatalf("account_id = %q，期望 email 优先 %q", out.Account.AccountID, grokTestEmail)
	}

	// 会话已消费：再点是「尚未发起」。
	resp = g.completeGrok()
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_login_not_started" {
		t.Fatalf("code = %q，期望 agent_login_not_started", code)
	}

	// 与 codex 行共存：一 provider 一行。
	body := fmt.Sprintf(`{"auth_json":%q,"label":"codex 那份","default_model":""}`,
		agentAuthJSON(agentAccess, agentRefresh, agentAcctID))
	wantStatus(t, g.do("POST", "/admin/v1/agent-accounts/import", g.cookie, body), http.StatusOK)
	list := g.listAccounts()
	if len(list) != 2 {
		t.Fatalf("列表 = %d 行，期望 codex + grok 两行", len(list))
	}

	// 审计与响应不泄令牌（§15.1）。
	for _, row := range auditRows(t, g.dir) {
		for _, leak := range []string{grokTestAccess, grokTestRefresh, "fake-admin-device-code"} {
			if strings.Contains(row.detail, leak) {
				t.Errorf("审计 detail 泄露 %q: %s", leak, row.detail)
			}
		}
	}
}

// TestGrokAgentLoginRejected：access_denied 是确定性拒绝——清会话、明说重来。
func TestGrokAgentLoginRejected(t *testing.T) {
	g := newGrokAdminEnv(t)
	g.startGrok()
	g.issuer.setMode("denied")
	resp := g.completeGrok()
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "agent_login_rejected" {
		t.Fatalf("code = %q，期望 agent_login_rejected", code)
	}
	// 会话已清：再点是「尚未发起」，不是再打一次令牌端点。
	resp = g.completeGrok()
	wantStatus(t, resp, http.StatusConflict)
	if code := errCode(t, resp); code != "agent_login_not_started" {
		t.Fatalf("code = %q，期望 agent_login_not_started", code)
	}
}

// TestGrokAgentImport：粘贴 ~/.grok/auth.json 兜底——只收订阅记录；
// 只有 API密钥 登录的文件被拒并把话说明白。
func TestGrokAgentImport(t *testing.T) {
	g := newGrokAdminEnv(t)
	body := fmt.Sprintf(`{"provider":"grok","auth_json":%q,"label":"我的 Grok","default_model":"grok-4.5"}`,
		grokAdminAuthJSON(grokTestAccess, grokTestRefresh))
	resp := g.do("POST", "/admin/v1/agent-accounts/import", g.cookie, body)
	wantStatus(t, resp, http.StatusOK)
	var out struct {
		Account agentDTO `json:"account"`
	}
	decodeInto(t, resp, &out)
	if out.Account.Provider != "grok" || out.Account.AccountID != grokTestEmail ||
		out.Account.DefaultModel != "grok-4.5" {
		t.Fatalf("导入行 = %+v", out.Account)
	}

	apiOnly := `{"xai::api_key":{"key":"xai-fake","auth_mode":"api_key"}}`
	body = fmt.Sprintf(`{"provider":"grok","auth_json":%q}`, apiOnly)
	resp = g.do("POST", "/admin/v1/agent-accounts/import", g.cookie, body)
	wantStatus(t, resp, http.StatusBadRequest)
	if code := errCode(t, resp); code != "invalid_auth_json" {
		t.Fatalf("code = %q，期望 invalid_auth_json", code)
	}
}

// TestAgentProviderValidation：provider 词汇表在管理面把守（列上刻意无 CHECK）。
// 空 = codex（grok 加入前的调用形态照旧成立），未知值 400。
func TestAgentProviderValidation(t *testing.T) {
	g := newGrokAdminEnv(t)
	for _, body := range []string{`{"provider":"claude"}`, `{"provider":"GROK"}`} {
		resp := g.do("POST", "/admin/v1/agent-accounts/login/start", g.cookie, body)
		wantStatus(t, resp, http.StatusBadRequest)
		if code := errCode(t, resp); code != "agent_provider_invalid" {
			t.Fatalf("provider 体 %s 的 code = %q，期望 agent_provider_invalid", body, code)
		}
	}
	// 空 body 与空 provider 都落 codex：start 是本地动作、回 authorize_url。
	for _, body := range []string{"", "{}", `{"provider":""}`} {
		resp := g.do("POST", "/admin/v1/agent-accounts/login/start", g.cookie, body)
		wantStatus(t, resp, http.StatusOK)
		var out struct {
			Provider     string `json:"provider"`
			AuthorizeURL string `json:"authorize_url"`
		}
		decodeInto(t, resp, &out)
		if out.Provider != "codex" || out.AuthorizeURL == "" {
			t.Fatalf("body %q → %+v，期望 codex + authorize_url", body, out)
		}
	}
}

// listAccounts 与 agentEnv.list 同形（本文件的环境没有那个方法可继承）。
func (g *grokAdminEnv) listAccounts() []agentDTO {
	g.t.Helper()
	resp := g.do("GET", "/admin/v1/agent-accounts", g.cookie, "")
	wantStatus(g.t, resp, http.StatusOK)
	var out struct {
		Accounts []agentDTO `json:"accounts"`
	}
	decodeInto(g.t, resp, &out)
	return out.Accounts
}
