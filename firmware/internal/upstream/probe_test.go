package upstream

// probe_test.go 覆盖探测的构造与摘要口径：请求形状（路径/方法/头/体）、
// 2xx 与非 2xx 的结果字段、上游错误体三种形态的摘要提取、传输层错误的归类
// 与去冗（url.Error 的完整 URL 外层剥掉，地址只在 dial 原因里出现一次），
// 以及摘要的单行化与长度截断。
//
// 端到端（经管理端点、含审计与 §15.1 全链路扫描）在 internal/admin 的
// sourcetest_test.go；本文件只测本包自己的口径。

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
)

func testClient() *http.Client {
	return NewHTTPClient(config.UpstreamTimeouts{}.Normalized(), nil)
}

// TestProbeRequestShape：两个协议各自的路径、anthropic-version、凭证注入与
// 最小请求体（来源侧模型 ID + max_tokens=1）。
func TestProbeRequestShape(t *testing.T) {
	type capture struct {
		method, path, auth, version, apiKey, ct, body string
	}
	var got capture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = capture{
			method: r.Method, path: r.URL.Path,
			auth: r.Header.Get("Authorization"), version: r.Header.Get("anthropic-version"),
			apiKey: r.Header.Get("x-api-key"), ct: r.Header.Get("Content-Type"), body: string(b),
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	acct := Account{Name: "probe", Type: config.UpstreamMock, APIKey: "sk-probe-1234", BaseURL: srv.URL}
	for _, tc := range []struct{ protocol, wantPath, wantVersion string }{
		{config.ProtocolOpenAIChat, "/chat/completions", ""},
		{config.ProtocolAnthropicMessages, "/messages", probeAnthropicVersion},
	} {
		res := Probe(context.Background(), testClient(), acct, tc.protocol, "up-model-id")
		if !res.OK || res.Status != http.StatusOK || res.LatencyMS < 1 || res.Message != "" {
			t.Errorf("[%s] 2xx 结果异常: %+v", tc.protocol, res)
		}
		if got.method != http.MethodPost || got.path != tc.wantPath {
			t.Errorf("[%s] 请求行 = %s %s", tc.protocol, got.method, got.path)
		}
		if got.version != tc.wantVersion {
			t.Errorf("[%s] anthropic-version = %q, 期望 %q", tc.protocol, got.version, tc.wantVersion)
		}
		if got.auth != "Bearer sk-probe-1234" || got.apiKey != "" {
			t.Errorf("[%s] 凭证注入异常: auth=%q x-api-key=%q", tc.protocol, got.auth, got.apiKey)
		}
		if got.ct != "application/json" {
			t.Errorf("[%s] Content-Type = %q", tc.protocol, got.ct)
		}
		if !strings.Contains(got.body, `"model":"up-model-id"`) || !strings.Contains(got.body, `"max_tokens":1`) {
			t.Errorf("[%s] 请求体 = %s", tc.protocol, got.body)
		}
	}
}

// OpenCode Go 会拒绝缺少会话标识的探测，并按上游协议分别读取鉴权头。
func TestProbeOpenCodeGoIdentityAndAuth(t *testing.T) {
	var mu sync.Mutex
	sessions := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sid := r.Header.Get(openCodeSessionHeader)
		if sid == "" || sessions[sid] || !strings.HasPrefix(r.UserAgent(), "llmgate/") {
			t.Error("探测必须使用独立会话和 LLM Gate 客户端标识")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sessions[sid] = true
		if r.URL.Path == "/messages" {
			if r.Header.Get("x-api-key") != "sk-probe-fake" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") == "" {
				t.Error("Messages 探测鉴权错误")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		} else if r.Header.Get("Authorization") != "Bearer sk-probe-fake" || r.Header.Get("x-api-key") != "" {
			t.Error("Chat 探测鉴权错误")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	acct := Account{Type: config.UpstreamOpenCodeGo, APIKey: "sk-probe-fake", BaseURL: srv.URL}
	for _, p := range []string{config.ProtocolOpenAIChat, config.ProtocolAnthropicMessages, config.ProtocolOpenAIChat} {
		if res := Probe(t.Context(), testClient(), acct, p, "test-model"); !res.OK {
			t.Fatalf("探测未通过: %+v", res)
		}
	}
}

// TestProbeNonJSONAndEmptyErrorBody：非 JSON 与空错误体的兜底摘要。
func TestProbeErrorBodies(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"openai", `{"error":{"message":"Insufficient balance","type":"invalid_request_error"}}`,
			"invalid_request_error: Insufficient balance", http.StatusPaymentRequired},
		{"anthropic", `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			"authentication_error: invalid x-api-key", http.StatusUnauthorized},
		{"no_type", `{"error":{"message":"just a message"}}`, "just a message", http.StatusBadRequest},
		{"html", "<html>\n  502 Bad Gateway\n</html>", "<html> 502 Bad Gateway </html>", http.StatusBadGateway},
		{"empty", "", "上游未返回错误详情", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			io.WriteString(w, tc.body)
		}))
		acct := Account{Type: config.UpstreamMock, BaseURL: srv.URL}
		res := Probe(context.Background(), testClient(), acct, config.ProtocolOpenAIChat, "m")
		srv.Close()
		if res.OK || res.Status != tc.status {
			t.Errorf("[%s] 结果异常: %+v", tc.name, res)
		}
		if res.Message != tc.want {
			t.Errorf("[%s] message = %q, 期望 %q", tc.name, res.Message, tc.want)
		}
	}
}

// TestProbeConnectionRefused：拿不到 HTTP 响应时 Status=0、Message 非空，
// 且不含 url.Error 的 `Post "<完整 URL>":` 外层（地址只在 dial 原因里出现）。
func TestProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	acct := Account{Type: config.UpstreamMock, BaseURL: "http://" + addr}
	res := Probe(context.Background(), testClient(), acct, config.ProtocolOpenAIChat, "m")
	if res.OK || res.Status != 0 || res.Message == "" {
		t.Fatalf("拒连结果异常: %+v", res)
	}
	if strings.Contains(res.Message, "chat/completions") || strings.Contains(res.Message, `Post "`) {
		t.Errorf("摘要仍带 url.Error 外层: %q", res.Message)
	}
	if !strings.HasPrefix(res.Message, "连接上游失败：") || !strings.Contains(res.Message, addr) {
		t.Errorf("摘要形态异常: %q", res.Message)
	}
}

// TestProbeTimeout：ctx 超时归类为「探测超时」，不回显底层 url.Error。
func TestProbeTimeout(t *testing.T) {
	// release 由测试收尾时关闭，保证 handler 一定退出：httptest.Server.Close
	// 会等在途 handler 结束，只等 r.Context() 会把「客户端超时」变成死锁。
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	acct := Account{Type: config.UpstreamMock, BaseURL: srv.URL}
	res := Probe(ctx, testClient(), acct, config.ProtocolOpenAIChat, "m")
	if res.OK || res.Status != 0 || res.Message != "探测超时：连接或响应未在时限内完成" {
		t.Errorf("超时结果异常: %+v", res)
	}
}

// TestProbeUnservedProtocol：ark 型不服务 anthropic——防御分支不发请求。
func TestProbeUnservedProtocol(t *testing.T) {
	acct := Account{Type: config.UpstreamArk, APIKey: "k"}
	res := Probe(context.Background(), testClient(), acct, config.ProtocolAnthropicMessages, "m")
	if res.OK || res.Status != 0 || res.Message != "该上游不服务此入口协议" {
		t.Errorf("未服务协议的结果异常: %+v", res)
	}
}

// TestTransportMessageClassification：哨兵错误的归类优先于 url.Error 剥壳。
func TestTransportMessageClassification(t *testing.T) {
	wrapped := &url.Error{Op: "Post", URL: "http://x/v1/messages", Err: errors.New("dial tcp 1.2.3.4:80: i/o timeout")}
	if got := transportMessage(wrapped); got != "连接上游失败：dial tcp 1.2.3.4:80: i/o timeout" {
		t.Errorf("url.Error 剥壳 = %q", got)
	}
	if got := transportMessage(&url.Error{Op: "Post", URL: "http://x", Err: context.DeadlineExceeded}); got != "探测超时：连接或响应未在时限内完成" {
		t.Errorf("超时归类 = %q", got)
	}
	if got := transportMessage(&url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}); got != "探测已取消" {
		t.Errorf("取消归类 = %q", got)
	}
}

// TestTrimMessage：控制字符压平为单行、超长截断到上限并加省略号。
func TestTrimMessage(t *testing.T) {
	if got := trimMessage("  line one\n\tline\x00two  "); got != "line one line two" {
		t.Errorf("单行化 = %q", got)
	}
	long := strings.Repeat("很长的上游错误", 100)
	got := trimMessage(long)
	if n := len([]rune(got)); n != probeMessageMaxRunes+1 { // +1 为省略号
		t.Errorf("截断后 = %d 个字符, 期望 %d", n, probeMessageMaxRunes+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("截断应加省略号: %q", got)
	}
	if got := trimMessage("短"); got != "短" {
		t.Errorf("短文本不应改动: %q", got)
	}
}

// TestCleanMessage：导出的消毒器（trimMessage 的通用档）——无效 UTF-8 替换
// 为 �、控制字符压平为单行、按 rune 截断加省略号。网关任务面把厂商错误信息
// 送进客户端响应与 aigc_tasks.error_message 之前走的就是它。
func TestCleanMessage(t *testing.T) {
	if got := CleanMessage(string([]byte{'a', 0xff, 'b'}), 100); got != "a�b" {
		t.Errorf("无效 UTF-8 未替换: %q", got)
	}
	if got := CleanMessage("x"+string(rune(0))+"y"+"\n"+"z", 100); got != "x y z" {
		t.Errorf("控制字符未压平为单行: %q", got)
	}
	if got := CleanMessage("abcdef", 3); got != "abc…" {
		t.Errorf("截断异常: %q", got)
	}
	if got := CleanMessage("", 10); got != "" {
		t.Errorf("空串应保持为空: %q", got)
	}
}

// TestInnerErrText：*url.Error 的外层（携完整 URL——产物签名 URL 的查询串是
// 访问凭证）被剥掉，只留内层拨号/传输原因；非 url.Error 原样返回。
func TestInnerErrText(t *testing.T) {
	inner := errors.New("dial tcp 1.2.3.4:443: connection refused")
	ue := &url.Error{Op: "Get", URL: "https://cdn.example.com/v.mp4?signature=SECRETVALUE", Err: inner}
	got := InnerErrText(ue)
	if got != inner.Error() {
		t.Errorf("InnerErrText(url.Error) = %q，期望内层原因", got)
	}
	if strings.Contains(got, "SECRETVALUE") {
		t.Errorf("内层文本泄露完整 URL: %q", got)
	}
	if got := InnerErrText(errors.New("plain failure")); got != "plain failure" {
		t.Errorf("InnerErrText(plain) = %q", got)
	}
}
