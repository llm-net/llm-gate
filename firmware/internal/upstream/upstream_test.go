package upstream_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// 型号的上游协议只收窄适配器；别名按实际来源 ID，其他平台和自定义保留原契约。
func TestModelEndpointProtocols(t *testing.T) {
	doc := platformcatalog.Builtin()
	for _, tc := range []struct {
		adapter, model, mapped, protocol string
		want                             bool
	}{
		{"opencode_go", "longcat-2.0", "", "openai_chat", true},
		{"opencode_go", "mimo-v2.5", "", "anthropic_messages", false},
		{"opencode_go", "mimo-v2.5-pro", "", "anthropic_messages", false},
		{"opencode_go", "alias", "mimo-v2.5", "openai_chat", true},
		{"opencode_go", "longcat-2.0", "minimax-m3", "openai_chat", false},
		{"opencode_go", "longcat-2.0", "minimax-m3", "anthropic_messages", true},
		{"opencode_go", "gpt-5.6-luna", "", "openai_chat", false},
		{"opencode_go", "gpt-5.6-luna", "", "openai_responses", false},
		{"opencode_go", "custom-model", "", "openai_chat", true},
		{"opencode_go", "custom-model", "", "anthropic_messages", true},
		{"qwen_plan", "mimo-v2.5", "", "anthropic_messages", true},
	} {
		a := upstream.Account{Type: tc.adapter}
		if _, ok := a.ModelEndpoint(doc, tc.adapter, tc.model, tc.mapped, tc.protocol); ok != tc.want {
			t.Errorf("%+v: got %v", tc, ok)
		}
	}
	// 声明来自当前目录数据，不能硬编码在模型名或适配器里。
	doc = platformcatalog.Doc{Platforms: []platformcatalog.Platform{{ID: "opencode_go", Models: []platformcatalog.Model{
		{Name: "longcat-2.0", UpstreamProtocols: []string{"anthropic_messages"}},
		{Name: "none", UpstreamProtocols: []string{}},
	}}}}
	a := upstream.Account{Type: "opencode_go"}
	if _, ok := a.ModelEndpoint(doc, "opencode_go", "longcat-2.0", "", "openai_chat"); ok {
		t.Fatal("没有采用生效目录中的协议声明")
	}
	if _, ok := a.ModelEndpoint(doc, "opencode_go", "none", "", "openai_chat"); ok {
		t.Fatal("显式空协议集合被当成省略")
	}
}

func TestOpenCodeGoMessagesAuthorization(t *testing.T) {
	a := upstream.Account{Type: config.UpstreamOpenCodeGo, APIKey: "sk-fake-upstream"}
	h := http.Header{"Authorization": {"Bearer sk-fake-client"}, "X-Api-Key": {"sk-fake-client"}, "X-Session-Id": {"test-session"}}
	a.Authorize(h, config.ProtocolAnthropicMessages)
	if h.Get("Authorization") != "" || h.Get("x-api-key") != a.APIKey || h.Get("x-opencode-session") != "test-session" || h.Get("anthropic-version") != "2023-06-01" {
		t.Fatal("Messages 鉴权或会话派生错误")
	}
}

// TestEndpointResolution：(type, 协议) 二维内置端点表与 URL 拼接
// （iteration-3 Phase 1；iteration-5 起账户由 store 行构造）。内置端点以官方
// 供应商文档为准；base_url 覆盖时两协议共用同一地址、尾随斜杠被剥除。
func TestEndpointResolution(t *testing.T) {
	accounts := map[string]upstream.Account{
		"ds":   {Name: "ds", Type: config.UpstreamDeepseek, APIKey: "test-key-not-real"},
		"ark":  {Name: "ark", Type: config.UpstreamArk, APIKey: "test-key-not-real"},
		"plan": {Name: "plan", Type: config.UpstreamArkPlan, APIKey: "test-key-not-real"},
		"qwen": {Name: "qwen", Type: config.UpstreamQwenPlan, APIKey: "test-key-not-real"},
		"ocg":  {Name: "ocg", Type: config.UpstreamOpenCodeGo, APIKey: "test-key-not-real"},
		"mm":   {Name: "mm", Type: config.UpstreamMinimax, APIKey: "test-key-not-real"},
		"ac":   {Name: "ac", Type: config.UpstreamAnthropicCompat, APIKey: "test-key-not-real", BaseURL: "https://api.example.invalid/v1/"},
		"mock": {Name: "mock", Type: config.UpstreamMock, BaseURL: "http://127.0.0.1:18080/v1/"},
	}
	cases := []struct {
		account, protocol, path string
		want                    string // "" 表示该 (type, 协议) 无端点，期望 ok=false
	}{
		{"ds", config.ProtocolOpenAIChat, "/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"ds", config.ProtocolAnthropicMessages, "/messages", "https://api.deepseek.com/anthropic/v1/messages"},
		{"ark", config.ProtocolOpenAIChat, "/chat/completions", "https://ark.cn-beijing.volces.com/api/v3/chat/completions"},
		// 方舟按量的 Anthropic 端点未实测验证：无内置条目——该来源不进
		// /v1/messages 的候选（决策 4 的运行时协议过滤据此实现）。
		{"ark", config.ProtocolAnthropicMessages, "/messages", ""},
		{"plan", config.ProtocolOpenAIChat, "/chat/completions", "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions"},
		{"plan", config.ProtocolAnthropicMessages, "/messages", "https://ark.cn-beijing.volces.com/api/plan/v1/messages"},
		// 百炼 Token Plan 的两个协议不同段（/compatible-mode/v1 与
		// /apps/anthropic/v1），端点根都是「拼路径前」的形态：厂商给客户端填的
		// ANTHROPIC_BASE_URL 不带 /v1（SDK 自补），此处必须带，与 deepseek 同口径。
		{"qwen", config.ProtocolOpenAIChat, "/chat/completions", "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions"},
		{"qwen", config.ProtocolAnthropicMessages, "/messages", "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1/messages"},
		{"qwen", config.ProtocolAnthropicMessages, "/messages/count_tokens", "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1/messages/count_tokens"},
		// 订阅套餐同 ark_plan：不服务 AIGC 协议。
		{"qwen", config.ProtocolMinimaxVideo, "/v2/video_generation", ""},
		// OpenCode Go：文本双入口同一端点根 /zen/go/v1，/chat/completions 与
		// /messages（含 count_tokens）直接拼在其后；不服务 AIGC 协议。
		{"ocg", config.ProtocolOpenAIChat, "/chat/completions", "https://opencode.ai/zen/go/v1/chat/completions"},
		{"ocg", config.ProtocolAnthropicMessages, "/messages", "https://opencode.ai/zen/go/v1/messages"},
		{"ocg", config.ProtocolAnthropicMessages, "/messages/count_tokens", "https://opencode.ai/zen/go/v1/messages/count_tokens"},
		{"ocg", config.ProtocolMinimaxVideo, "/v2/video_generation", ""},
		{"ocg", config.ProtocolArkVideo, "/contents/generations/tasks", ""},
		{"mock", config.ProtocolOpenAIChat, "/chat/completions", "http://127.0.0.1:18080/v1/chat/completions"},
		{"mock", config.ProtocolAnthropicMessages, "/messages", "http://127.0.0.1:18080/v1/messages"},
		{"ac", config.ProtocolAnthropicMessages, "/messages", "https://api.example.invalid/v1/messages"},
		{"ac", config.ProtocolOpenAIChat, "/chat/completions", ""},
		// AIGC 协议（2026-08-09 厂商官方接口改版）：方舟的 ark_video/ark_image
		// 条目已随设备自造信封一起移除（Seedance/Seedream 待按官方接口重新
		// 接入）；ark_plan 依旧刻意不服务，deepseek 无此类端点。
		{"ark", config.ProtocolMinimaxVideo, "/v2/video_generation", ""},
		{"plan", config.ProtocolMinimaxVideo, "/v2/video_generation", ""},
		{"ds", config.ProtocolMinimaxVideo, "/v2/video_generation", ""},
		// minimax 只服务 minimax_video；端点根是裸站点地址（国内缺省），
		// 版本前缀 /v2 落在请求路径上——站点值要能整体充当 base_url 覆盖值。
		{"mm", config.ProtocolMinimaxVideo, "/v2/video_generation", "https://api.minimaxi.com/v2/video_generation"},
		{"mm", config.ProtocolOpenAIChat, "/chat/completions", ""},
		{"mm", config.ProtocolAnthropicMessages, "/messages", ""},
	}
	for _, c := range cases {
		a := accounts[c.account]
		got, ok := a.URL(c.protocol, c.path)
		if c.want == "" {
			if ok {
				t.Errorf("(%s, %s) 应无端点，got %q", c.account, c.protocol, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("(%s, %s) URL = %q（ok=%v），期望 %q", c.account, c.protocol, got, ok, c.want)
		}
	}
}

// TestEndpointForMock：mock 类型没有内置条目——没有 base_url 的 mock 账户
// 不服务任何协议（选路会把它筛掉，而不是拼出半截地址去拨号）。
func TestEndpointForMock(t *testing.T) {
	for _, proto := range []string{config.ProtocolOpenAIChat, config.ProtocolAnthropicMessages} {
		if base, ok := upstream.EndpointFor(config.UpstreamMock, proto); ok {
			t.Errorf("EndpointFor(mock, %s) = %q，期望无内置端点", proto, base)
		}
		a := upstream.Account{Name: "broken", Type: config.UpstreamMock}
		if _, ok := a.Endpoint(proto); ok {
			t.Errorf("无 base_url 的 mock 账户不应服务 %s", proto)
		}
	}
	if _, ok := upstream.EndpointFor("no-such-type", config.ProtocolOpenAIChat); ok {
		t.Error("未知 type 不应有内置端点")
	}
}

// TestBaseURLDoesNotWidenProtocolSupport（决策 4）：base_url 只换地址不扩协议。
// 内置端点表说 ark 不服务 anthropic，就算给它配了 base_url 也不服务——否则
// config 侧删掉的 ark×anthropic 交叉校验就没有任何地方再表达了。
func TestBaseURLDoesNotWidenProtocolSupport(t *testing.T) {
	ark := upstream.Account{Name: "ark-dev", Type: config.UpstreamArk,
		APIKey: "test-key-not-real", BaseURL: "http://127.0.0.1:18080/v1"}
	if got, ok := ark.Endpoint(config.ProtocolAnthropicMessages); ok {
		t.Errorf("配了 base_url 的 ark 账户不应服务 anthropic_messages，得到 %q", got)
	}
	// 它支持的协议仍走 base_url 覆盖（dev 指向 mock 的用法不受影响）。
	if got, ok := ark.URL(config.ProtocolOpenAIChat, "/chat/completions"); !ok ||
		got != "http://127.0.0.1:18080/v1/chat/completions" {
		t.Errorf("ark 的 openai 端点 = %q（ok=%v），期望 base_url 覆盖生效", got, ok)
	}
	// 双协议 type 的 base_url 覆盖两协议都生效。
	ds := upstream.Account{Name: "ds-dev", Type: config.UpstreamDeepseek,
		APIKey: "test-key-not-real", BaseURL: "http://127.0.0.1:18080/v1"}
	for _, proto := range []string{config.ProtocolOpenAIChat, config.ProtocolAnthropicMessages} {
		if _, ok := ds.Endpoint(proto); !ok {
			t.Errorf("deepseek 应服务 %s", proto)
		}
	}
	// mock 的 base_url 只对文本双协议生效：AIGC 入口按上游 type 选厂商
	// 适配器，mock 说不清自己该被当哪家——不进那些入口的候选。
	mock := upstream.Account{Name: "mk", Type: config.UpstreamMock, BaseURL: "http://127.0.0.1:18080/v1"}
	if got, ok := mock.Endpoint(config.ProtocolMinimaxVideo); ok {
		t.Errorf("带 base_url 的 mock 不应服务 %s，得到 %q", config.ProtocolMinimaxVideo, got)
	}
}

// TestMinimaxSiteOverride：minimax 的 base_url 是站点选择（国际站覆盖国内
// 缺省），且不扩协议——覆盖了照样只服务 minimax_video。可写值限定在双站点
// 由管理面校验，本包只负责覆盖机制本身。
func TestMinimaxSiteOverride(t *testing.T) {
	intl := upstream.Account{Name: "mm-intl", Type: config.UpstreamMinimax,
		APIKey: "test-key-not-real", BaseURL: upstream.MinimaxSiteIntl}
	if got, ok := intl.URL(config.ProtocolMinimaxVideo, "/v2/video_generation"); !ok ||
		got != "https://api.minimax.io/v2/video_generation" {
		t.Errorf("国际站端点 = %q（ok=%v），期望 base_url 站点覆盖生效", got, ok)
	}
	if _, ok := intl.Endpoint(config.ProtocolOpenAIChat); ok {
		t.Error("配了站点 base_url 的 minimax 不应服务 openai_chat（覆盖不扩协议）")
	}
}

// TestAuthorize：客户端凭证被换成上游凭证（x-api-key 删除，统一 Bearer）。
func TestAuthorize(t *testing.T) {
	a := upstream.Account{Name: "ds", Type: config.UpstreamDeepseek, APIKey: "sk-upstream-not-real"}
	h := http.Header{}
	h.Set("x-api-key", "client-key")
	h.Set("Authorization", "Bearer client-key")
	a.Authorize(h, config.ProtocolOpenAIChat)
	if got := h.Get("Authorization"); got != "Bearer sk-upstream-not-real" {
		t.Errorf("Authorization = %q，期望重写为上游 Key", got)
	}
	if got := h.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key 应被删除，实得 %q", got)
	}
}

func TestAuthorizeAnthropicCompat(t *testing.T) {
	a := upstream.Account{Name: "anthropic-compatible", Type: config.UpstreamAnthropicCompat, APIKey: "sk-upstream-not-real"}
	h := http.Header{}
	h.Set("Authorization", "Bearer client-key")
	a.Authorize(h, config.ProtocolOpenAIChat)
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("Authorization 应被删除，实得 %q", got)
	}
	if got := h.Get("x-api-key"); got != "sk-upstream-not-real" {
		t.Errorf("x-api-key = %q，期望上游 Key", got)
	}
	if got := h.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q，期望兼容缺省版本", got)
	}
}

// TestAuthorizeOpenCodeGoDerivesSession：opencode_go 在注入 Bearer 之余，把客户端的
// X-Session-Id 派生成 Go 的会话亲和头 x-opencode-session——OpenCode CLI 对 gate 生成
// 的 llmgate-<摘要> provider 只发前者。客户端自己带了 x-opencode-session 时一个字节
// 不动；没有可派生的值、值含不可见字符或超长时不补；别的类型不做这件事。
func TestAuthorizeOpenCodeGoDerivesSession(t *testing.T) {
	ocg := upstream.Account{Name: "ocg", Type: config.UpstreamOpenCodeGo, APIKey: "sk-upstream-not-real"}

	h := http.Header{}
	h.Set("Authorization", "Bearer client-key")
	h.Set("X-Session-Id", "ses_0123456789abcdef")
	ocg.Authorize(h, config.ProtocolOpenAIChat)
	if got := h.Get("Authorization"); got != "Bearer sk-upstream-not-real" {
		t.Errorf("Authorization = %q，期望重写为上游 Key", got)
	}
	if got := h.Get("x-opencode-session"); got != "ses_0123456789abcdef" {
		t.Errorf("x-opencode-session = %q，期望从 X-Session-Id 派生", got)
	}
	if got := h.Get("X-Session-Id"); got != "ses_0123456789abcdef" {
		t.Errorf("X-Session-Id = %q，派生不该动原头", got)
	}

	h = http.Header{}
	h.Set("x-opencode-session", "ses_native")
	h.Set("X-Session-Id", "ses_other")
	ocg.Authorize(h, config.ProtocolOpenAIChat)
	if got := h.Get("x-opencode-session"); got != "ses_native" {
		t.Errorf("客户端自带 x-opencode-session 时应原样保留，实得 %q", got)
	}

	for name, bad := range map[string]string{
		"空":       "",
		"仅空白":     "   ",
		"含空格":     "ses 1",
		"含控制字符":   "ses\x01",
		"非 ASCII": "ses_会话",
		"超长":      strings.Repeat("a", 257),
	} {
		h = http.Header{}
		h.Set("X-Session-Id", bad)
		ocg.Authorize(h, config.ProtocolOpenAIChat)
		if got := h.Get("x-opencode-session"); got != "" {
			t.Errorf("X-Session-Id 为%s时不应派生，实得 %q", name, got)
		}
	}

	h = http.Header{}
	h.Set("X-Session-Id", "ses_0123456789abcdef")
	upstream.Account{Name: "qp", Type: config.UpstreamQwenPlan, APIKey: "sk-upstream-not-real"}.Authorize(h, config.ProtocolOpenAIChat)
	if got := h.Get("x-opencode-session"); got != "" {
		t.Errorf("qwen_plan 不应派生 x-opencode-session，实得 %q", got)
	}
}

// TestAccountLogValueRedacts（§15.1 纵深防御）：账户整体进 logger 也只出名称
// 与类型，凭证不落日志。
func TestAccountLogValueRedacts(t *testing.T) {
	const secret = "sk-upstream-secret-not-real"
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("probe",
		"upstream", upstream.Account{Name: "ds", Type: config.UpstreamDeepseek, APIKey: secret, BaseURL: "http://x/v1"})
	out := buf.String()
	if bytes.Contains(buf.Bytes(), []byte(secret)) {
		t.Errorf("Account 进日志泄露凭证:\n%s", out)
	}
	for _, want := range []string{`"name":"ds"`, `"type":"deepseek"`} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("日志缺字段 %s:\n%s", want, out)
		}
	}
}
