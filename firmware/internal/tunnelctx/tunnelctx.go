// Package tunnelctx 定义 Cloudflare Tunnel 可信入口的两样共享物：
//
//   - **请求来源标记**：只有从专用 Unix socket（gateway 的 Tunnel listener）到达、
//     且 Host / X-Forwarded-Proto / CF-Connecting-IP 全部校验通过的请求，才会在
//     context 里带上 [Info]。它不可由任何 HTTP 头构造——普通 LAN/TLS listener 上
//     伪造 X-Forwarded-Proto、CF-Connecting-IP 或 Forwarded 头一概无效。管理面据
//     它决定会话 Cookie 是否 Secure、审计与登录退避用哪个客户端 IP。
//   - **路由暴露注册表**：数据面与管理面的每一条路由在注册时都必须声明自己经
//     Tunnel 暴露到公网的档位（[LANOnly] / [API] / [Admin]）。Tunnel listener 用
//     这份注册表按 profile 生成显式 allowlist（`api_only` / `api_and_admin`），
//     不用 `/v1/*` 之类的宽泛前缀猜测；未列出的路径在 Tunnel 侧一律 404。
//
// 两样东西放在一个包里是因为它们只为同一件事服务：让「从公网经 Cloudflare 进来」
// 这条路径拥有与 LAN 直连不同、且不可伪造的 request provenance。
package tunnelctx

import (
	"context"
	"net/http"
)

// Info 是可信 Tunnel 请求的来源事实。ClientIP 只用于审计与防爆破，
// 不作为身份、授权或设备所有权依据。
type Info struct {
	// ClientIP 是 Cloudflare 边缘给出的单值 CF-Connecting-IP（net/netip 严格解析后的
	// 规范形态）；缺失或非法时恒为 UnknownIP，不回退读取可追加、可伪造的 X-Forwarded-For。
	ClientIP string
	// Hostname 是本次请求命中的规范化 public hostname（与设备配置逐字相等）。
	Hostname string
}

// UnknownIP 是 CF-Connecting-IP 缺失或非法时记录的固定占位。
const UnknownIP = "unknown"

// ProbeHeader 是公网探测的回执头：管理器把一个登记过的 nonce 放进它打到公网
// hostname，Tunnel 闸门在 origin socket 上见到即标记并剥离。它只用来分辨请求
// 是「经 socket 回到本机」还是「Cloudflare 回源填成了 http://localhost:80、直接
// 打在 LAN listener 上」——光看 /healthz 200 分不出这两种情形。LAN listener 不读它。
const ProbeHeader = "X-LlmGate-Tunnel-Probe"

type ctxKey struct{}

// With 把可信 Tunnel 来源写进 context。只有 Tunnel listener 的闸门可以调用它。
func With(ctx context.Context, info Info) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

// From 取出可信 Tunnel 来源；不是 Tunnel 请求时 ok 为 false。
func From(ctx context.Context) (Info, bool) {
	info, ok := ctx.Value(ctxKey{}).(Info)
	return info, ok
}

// Trusted 报告请求是否经可信 Tunnel 入口到达。
func Trusted(r *http.Request) bool {
	_, ok := From(r.Context())
	return ok
}

// HTTPS 报告请求在客户端侧是否为 HTTPS：Tunnel 闸门只放行单值
// `X-Forwarded-Proto: https`，所以可信 Tunnel 请求恒为 HTTPS。普通 listener 上
// 这个判定永远为 false——它们从不读取转发头。
func HTTPS(r *http.Request) bool {
	return Trusted(r)
}

// Secure 是「会话 Cookie 该不该带 Secure」的唯一判据：直连 TLS，或可信 Tunnel。
func Secure(r *http.Request) bool {
	return r.TLS != nil || HTTPS(r)
}

// ---- 路由暴露注册表 ----

// Exposure 是一条路由经 Tunnel 暴露到公网的档位。
type Exposure uint8

const (
	// LANOnly 永远不经 Tunnel 暴露：固件上传/安装/回退、网络设置、Key 明文解封等
	// 高风险管理员能力，以及各 mux 的兜底 404。
	LANOnly Exposure = iota
	// API 进入 `api_only` profile：模型、Agent、模型目录、gate 接入与其固定安装辅助
	// 路由、/healthz。
	API
	// Admin 只进入 `api_and_admin` profile：管理界面、根路径跳转与管理员 API。
	Admin
)

// String 是档位的机读名（测试与状态读数用）。
func (e Exposure) String() string {
	switch e {
	case API:
		return "api"
	case Admin:
		return "admin"
	default:
		return "lan_only"
	}
}

// Route 是注册表里的一条：ServeMux 模式串与它的暴露档位。
type Route struct {
	Pattern  string
	Exposure Exposure
}

// Mux 是带暴露登记的 http.ServeMux 包装：注册路由必须同时声明档位，忘了声明在
// 编译期就过不去——新增管理路由时「要不要暴露到公网」因此是一个显式决定。
type Mux struct {
	mux    *http.ServeMux
	routes []Route
}

// NewMux 构造空注册表。
func NewMux() *Mux { return &Mux{mux: http.NewServeMux()} }

// Handle 注册一条路由并登记档位。
func (m *Mux) Handle(pattern string, e Exposure, h http.Handler) {
	m.mux.Handle(pattern, h)
	m.routes = append(m.routes, Route{Pattern: pattern, Exposure: e})
}

// HandleFunc 是 Handle 的函数形式。
func (m *Mux) HandleFunc(pattern string, e Exposure, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, e, http.HandlerFunc(h))
}

// ServeHTTP 委托给底层 ServeMux。
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// Routes 返回登记表的副本。
func (m *Mux) Routes() []Route {
	out := make([]Route, len(m.routes))
	copy(out, m.routes)
	return out
}

// RouteLister 由持有注册表的 handler 实现（管理面 handler 装配体），让网关在装配
// Tunnel allowlist 时把管理面的路由一并纳入。
type RouteLister interface {
	TunnelRoutes() []Route
}

// Profile 是 Tunnel 公网路由策略：固定两档，不允许用户填写 path、正则或 origin。
type Profile string

const (
	ProfileAPIOnly     Profile = "api_only"
	ProfileAPIAndAdmin Profile = "api_and_admin"
)

// Valid 报告 profile 取值是否合法。
func (p Profile) Valid() bool {
	return p == ProfileAPIOnly || p == ProfileAPIAndAdmin
}

// Policy 是 Tunnel listener 当前生效的策略快照。
type Policy struct {
	// Hostname 是规范化的 public hostname；请求 Host 必须与之逐字相等。
	Hostname string
	Profile  Profile
}

// Matcher 是某个 profile 下的显式 allowlist：用一张只登记了允许路由的 ServeMux
// 做匹配，匹配语义与真正的业务路由完全一致（同一套 Go 1.22+ 模式匹配）。
type Matcher struct {
	api   *http.ServeMux
	admin *http.ServeMux
}

// NewMatcher 按注册表生成两档 allowlist：api 只含 API 档路由，admin 含 API 与
// Admin 两档。LANOnly 档永远不进任何一张。模式冲突（同一路径在两份注册表里都
// 登记）会在这里 panic——那是装配期错误，必须在启动时暴露。
func NewMatcher(routes []Route) *Matcher {
	m := &Matcher{api: http.NewServeMux(), admin: http.NewServeMux()}
	marker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, rt := range routes {
		switch rt.Exposure {
		case API:
			m.api.Handle(rt.Pattern, marker)
			m.admin.Handle(rt.Pattern, marker)
		case Admin:
			m.admin.Handle(rt.Pattern, marker)
		}
	}
	return m
}

// Allows 报告请求在给定 profile 下是否命中 allowlist。ServeMux.Handler 对无匹配
// 返回空模式串（含方法不匹配的情形），那就是「未列出」。
func (m *Matcher) Allows(r *http.Request, p Profile) bool {
	var mux *http.ServeMux
	switch p {
	case ProfileAPIOnly:
		mux = m.api
	case ProfileAPIAndAdmin:
		mux = m.admin
	default:
		return false
	}
	_, pattern := mux.Handler(r)
	return pattern != ""
}

// AdminOnly 报告请求是否**只有**在 api_and_admin 下才可达（即它是 Admin 档路由）。
// Tunnel 闸门据此在管理面暴露被临时收窄（出厂口令未改）时只挡这一档。
func (m *Matcher) AdminOnly(r *http.Request) bool {
	if _, pattern := m.api.Handler(r); pattern != "" {
		return false
	}
	_, pattern := m.admin.Handler(r)
	return pattern != ""
}
