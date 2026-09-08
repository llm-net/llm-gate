// authjson_test.go 是迭代 11 Phase 2 可执行验收的一半：auth.json 的
// round-trip 保真、account_id 从 id_token claim 提取、形态最小校验，
// 以及 §15.1 的自遮蔽（%v / %+v / %#v / slog / json.Marshal 五条路都不该吐出令牌）。
//
// 本文件里的令牌**全是假的**（AGENTS.md：仓库内一切文件只用假凭据）。
// JWT 只有前两段是真格式，签名段是占位——本包本就不验签。
package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 假令牌：足够长、形状像真的，但都不是任何真实凭据。
const (
	fakeAccess  = "fake-access-token-AAAA1111"
	fakeRefresh = "fake-refresh-token-BBBB2222"
	fakeAccount = "acct_fake_0001"
)

// fakeJWT 造一份只有载荷段可解的 JWT（不签名——本包不验签）。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + ".fake-signature"
}

// fakeIDToken 造一份带 chatgpt_account_id 命名空间 claim 的 id_token。
func fakeIDToken(t *testing.T, accountID string) string {
	t.Helper()
	return fakeJWT(t, map[string]any{
		"sub": "user_fake",
		idTokenAccountClaim: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  "plus",
		},
	})
}

// TestParseAuthJSONRoundTrip 钉住保真回写：本包不认识的键（顶层的
// OPENAI_API_KEY、tokens 里的厂商新字段）必须原样活过一次 parse→JSON。
// 这条是刷新路径的安全网——每刷新一次就重新密封落库一次，丢字段是不可逆的。
func TestParseAuthJSONRoundTrip(t *testing.T) {
	idToken := fakeIDToken(t, fakeAccount)
	blob := fmt.Sprintf(`{
		"OPENAI_API_KEY": null,
		"future_field": {"kept": true},
		"tokens": {
			"id_token": %q,
			"access_token": %q,
			"refresh_token": %q,
			"account_id": %q,
			"future_token_field": "keep-me"
		},
		"last_refresh": "2026-08-11T01:02:03Z"
	}`, idToken, fakeAccess, fakeRefresh, fakeAccount)

	a, err := ParseAuthJSON(blob)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	if a.AccessToken() != fakeAccess {
		t.Fatalf("access_token = %q", a.AccessToken())
	}
	if a.refreshToken != fakeRefresh {
		t.Fatalf("refresh_token 未解出")
	}
	if a.AccountID() != fakeAccount {
		t.Fatalf("account_id = %q, 想要 %q", a.AccountID(), fakeAccount)
	}
	if got := a.LastRefresh().UTC().Format(time.RFC3339); got != "2026-08-11T01:02:03Z" {
		t.Fatalf("last_refresh = %q", got)
	}

	out, err := a.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("回写的 auth.json 不是合法 JSON: %v", err)
	}
	if string(got["OPENAI_API_KEY"]) != "null" {
		t.Errorf("OPENAI_API_KEY 丢了: %s", got["OPENAI_API_KEY"])
	}
	if string(got["future_field"]) != `{"kept":true}` {
		t.Errorf("未知顶层字段丢了: %s", got["future_field"])
	}
	var tokens map[string]string
	if err := json.Unmarshal(got["tokens"], &tokens); err != nil {
		t.Fatalf("tokens 段: %v", err)
	}
	if tokens["future_token_field"] != "keep-me" {
		t.Errorf("tokens 里的未知字段丢了: %v", tokens["future_token_field"])
	}

	// 二次解析必须与首次逐项相同（回写没有偷偷改语义）。
	again, err := ParseAuthJSON(out)
	if err != nil {
		t.Fatalf("二次 ParseAuthJSON: %v", err)
	}
	if again.AccessToken() != a.AccessToken() || again.refreshToken != a.refreshToken ||
		again.idToken != a.idToken || again.AccountID() != a.AccountID() ||
		!again.LastRefresh().Equal(a.LastRefresh()) {
		t.Fatalf("round-trip 后字段不一致")
	}
	// 幂等：再回写一次应当逐字节相同（map marshal 按键排序）。
	out2, err := again.JSON()
	if err != nil {
		t.Fatalf("二次 JSON: %v", err)
	}
	if out2 != out {
		t.Fatalf("回写不幂等:\n%s\n%s", out, out2)
	}
}

// TestParseAuthJSONAccountIDSources 钉住 account_id 的两个来源与优先级：
// 显式 tokens.account_id 优先，缺了才去解 id_token 的 claim。
func TestParseAuthJSONAccountIDSources(t *testing.T) {
	t.Run("从 id_token claim 提取", func(t *testing.T) {
		blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":%q}}`,
			fakeAccess, fakeRefresh, fakeIDToken(t, "acct_fake_from_claim"))
		a, err := ParseAuthJSON(blob)
		if err != nil {
			t.Fatalf("ParseAuthJSON: %v", err)
		}
		if a.AccountID() != "acct_fake_from_claim" {
			t.Fatalf("account_id = %q", a.AccountID())
		}
	})

	t.Run("显式 account_id 优先", func(t *testing.T) {
		blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":%q,"account_id":"acct_fake_explicit"}}`,
			fakeAccess, fakeRefresh, fakeIDToken(t, "acct_fake_from_claim"))
		a, err := ParseAuthJSON(blob)
		if err != nil {
			t.Fatalf("ParseAuthJSON: %v", err)
		}
		if a.AccountID() != "acct_fake_explicit" {
			t.Fatalf("account_id = %q", a.AccountID())
		}
	})

	t.Run("id_token 不可解时为空且不 panic", func(t *testing.T) {
		blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":"not-a-jwt"}}`,
			fakeAccess, fakeRefresh)
		a, err := ParseAuthJSON(blob)
		if err != nil {
			t.Fatalf("ParseAuthJSON: %v", err)
		}
		if a.AccountID() != "" {
			t.Fatalf("account_id = %q, 想要空", a.AccountID())
		}
	})
}

// TestParseAuthJSONRejectsBadShape 钉住最小形态校验，并顺带钉住
// §15.1：拒绝的错误文本里不得出现令牌原文。
func TestParseAuthJSONRejectsBadShape(t *testing.T) {
	cases := []struct {
		name string
		blob string
	}{
		{"空", "  "},
		{"不是 JSON", "not json at all"},
		{"缺 tokens 段", fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, fakeAccess, fakeRefresh)},
		{"tokens 不是对象", `{"tokens":"nope"}`},
		{"缺 refresh_token", fmt.Sprintf(`{"tokens":{"access_token":%q}}`, fakeAccess)},
		{"缺 access_token", fmt.Sprintf(`{"tokens":{"refresh_token":%q}}`, fakeRefresh)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAuthJSON(tc.blob)
			if err == nil {
				t.Fatalf("想要报错，得到 %v", a)
			}
			assertNoSecrets(t, err.Error())
		})
	}
}

// TestAuthMasksCredentials 钉住 §15.1 的自遮蔽：把 Auth 直接塞进 fmt 的常见
// 写法（含走 GoStringer 的 %#v，指针与解引用值都试）、slog 与 json.Marshal，
// 都不该吐出令牌。这是「顺手 %+v 进日志」这类事故的唯一一道机械防线。
func TestAuthMasksCredentials(t *testing.T) {
	blob := fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":%q,"id_token":%q,"account_id":%q}}`,
		fakeAccess, fakeRefresh, fakeIDToken(t, fakeAccount), fakeAccount)
	a, err := ParseAuthJSON(blob)
	if err != nil {
		t.Fatalf("ParseAuthJSON: %v", err)
	}
	enc, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, s := range []string{
		fmt.Sprintf("%v", a),
		fmt.Sprintf("%+v", a),
		fmt.Sprintf("%v", *a),
		fmt.Sprintf("%+v", *a),
		fmt.Sprintf("%#v", a),
		fmt.Sprintf("%#v", *a),
		a.LogValue().String(),
		string(enc),
	} {
		assertNoSecrets(t, s)
		if !strings.Contains(s, fakeAccount) && !strings.Contains(s, "{}") {
			// account_id 是明文值，遮蔽后仍应可见（它正是用来认账号的）；
			// json.Marshal 那条路没有导出字段，得到 {} 也算过。
			t.Errorf("遮蔽过头，连 account_id 都看不到了: %s", s)
		}
	}
}

// TestLoginMasksVerifier：登录会话带 code_verifier，同样不许被 fmt 与 slog
// 带出去（%#v 走 GoStringer，单独在列）。
func TestLoginMasksVerifier(t *testing.T) {
	login, err := NewClient("").NewLogin()
	if err != nil {
		t.Fatalf("NewLogin: %v", err)
	}
	for _, s := range []string{
		fmt.Sprintf("%v", login),
		fmt.Sprintf("%+v", *login),
		fmt.Sprintf("%#v", *login),
		login.LogValue().String(),
	} {
		if strings.Contains(s, login.verifier) {
			t.Fatalf("code_verifier 泄漏进了格式化输出: %s", s)
		}
	}
}

// assertNoSecrets 断言一段将被人看到（错误文本、日志、格式化输出）的字符串里
// 没有任何假令牌的痕迹。
func assertNoSecrets(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{fakeAccess, fakeRefresh} {
		if strings.Contains(s, secret) {
			t.Fatalf("输出里含令牌原文: %s", s)
		}
	}
}
