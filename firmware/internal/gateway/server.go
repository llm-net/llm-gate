// Package gateway 实现 llmgate gatewayd 的 HTTP 服务层：Server 装配、中间件链
// （request-id → 访问日志 → panic recovery → 认证）与 /healthz、/v1/*、
// /minimax/*（MiniMax 协议面）、/ark/*（火山方舟协议面）路由。厂商协议面的
// 客户端路径恒是 `/<厂商段>` + **源头厂商站点根之后的那一段**：路径首段就是
// 厂商身份，一家一段（config.ProtocolFace*）。
// 2026-08-06 决策：全部服务共用这一个监听器（单端口，板上 80）——Server 同时
// 持有管理面 handler，根路由按路径前缀分发：数据面那几段前缀走数据面中间件
// 链，其余（/admin/*、/ 等）整链交给管理面（internal/admin），两条链的认证与
// 访问日志相互独立、各自只过一遍。只用标准库 net/http（Go 1.22+ 路由），
// 不引入 Web 框架。
//
// 落日志遵守 §15.1 硬规则：一律经 internal/logging 的脱敏途径，
// 任何级别绝不记请求/响应 body 内容与凭证明文。
package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/codexauth"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/devtoolpolicy"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/grokauth"
	"github.com/llm-net/llm-gate/firmware/internal/platformcatalog"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// shutdownTimeout 是优雅停机 drain 在途请求的上限；超时后强制关闭剩余连接。
// systemd 侧的 TimeoutStopSec 须大于此值（部署迭代落地时对齐）。
const shutdownTimeout = 15 * time.Second

// Server 是 gatewayd 的 HTTP 服务。
type Server struct {
	cfg *config.Config
	log *slog.Logger
	// store 是模型目录与上游账户的唯一事实源：选路每请求点查（决策 3），
	// 没有内存目录缓存。
	store       *store.Store
	client      *http.Client // 共享的上游 HTTP 客户端（分层超时见 internal/upstream）
	quotaClient *agentquota.Client
	agentQuota  *agentquota.Manager
	// egress 是出站策略（internal/egress）：数据面的上游/订阅客户端与 Codex/Grok 的
	// OAuth 客户端都经它选路。nil = 恒直连（部分测试）。
	egress egress.Router
	keys   KeyAuthorizer
	// admin 是管理控制面的完整 handler（自带中间件链），挂载在共享监听器
	// 上除数据面路径以外的全部路径；nil 时（部分测试）退回数据面 404。
	admin http.Handler
	// meter 是用量计量出口（iteration-9，装配期经 EnableMetering 注入）。
	// nil = 未装配：整条计量链路跳过，转发语义不变。
	meter Metering
	hs    *http.Server
	// tlsHS 与明文 hs 共用 Handler，但必须是独立 http.Server；客户静态证书
	// 配置缺省关闭。
	tlsHS *http.Server
	// 内网域名 HTTPS 监听（domaintls.go）：证书逐握手向 provider 现取（internal/landomain），
	// 监听在证书签出后由管理器拉起、释放域名时关闭；同样必须是独立 http.Server——
	// net/http 的 HTTP/2 初始化在 Serve/ServeTLS 间共用一个 sync.Once，挂回 hs 会让
	// 浏览器协商上 h2 后连接直接断。
	domainCerts DomainCertProvider
	domainMu    sync.Mutex
	domainTLS   *domainListener

	// Agents（Codex / Grok Build 订阅代理，迭代 11 + grok 扩展）的进程内状态，
	// 装配与用法全在 responses.go；agentMu 同护下面全部字段。
	//
	// 订阅句柄本身**不在这里**：它在库里（agent_accounts，device-key 密封），
	// 每个请求点查一次。这里只有解封后各句柄的令牌世代与单飞刷新状态
	// （agentSessions，一个 provider 一个），以及外部地址的开发期覆盖点
	// （issuer/backend 各 provider 一对）。
	agentMu      sync.Mutex
	agentIssuer  string // 空 = codexauth.DefaultIssuer
	agentBackend string // 空 = codexBackendURL
	agentAuth    *codexauth.Client
	grokIssuer   string // 空 = grokauth.DefaultIssuer
	grokBackend  string // 空 = grokBackendURL
	grokAuth     *grokauth.Client
	// grokModels 是订阅模型目录上游的开发期覆盖点（空 = grokModelsURL，
	// grok_managed.go）。同样与令牌无关。
	grokModels string
	// grokImagine 是 Grok Imagine 图片/视频官方 API 端点根的开发期覆盖点
	// （空 = grokImagineBaseURL，imagine_grok.go）。同样与令牌无关。
	grokImagine   string
	agentSessions map[string]*agentSession

	// Claude OAuth joins agentSessions for refresh; setup-token stays static.
	// claudeMu only guards the model endpoint override used in tests.
	claudeMu      sync.RWMutex
	claudeBackend string

	// Cursor 订阅（cursor.go）：库里封存的是 Dashboard API Key（每请求点查，
	// 同 Claude 的静态凭据口径），上游 accessToken 是设备侧换发的产物，只活在
	// cursorSess 的内存缓存里，恒不落盘。cursorMu 同护端点覆盖与会话缓存，
	// cursorBackend 空值恒解析为编译常量 api2.cursor.sh；受管 HTTP/1.1 的
	// exchange、aiserver.v1 与 agent.v1/RunSSE 都使用这同一根。
	cursorMu       sync.Mutex
	cursorBackend  string
	cursorSess     *cursorSession
	cursorRequests cursorRequestModels
	// cursorClientKey 是设备本地 JWT 的稳定签名根：首次使用时生成并以设备密钥
	// 封存在 settings，cursorClientKeyOnce 保证进程内只装载一次。
	cursorClientKeyOnce sync.Once
	cursorClientKey     []byte
	cursorClientKeyErr  error

	// agentModels 供 GET /agents/v1/models 的读数（装配期经 SetAgentModels
	// 注入，生产实现是 *admin.Server）。数据面自己不解析模型目录数据——
	// 「哪个名字属于哪份订阅」的判据只有管理面那一份，见 responses.go。
	// nil = 未接线（部分测试）：那条端点答空列表，不是故障。
	agentModels AgentModels
	// platformModels 是当前生效的数据升级目录。未接线的测试环境退回固件内嵌
	// 基线；生产由 admin.Server 提供与管理台相同的版本选择结果。
	platformModels PlatformModels
	// devTools 是单把 API 密钥的开发工具策略投影器。生产装配恒注入；nil 只
	// 出现在不涉及开发工具面的窄测试里，相关端点失败闭合。
	devTools *devtoolpolicy.Resolver
	// keyAccess 供 GET /gate-helper/v1/endpoints 的读数（装配期经 SetKeyAccess
	// 注入，生产实现是 *admin.Server）：地址读数与「按 Key 能调哪些模型」的组装
	// 只有管理面那一份（internal/admin/endpoints.go），数据面只做 Key 鉴权与编码。
	// nil = 未接线（部分测试）：那条端点答 503，不是假装读到了空地址。
	keyAccess KeyAccess

	// Cloudflare Tunnel 专用入口（tunnel.go）：root 是 Handler() 装配出的根
	// handler（Tunnel listener 经闸门后交给它）；tunnelMatcher 是按数据面与管理面
	// 路由注册表生成的显式 allowlist；tunnelPolicy 是当前策略快照（hostname +
	// profile），tunnelGate 报告此刻允不允许放行 Admin 档路由（口令不是出厂缺省）。
	root          http.Handler
	tunnelMatcher *tunnelctx.Matcher
	tunnelPolicy  atomic.Pointer[tunnelctx.Policy]
	tunnelGate    atomic.Pointer[func() bool]
	tunnelMu      sync.Mutex
	tunnel        *tunnelListener
	// tunnelProbes 是公网探测的待确认 nonce 登记表（tunnel.go BeginTunnelProbe /
	// EndTunnelProbe）：只有登记过的 nonce 才会被闸门标记。
	tunnelProbeMu sync.Mutex
	tunnelProbes  map[string]tunnelProbe
}

type PlatformModels interface {
	EffectivePlatformModels(ctx context.Context) platformcatalog.Doc
}

func (s *Server) SetPlatformModels(models PlatformModels) { s.platformModels = models }

// KeyAccess 组装「凭这把 Key 怎么连这台设备、能调哪些模型」的接入读数（管理面
// 实现：internal/admin/keyaccess.go）。返回值直接 JSON 编码；形状由管理面定义，与
// GET /admin/v1/endpoints 同源、按 keyID 的 API模型范围裁剪、不带订阅读数。
// r 只用于取「本次连接落在本机哪个地址上」与 context，实现方不得从它读凭据。
type KeyAccess interface {
	KeyAccessSnapshot(r *http.Request, keyID int64) (any, error)
}

// SetKeyAccess 注入接入读数的执行体（装配期一次性，之后只读）。
func (s *Server) SetKeyAccess(access KeyAccess) { s.keyAccess = access }

func (s *Server) SetDevToolPolicy(policy *devtoolpolicy.Resolver) { s.devTools = policy }

func (s *Server) effectivePlatformModels(ctx context.Context) platformcatalog.Doc {
	if s.platformModels != nil {
		return s.platformModels.EffectivePlatformModels(ctx)
	}
	return platformcatalog.Builtin()
}

// New 装配 Server。cfg 已经 config.Load 校验；logger 来自 internal/logging；
// st 提供模型目录/上游账户的每请求点查（三表联查 + 凭证解密）；keys 校验
// 客户端 Key（生产为 StoreKeyAuthorizer——SQLite 摘要点查，YAML Key 经启动
// 导入落库）；adminHandler 是 internal/admin 装配好的管理面（单端口决策，
// 见包注释），生产必传，测试可 nil。
func New(cfg *config.Config, logger *slog.Logger, st *store.Store, keys KeyAuthorizer, adminHandler http.Handler, router egress.Router) *Server {
	s := &Server{
		cfg:         cfg,
		log:         logger,
		store:       st,
		client:      upstream.NewHTTPClient(cfg.UpstreamTimeouts.Normalized(), router),
		quotaClient: agentquota.NewClient(router),
		egress:      router,
		keys:        keys,
		admin:       adminHandler,
	}
	handler := s.Handler()
	s.root = handler
	s.hs = newHTTPServer(cfg.Listen, handler, logger)
	if cfg.TLS.Enabled() {
		s.tlsHS = newHTTPServer(cfg.TLS.Listen, handler, logger)
	}
	return s
}

func newHTTPServer(addr string, handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:    addr,
		Handler: handler,
		// 防慢速头攻击；不设 ReadTimeout/WriteTimeout——chat 转发与 SSE
		// 长响应（Phase 4）不能被整体截断，超时治理在上游客户端分层做。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		// net/http 内部错误（握手失败等）也经脱敏 JSON logger 落盘。
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Handler 返回完整根 handler（测试直接驱动它，不起监听）：数据面子树装配
// 自己的中间件链，根路由再按路径前缀把非数据面路径整链交给管理面——两条链
// 各自只过一遍（request-id/访问日志/认证互不叠加）。
func (s *Server) Handler() http.Handler {
	mux := tunnelctx.NewMux()
	// /healthz 不鉴权：仅表明进程存活，不暴露任何配置或业务信息。
	mux.HandleFunc("GET /healthz", tunnelctx.API, s.handleHealthz)
	mux.Handle("GET /v1/models", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleModels)))
	mux.Handle("POST /v1/chat/completions", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleChatCompletions)))
	mux.Handle("POST /v1/messages", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleMessages)))
	mux.Handle("POST /v1/messages/count_tokens", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleCountTokens)))
	// 标准 Responses 目录面（2026-08-13，responses_catalog.go）：第三个目录
	// 文本入口——只说 Responses 的目录客户端（今天就是官方 codex）经它够到
	// 模型目录；转换面（请求 Responses→chat、响应 chat→Responses），选路与
	// 计量按目录口径，契约见 docs/firmware-gateway.md「Responses 目录面」。
	// 与下面的订阅代理无任何历史语义关系（该路径 2026-08-12 起空置至今）。
	mux.Handle("POST /v1/responses", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleResponsesCatalog)))
	// 开发工具接入面 /agents/<tool>/…：一个工具一张脸。每张脸都按这把 Key 的
	// 开发工具策略生效——勾选的目录模型走目录面，其余名字走该工具的订阅代理；
	// 子树先过 withDevTool（devtools.go）：这把 Key 既没勾该工具的订阅、也没有
	// 投影到它的目录模型时，整棵子树 403 devtool_not_allowed（Grok / Cursor 没有
	// 目录投影，闸门恒等于「可用订阅」勾选）。接入面**不进模型目录选路**：订阅
	// 那一半只解析「已连接的订阅账号」并把请求转给它背后的后端（responses.go /
	// claude.go / cursor.go）。错误体风格按路径（entryErrorStyle）。
	codexFace := func(h http.Handler) http.Handler { return s.withAuth(s.withDevTool("codex", h)) }
	grokFace := func(h http.Handler) http.Handler { return s.withAuth(s.withDevTool("grok", h)) }
	claudeFace := func(h http.Handler) http.Handler { return s.withClaudeAuth(s.withDevTool("claude", h)) }
	// Codex（responses_codex.go）：勾选目录模型走标准 Responses 目录面，其余
	// 可见模型走 Codex 订阅代理；models 与 model-catalog 给出两者的按 Key 并集，
	// gate 把同一投影写进独立的本地选择器目录。
	mux.Handle("POST /agents/codex/v1/responses", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexResponses)))
	mux.Handle("GET /agents/codex/v1/models", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexModels)))
	mux.Handle("GET /agents/codex/v1/model-catalog", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexModelCatalog)))
	// 画图门（images_codex.go）：Codex 内置 image_gen 工具打 {base_url}/images/*，
	// 设备把它翻成一条带 image_generation 工具的订阅 Responses 调用再翻回来。
	mux.Handle("POST /agents/codex/v1/images/generations", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexImagesGenerations)))
	mux.Handle("POST /agents/codex/v1/images/edits", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexImagesEdits)))
	mux.Handle("/agents/codex/", tunnelctx.API, codexFace(http.HandlerFunc(handleNotFound)))
	// Grok Build（responses.go）：恒订阅制，没有目录模型可混；模型清单由
	// /grok-helper/managed-config 渲染成受管配置条目下发，条目的 base_url 指回这里。
	mux.Handle("POST /agents/grok/v1/responses", tunnelctx.API, grokFace(http.HandlerFunc(s.handleGrokResponses)))
	// Grok Imagine 门（imagine_grok.go）：这份订阅自带的图片/视频生成，xAI 官方
	// 路径原样、只换鉴权的逐字节透传；视频轮询 GET 不计量。
	mux.Handle("POST /agents/grok/v1/images/generations", tunnelctx.API, grokFace(http.HandlerFunc(s.handleGrokImagesGenerations)))
	mux.Handle("POST /agents/grok/v1/images/edits", tunnelctx.API, grokFace(http.HandlerFunc(s.handleGrokImagesEdits)))
	mux.Handle("POST /agents/grok/v1/videos/generations", tunnelctx.API, grokFace(http.HandlerFunc(s.handleGrokVideosGenerations)))
	mux.Handle("GET /agents/grok/v1/videos/{request_id}", tunnelctx.API, grokFace(http.HandlerFunc(s.handleGrokVideoPoll)))
	mux.Handle("/agents/grok/", tunnelctx.API, grokFace(http.HandlerFunc(handleNotFound)))
	// Claude Code（claude_mixed.go + claude.go）：客户反向代理可以在边缘终止 HTTPS
	// 后以 HTTP 回源，因此不检查 r.TLS 或转发头；客户端只把自己的客户端 API 密钥
	// 作为网关 Bearer 交给设备，设备从 agent_accounts 解封 setup-token 后替换
	// 认证头并发往固定 Anthropic 上游。子树兜底沿用同一套鉴权后答 Anthropic 形 404。
	mux.Handle("POST /agents/claude/v1/messages", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedMessages)))
	mux.Handle("POST /agents/claude/v1/messages/count_tokens", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedCountTokens)))
	mux.Handle("GET /agents/claude/v1/models", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedModels)))
	mux.Handle("HEAD /agents/claude/api/hello", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedHello)))
	mux.Handle("/agents/claude/", tunnelctx.API, claudeFace(http.HandlerFunc(handleNotFound)))
	// Cursor 集中订阅代理（cursor.go）：exchange 真验上游订阅后签发设备本地
	// JWT，aiserver.v1 / agent.v1 RPC 同时接受这份 JWT 与原客户端 Key；两个
	// 命名空间都整面转发到 api2（受管 HTTP/1.1 对话使用 RunSSE），
	// 子树内其余路径（含非 POST）由 handleCursorRPC 按正则拒成 Connect 形 404、
	// 不出网。错误体风格见 middleware.go 的 connectErrorStyle。Cursor 没有目录
	// 投影，withSubscription 就是它的子树闸。
	mux.Handle("POST /agents/cursor/auth/exchange_user_api_key", tunnelctx.API, s.withAuth(s.withSubscription("cursor", true, http.HandlerFunc(s.handleCursorExchange))))
	mux.Handle("/agents/cursor/", tunnelctx.API, s.withCursorAuth(s.withSubscription("cursor", false, http.HandlerFunc(s.handleCursorRPC))))
	// 兼容别名：旧版 gate 与手工配置仍会打的旧路径，接同一组处理器、过同一道
	// 闸。/agents/v1/responses 按 model 前缀分流 Codex / Grok（handleResponses），
	// /agents/v1/models 列这把 Key 能调到的订阅模型；/codex/v1/* 与 /claude/*
	// 分别是 Codex、Claude Code 接入面的别名。
	mux.Handle("POST /agents/v1/responses", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleResponses)))
	mux.Handle("GET /agents/v1/models", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleAgentModels)))
	mux.Handle("POST /codex/v1/responses", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexResponses)))
	mux.Handle("GET /codex/v1/models", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexModels)))
	mux.Handle("GET /codex/v1/model-catalog", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexModelCatalog)))
	mux.Handle("POST /codex/v1/images/generations", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexImagesGenerations)))
	mux.Handle("POST /codex/v1/images/edits", tunnelctx.API, codexFace(http.HandlerFunc(s.handleCodexImagesEdits)))
	mux.Handle("POST /claude/v1/messages", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedMessages)))
	mux.Handle("POST /claude/v1/messages/count_tokens", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedCountTokens)))
	mux.Handle("GET /claude/v1/models", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedModels)))
	mux.Handle("HEAD /claude/api/hello", tunnelctx.API, claudeFace(http.HandlerFunc(s.handleClaudeMixedHello)))
	mux.Handle("/claude/", tunnelctx.API, claudeFace(http.HandlerFunc(handleNotFound)))
	// /agents/ 下未实现的路径同 /v1/ 兜底纪律：先认证再 404，OpenAI 风格。
	mux.Handle("/agents/", tunnelctx.API, s.withAuth(http.HandlerFunc(handleNotFound)))
	// Grok Build 受管区渲染面（grok_managed.go）：gate 凭
	// 客户端 API 密钥来取 config.toml 受管区文本。/grok-helper/ 前缀其余路径
	//（install.sh|.ps1 静态脚本、cli/{name} 官方二进制透传）仍归管理面、
	// 无鉴权——root 路由只为本路径单开一条到数据面链的分流。
	mux.Handle("GET /grok-helper/managed-config", tunnelctx.API, s.withAuth(s.withSubscription("grok", true, http.HandlerFunc(s.handleGrokManagedConfig))))
	// gate 客户端每次关联、状态读取与启动前查询一次的 Key 专属策略。
	mux.Handle("GET /gate-helper/v1/config", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleGateConfig)))
	// 凭 Key 自证的接入读数（devtools.go handleGateEndpoints）：设备界面的「接入
	// 方法」页（/ui/connect，不要求管理员会话）拿使用者贴入的 Key 读它——地址、
	// 按 Key 裁剪的模型清单与铭牌，订阅读数在上面那条 config 里。
	mux.Handle("GET /gate-helper/v1/endpoints", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleGateEndpoints)))
	// —— 厂商协议面（视频 / 图像）——
	//
	// **一家一段，段名就是厂商 slug**（config.ProtocolFace*）：`/minimax`、`/ark`。
	// 设备只有一个主机名（内网 IP / 内网域名 / 外网映射三条路径都指向同一台机器，
	// 没有子域名可用），厂商面只能落在**路径首段**上。首段之后仍是厂商站点根之后
	// 的原样那一段：客户端把厂商 SDK 的 base_url 从 `https://api.minimaxi.com` 换成
	// `https://<设备地址>/minimax` 就能直接用，方法、路径尾段、查询串、请求/响应
	// 形态与任务 id 一个都不变。设备→厂商那一跳**不带首段**（适配器的 submitPath /
	// queryPath / cancelPath 是上游路径，别混）。
	//
	// 为什么非分不可：厂商的官方根互相重叠，也与设备自有的 `/v1` 聚合面重叠，
	// `/v2` 更是先到先得。段名一分，「这条路径属于哪家的哪套文档」在 URL 第一段
	// 就写死了。每个面自带兜底：`/<段>/` 下未挂载的路径先认证再 404，错误形按
	// 首段归该厂商（entryErrorStyle）。无厂商段的路径（/v2/…、/api/v3/…）不是
	// 数据面入口：它们落进根路由的管理面兜底，对客户端就是「这台设备上没有
	// 这个接口」——TestVideoFaceSegmentRequired 钉着这条。
	//
	// MiniMax 协议面（video.go + video_minimax.go）：视频模型的客户端入口就是
	// MiniMax 官方路径，设备纯转发。任务列表 GET /minimax/v2/query/video_generation、
	// 素材上传 /minimax/v1/files/upload 与再生成 /minimax/v2/video_regeneration
	// 刻意不挂载（理由见 video.go 文件头），落进本面的 404 兜底。
	mux.Handle("POST /minimax/v2/video_generation", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleMinimaxVideoSubmit)))
	mux.Handle("POST /minimax/v2/h3_context_ir", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleMinimaxContextIRSubmit)))
	mux.Handle("GET /minimax/v2/query/video_generation/{task_id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleMinimaxVideoQuery)))
	mux.Handle("DELETE /minimax/v2/video_generation/{task_id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleMinimaxVideoCancel)))
	mux.Handle("/minimax/", tunnelctx.API, s.withAuth(http.HandlerFunc(handleNotFound)))
	// 火山方舟协议面（video.go + video_ark.go + images.go）：Seedance 视频任务面
	// 与 Seedream 同步出图共用 `/ark` 一段——面按**厂商**分，不按模态分，方舟的
	// 两个协议本就同一份文档、同一个站点根。按量与套餐两种上游共用这一组路由
	// ——端点根的差异（/api/v3 与 /api/plan/v3）只发生在设备→厂商那一跳，属于
	// 上游账户差异，不外显。任务列表 GET /ark/api/v3/contents/generations/tasks
	// 刻意不挂载（厂商侧是全账户视角，转发会把别人的任务泄给同设备任何 Key
	// ——同 minimax 的同名理由），落进本面的 404 兜底；方舟的文本
	// /ark/api/v3/chat/completions 同样不挂载：文本入口恒是 /v1/chat/completions，
	// 一个协议只留一个入口。
	mux.Handle("POST /ark/api/v3/contents/generations/tasks", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleArkVideoSubmit)))
	mux.Handle("GET /ark/api/v3/contents/generations/tasks/{task_id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleArkVideoQuery)))
	mux.Handle("DELETE /ark/api/v3/contents/generations/tasks/{task_id}", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleArkVideoCancel)))
	mux.Handle("POST /ark/api/v3/images/generations", tunnelctx.API, s.withAuth(http.HandlerFunc(s.handleArkImagesGenerations)))
	mux.Handle("/ark/", tunnelctx.API, s.withAuth(http.HandlerFunc(handleNotFound)))
	// /v1 下未实现的路径：先认证再 404，无凭证一律 401（与 OpenAI 行为一致）；
	// 错误体风格按路径选择（/v1/messages 系 Anthropic 风格、/minimax/ 系 MiniMax
	// v2 形，其余——含方舟系——OpenAI 风格，方舟自己的错误体就是这一形）。
	mux.Handle("/v1/", tunnelctx.API, s.withAuth(http.HandlerFunc(handleNotFound)))
	// 数据面子树内的兜底（如 /healthz 的非 GET 方法）：保持数据面 404 风格。
	// 它不进 Tunnel allowlist——Tunnel 侧未列出的路径由闸门自己答 404。
	mux.HandleFunc("/", tunnelctx.LANOnly, handleNotFound)
	data := s.withRequestID(s.withAccessLog(s.withRecovery(mux)))

	root := http.NewServeMux()
	root.Handle("/healthz", data)
	root.Handle("/v1/", data)
	// 厂商协议面各占一个根级首段（config.ProtocolFace*）。
	root.Handle("/minimax/", data)
	root.Handle("/ark/", data)
	root.Handle("/agents/", data)
	root.Handle("/codex/", data)
	root.Handle("/claude/", data)
	root.Handle("/gate-helper/v1/", data)
	// 只此一条走数据面链；/grok-helper/ 其余路径（静态接入脚本、官方 CLI 透传）归管理面。
	root.Handle("/grok-helper/managed-config", data)
	if s.admin != nil {
		// 其余一切路径（/、/admin/v1/*、管理界面静态资源、未知路径）归管理面，
		// 未知路径的 404 风格也随之是管理面 JSON。
		root.Handle("/", s.admin)
	} else {
		root.Handle("/", data)
	}
	// Tunnel allowlist：数据面注册表 + 管理面注册表（管理面 handler 实现了
	// RouteLister 才纳入；测试桩没实现就只有数据面路由）。模式冲突在这里 panic
	// ——那是装配期错误，必须在启动时暴露。
	routes := mux.Routes()
	if lister, ok := s.admin.(tunnelctx.RouteLister); ok {
		routes = append(routes, lister.TunnelRoutes()...)
	}
	s.tunnelMatcher = tunnelctx.NewMatcher(routes)
	return root
}

// Run 监听并服务 HTTP，以及配置时的客户自管 HTTPS，直到 ctx 取消
// （SIGTERM/SIGINT）后优雅停机：停止接受新连接，两个监听共享同一段
// shutdownTimeout 来 drain 在途请求，超时后强制关闭。
// 正常停机返回 nil；监听失败或服务异常退出返回错误。
func (s *Server) Run(ctx context.Context) error {
	var tlsConfig *tls.Config
	if s.cfg.TLS.Enabled() {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLS.CertFile, s.cfg.TLS.KeyFile)
		if err != nil {
			return fmt.Errorf("加载 TLS 证书或私钥: %w", err)
		}
		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
		}
		s.tlsHS.TLSConfig = tlsConfig
	}

	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", s.cfg.Listen, err)
	}
	var tlsLn net.Listener
	if tlsConfig != nil {
		tlsLn, err = net.Listen("tcp", s.cfg.TLS.Listen)
		if err != nil {
			ln.Close()
			return fmt.Errorf("TLS 监听 %s: %w", s.cfg.TLS.Listen, err)
		}
	}
	// 客户端 Key 与模型目录都已迁 SQLite（iteration-4/5）：内容随管理面动态
	// 变化，启动日志只报一次库内可服务模型数作为体检，不做任何缓存。
	attrs := []any{
		"version", buildinfo.Version,
		"listen", ln.Addr().String(),
		"servable_models", s.servableModelCount(ctx),
	}
	if tlsLn != nil {
		attrs = append(attrs, "tls_listen", tlsLn.Addr().String())
	}
	s.log.Info("gatewayd 已启动", attrs...)

	type serveResult struct {
		name string
		err  error
	}
	serverCount := 1
	serveErr := make(chan serveResult, 2)
	go func() { serveErr <- serveResult{name: "HTTP", err: s.hs.Serve(ln)} }()
	if tlsLn != nil {
		serverCount++
		go func() { serveErr <- serveResult{name: "HTTPS", err: s.tlsHS.ServeTLS(tlsLn, "", "")} }()
	}

	var failed *serveResult
	received := 0
	select {
	case result := <-serveErr:
		failed = &result
		received = 1
	case <-ctx.Done():
		s.log.Info("收到退出信号，优雅停机", "drain_timeout", shutdownTimeout.String())
	}

	shCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	type namedServer struct {
		name string
		srv  *http.Server
	}
	servers := []namedServer{{name: "HTTP", srv: s.hs}}
	if s.tlsHS != nil {
		servers = append(servers, namedServer{name: "HTTPS", srv: s.tlsHS})
	}
	shutdownDone := make(chan struct{}, len(servers))
	for _, item := range servers {
		item := item
		go func() {
			if err := item.srv.Shutdown(shCtx); err != nil {
				s.log.Warn("drain 超时，强制关闭在途连接", "listener", item.name, "err", err.Error())
				item.srv.Close()
			}
			shutdownDone <- struct{}{}
		}()
	}
	for range servers {
		<-shutdownDone
	}
	for received < serverCount {
		<-serveErr
		received++
	}
	// Tunnel origin socket 随进程一起收：cloudflared 会自己重连，下一次启动由
	// 管理器 Resume 重建。
	if err := s.StopTunnel(); err != nil {
		s.log.Warn("关闭 Tunnel origin socket 失败", "err", err.Error())
	}
	// 内网域名 HTTPS 监听同理：下一次启动由 landomain.Manager.Run 按已存证书恢复。
	if err := s.StopDomainTLS(); err != nil {
		s.log.Warn("关闭内网域名 HTTPS 监听失败", "err", err.Error())
	}
	s.log.Info("gatewayd 已停止")
	if failed != nil {
		return fmt.Errorf("%s 服务异常退出: %w", failed.name, failed.err)
	}
	return nil
}

// handleHealthz 是无鉴权存活探针。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}` + "\n"))
}

// modelObject 与 modelList 对应 OpenAI GET /v1/models 的响应形态。
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // 恒为 "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"` // 恒为 "list"
	Data   []modelObject `json:"data"`
}

// handleModels 实现 GET /v1/models：OpenAI list 格式，只列「启用、且至少有
// 一条启用来源（其上游也启用）」的模型（iteration-5 决策 4 的口径，与选路的
// model_not_found 判定一致），按原始名字典序；created 取模型建行时刻。
// 上游账户名、来源侧模型 ID 与来源数量一概不出现。
//
// 清单再按这把 Key 的 API模型策略收窄（apimodels.go）：调不到的名字不该出现
// 在它自己的模型清单里，两处口径必须同一份策略。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.listServableModels(r.Context())
	if err != nil {
		if r.Context().Err() != nil { // 客户端中途断开，不是库故障
			s.log.Info("客户端断开，模型列表点查已取消", "request_id", infoFrom(r.Context()).id)
			return
		}
		// store 的错误只含约束/列名，不含请求内容与凭证。
		s.log.Error("列出可服务模型失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			internalErrorMessage)
		return
	}
	scope, err := s.store.LookupKeyAPIModelScope(r.Context(), infoFrom(r.Context()).keyID)
	if err != nil {
		if r.Context().Err() != nil {
			s.log.Info("客户端断开，API模型策略点查已取消", "request_id", infoFrom(r.Context()).id)
			return
		}
		s.log.Error("查询API模型策略失败", "request_id", infoFrom(r.Context()).id, "err", err.Error())
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "internal_error",
			internalErrorMessage)
		return
	}
	list := modelList{Object: "list", Data: make([]modelObject, 0, len(models))}
	for _, m := range models {
		if !scope.Allows(m.ID) {
			continue
		}
		list.Data = append(list.Data, modelObject{
			ID:      m.Name,
			Object:  "model",
			Created: m.CreatedAt.Unix(),
			OwnedBy: "llmgate",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

// servableModelCount 只为启动日志的一次性体检：库不可读时返回 -1 并记警告，
// 绝不因此阻断启动（模型目录随时可经管理台修正）。
func (s *Server) servableModelCount(ctx context.Context) int {
	models, err := s.listServableModels(ctx)
	if err != nil {
		s.log.Warn("启动体检：列出可服务模型失败", "err", err.Error())
		return -1
	}
	return len(models)
}

// handleNotFound 对未实现路径返回 404，错误体风格按路径对应的入口协议选择。
func handleNotFound(w http.ResponseWriter, r *http.Request) {
	entryErrorStyle(r)(w, http.StatusNotFound, "not_found",
		fmt.Sprintf("Unknown request URL: %s %s", r.Method, r.URL.Path))
}
