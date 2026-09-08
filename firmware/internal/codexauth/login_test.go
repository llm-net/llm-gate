// login_test.go：授权码 + PKCE + 粘贴回调地址那条路（决策 4 备选①，已升为主路径）。
//
// 浏览器那半段（管理员在 OpenAI 页面上登录授权）没法在测试里跑——它要一个真
// ChatGPT 账号。这里钉住的是盒子这半段：authorize URL 的参数、PKCE 挑战与
// verifier 的对应关系、换码请求的形态，以及粘贴入口对人手复制的容错与该拒的拒。
package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestNewLoginAuthorizeURL 钉住 authorize URL 的参数集。
//
// id_token_add_organizations 是**载荷参数**：少了它 id_token 就不带
// chatgpt_account_id，代理注入 ChatGPT-Account-ID 那一步会拿到空值，
// 整条代理路 401——而现象离病因很远，所以要在这里钉住。
func TestNewLoginAuthorizeURL(t *testing.T) {
	c := NewClient("")
	login, err := c.NewLogin()
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	u, err := url.Parse(login.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize URL 不合法: %v", err)
	}
	if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != authorizePath {
		t.Fatalf("authorize URL 指向了别处: %s", login.AuthorizeURL)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             ClientID,
		"redirect_uri":          RedirectURI,
		"scope":                 Scope,
		"code_challenge_method": "S256",
		paramAddOrganizations:   "true",
		paramSimplifiedFlow:     "true",
		"state":                 login.State,
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("%s = %q, 想要 %q", k, got, v)
		}
	}
	// PKCE：URL 里带的是 verifier 的 SHA-256，verifier 本身不出盒子。
	sum := sha256.Sum256([]byte(login.verifier))
	if got, want := q.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Errorf("code_challenge 与 verifier 对不上")
	}
	if strings.Contains(login.AuthorizeURL, login.verifier) {
		t.Error("authorize URL 里不该出现 code_verifier 原文")
	}
	if len(login.verifier) < 43 || len(login.verifier) > 128 {
		t.Errorf("code_verifier 长度 %d 超出 RFC 7636 的 43–128", len(login.verifier))
	}
	// 两次登录不该撞：state 与 verifier 都是每次现生成的。
	other, err := c.NewLogin()
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	if other.State == login.State || other.verifier == login.verifier {
		t.Error("两次登录会话的随机数撞了")
	}
}

// TestExchangeAuthCode 走完盒子这半段：假令牌端点核对换码请求的每一项
// （含 code_verifier 与 authorize 时那个挑战的对应关系），回一份令牌，
// 断言落成一份形态正确的 auth.json 句柄。
func TestExchangeAuthCode(t *testing.T) {
	var challenge string
	ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
		writeTokens(w, "fake-access-new", "fake-refresh-new", fakeIDToken(t, "acct_fake_login"), 3600)
	})
	c := NewClient(ts.srv.URL)
	login, err := c.NewLogin()
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	challenge = mustQuery(t, login.AuthorizeURL).Get("code_challenge")

	callback := "http://localhost:1455/auth/callback?code=fake-auth-code&state=" + url.QueryEscape(login.State)
	auth, err := c.ExchangeAuthCode(context.Background(), login, callback)
	if err != nil {
		t.Fatalf("ExchangeAuthCode: %v", err)
	}

	form := ts.lastForm()
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("code") != "fake-auth-code" {
		t.Errorf("code = %q", form.Get("code"))
	}
	// redirect_uri 换码时要原样再报一次，且必须与 authorize 时逐字节一致。
	if form.Get("redirect_uri") != RedirectURI {
		t.Errorf("redirect_uri = %q", form.Get("redirect_uri"))
	}
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != challenge {
		t.Errorf("换码带的 code_verifier 与 authorize 时的挑战对不上")
	}

	if auth.AccessToken() != "fake-access-new" || auth.refreshToken != "fake-refresh-new" {
		t.Fatalf("句柄里不是新令牌")
	}
	if auth.AccountID() != "acct_fake_login" {
		t.Errorf("account_id = %q，想要从 id_token claim 解出来的那个", auth.AccountID())
	}
	// 造出来的 blob 要能被自己解回去（管理面拿它去密封落库）。
	blob, err := auth.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if _, err := ParseAuthJSON(blob); err != nil {
		t.Fatalf("造出来的 auth.json 自己解不开: %v", err)
	}
	if !strings.Contains(blob, `"OPENAI_API_KEY":null`) {
		t.Errorf("造出来的 auth.json 与 codex 的形态不一致: %s", blob)
	}
}

// TestExchangeAuthCodeRejects 钉住该拒的都拒，且**在打令牌端点之前就拒**——
// state 对不上还去换码，等于把攻击者塞过来的授权码拿去认领账号。
func TestExchangeAuthCodeRejects(t *testing.T) {
	newLogin := func(t *testing.T, c *Client) *Login {
		t.Helper()
		l, err := c.NewLogin()
		if err != nil {
			t.Fatalf("NewLogin: %v", err)
		}
		return l
	}
	cases := []struct {
		name     string
		callback string
		mutate   func(*Login)
		wantIn   string
	}{
		{"空输入", "  ", nil, "整条回调地址"},
		{"不像 URL", "我点了确认", nil, "整条"},
		{"没有 code", "http://localhost:1455/auth/callback?state=x", nil, "没有 code"},
		{"state 不匹配", "http://localhost:1455/auth/callback?code=c&state=someone-else", nil, "state"},
		{"回调自带 error", "http://localhost:1455/auth/callback?error=access_denied&error_description=x", nil, "access_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTokenServer(t, func(int64, http.ResponseWriter) {
				t.Error("不该打令牌端点")
			})
			c := NewClient(ts.srv.URL)
			login := newLogin(t, c)
			if tc.mutate != nil {
				tc.mutate(login)
			}
			_, err := c.ExchangeAuthCode(context.Background(), login, tc.callback)
			if err == nil {
				t.Fatal("想要报错")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("错误文本 %q 里没有 %q", err.Error(), tc.wantIn)
			}
			if ts.calls.Load() != 0 {
				t.Errorf("拒绝前不该打令牌端点")
			}
		})
	}

	t.Run("会话过期", func(t *testing.T) {
		ts := newTokenServer(t, func(int64, http.ResponseWriter) { t.Error("不该打令牌端点") })
		c := NewClient(ts.srv.URL)
		login := newLogin(t, c)
		c.now = func() time.Time { return login.CreatedAt.Add(LoginTTL + time.Second) }
		_, err := c.ExchangeAuthCode(context.Background(),
			login, "http://localhost:1455/auth/callback?code=c&state="+login.State)
		if err == nil || !strings.Contains(err.Error(), "重新发起登录") {
			t.Fatalf("想要过期错误，得到 %v", err)
		}
	})

	t.Run("会话不存在", func(t *testing.T) {
		c := NewClient("")
		if _, err := c.ExchangeAuthCode(context.Background(), nil, "http://x/?code=c"); err == nil {
			t.Fatal("想要报错")
		}
	})

	t.Run("没有 refresh_token 的应答要拒", func(t *testing.T) {
		ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
			writeTokens(w, "fake-access-new", "", "", 3600)
		})
		c := NewClient(ts.srv.URL)
		login := newLogin(t, c)
		_, err := c.ExchangeAuthCode(context.Background(), login,
			"http://localhost:1455/auth/callback?code=c&state="+login.State)
		if err == nil || !strings.Contains(err.Error(), "refresh_token") {
			t.Fatalf("想要拒绝无 refresh_token 的句柄，得到 %v", err)
		}
	})
}

// TestExchangeAuthCodeTolerantPaste：这是一条**人手复制**的路，
// 首尾空白与缺 scheme 的写法都要认——认不出来的代价是管理员反复试，
// 而这里没有任何安全上的理由苛刻（state 照样核对）。
func TestExchangeAuthCodeTolerantPaste(t *testing.T) {
	for _, name := range []string{"带首尾空白", "没有 scheme", "尾巴带片段"} {
		t.Run(name, func(t *testing.T) {
			ts := newTokenServer(t, func(n int64, w http.ResponseWriter) {
				writeTokens(w, "fake-access-new", "fake-refresh-new", "", 3600)
			})
			c := NewClient(ts.srv.URL)
			login, err := c.NewLogin()
			if err != nil {
				t.Fatalf("NewLogin: %v", err)
			}
			raw := "http://localhost:1455/auth/callback?code=fake-auth-code&state=" + login.State
			switch name {
			case "带首尾空白":
				raw = "\n  " + raw + "  \n"
			case "没有 scheme":
				raw = strings.TrimPrefix(raw, "http://")
			case "尾巴带片段":
				raw += "#_=_"
			}
			if _, err := c.ExchangeAuthCode(context.Background(), login, raw); err != nil {
				t.Fatalf("ExchangeAuthCode(%q): %v", raw, err)
			}
			// state 与 code 都必须原样送到端点：片段/空白不该粘在值上。
			if got := ts.lastForm().Get("code"); got != "fake-auth-code" {
				t.Fatalf("code = %q，被粘上了别的东西", got)
			}
		})
	}
}

func mustQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析 URL: %v", err)
	}
	return u.Query()
}
