// Package upstream 维护上游端点解析与访问上游的 HTTP 客户端。
//
// 上游账户不再有内存注册表：账户行存 SQLite（iteration-5），数据面每请求点查
// 三表后就地构造 [Account]（决策 3）。本包只剩三件事——内置端点表
// [EndpointFor]、由 DB 行构造的 [Account]（URL 拼接 + 凭证注入）、
// 共享 HTTP 客户端 [NewHTTPClient]。
//
// 端点解析：deepseek/ark/ark_plan/qwen_plan/opencode_go/minimax 走内置 (上游 type, 入口
// 协议) 二维端点表（以官方供应商文档为准）；上游行给出 base_url 时以之覆盖——
// deepseek/ark 系仅 dev 用（如指向本地 mock 上游），minimax 则以之表达国内/国际双站点
// （管理面把可写值限定在 [MinimaxSiteCN]/[MinimaxSiteIntl] 双值里，见其注释）。
// 覆盖值对该 type 服务的全部协议共用，端点白名单硬化在部署迭代收敛。
// mock 与 openai_compat 没有内置端点，必须显式配置 base_url，可服务协议由
// [baseURLProtocols] 圈定（mock 文本双协议；openai_compat 仅 openai_chat）——
// 视频/图片协议的假上游测试用真实类型 + base_url 覆盖表达（选路按 type 选
// 适配器，泛型行说不清自己该被当哪家厂商对待）。
//
// 特化平台能力（2026-08-09）：内置端点表之上，特定平台还有协议转发之外的
// 平台 API 能力——首个是余额查询 [SupportsBalance]/[QueryBalance]（balance.go，
// 当前仅 deepseek）。通用 openai_compat 恒不参与这类能力：它只知道转发形状，
// 不知道平台。
// 某个 (type, 协议) 没有端点不是错误，而是路由的过滤条件：入口按它筛掉不支持
// 本入口协议的候选来源（决策 4），全被筛掉即 404 protocol_mismatch。
//
// HTTP 客户端分层超时（配置 upstream_timeouts 可调，缺省见 config 包）：
//
//   - connect：TCP 建连；tls_handshake：TLS 握手
//   - response_header：发出请求到收到响应头。非流式 chat 的上游在生成完成
//     后才回头部，此层须覆盖整个生成期，缺省 5m
//   - overall：单次请求整体硬上限（含响应体读完/转发完，SSE 长流同样受限），
//     缺省 10m
//
// 上游凭证只经 [Account.Authorize] 注入请求头，绝不落日志（§15.1）：
// [Account] 实现 slog.LogValuer，误把整个账户传进 logger 也只输出名称与类型。
package upstream

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
)

// MiniMax 双站点（迭代 8）：国内站与国际站的账号、Key、余额完全独立，国内
// Key 打国际域名会 401（docs-upstream/minimax-h3-video.md）。站点选择经上游行
// 的 base_url 表达，但**只在这两个内置值里选**（空 = 国内缺省）——管理 API
// 据此校验；开放自由 base_url 会把解密后的上游 Key 发往任意主机，沿用
// 「mock 之外禁改 base_url」的既有安全裁决。路径带 /v2 版本前缀（端点根是
// 裸站点地址），与 deepseek/ark 系把版本前缀放进端点根不同：站点值要能整体
// 充当 base_url 覆盖值，前缀只能落在协议各自的请求路径上。
const (
	MinimaxSiteCN   = "https://api.minimaxi.com"
	MinimaxSiteIntl = "https://api.minimax.io"
)

// builtinEndpoints 是产品内置端点表：(上游 type, 入口协议) → 端点根。
// ark（方舟按量）没有 anthropic 条目——该端点未实测验证，故 ark 型来源不会
// 进 /v1/messages 的候选（iteration-3 设计决策 5，iteration-5 决策 4 改由
// 运行时过滤表达，config 侧交叉校验已删除）。
//
// AIGC 协议（2026-08-09 厂商官方接口改版；方舟两族 2026-08-10 接入）：
// minimax 只服务 minimax_video（端点根是裸站点地址，/v2 版本前缀落在各请求
// 路径上）。方舟的 ark_video（Seedance）与 ark_image（Seedream）**按量与订阅
// 都服务**，各自挂在自己那条端点根上——两条根都是「站点 + /api/(plan/)v3」，
// 与该 type 的 openai_chat 同根，请求路径 /contents/generations/tasks 与
// /images/generations 由适配器拼。
//
// 订阅根服务 AIGC 是 2026-08-10 用真实 Coding Plan Key 实测出来的，与
// docs-upstream/ark-seedance-video.md 里「Coding Plan 的 Base URL 与此不同，
// 用错会产生额外费用」那句**不矛盾但方向相反**：那句提醒的是别拿套餐 Key 去
// 打按量根，而实测下套餐 Key 在按量根上直接 401（打不通，也就谈不上多计费），
// 在 /api/plan/v3 的 /images/generations 与 /contents/generations/tasks 上则
// 双双 400 MissingParameter（路由在、鉴权过）。所以两个 type 的差别只是端点
// 根，不是「订阅不做 AIGC」。deepseek/qwen_plan/opencode_go 无此类端点。
var builtinEndpoints = map[string]map[string]string{
	config.UpstreamDeepseek: {
		config.ProtocolOpenAIChat:        "https://api.deepseek.com/v1",
		config.ProtocolAnthropicMessages: "https://api.deepseek.com/anthropic/v1",
	},
	config.UpstreamArk: {
		config.ProtocolOpenAIChat: "https://ark.cn-beijing.volces.com/api/v3",
		config.ProtocolArkVideo:   "https://ark.cn-beijing.volces.com/api/v3",
		config.ProtocolArkImage:   "https://ark.cn-beijing.volces.com/api/v3",
	},
	config.UpstreamArkPlan: {
		config.ProtocolOpenAIChat:        "https://ark.cn-beijing.volces.com/api/plan/v3",
		config.ProtocolAnthropicMessages: "https://ark.cn-beijing.volces.com/api/plan/v1",
		config.ProtocolArkVideo:          "https://ark.cn-beijing.volces.com/api/plan/v3",
		config.ProtocolArkImage:          "https://ark.cn-beijing.volces.com/api/plan/v3",
	},
	// 阿里云百炼 通义千问 Token Plan（2026-08-09）：文本双入口，但两个协议
	// **不同段**——OpenAI 兼容在 /compatible-mode/v1，Anthropic 兼容在
	// /apps/anthropic/v1。厂商文档给客户端填的 ANTHROPIC_BASE_URL 是
	// .../apps/anthropic（不带 /v1，官方 SDK 自己补 /v1/messages），此处存的
	// 是「拼 /messages 前」的端点根，故要带上 /v1——与 deepseek 的
	// /anthropic/v1 同一口径。2026-08-09 无凭证实探证实：带 /v1 的
	// /messages、/messages/count_tokens 与 /chat/completions 均回 401
	// （路由在），不带 /v1 回 404。视频/图片无此类端点。
	config.UpstreamQwenPlan: {
		config.ProtocolOpenAIChat:        "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
		config.ProtocolAnthropicMessages: "https://token-plan.cn-beijing.maas.aliyuncs.com/apps/anthropic/v1",
	},
	// OpenCode Go 包月套餐的 Chat 与 Messages 使用同一端点根；具体型号按目录
	// upstream_protocols 经 ModelEndpoint 筛选，不能以路径存在推断型号双协议可用。
	// Chat 使用 Bearer，Messages 使用 x-api-key。GET /models 的裸名字进请求体，
	// CLI 的 opencode-go/ 前缀不进请求体。Go 侧靠客户端自带的 User-Agent 与会话头
	// （Claude Code / Codex 的原生会话头、OpenCode 的 x-opencode-session）优化路由与
	// prompt cache，这些业务头由 copyForwardHeaders 原样透传；OpenCode 对设备这种
	// 自定义 provider 只发 X-Session-Id，[Account.Authorize] 把它派生成
	// x-opencode-session（见 deriveOpenCodeSession），其余不改写、不代造。
	// 无 AIGC 端点。Zen 按量面 /zen/v1 是另一套权益，不在本类型。
	config.UpstreamOpenCodeGo: {
		config.ProtocolOpenAIChat:        "https://opencode.ai/zen/go/v1",
		config.ProtocolAnthropicMessages: "https://opencode.ai/zen/go/v1",
	},
	config.UpstreamMinimax: {
		config.ProtocolMinimaxVideo: MinimaxSiteCN,
	},
}

// baseURLProtocols 是无内置端点条目、地址完全由 base_url 给出的类型各自可
// 服务的协议集合。mock（dev 桩）双文本入口；openai_compat（通用 OpenAI 兼容
// 适配）只服务 openai_chat——它按 OpenAI 形状转发，端点根通常以 /v1 结尾，
// 设备在其后拼 /chat/completions。anthropic 入口与方舟 / MiniMax 的厂商协议面
// **不在候选**：一条泛型行说不清自己该被当哪家厂商适配。
// anthropic_compat 只服务 Anthropic Messages，并使用 x-api-key 鉴权。
var baseURLProtocols = map[string]map[string]bool{
	config.UpstreamMock: {
		config.ProtocolOpenAIChat:        true,
		config.ProtocolAnthropicMessages: true,
	},
	config.UpstreamOpenAICompat: {
		config.ProtocolOpenAIChat: true,
	},
	config.UpstreamAnthropicCompat: {
		config.ProtocolAnthropicMessages: true,
	},
}

// EndpointFor 查内置端点表：给定上游 type 与入口协议返回端点根（无尾随斜杠）。
// ok 为 false 表示该类型不服务此协议（如 ark × anthropic_messages），
// 调用方据此把该来源排除出本入口的候选。type mock 无内置条目，恒 false——
// mock 必须由上游行的 base_url 提供地址。
func EndpointFor(upstreamType, protocol string) (string, bool) {
	base, ok := builtinEndpoints[upstreamType][protocol]
	return base, ok
}

// Account 是一个可转发的上游账户，由 store 的路由视图行就地构造（每请求点查，
// 不缓存）。APIKey 是解密后的明文，只允许经 [Account.Authorize] 进出站请求头。
type Account struct {
	Name string
	Type string
	// APIKey 是上游凭证明文：绝不进日志、审计 detail 或管理 API 响应（§15.1）。
	APIKey string
	// BaseURL 覆盖内置端点表，仅 dev/mock 用；两协议共用该值。
	BaseURL string
	// EgressMode 是该账号的出站方式覆盖（inherit|direct|proxy，internal/egress）：
	// 装配上游请求时经 [Account.EgressContext] 进 ctx，由出站 Transport 按它选路。
	EgressMode string
}

// EgressContext 把本账号的出站方式覆盖放进 ctx——每个发往本账号的请求都必须用它
// 派生的 ctx 构造，同一 host 的两个账号才能各走各的出口。
func (a Account) EgressContext(ctx context.Context) context.Context {
	return egress.WithMode(ctx, egress.Mode(a.EgressMode))
}

// Endpoint 返回本账户在 protocol 下的端点根；ok 为 false 表示本账户不服务
// 该协议。
//
// 支持矩阵由 type 说了算，base_url 只换地址不扩协议：有内置条目的 type
// （deepseek/ark/ark_plan/qwen_plan/opencode_go/minimax）先查表，查不到即不服务——base_url 覆盖了
// 也一样。否则 `type: ark` 配上一个 base_url 就能溜进 /v1/messages 的候选，而
// iteration-5 决策 4 删掉 config 侧 ark×anthropic 交叉校验的前提，正是"运行时
// 协议过滤天然表达它"。无内置条目的类型（mock/openai_compat/
// anthropic_compat）按
// baseURLProtocols 圈定协议；没给 base_url 的行什么都不服务，选路把它筛掉
// 而不是拼出半截地址去拨号（写入侧的 base_url 必填校验就是为堵住这种行）。
func (a Account) Endpoint(protocol string) (string, bool) {
	builtin, hasBuiltin := EndpointFor(a.Type, protocol)
	if _, known := builtinEndpoints[a.Type]; known {
		if !hasBuiltin {
			return "", false // 该 type 不服务此协议，base_url 也不放行
		}
	} else if !baseURLProtocols[a.Type][protocol] {
		return "", false // 无内置条目的类型只服务 baseURLProtocols 圈定的协议
	}
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/"), true // 覆盖（mock/openai_compat/minimax 站点）
	}
	return builtin, hasBuiltin
}

// ModelEndpoint 将适配器端点与来源侧模型的上游协议声明求交集。protocol 是
// 真正发往上游的协议；目录 Responses 的调用方必须先映射到 Chat，不能借
// 原生 Responses 声明声称该模型接受 Chat 请求。
func (a Account) ModelEndpoint(doc platformcatalog.Doc, catalogID, model, upstreamModelID, protocol string) (string, bool) {
	protocols := doc.ModelProtocolsFor(catalogID, a.Type, model, upstreamModelID)
	if protocols != nil && !slices.Contains(protocols, protocol) {
		return "", false
	}
	return a.Endpoint(protocol)
}

// URL 返回 protocol 对应端点根拼接 path 的完整地址；path 须以 "/" 开头
// （如 "/chat/completions"、"/messages"）。ok 为 false 表示本账户不服务该
// 协议——选路已按此过滤过候选，调用方遇到即按内部错误处理。
func (a Account) URL(protocol, path string) (string, bool) {
	base, ok := a.Endpoint(protocol)
	if !ok {
		return "", false
	}
	return base + path, true
}

// Authorize 把 h 中的客户端凭证替换为本账户的凭证，并补上该平台要求的身份头。
// OpenAI 形适配器使用 Bearer；Anthropic 兼容适配器使用 x-api-key 并补缺省 API
// 版本；opencode_go 的 Messages 同样使用 x-api-key，其余协议使用 Bearer，
// 并按 [deriveOpenCodeSession] 补会话亲和头。protocol 恒为实际发送的上游协议。
func (a Account) Authorize(h http.Header, protocol string) {
	if a.Type == config.UpstreamOpenCodeGo {
		deriveOpenCodeSession(h)
	}
	if a.Type == config.UpstreamAnthropicCompat || (a.Type == config.UpstreamOpenCodeGo && protocol == config.ProtocolAnthropicMessages) {
		h.Del("Authorization")
		h.Set("x-api-key", a.APIKey)
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", "2023-06-01")
		}
		return
	}
	h.Del("x-api-key")
	h.Set("Authorization", "Bearer "+a.APIKey)
}

// OpenCode Go 的会话亲和头与它的派生来源。Go 侧要求客户端为每个对话带稳定的
// x-opencode-session 以优化路由与 prompt cache；OpenCode CLI 只对 provider ID
// 以 opencode 开头的 provider 发这个头（session/llm/request.ts），对 gate 生成的
// llmgate-<摘要> provider 发的是 X-Session-Id。Claude Code / Codex 的原生会话头
// Go 侧直接认，不在此映射。
const (
	openCodeSessionHeader       = "x-opencode-session"
	openCodeClientSessionHeader = "X-Session-Id"
	maxOpenCodeSessionLen       = 256
)

// deriveOpenCodeSession 在出站请求没有 x-opencode-session 时，把客户端自带的
// X-Session-Id 复制成它——不是代造随机值，只是把客户端已有的稳定会话 id 换个
// 名字交给 Go；客户端自己带了 x-opencode-session 时一个字节不动。值只收可见
// ASCII 且限长：原头本就原样透传，这里只是不把奇形怪状的值再抄一份。
func deriveOpenCodeSession(h http.Header) {
	if h.Get(openCodeSessionHeader) != "" {
		return
	}
	sid := strings.TrimSpace(h.Get(openCodeClientSessionHeader))
	if sid == "" || len(sid) > maxOpenCodeSessionLen {
		return
	}
	for i := 0; i < len(sid); i++ {
		if sid[i] <= ' ' || sid[i] > '~' {
			return
		}
	}
	h.Set(openCodeSessionHeader, sid)
}

// LogValue 实现 slog.LogValuer：账户进日志只输出名称与类型，凭证与端点不外露
// （§15.1 的纵深防御——调用方本就不该把 Account 传给 logger）。
func (a Account) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", a.Name), slog.String("type", a.Type))
}

// NewHTTPClient 构造分层超时的共享上游 HTTP 客户端（超时含义见包文档）。
// 全部上游共用一个实例，连接池按 host 复用。router 是出站策略（internal/egress，
// scope model_api）：nil 恒直连；非 nil 时按设备级出口与每个请求 ctx 里的账号覆盖选路，
// direct / proxy 两个连接池互不混用。任何路径都不读 HTTP_PROXY 等环境变量。
func NewHTTPClient(t config.UpstreamTimeouts, router egress.Router) *http.Client {
	base := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(t.Connect),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(t.TLSHandshake),
		ResponseHeaderTimeout: time.Duration(t.ResponseHeader),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Timeout: time.Duration(t.Overall),
		// 3xx 原样透传给客户端，网关不跟随重定向（透传语义；
		// 也避免重定向后自动摘除鉴权头等隐式行为）。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: egress.TransportFor(router, egress.ScopeModelAPI, base),
	}
}

// CatalogWireProtocol 返回目录协议面对应的上游协议。Responses 通过固件的
// 无状态转换调用 Chat；这不表示上游支持原生 Responses。
func CatalogWireProtocol(surface string) string {
	if surface == config.ProtocolOpenAIResponses {
		return config.ProtocolOpenAIChat
	}
	return surface
}
