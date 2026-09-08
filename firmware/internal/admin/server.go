// Package admin 实现管理控制面：承载 /admin/v1/* 管理 API 与设备界面静态
// 资源（/ui/ 嵌入单页应用、/ → 302 /ui/，见 ui.go）。2026-08-06 决策：管理面
// 与数据面共用同一个监听器（单端口，板上 80），由 gateway.Server 按路径前缀
// 分发——本包不再自持 http.Server，只输出装配完整中间件链的 Handler；认证
// 中间件与数据面仍相互独立（§13.10 的其余要求不变）。只用标准库 net/http
// （Go 1.22+ 路由），不引入 Web 框架。
//
// 权限模型：**设备只有一个管理员、一个登录口令**（0019 起「用户」概念整个
// 退场）。/admin/v1/* 除 login 外一律要求有效会话，没有角色、没有自助子树、
// 没有按主体的可见性裁剪——一条会话就是全部权限。
//
// 认证模型：会话 Cookie 为 HttpOnly + SameSite=Strict + Path=/；请求实际经
// 客户自管 TLS 到达设备时再加 Secure（不信任转发头）。CSRF 取
// 「SameSite=Strict + 变更请求强制 X-LlmGate-CSRF: 1 + Content-Type 必须
// application/json」三重简版。中间件链 request-id → 访问日志 →
// recovery → CSRF → 会话认证（只豁免 login 与非 API 路径）。
//
// §15.1 纪律：口令、会话令牌、Key 明文在任何级别不落日志；访问
// 日志只经 logging.BodyMeter 记 body 元数据；审计 detail 只含 label/
// display_prefix 等非敏感字段。
package admin

import (
	"log/slog"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/agentquota"
	"github.com/llm-net/llm-gate/firmware/internal/auth"
	"github.com/llm-net/llm-gate/firmware/internal/claudehelper"
	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
	"github.com/llm-net/llm-gate/firmware/internal/codexhelper"
	"github.com/llm-net/llm-gate/firmware/internal/config"
	"github.com/llm-net/llm-gate/firmware/internal/cursorhelper"
	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/gatehelper"
	"github.com/llm-net/llm-gate/firmware/internal/grokhelper"
	"github.com/llm-net/llm-gate/firmware/internal/landomain"
	"github.com/llm-net/llm-gate/firmware/internal/mihomo"
	"github.com/llm-net/llm-gate/firmware/internal/netconfig"
	"github.com/llm-net/llm-gate/firmware/internal/opencodehelper"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/sysinfo"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
	"github.com/llm-net/llm-gate/firmware/internal/ui"
	"github.com/llm-net/llm-gate/firmware/internal/update"
	"github.com/llm-net/llm-gate/firmware/internal/upstream"
)

// SessionCookieName 是管理面会话 Cookie 名。
const SessionCookieName = "llmgate_admin_session"

// Server 是管理控制面的 handler 装配体：监听与停机归数据面 gateway.Server
// （单端口决策，见包注释），本类型只装配路由与中间件链。
type Server struct {
	log  *slog.Logger
	st   *store.Store
	auth *auth.Service
	// upstreamClient 只为「测试来源」端点向上游发探测请求（与数据面共用
	// 同一套分层超时配置，但实例独立——管理面探测不与数据面争连接池）。
	upstreamClient *http.Client
	// sys 按需采集设备状态即时快照；rec 提供历史序列（后台分钟采样与
	// 10 分钟归档由 gatewayd 驱动，见 internal/sysinfo 包注释）。
	sys *sysinfo.Collector
	rec *sysinfo.Recorder
	// net 是「设备设置」页的网络配置状态机（nmcli/NetworkManager，含确认-
	// 回滚窗口）；由 gatewayd 构造并注入审计回调。
	net *netconfig.Manager
	// usage 是「用量」页的读数出口，也给密钥列表捎上每把密钥的已用额
	// （*usage.Meter，装配后经 SetUsageReader
	// 注入；nil = 用量端点答 503、密钥列表不带 spend）。走注入而不是构造入参：
	// 计量器要先有 gateway 才装得起来（懒对账的探针是它），而管理面 handler
	// 先于 gateway 构造。
	usage UsageReader
	// catalog 是数据升级两份官网静态文件的来源（*officialsite.Client，装配后经
	// SetCatalogSource 注入）。nil 只会出现在测试里，升级端点如实降级。
	catalog CatalogSource
	// apps 是「推荐应用」页透传的官网来源（同一个 *officialsite.Client，装配后经
	// SetAppsSource 注入）。nil 时透传路由答提示页。
	apps AppsSource
	// appsPass 是透传的进程内状态（并发闸 + 失败翻转记录），由 New 构造。
	appsPass appsPassState
	// grokCLI / codexCLI / claudeCLI / openCodeCLI / cursorCLI 是五种官方 CLI
	// 安装物的白名单透传（无会话公共 GET|HEAD，分别见各 helper 的 cli.go）。
	// 由 New 构造。
	grokCLI     *grokhelper.CLIProxy
	codexCLI    *codexhelper.CLIProxy
	claudeCLI   *claudehelper.CLIProxy
	openCodeCLI *opencodehelper.CLIProxy
	cursorCLI   *cursorhelper.CLIProxy
	// catalogAuto 是数据升级自动检查的进程内状态（上次检查/上次更新的时刻、
	// 与手动「立即更新」共用的单飞锁，见 catalogsync.go）。零值可用。
	catalogAuto catalogAutoState
	// storedWarn 把「已存官网数据文件读不出来」的降级告警压成状态翻转两条
	// （见 catalogsync.go 的 storedCatalogWarn）。零值可用。
	storedWarn storedCatalogWarn
	// agents 是「模型接入」页订阅接入标签页的进程内状态（见
	// agentaccounts.go）：在飞的
	// 那一次 PKCE 登录会话（含 code_verifier，绝不落库）、与 OpenAI 之间的
	// OAuth 客户端，以及令牌状态的持有方（数据面）。零值可用。
	agents      agentLoginState
	claudeLogin claudeLoginState
	agentQuota  *agentquota.Manager
	// fw 是固件升级管理器（*update.Manager，装配后经 SetFirmwareUpdater
	// 注入）。nil = 升级端点答 503 firmware_unavailable（测试路径；真实
	// gatewayd 恒注入，见 firmware.go）。
	fw *update.Manager
	// httpPort 是设备单一监听端口，供接入指引拼本地地址。
	httpPort int
	// hardwareModel 是 boardinfo 型号档案识别出的技术型号代号。gatewayd
	// 启动时只读识别一次后注入；空串表示当前平台无法识别，界面不显示该格。
	hardwareModel string
	// cf 是 Cloudflare Tunnel 公网接入管理器（*cloudflared.Manager，装配后经
	// SetCloudflareTunnel 注入）。nil = Tunnel 端点答 503 cloudflare_unavailable，
	// 外网映射与其余管理面照常（测试路径；真实 gatewayd 恒注入，见 cloudflare.go）。
	cf *cloudflared.Manager
	// lan 是内网域名管理器（*landomain.Manager，装配后经 SetLanDomain 注入）。nil =
	// 内网域名端点答 503 lan_domain_unavailable，接入读数不带域名地址（测试路径；真实
	// gatewayd 恒注入，见 landomain.go）。
	lan *landomain.Manager
	// egress 是出站代理策略（*egress.Manager，构造入参）：管理面的上游探测、CLI 安装物
	// 透传与 OAuth 客户端都经它选路，egress.go 的端点读写它。nil = 恒直连，端点答 503。
	egress *egress.Manager
	// proxyCore 是内置代理内核管理器（*mihomo.Manager，装配后经 SetProxyCore 注入）。nil =
	// 内核端点答 503 proxy_core_unavailable（测试路径；真实 gatewayd 恒注入，见 proxycore.go）。
	proxyCore *mihomo.Manager
}

// Handler 是 Server.Handler() 装配出的根 handler：除了服务请求，还把管理面的路由
// 注册表（每条路由的 Tunnel 暴露档位）交给网关生成公网 allowlist。
type Handler struct {
	http.Handler
	routes []tunnelctx.Route
}

// TunnelRoutes 实现 tunnelctx.RouteLister。
func (h *Handler) TunnelRoutes() []tunnelctx.Route {
	return append([]tunnelctx.Route(nil), h.routes...)
}

// New 装配管理面 Server。cfg 只取上游探测的分层超时；st/authSvc 与数据面
// 共享同一 SQLite 与账号核心；sysCol/rec 由 gatewayd 构造并与记录器共享
// （记录器生命周期归 gatewayd，不归本包）。
func New(cfg *config.Config, logger *slog.Logger, st *store.Store, authSvc *auth.Service,
	sysCol *sysinfo.Collector, rec *sysinfo.Recorder, netMgr *netconfig.Manager,
	hardwareModel string, eg *egress.Manager) *Server {
	// 出站策略以 Router 接口注入各客户端：nil *egress.Manager 也是合法 Router（恒直连），
	// 但接口值必须保持 nil 指针语义，故不把 nil 指针装进非 nil 接口再传。
	var router egress.Router
	if eg != nil {
		router = eg
	}
	s := &Server{
		// 与数据面共用一个进程与 logger，加 srv=admin 便于在 journal 里区分两面。
		log:  logger.With("srv", "admin"),
		st:   st,
		auth: authSvc,
		// Normalized 幂等：config.Load 已归一，测试手搓的零值配置也取到缺省。
		upstreamClient: upstream.NewHTTPClient(cfg.UpstreamTimeouts.Normalized(), router),
		sys:            sysCol,
		rec:            rec,
		net:            netMgr,
		httpPort:       listenPort(cfg.Listen),
		hardwareModel:  hardwareModel,
		appsPass:       newAppsPassState(),
		grokCLI:        grokhelper.NewCLIProxy(logger.With("srv", "admin"), grokhelper.WithEgress(router)),
		codexCLI:       codexhelper.NewCLIProxy(logger.With("srv", "admin"), codexhelper.WithEgress(router)),
		claudeCLI:      claudehelper.NewCLIProxy(logger.With("srv", "admin"), claudehelper.WithEgress(router)),
		openCodeCLI:    opencodehelper.NewCLIProxy(logger.With("srv", "admin"), opencodehelper.WithEgress(router)),
		cursorCLI:      cursorhelper.NewCLIProxy(logger.With("srv", "admin"), cursorhelper.WithEgress(router)),
		egress:         eg,
	}
	s.agents.egress = router
	return s
}

// SetCLIArtifactBases 把官方 grok CLI 透传的出站基址换成测试桩。空串表示
// 这一侧不试。生产不该调用。
func (s *Server) SetCLIArtifactBases(primary, fallback string) {
	if s.grokCLI != nil {
		s.grokCLI.SetBases(primary, fallback)
	}
}

// SetCodexCLIArtifactBases 把官方 Codex 安装脚本与 release 透传来源换成测试桩。
// 生产不该调用。
func (s *Server) SetCodexCLIArtifactBases(installer, releases string) {
	if s.codexCLI != nil {
		s.codexCLI.SetBases(installer, releases)
	}
}

// SetClaudeCLIArtifactBase 把官方 Claude Code release 透传来源换成测试桩。
// 生产不该调用。
func (s *Server) SetClaudeCLIArtifactBase(base string) {
	if s.claudeCLI != nil {
		s.claudeCLI.SetBase(base)
	}
}

// SetOpenCodeCLIArtifactBases 把官方 OpenCode release 元数据与归档来源换成测试桩。
// 生产不该调用。
func (s *Server) SetOpenCodeCLIArtifactBases(api, releases string) {
	if s.openCodeCLI != nil {
		s.openCodeCLI.SetBases(api, releases)
	}
}

// SetCursorCLIArtifactBases 把官方 Cursor CLI 安装脚本与整树归档来源换成
// 测试桩。生产不该调用。
func (s *Server) SetCursorCLIArtifactBases(installer, lab string) {
	if s.cursorCLI != nil {
		s.cursorCLI.SetBases(installer, lab)
	}
}

// Handler 返回完整中间件链装配后的根 handler：gateway.Server 把它挂载到
// 共享监听器上除 /v1/*、/healthz 以外的全部路径（测试直接驱动它，不起监听）。
func (s *Server) Handler() http.Handler {
	mux := tunnelctx.NewMux()
	// 登录是唯一的免会话 /admin/v1 端点。设备出厂即带默认登录口令
	// （auth.Service.EnsureDefaultPassword），管理台开机第一屏就是登录页。
	// 唯一的挂载表是 requiresSession 的白名单。
	mux.HandleFunc("POST /admin/v1/login", tunnelctx.Admin, s.handleLogin)
	mux.HandleFunc("POST /admin/v1/logout", tunnelctx.Admin, s.handleLogout)
	// 设备铭牌：另一条免会话端点（见 version.go）。登录页在会话之前，
	// 从这里读取固件版本与公开的板卡型号代号；不回任何设备身份。
	mux.HandleFunc("GET /admin/v1/version", tunnelctx.Admin, s.handleVersion)
	// 会话探针：界面 boot 问一句「我登着吗」。有效会话 204，否则中间件已
	// 401——刻意不回 body，设备没有「当前用户」这种东西可答。
	mux.HandleFunc("GET /admin/v1/session", tunnelctx.Admin, s.handleSession)
	// 改登录口令：验旧口令 → 改 → 轮换本浏览器的会话（其余会话全部失效）。
	mux.HandleFunc("POST /admin/v1/password", tunnelctx.Admin, s.handlePassword)
	// 接入读数（使用API页）：只读、无入参，回的全是「怎么连这台设备、能调
	// 哪些模型」这类公开事实，见 endpoints.go。
	mux.HandleFunc("GET /admin/v1/endpoints", tunnelctx.Admin, s.handleEndpoints)
	// 设备名仅用于本地管理界面的显示，修改沿用管理员会话与 CSRF 校验。
	mux.HandleFunc("PUT /admin/v1/system/device-name", tunnelctx.Admin, s.handleSetDeviceName)
	// 推荐应用页的状态：只回 {state}（ready / website_unavailable），管理台
	// 据此摆 iframe 还是提示卡；页面本体走下面无会话的 /apps/ 透传路由。
	mux.HandleFunc("GET /admin/v1/apps", tunnelctx.Admin, s.handleApps)
	// API密钥：签发、改标签、限额/按量额度、启停、删除、复制明文。设备只有一个管理员，密钥
	// 因此没有属主这一维（见 keys.go）。
	mux.HandleFunc("GET /admin/v1/keys", tunnelctx.Admin, s.handleListKeys)
	mux.HandleFunc("POST /admin/v1/keys", tunnelctx.Admin, s.handleCreateKey)
	mux.HandleFunc("PATCH /admin/v1/keys/{id}", tunnelctx.Admin, s.handlePatchKey)
	mux.HandleFunc("DELETE /admin/v1/keys/{id}", tunnelctx.Admin, s.handleDeleteKey)
	mux.HandleFunc("POST /admin/v1/keys/{id}/plaintext", tunnelctx.LANOnly, s.handleRevealKey)
	mux.HandleFunc("POST /admin/v1/keys/{id}/metered-allowance", tunnelctx.Admin, s.handleAdjustKeyMeteredAllowance)
	mux.HandleFunc("GET /admin/v1/keys/{id}/dev-tools", tunnelctx.Admin, s.handleGetKeyDevTools)
	mux.HandleFunc("PUT /admin/v1/keys/{id}/dev-tools", tunnelctx.Admin, s.handlePutKeyDevTools)
	// 单把 Key 经 API 调用可用的模型范围（缺省不限制，见 apimodels.go）。
	// 与上面那对开发工具端点互不收窄：这边管厂商兼容 API 面，那边管 CLI 面。
	mux.HandleFunc("GET /admin/v1/keys/{id}/api-models", tunnelctx.Admin, s.handleGetKeyAPIModels)
	mux.HandleFunc("PUT /admin/v1/keys/{id}/api-models", tunnelctx.Admin, s.handlePutKeyAPIModels)
	// 上游账户与模型目录（iteration-5 Phase 4）：SQLite 是唯一事实源，
	// 三表的改动经每请求点查对数据面即时生效（无缓存层）。
	mux.HandleFunc("GET /admin/v1/upstreams", tunnelctx.Admin, s.handleListUpstreams)
	mux.HandleFunc("GET /admin/v1/upstream-platforms", tunnelctx.Admin, s.handleListUpstreamPlatforms)
	mux.HandleFunc("POST /admin/v1/upstreams", tunnelctx.Admin, s.handleCreateUpstream)
	mux.HandleFunc("PATCH /admin/v1/upstreams/{id}", tunnelctx.Admin, s.handlePatchUpstream)
	mux.HandleFunc("DELETE /admin/v1/upstreams/{id}", tunnelctx.Admin, s.handleDeleteUpstream)
	// 上游余额查询（特化平台能力，2026-08-09）：向平台余额 API 发一次只读
	// 查询；金额只进本响应，不落日志与审计（见 balance.go）。
	mux.HandleFunc("POST /admin/v1/upstreams/{id}/balance", tunnelctx.Admin, s.handleUpstreamBalance)
	// 「API密钥接入 → 添加模型」（2026-08-14）：账号平台的可选清单与幂等添加。
	mux.HandleFunc("GET /admin/v1/upstreams/{id}/models", tunnelctx.Admin, s.handleListUpstreamModels)
	mux.HandleFunc("POST /admin/v1/upstreams/{id}/models", tunnelctx.Admin, s.handleAddUpstreamModels)
	mux.HandleFunc("GET /admin/v1/models", tunnelctx.Admin, s.handleListModels)
	mux.HandleFunc("POST /admin/v1/models", tunnelctx.Admin, s.handleCreateModel)
	mux.HandleFunc("PATCH /admin/v1/models/{id}", tunnelctx.Admin, s.handlePatchModel)
	mux.HandleFunc("DELETE /admin/v1/models/{id}", tunnelctx.Admin, s.handleDeleteModel)
	mux.HandleFunc("POST /admin/v1/models/{id}/sources", tunnelctx.Admin, s.handleCreateModelSource)
	mux.HandleFunc("PATCH /admin/v1/sources/{id}", tunnelctx.Admin, s.handlePatchModelSource)
	mux.HandleFunc("DELETE /admin/v1/sources/{id}", tunnelctx.Admin, s.handleDeleteModelSource)
	// 来源连通性测试：对该来源能服务的每个入口协议各发一次最小探测请求。
	mux.HandleFunc("POST /admin/v1/sources/{id}/test", tunnelctx.Admin, s.handleTestModelSource)
	// 用量读数（「用量」页）：按密钥/模型/上游/入口/kind 五个维度分解 +
	// 按日序列 + 全量明细环。
	mux.HandleFunc("GET /admin/v1/usage", tunnelctx.Admin, s.handleUsage)
	// 设备状态读数（「设备状态」页）：即时快照按需采样；历史序列只在查询
	// 时读归档文件。两者都无轮询。
	mux.HandleFunc("GET /admin/v1/system", tunnelctx.Admin, s.handleSystemStatus)
	mux.HandleFunc("GET /admin/v1/system/history", tunnelctx.Admin, s.handleSystemHistory)
	// 设备设置——网络配置（「设备设置」页）：查看物理网卡与 IPv4 配置、
	// 修改设备 IP（延迟应用 + 确认否则回滚，见 internal/netconfig）。
	mux.HandleFunc("GET /admin/v1/system/network", tunnelctx.Admin, s.handleNetworkStatus)
	mux.HandleFunc("PUT /admin/v1/system/network", tunnelctx.LANOnly, s.handleNetworkUpdate)
	mux.HandleFunc("POST /admin/v1/system/network/confirm", tunnelctx.LANOnly, s.handleNetworkConfirm)
	// 公网接入方式（external.go）：外网映射与 Cloudflare Tunnel 二选一。外网映射
	// 只登记管理员在自己网络边界配置好的公开基址，设备不建立远程连接；生效的
	// 地址并入 GET /admin/v1/endpoints。
	mux.HandleFunc("GET /admin/v1/system/external", tunnelctx.Admin, s.handleGetExternal)
	mux.HandleFunc("PUT /admin/v1/system/external", tunnelctx.Admin, s.handleSetExternal)
	// 出站代理（egress.go，docs-dev/firmware-egress-proxy.md）：SOCKS5 / Clash 端点、五类
	// 流量的出口与显式连通性测试。改的是这台设备全部外连的去向且携带代理凭据，写入与
	// 测试恒 LANOnly；读数 Admin 档（不含凭据）。
	mux.HandleFunc("GET /admin/v1/system/egress", tunnelctx.Admin, s.handleEgressStatus)
	mux.HandleFunc("PATCH /admin/v1/system/egress", tunnelctx.LANOnly, s.handleEgressUpdate)
	mux.HandleFunc("POST /admin/v1/system/egress/test", tunnelctx.LANOnly, s.handleEgressTest)
	// 内置代理内核（proxycore.go，docs-dev/firmware-egress-proxy.md §8）：Clash 订阅、节点选择、
	// 内核启停与 Mihomo 组件的清单/下载/上传/安装/回退。订阅地址是凭据、内核改的是全部
	// 外连的去向：写入端点恒 LANOnly，读数 Admin 档。
	mux.HandleFunc("GET /admin/v1/system/proxy-core", tunnelctx.Admin, s.handleProxyCoreStatus)
	mux.HandleFunc("PUT /admin/v1/system/proxy-core/subscription", tunnelctx.LANOnly, s.handleProxyCoreSetSubscription)
	mux.HandleFunc("POST /admin/v1/system/proxy-core/subscription/refresh", tunnelctx.LANOnly, s.handleProxyCoreRefresh)
	mux.HandleFunc("DELETE /admin/v1/system/proxy-core/subscription", tunnelctx.LANOnly, s.handleProxyCoreClearSubscription)
	mux.HandleFunc("PUT /admin/v1/system/proxy-core/node", tunnelctx.LANOnly, s.handleProxyCoreSelectNode)
	mux.HandleFunc("POST /admin/v1/system/proxy-core/latency", tunnelctx.LANOnly, s.handleProxyCoreLatency)
	mux.HandleFunc("POST /admin/v1/system/proxy-core/enable", tunnelctx.LANOnly, s.handleProxyCoreEnable)
	mux.HandleFunc("POST /admin/v1/system/proxy-core/disable", tunnelctx.LANOnly, s.handleProxyCoreDisable)
	mux.HandleFunc("GET /admin/v1/system/components/mihomo", tunnelctx.Admin, s.handleMihomoStatus)
	mux.HandleFunc("POST /admin/v1/system/components/mihomo/check", tunnelctx.LANOnly, s.handleMihomoCheck)
	mux.HandleFunc("POST /admin/v1/system/components/mihomo/manifest", tunnelctx.LANOnly, s.handleMihomoManifest)
	mux.HandleFunc("POST /admin/v1/system/components/mihomo/download", tunnelctx.LANOnly, s.handleMihomoDownload)
	mux.HandleFunc("POST "+MihomoUploadPath, tunnelctx.LANOnly, s.handleMihomoUpload)
	mux.HandleFunc("POST /admin/v1/system/components/mihomo/install", tunnelctx.LANOnly, s.handleMihomoInstall)
	mux.HandleFunc("POST /admin/v1/system/components/mihomo/rollback", tunnelctx.LANOnly, s.handleMihomoRollback)
	mux.HandleFunc("DELETE /admin/v1/system/components/mihomo/staged", tunnelctx.LANOnly, s.handleMihomoDiscard)
	mux.HandleFunc("DELETE /admin/v1/system/components/mihomo", tunnelctx.LANOnly, s.handleMihomoRemove)
	// 内网域名（landomain.go）：提供方式选择、LLM Gate官网账号关联、域名申领、证书
	// 签发/续期与释放。改的是这台设备对外的名字与监听，写入端点恒 LANOnly。
	mux.HandleFunc("GET /admin/v1/system/lan-domain", tunnelctx.Admin, s.handleLanDomainStatus)
	mux.HandleFunc("PUT /admin/v1/system/lan-domain", tunnelctx.LANOnly, s.handleLanDomainUpdate)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/link/start", tunnelctx.LANOnly, s.handleLanDomainLinkStart)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/link/wait", tunnelctx.LANOnly, s.handleLanDomainLinkWait)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/link/cancel", tunnelctx.LANOnly, s.handleLanDomainLinkCancel)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/unlink", tunnelctx.LANOnly, s.handleLanDomainUnlink)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/claim", tunnelctx.LANOnly, s.handleLanDomainClaim)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/register", tunnelctx.LANOnly, s.handleLanDomainRegister)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/dns-check", tunnelctx.LANOnly, s.handleLanDomainDNSCheck)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/certificate", tunnelctx.LANOnly, s.handleLanDomainIssue)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/certificate/wait", tunnelctx.LANOnly, s.handleLanDomainIssueWait)
	mux.HandleFunc("PUT /admin/v1/system/lan-domain/target", tunnelctx.LANOnly, s.handleLanDomainTarget)
	mux.HandleFunc("POST /admin/v1/system/lan-domain/release", tunnelctx.LANOnly, s.handleLanDomainRelease)
	// Cloudflare Tunnel（cloudflare.go，docs-dev/firmware-cloudflare-tunnel.md §6.2）：配置、
	// 启停、自检、删除本机配置；cloudflared 组件的清单/下载/上传/安装/回退。
	// 制品上传是裸二进制体，withCSRF 按 ComponentUploadPath 精确放行；它与固件
	// 上传一样只限 LAN（公网套餐的请求体上限与高风险能力都不该经 Cloudflare）。
	mux.HandleFunc("GET /admin/v1/system/cloudflare-tunnel", tunnelctx.Admin, s.handleCloudflareStatus)
	mux.HandleFunc("PUT /admin/v1/system/cloudflare-tunnel", tunnelctx.Admin, s.handleCloudflareUpdate)
	mux.HandleFunc("POST /admin/v1/system/cloudflare-tunnel/enable", tunnelctx.Admin, s.handleCloudflareEnable)
	mux.HandleFunc("POST /admin/v1/system/cloudflare-tunnel/disable", tunnelctx.Admin, s.handleCloudflareDisable)
	mux.HandleFunc("POST /admin/v1/system/cloudflare-tunnel/test", tunnelctx.Admin, s.handleCloudflareTest)
	mux.HandleFunc("POST /admin/v1/system/cloudflare-tunnel/delete", tunnelctx.Admin, s.handleCloudflareDelete)
	mux.HandleFunc("GET /admin/v1/system/components/cloudflared", tunnelctx.Admin, s.handleComponentStatus)
	mux.HandleFunc("POST /admin/v1/system/components/cloudflared/check", tunnelctx.Admin, s.handleComponentCheck)
	mux.HandleFunc("POST /admin/v1/system/components/cloudflared/manifest", tunnelctx.Admin, s.handleComponentManifest)
	mux.HandleFunc("POST /admin/v1/system/components/cloudflared/download", tunnelctx.Admin, s.handleComponentDownload)
	mux.HandleFunc("POST "+ComponentUploadPath, tunnelctx.LANOnly, s.handleComponentUpload)
	mux.HandleFunc("POST /admin/v1/system/components/cloudflared/install", tunnelctx.Admin, s.handleComponentInstall)
	mux.HandleFunc("POST /admin/v1/system/components/cloudflared/rollback", tunnelctx.Admin, s.handleComponentRollback)
	mux.HandleFunc("DELETE /admin/v1/system/components/cloudflared/staged", tunnelctx.Admin, s.handleComponentDiscard)
	// 卸载整个组件目录是不可逆的本机操作，与制品上传同档只限 LAN。
	mux.HandleFunc("DELETE /admin/v1/system/components/cloudflared", tunnelctx.LANOnly, s.handleComponentRemove)
	// 设备设置——数据升级状态与手动「立即更新」：一次取两份公开
	// 静态文件（官方价目 / 平台模型信息），与每小时那次自动更新
	// 共用同一个核心与同一把单飞锁（见 catalogsync.go）。
	mux.HandleFunc("GET /admin/v1/system/data", tunnelctx.Admin, s.handleDataStatus)
	mux.HandleFunc("POST /admin/v1/system/data/update", tunnelctx.Admin, s.handleSyncCatalog)
	// 设备设置——固件升级（docs/firmware-update.md）：检查/下载
	// 官网检查/下载与手动上传均可用，安装与回退经 UDS 转升级引擎
	// （llmgate updated）。upload 是全管理面唯一的非 JSON 变更请求（octet-stream
	// 裸字节），withCSRF 按 FirmwareUploadPath 精确放行（见 firmware.go）。
	mux.HandleFunc("GET /admin/v1/system/firmware", tunnelctx.Admin, s.handleFirmwareStatus)
	mux.HandleFunc("POST /admin/v1/system/firmware/check", tunnelctx.Admin, s.handleFirmwareCheck)
	mux.HandleFunc("POST /admin/v1/system/firmware/download", tunnelctx.Admin, s.handleFirmwareDownload)
	mux.HandleFunc("POST "+FirmwareUploadPath, tunnelctx.LANOnly, s.handleFirmwareUpload)
	mux.HandleFunc("POST /admin/v1/system/firmware/install", tunnelctx.LANOnly, s.handleFirmwareInstall)
	mux.HandleFunc("POST /admin/v1/system/firmware/rollback", tunnelctx.LANOnly, s.handleFirmwareRollback)
	mux.HandleFunc("DELETE /admin/v1/system/firmware/staged", tunnelctx.LANOnly, s.handleFirmwareDiscard)
	// Agents 订阅账号：Codex/Grok 走 OAuth 或 auth.json、
	// Claude 走 setup-token 提交、Cursor 走 API Key 粘贴；共用改名/启停/删除，
	// Claude 不支持自检（Cursor 的自检 = 数据面重走一次 exchange）。凭据与令牌一律不出响应
	// （见 agentaccounts.go）。
	mux.HandleFunc("GET /admin/v1/agent-accounts", tunnelctx.Admin, s.handleListAgentAccounts)
	mux.HandleFunc("POST /admin/v1/agent-accounts/login/start", tunnelctx.Admin, s.handleAgentLoginStart)
	mux.HandleFunc("POST /admin/v1/agent-accounts/login/callback", tunnelctx.Admin, s.handleAgentLoginCallback)
	mux.HandleFunc("POST /admin/v1/agent-accounts/import", tunnelctx.Admin, s.handleImportAgentAccount)
	mux.HandleFunc("POST /admin/v1/agent-accounts/claude/setup-token", tunnelctx.Admin, s.handleConnectClaudeSetupToken)
	mux.HandleFunc("POST /admin/v1/agent-accounts/claude/login/start", tunnelctx.Admin, s.handleClaudeLoginStart)
	mux.HandleFunc("POST /admin/v1/agent-accounts/claude/login/callback", tunnelctx.Admin, s.handleClaudeLoginCallback)
	mux.HandleFunc("POST /admin/v1/agent-accounts/cursor/api-key", tunnelctx.Admin, s.handleConnectCursorAPIKey)
	mux.HandleFunc("GET /admin/v1/cursor-pricing", tunnelctx.Admin, s.handleCursorPrices)
	mux.HandleFunc("PATCH /admin/v1/agent-accounts/{id}", tunnelctx.Admin, s.handlePatchAgentAccount)
	mux.HandleFunc("DELETE /admin/v1/agent-accounts/{id}", tunnelctx.Admin, s.handleDeleteAgentAccount)
	mux.HandleFunc("POST /admin/v1/agent-accounts/{id}/refresh", tunnelctx.Admin, s.handleRefreshAgentAccount)
	mux.HandleFunc("POST /admin/v1/agent-accounts/{id}/quota/sync", tunnelctx.Admin, s.handleSyncAgentQuota)
	// Agent 订阅模型没有管理端点（2026-08-15）：它们由模型目录数据定义、连上
	// 订阅即由收敛器建行（agentmodels.go），管理台只读。
	// 界面入口：/ 精确匹配引到 /ui/，/admin/ 是旧管理台书签的客户端跳转页
	// （fragment 到不了服务端，302 换不出 hash 里那一段；见 ui.go）。
	// GET /{$} 只匹配根本身，吞不掉任何 API 路径。
	mux.HandleFunc("GET /{$}", tunnelctx.Admin, s.handleRootRedirect)
	mux.HandleFunc("GET /admin/", tunnelctx.Admin, s.handleAdminLegacy)
	// 设备界面静态资源（internal/ui）：无会话单页子树，/ui 无斜杠由 ServeMux
	// 自动补斜杠跳转。挂在管理面链上，图的只是共享这条链的 request-id /
	// 访问日志 / recovery——界面与管理面没有别的耦合。
	//
	// 为什么界面全收在 /ui/ 这一个根级前缀下：根命名空间是厂商 API 兼容面
	// （/v1 /v2 /api/v3 /agents，兜底 JSON 404 是客户端契约），界面占单一前缀就
	// 与它天然不相交，深链（/ui/keys 刷新不 404）也不必往根上白名单注册顶层段。
	// 管理台的 /admin/ 在设置区迁完前保持可达。
	mux.Handle("GET /ui/", tunnelctx.Admin, ui.UIHandler())
	// 「推荐应用」页透传（无会话公共 GET|HEAD，2026-08-15 晚）：把 /apps/* 逐请求
	// 转给 LLM Gate官网同路径、响应关进沙箱送回，设备一份都不存。为什么无会话、沙箱
	// 挡的是什么、标记闸防的是什么，见 apps.go 文件头三条口径。
	mux.HandleFunc("GET /apps/", tunnelctx.Admin, s.handleAppsPassthrough)
	// 一键接入脚本（无会话公共 GET，与登录页资源同性质）只有 gate-helper
	// 一对；工具专属 install 路径不存在。所有脚本都不含上游凭据。
	// 五种工具各有 /…-helper/cli/*：把官方公开安装物从固定来源白名单
	// 透传给够不着外网的电脑（不内嵌、不落盘，见各 helper 的 cli.go）。
	// 管理台生成的安装命令从设备自己的地址取它——凡能打开该页的浏览器必然够得着。
	mux.Handle("GET /codex-helper/cli/{path...}", tunnelctx.API, s.codexCLI)
	mux.Handle("HEAD /codex-helper/cli/{path...}", tunnelctx.API, s.codexCLI)
	// 官方 grok CLI 安装物白名单透传：无会话，与 gate 安装脚本同性质。
	mux.Handle("GET /grok-helper/cli/{name}", tunnelctx.API, s.grokCLI)
	mux.Handle("HEAD /grok-helper/cli/{name}", tunnelctx.API, s.grokCLI)
	// Claude Code release 指针、manifest 与八个平台二进制白名单透传。
	mux.Handle("GET /claude-helper/cli/{path...}", tunnelctx.API, s.claudeCLI)
	mux.Handle("HEAD /claude-helper/cli/{path...}", tunnelctx.API, s.claudeCLI)
	// OpenCode 稳定 release 元数据与六平台 CLI 归档白名单透传。
	mux.Handle("GET /opencode-helper/cli/{path...}", tunnelctx.API, s.openCodeCLI)
	mux.Handle("HEAD /opencode-helper/cli/{path...}", tunnelctx.API, s.openCodeCLI)
	// Cursor CLI 官方安装脚本与六平台整树归档白名单透传。
	mux.Handle("GET /cursor-helper/cli/{path...}", tunnelctx.API, s.cursorCLI)
	mux.Handle("HEAD /cursor-helper/cli/{path...}", tunnelctx.API, s.cursorCLI)
	// gate 客户端与首次安装脚本随固件内嵌，匿名提供固定文件名的
	// GET/HEAD/Range；handler 自身在打开制品前完成白名单校验。
	gateFiles := gatehelper.Handler()
	mux.Handle("GET /gate-helper/{name}", tunnelctx.API, gateFiles)
	mux.Handle("HEAD /gate-helper/{name}", tunnelctx.API, gateFiles)
	mux.Handle("GET /gate-helper/releases/{name}", tunnelctx.API, gateFiles)
	mux.Handle("HEAD /gate-helper/releases/{name}", tunnelctx.API, gateFiles)
	// 其余路径（含未注册方法的 /admin/v1 落空匹配）：统一 JSON 404。
	// /admin/v1/ 下的未知路径经会话中间件先认证再 404，不给未认证方探测面。
	mux.HandleFunc("/", tunnelctx.LANOnly, s.handleNotFound)
	return &Handler{
		Handler: s.withRequestID(s.withAccessLog(s.withRecovery(s.withCSRF(s.withSession(mux))))),
		routes:  mux.Routes(),
	}
}

// handleNotFound 对未知路径返回统一 JSON 404。
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "路径不存在")
}
