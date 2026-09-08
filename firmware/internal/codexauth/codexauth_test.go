// codexauth_test.go：错误分类（两类失败两种反应的判据）与**真实端点探针**。
//
// 探针门控在 CODEX_LOGIN_PROBE=1，缺省 skip——全仓 `go test ./...` 必须离线可跑
// （受限现场、CI、smoke 都不出网）。它是包注释里那条结论（auth.openai.com 上
// 没有设备码流）的复验网：人工跑一次即可，OpenAI 哪天加回设备码流它会失败，
// 那时再回来改迭代计划决策 4。
package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestParseOAuthError 钉住两种错误体形态都要认。只按 RFC 解，实测那条 401 的
// 原因就会静默丢成空串，排障时只剩一个光秃秃的状态码。
func TestParseOAuthError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"RFC 扁平形", `{"error":"invalid_grant","error_description":"…"}`, "invalid_grant"},
		{"OpenAI 嵌套形", `{"error":{"message":"…","type":"invalid_request_error","code":"token_expired"}}`, "token_expired"},
		{"嵌套形无 code 退回 type", `{"error":{"message":"…","type":"invalid_request_error"}}`, "invalid_request_error"},
		{"没有 error 字段", `{"foo":"bar"}`, ""},
		{"不是 JSON", `<html>Just a moment…</html>`, ""},
		{"错误码里的杂质被剔掉", `{"error":"bad code\ninjected"}`, "badcodeinjected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseOAuthError([]byte(tc.body)); got != tc.want {
				t.Fatalf("parseOAuthError = %q, 想要 %q", got, tc.want)
			}
		})
	}
}

// TestOAuthErrorDeterministic 钉住决策 5 的判据：**带 OAuth 错误码的** 4xx 是
// 确定性拒绝（停手等管理员重新登录），5xx / 408 / 429 / 传输错是可重试
// （保留凭据下次再试）。
//
// 空错误码那一组是第二道闸：4xx 也可能根本不是签发方给的（Cloudflare 质询页、
// issuer 配错得到的 404、中间反代自造的 4xx），而落闩的代价是逼管理员重新走
// 一遍浏览器登录，方向必须偏向「宁可多试几次」。
func TestOAuthErrorDeterministic(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   bool
	}{
		{http.StatusBadRequest, "invalid_grant", true},
		{http.StatusUnauthorized, "token_expired", true},
		{http.StatusForbidden, "access_denied", true},
		{http.StatusRequestTimeout, "x", false},
		{http.StatusTooManyRequests, "x", false},
		{http.StatusInternalServerError, "x", false},
		{http.StatusBadGateway, "x", false},
		{http.StatusServiceUnavailable, "x", false},
		// 没有错误码 = 对面根本没说 OAuth：一律按可重试处理。
		{http.StatusForbidden, "", false},    // Cloudflare 质询页
		{http.StatusNotFound, "", false},     // issuer 配错
		{http.StatusUnauthorized, "", false}, // 前置反代挡下来的
	}
	for _, tc := range cases {
		err := error(&OAuthError{Phase: phaseRefresh, Status: tc.status, Code: tc.code})
		if got := Deterministic(err); got != tc.want {
			t.Errorf("HTTP %d（错误码 %q）: Deterministic = %v, 想要 %v", tc.status, tc.code, got, tc.want)
		}
	}
	// 非 OAuthError 一律按可重试处理：不确定就当能再试一次，这个方向是安全的。
	if Deterministic(errors.New("dial tcp: connection refused")) {
		t.Error("传输错不该判为确定性拒绝")
	}
	if Deterministic(nil) {
		t.Error("nil 不该判为确定性拒绝")
	}
}

// TestOAuthErrorMessage 钉住错误文本形态：一行、带阶段名与状态码，
// 不带厂商长文案（§15.1 的落实尺度）。
func TestOAuthErrorMessage(t *testing.T) {
	got := (&OAuthError{Phase: phaseRefresh, Status: 401, Code: "token_expired"}).Error()
	for _, want := range []string{phaseRefresh, "401", "token_expired"} {
		if !strings.Contains(got, want) {
			t.Errorf("错误文本 %q 里没有 %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("错误文本不该多行: %q", got)
	}
	if noCode := (&OAuthError{Phase: phaseExchange, Status: 500}).Error(); strings.Contains(noCode, "（）") {
		t.Errorf("无错误码时不该留空括号: %q", noCode)
	}
}

// TestCodexLoginProbe 是**出网**探针（缺省 skip）。
//
// 跑法：`CODEX_LOGIN_PROBE=1 go test ./internal/codexauth -run Probe -v`
// 全程只用假凭据，不需要 ChatGPT 账号，也不会在 OpenAI 侧留下任何状态。
func TestCodexLoginProbe(t *testing.T) {
	if os.Getenv("CODEX_LOGIN_PROBE") != "1" {
		t.Skip("出网探针默认不跑：设 CODEX_LOGIN_PROBE=1 手动执行")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ① 发现文档：设备码流不该在场。这两条是「决策 4 备选①升为主路径」的依据，
	// 哪天它们变了，本迭代的登录形态就该重新评估。
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		DefaultIssuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatalf("构造发现请求: %v", err)
	}
	resp, err := newHTTPClient(nil).Do(req)
	if err != nil {
		t.Fatalf("拉发现文档失败（受限现场无法出网？）: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, tokenRespCap))
	if err != nil {
		t.Fatalf("读发现文档: %v", err)
	}
	var disco struct {
		TokenEndpoint            string   `json:"token_endpoint"`
		AuthorizationEndpoint    string   `json:"authorization_endpoint"`
		DeviceAuthorizationEndpt string   `json:"device_authorization_endpoint"`
		GrantTypesSupported      []string `json:"grant_types_supported"`
	}
	if err := json.Unmarshal(raw, &disco); err != nil {
		t.Fatalf("发现文档不是 JSON: %v", err)
	}
	t.Logf("token_endpoint=%s authorization_endpoint=%s grant_types=%v",
		disco.TokenEndpoint, disco.AuthorizationEndpoint, disco.GrantTypesSupported)
	if disco.DeviceAuthorizationEndpt != "" {
		t.Errorf("OpenAI 现在有 device_authorization_endpoint=%s 了——"+
			"迭代 11 决策 4 的前提变了，回去重新评估登录形态", disco.DeviceAuthorizationEndpt)
	}
	for _, g := range disco.GrantTypesSupported {
		if strings.Contains(g, "device_code") {
			t.Errorf("OpenAI 现在支持 %s 了——同上，回去重新评估", g)
		}
	}
	if !slices.Contains(disco.GrantTypesSupported, "refresh_token") ||
		!slices.Contains(disco.GrantTypesSupported, "authorization_code") {
		t.Fatalf("本迭代赖以工作的两种 grant 不全: %v", disco.GrantTypesSupported)
	}

	// ② 令牌端点：拿一份**假** refresh token 打一次，断言它对普通 HTTP 客户端
	// 正常答复（不是 Cloudflare 质询页），且失败被判为确定性拒绝——刷新路径
	// 的两类失败分类就压在这条判据上。
	c := NewClient("")
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", "fake-refresh-token-not-real")
	form.Set("scope", Scope)
	_, err = c.postToken(ctx, form, phaseRefresh)
	var oe *OAuthError
	if !errors.As(err, &oe) {
		t.Fatalf("想要 OAuthError（说明端点正常答复了），得到 %v", err)
	}
	t.Logf("假 refresh token → HTTP %d，错误码 %q", oe.Status, oe.Code)
	if !oe.Deterministic() {
		t.Errorf("无效 refresh token 该判为确定性拒绝，得到 HTTP %d", oe.Status)
	}
	assertNoSecrets(t, err.Error())
}
