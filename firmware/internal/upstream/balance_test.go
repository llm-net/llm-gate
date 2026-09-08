package upstream_test

// balance_test.go 覆盖 2026-08-09 的两笔扩展：
//
//   - openai_compat 的端点语义：无内置端点、经 base_url 只服务 openai_chat
//     单协议（anthropic 与视频/图片一律不进候选）；没给 base_url 什么都不服务。
//   - 平台余额查询：支持矩阵（仅 deepseek）、请求形状（GET /user/balance +
//     Bearer 注入）、成功解析、错误摘要、2xx 不可解析如实报失败。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// TestEndpointForOpenAICompat：openai_compat 与 mock 同属无内置端点的类型，
// 但协议集更窄——只有 openai_chat。
func TestEndpointForOpenAICompat(t *testing.T) {
	if _, ok := upstream.EndpointFor(config.UpstreamOpenAICompat, config.ProtocolOpenAIChat); ok {
		t.Error("openai_compat 不应有内置端点")
	}

	// 没给 base_url：什么协议都不服务（写入侧必填校验的运行时对应面）。
	bare := upstream.Account{Name: "oc", Type: config.UpstreamOpenAICompat, APIKey: "test-key-not-real"}
	for _, proto := range []string{
		config.ProtocolOpenAIChat, config.ProtocolAnthropicMessages,
		config.ProtocolMinimaxVideo,
	} {
		if _, ok := bare.Endpoint(proto); ok {
			t.Errorf("无 base_url 的 openai_compat 不应服务 %s", proto)
		}
	}

	// 带 base_url：仅 openai_chat（尾随斜杠剥除，URL 拼接同 mock）。
	acct := upstream.Account{Name: "oc", Type: config.UpstreamOpenAICompat, APIKey: "test-key-not-real", BaseURL: "http://127.0.0.1:18081/v1/"}
	got, ok := acct.URL(config.ProtocolOpenAIChat, "/chat/completions")
	if !ok || got != "http://127.0.0.1:18081/v1/chat/completions" {
		t.Errorf("openai_chat URL = %q ok=%v", got, ok)
	}
	for _, proto := range []string{
		config.ProtocolAnthropicMessages,
		config.ProtocolMinimaxVideo,
	} {
		if got, ok := acct.Endpoint(proto); ok {
			t.Errorf("openai_compat 不应服务 %s，得到 %q", proto, got)
		}
	}
}

// TestSupportsBalance：余额支持矩阵——只有 deepseek 有平台余额端点；方舟系
// 的余额在火山引擎账号层（AK/SK 签名）、百炼 Token Plan 的套餐余量在阿里云
// 账号层、OpenCode Go 的额度在 Zen 控制台、minimax 无公开端点、通用 openai_compat
// 不知道平台，都恒为否。
func TestSupportsBalance(t *testing.T) {
	if !upstream.SupportsBalance(config.UpstreamDeepseek) {
		t.Error("deepseek 应支持余额查询")
	}
	for _, typ := range []string{
		config.UpstreamArk, config.UpstreamArkPlan, config.UpstreamQwenPlan, config.UpstreamOpenCodeGo,
		config.UpstreamMinimax, config.UpstreamOpenAICompat, config.UpstreamMock,
		"no-such-type",
	} {
		if upstream.SupportsBalance(typ) {
			t.Errorf("SupportsBalance(%s) = true，期望 false", typ)
		}
	}
}

// balanceServer 起一个断言请求形状的假余额端点。
func balanceServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
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
	return srv, &auth
}

func balanceCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestQueryBalanceDeepseek：快乐路径——GET /user/balance、Bearer 注入、
// deepseek 形态解析（金额字符串原样带回）。
func TestQueryBalanceDeepseek(t *testing.T) {
	srv, auth := balanceServer(t, http.StatusOK,
		`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`)
	acct := upstream.Account{Name: "ds", Type: config.UpstreamDeepseek, APIKey: "test-key-not-real", BaseURL: srv.URL}

	res := upstream.QueryBalance(balanceCtx(t), srv.Client(), acct)
	if !res.OK || res.Status != http.StatusOK || res.Message != "" || !res.Available {
		t.Fatalf("结果异常: %+v", res)
	}
	if res.LatencyMS < 1 {
		t.Errorf("LatencyMS = %d，期望 ≥1", res.LatencyMS)
	}
	if len(res.Balances) != 1 {
		t.Fatalf("Balances 条数 = %d，期望 1", len(res.Balances))
	}
	b := res.Balances[0]
	if b.Currency != "CNY" || b.Total != "110.00" || b.Granted != "10.00" || b.ToppedUp != "100.00" {
		t.Errorf("金额读数异常: %+v", b)
	}
	if *auth != "Bearer test-key-not-real" {
		t.Errorf("余额请求未注入 Bearer 凭证")
	}
}

// TestQueryBalanceErrors：上游 401 提取 error.message 摘要；2xx 但形态不认识
// 时如实报失败（不编造读数）；不支持的类型走防御分支。
func TestQueryBalanceErrors(t *testing.T) {
	srv401, _ := balanceServer(t, http.StatusUnauthorized,
		`{"error":{"message":"Authentication Fails, Your api key is invalid","type":"authentication_error"}}`)
	acct := upstream.Account{Name: "ds", Type: config.UpstreamDeepseek, APIKey: "test-key-not-real", BaseURL: srv401.URL}
	res := upstream.QueryBalance(balanceCtx(t), srv401.Client(), acct)
	if res.OK || res.Status != http.StatusUnauthorized || !strings.Contains(res.Message, "api key is invalid") {
		t.Errorf("401 结果异常: %+v", res)
	}

	srvBad, _ := balanceServer(t, http.StatusOK, `<html>not a balance</html>`)
	acct.BaseURL = srvBad.URL
	res = upstream.QueryBalance(balanceCtx(t), srvBad.Client(), acct)
	if res.OK || res.Status != http.StatusOK || res.Message == "" || len(res.Balances) != 0 {
		t.Errorf("不可解析 2xx 应如实报失败: %+v", res)
	}

	mock := upstream.Account{Name: "mk", Type: config.UpstreamMock, APIKey: "test-key-not-real", BaseURL: srv401.URL}
	res = upstream.QueryBalance(balanceCtx(t), srv401.Client(), mock)
	if res.OK || res.Message == "" {
		t.Errorf("不支持的类型应报说明性失败: %+v", res)
	}
}
