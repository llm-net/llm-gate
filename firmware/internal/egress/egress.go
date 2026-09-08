// Package egress 是固件全部互联网出口的统一出站策略（docs-dev/firmware-egress-proxy.md）。
//
// 设备访问模型平台、订阅后端、Agent 授权端点、LLM Gate官网与公开安装物时，每一类流量
// （[Scope]）显式选择 `direct` 直连或 `proxy` 经管理员配置的 SOCKS5 端点（[Route]）；
// 普通上游来源还可以单独覆盖为 `inherit` / `direct` / `proxy`（[Mode]）。缺省全部直连，
// 与不带本包的固件行为完全一致；`HTTP_PROXY` 等环境变量在任何路径上都不生效。
//
// 边界：
//
//   - 只做 SOCKS5 TCP CONNECT（RFC 1928，用户名/口令认证 RFC 1929）。目标域名放进 CONNECT
//     请求由代理端解析（socks5h 语义），设备本地 DNS 只解析代理端点自己的名字。
//   - 选择代理后**失败关闭**：代理不可达、认证失败或目标连不上就让该路径的请求失败，绝不
//     静默回落直连。
//   - 代理不终止目标 TLS：证书校验、SNI、HTTP/2 与各客户端的分层超时照旧由 net/http 负责，
//     经代理访问明文 http:// 目标一律拒绝（SOCKS5 运营方不该看到提示词与上游凭据）。
//   - Clash / Mihomo 只经它的 SOCKS/mixed 端口接入，本包不识别、不解析任何 Clash 配置。
//   - 用户名与口令只活在快照里的拨号器中，[Profile] 的 String / GoString / LogValue 全部
//     自遮蔽；错误值 [Error] 只带类别，不带代理地址、凭据或目标 URL。
//
// 用法：每个出站客户端在构造自己的 *http.Transport（各自的超时、重定向与连接池不变）后，
// 经 [Router.Transport] 换成绑定某个 scope 的 RoundTripper；[Manager] 持有策略快照，
// 管理面改配置后原子发布新快照并关闭旧代理连接池的空闲连接，在飞请求按原路径完成。
package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Scope 是出站流量分类。每一类只能整体选择 direct 或 proxy。
type Scope string

const (
	// ScopeModelAPI：普通模型上游、余额/探测、Codex/Grok/Claude/Cursor 订阅数据面与订阅模型目录。
	ScopeModelAPI Scope = "model_api"
	// ScopeAgentAuth：OpenAI / xAI 的 OAuth 登录、换码与刷新。
	ScopeAgentAuth Scope = "agent_auth"
	// ScopeOfficialSite：LLM Gate官网的数据升级、固件索引/制品、推荐应用与设备关联接口。
	ScopeOfficialSite Scope = "official_site"
	// ScopeCLIArtifacts：Codex / Grok / Claude Code / Cursor / OpenCode 官方公开安装物的白名单透传。
	ScopeCLIArtifacts Scope = "cli_artifacts"
	// ScopeComponentArtifacts：cloudflared / Mihomo 等可选组件的官方制品下载。
	ScopeComponentArtifacts Scope = "component_artifacts"
	// ScopeProxySubscription：内置内核的 Clash 订阅拉取（首次拉取时内核还没起来，缺省直连）。
	ScopeProxySubscription Scope = "proxy_subscription"
)

// Scopes 是全部分类，顺序即管理界面展示顺序。
var Scopes = []Scope{ScopeModelAPI, ScopeAgentAuth, ScopeOfficialSite, ScopeCLIArtifacts, ScopeComponentArtifacts, ScopeProxySubscription}

// Valid 报告 s 是不是已知分类。
func (s Scope) Valid() bool {
	for _, k := range Scopes {
		if k == s {
			return true
		}
	}
	return false
}

// Label 是分类的界面名称（中文源文本）。
func (s Scope) Label() string {
	switch s {
	case ScopeModelAPI:
		return "模型与订阅接口"
	case ScopeAgentAuth:
		return "Agent 订阅授权"
	case ScopeOfficialSite:
		return "LLM Gate官网"
	case ScopeCLIArtifacts:
		return "开发工具安装物"
	case ScopeComponentArtifacts:
		return "可选组件制品"
	case ScopeProxySubscription:
		return "Clash 订阅拉取"
	}
	return string(s)
}

// Route 是一类流量的出口选择。
type Route string

const (
	RouteDirect Route = "direct"
	RouteProxy  Route = "proxy"
)

// ParseRoute 解析管理 API 传入的出口值。
func ParseRoute(s string) (Route, error) {
	switch Route(s) {
	case RouteDirect, RouteProxy:
		return Route(s), nil
	}
	return "", errors.New("出口只能是 direct 或 proxy")
}

// Mode 是单个上游来源对 model_api 出口的覆盖。
type Mode string

const (
	ModeInherit Mode = "inherit"
	ModeDirect  Mode = "direct"
	ModeProxy   Mode = "proxy"
)

// ParseMode 解析上游来源的 egress_mode；空串按 inherit。
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeInherit:
		return ModeInherit, nil
	case ModeDirect, ModeProxy:
		return Mode(s), nil
	}
	return "", errors.New("出站方式只能是 inherit、direct 或 proxy")
}

// Provider 标记管理员声明的代理端点种类。两种都只经标准 SOCKS5 通信，区别只在界面
// 引导：Clash / Mihomo 填的是它的 socks-port / mixed-port。
type Provider string

const (
	ProviderNone   Provider = ""
	ProviderSOCKS5 Provider = "socks5"
	ProviderClash  Provider = "clash"
	// ProviderMihomo 是板上内置代理内核（internal/mihomo）：地址由内核管理器在内核起来后写成
	// 127.0.0.1:<端口>、停用时清空，管理 API 不能直接改地址或填认证。
	ProviderMihomo Provider = "mihomo"
)

// ParseProvider 解析代理方式；空串表示不使用代理。
func ParseProvider(s string) (Provider, error) {
	switch Provider(s) {
	case ProviderNone, ProviderSOCKS5, ProviderClash, ProviderMihomo:
		return Provider(s), nil
	}
	return "", errors.New("代理方式只能是 socks5、clash、mihomo 或留空")
}

// Label 是代理方式的界面名称。
func (p Provider) Label() string {
	switch p {
	case ProviderSOCKS5:
		return "SOCKS5 代理"
	case ProviderClash:
		return "Clash / Mihomo"
	case ProviderMihomo:
		return "Clash 订阅（设备内置内核）"
	}
	return "不使用代理"
}

// Managed 报告该方式的端点地址由固件自己管理（内置内核），不由管理员填写。
func (p Provider) Managed() bool { return p == ProviderMihomo }

// Profile 是设备唯一的 SOCKS5 代理端点。Username / Password 是凭据：不进日志、审计、
// API 响应；本类型的三种格式化输出全部自遮蔽（值接收者，指针与值两种形态都挡住）。
type Profile struct {
	Provider Provider
	Address  string // host:port；代理端点为域名时只有这个名字经设备本地 DNS
	Username string
	Password string
}

// Configured 报告是否已声明代理端点。
func (p Profile) Configured() bool { return p.Provider != ProviderNone && p.Address != "" }

// HasAuth 报告是否配置了 RFC 1929 用户名/口令认证。
func (p Profile) HasAuth() bool { return p.Username != "" || p.Password != "" }

// String 只输出方式与地址，凭据以 [set] / [none] 表示。
func (p Profile) String() string {
	auth := "auth=none"
	if p.HasAuth() {
		auth = "auth=set"
	}
	return fmt.Sprintf("egress.Profile{provider=%s address=%s %s}", p.Provider, p.Address, auth)
}

// GoString 与 String 相同：%#v 不得把未导出字段打出来。
func (p Profile) GoString() string { return p.String() }

// LogValue 实现 slog.LogValuer：同样只有方式、地址与「是否设了认证」。
func (p Profile) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", string(p.Provider)),
		slog.String("address", p.Address),
		slog.Bool("auth", p.HasAuth()),
	)
}

// maxAuthLen 是 RFC 1929 用户名与口令各自的字节上限。
const maxAuthLen = 255

// Validate 校验 profile 形态（docs-dev/firmware-egress-proxy.md §5.2）：
// 地址是单一 host:port、端口 1–65535、不是 URL、不是未指定/组播/广播地址；
// 认证字段要么都空，要么都是 1–255 字节。错误文本不回显口令。
func (p Profile) Validate() error {
	if p.Provider == ProviderNone {
		if p.Address != "" || p.HasAuth() {
			return errors.New("未选择代理方式时不能填写代理地址或认证信息")
		}
		return nil
	}
	if _, err := ParseProvider(string(p.Provider)); err != nil {
		return err
	}
	if p.Provider.Managed() {
		if p.HasAuth() {
			return errors.New("内置内核不使用 SOCKS5 认证")
		}
		if p.Address == "" {
			return nil // 内核尚未运行：允许存在但视为未配置
		}
		if host, _, err := net.SplitHostPort(p.Address); err != nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("内置内核的地址必须是本机回环地址")
		}
		return nil
	}
	if err := ValidateAddress(p.Address); err != nil {
		return err
	}
	switch {
	case p.Username == "" && p.Password == "":
	case p.Username == "" || p.Password == "":
		return errors.New("SOCKS5 认证的用户名与口令必须同时填写，或都留空")
	case len(p.Username) > maxAuthLen || len(p.Password) > maxAuthLen:
		return fmt.Errorf("SOCKS5 用户名与口令各不能超过 %d 字节", maxAuthLen)
	}
	return nil
}

// ValidateAddress 校验代理端点地址：只收 host:port（IPv6 用方括号），拒绝 scheme、
// 路径、查询串、片段与 userinfo，拒绝 0.0.0.0 / :: / 广播 / 组播。回环与内网地址都
// 允许——板上本地 Mihomo 就是 127.0.0.1。
func ValidateAddress(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("代理地址不能为空，请填 host:port，例如 127.0.0.1:7891")
	}
	if strings.Contains(addr, "://") || strings.ContainsAny(addr, "/?#@ \t") {
		return errors.New("代理地址只能是 host:port，不能带协议、路径、查询串或账号")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("代理地址必须是 host:port，例如 127.0.0.1:7891 或 [::1]:7891")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("代理端口必须是 1–65535 的整数")
	}
	if host == "" {
		return errors.New("代理地址缺少主机名或 IP")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		switch {
		case ip.IsUnspecified():
			return errors.New("代理地址不能是未指定地址（0.0.0.0 / ::）")
		case ip.IsMulticast(), ip.Is4() && ip == netip.MustParseAddr("255.255.255.255"):
			return errors.New("代理地址不能是组播或广播地址")
		}
		return nil
	}
	if !validHostname(host) {
		return errors.New("代理主机名不合法：只允许字母、数字、连字符与点")
	}
	return nil
}

// validHostname 是 RFC 1123 风格的主机名形态检查（不做解析）。
func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Routes 是各分类的出口选择；缺席的分类视为 direct。
type Routes map[Scope]Route

// Get 取一类流量的出口（缺席 = direct）。
func (r Routes) Get(scope Scope) Route {
	if r == nil {
		return RouteDirect
	}
	if v, ok := r[scope]; ok && v == RouteProxy {
		return RouteProxy
	}
	return RouteDirect
}

// ProxyScopes 返回选择了 proxy 的分类（按 Scopes 顺序）。
func (r Routes) ProxyScopes() []Scope {
	var out []Scope
	for _, s := range Scopes {
		if r.Get(s) == RouteProxy {
			out = append(out, s)
		}
	}
	return out
}

// Normalized 返回一份五个分类齐全的副本。
func (r Routes) Normalized() Routes {
	out := make(Routes, len(Scopes))
	for _, s := range Scopes {
		out[s] = r.Get(s)
	}
	return out
}

// Policy 是一份完整的出站策略：代理 profile + 各分类出口。
type Policy struct {
	Profile Profile
	Routes  Routes
}

// Validate 校验整份策略：profile 形态合法，且只有在代理已配置时才允许任何分类选 proxy。
func (p Policy) Validate() error {
	if err := p.Profile.Validate(); err != nil {
		return err
	}
	for k := range p.Routes {
		if !k.Valid() {
			return fmt.Errorf("未知的流量分类 %q", string(k))
		}
	}
	if !p.Profile.Configured() {
		if scopes := p.Routes.ProxyScopes(); len(scopes) > 0 {
			names := make([]string, 0, len(scopes))
			for _, s := range scopes {
				names = append(names, s.Label())
			}
			return fmt.Errorf("尚未配置代理端点，「%s」不能选择经代理；请先填写代理，或把这些分类改回直连", strings.Join(names, "、"))
		}
	}
	return nil
}

// RouteFor 按分类与来源覆盖裁决最终出口：来源显式 direct / proxy 优先，否则跟随分类。
func (p Policy) RouteFor(scope Scope, mode Mode) Route {
	switch mode {
	case ModeDirect:
		return RouteDirect
	case ModeProxy:
		return RouteProxy
	}
	return p.Routes.Get(scope)
}

// ---- 请求上下文里的来源覆盖 ----

type modeKey struct{}

// WithMode 把一次具体来源尝试的 egress_mode 放进 ctx；数据面装配上游请求时调用。
// 同一来源的重试沿用同一个 ctx 值，跨来源尝试各自带各自的值。
func WithMode(ctx context.Context, mode Mode) context.Context {
	if mode == "" || mode == ModeInherit {
		return ctx
	}
	return context.WithValue(ctx, modeKey{}, mode)
}

// ModeFrom 读 ctx 里的来源覆盖；没有即 inherit。
func ModeFrom(ctx context.Context) Mode {
	if v, ok := ctx.Value(modeKey{}).(Mode); ok {
		return v
	}
	return ModeInherit
}

// ---- 错误分类 ----

// Category 是不含秘密的出站失败类别；日志、审计与管理界面只看它。
type Category string

const (
	// CategoryUnconfigured：路由选了 proxy 但代理未配置或凭据解封失败——失败关闭。
	CategoryUnconfigured Category = "proxy_unconfigured"
	// CategoryUnreachable：连不上代理端点（拒绝、无路由、DNS 失败）。
	CategoryUnreachable Category = "proxy_unreachable"
	// CategoryProtocol：端点不按 SOCKS5 应答（版本错、握手异常断开）。
	CategoryProtocol Category = "proxy_protocol"
	// CategoryAuthFailed：代理要求认证而设备没有，或用户名/口令被拒。
	CategoryAuthFailed Category = "proxy_auth_failed"
	// CategoryTargetFailed：代理连不上目标（REP 非 0）。
	CategoryTargetFailed Category = "proxy_target_failed"
	// CategoryTimeout：连代理或 SOCKS 握手超时。
	CategoryTimeout Category = "proxy_timeout"
	// CategoryTLSFailed：隧道建立后目标 TLS 握手或证书校验失败。
	CategoryTLSFailed Category = "tls_failed"
	// CategoryPlaintextRefused：拒绝经代理访问明文 http:// 目标。
	CategoryPlaintextRefused Category = "plaintext_refused"
)

// Text 是类别的界面说明（中文源文本）。
func (c Category) Text() string {
	switch c {
	case CategoryUnconfigured:
		return "该流量已选择经代理，但代理未配置或凭据不可用"
	case CategoryUnreachable:
		return "无法连接 SOCKS5 代理端点"
	case CategoryProtocol:
		return "代理端点没有按 SOCKS5 协议应答"
	case CategoryAuthFailed:
		return "SOCKS5 代理认证失败"
	case CategoryTargetFailed:
		return "代理无法连接目标服务"
	case CategoryTimeout:
		return "连接代理或 SOCKS5 握手超时"
	case CategoryTLSFailed:
		return "经代理的目标 TLS 握手失败"
	case CategoryPlaintextRefused:
		return "拒绝经代理访问明文 http:// 目标"
	}
	return string(c)
}

// Error 是本包产生的出站失败：只带分类、可安全展示的细节（如 SOCKS 应答码含义）与
// scope，不带代理地址、凭据或目标 URL。Unwrap 保留内层错误供 errors.As 判定超时。
type Error struct {
	Scope    Scope
	Category Category
	// Detail 是不含秘密的补充说明（可为空）。
	Detail  string
	err     error
	timeout bool
}

func (e *Error) Error() string {
	msg := "出站代理（" + e.Scope.Label() + "）：" + e.Category.Text()
	if e.Detail != "" {
		msg += "（" + e.Detail + "）"
	}
	return msg
}

func (e *Error) Unwrap() error { return e.err }

// Timeout / Temporary 让 *Error 满足 net.Error，既有调用方按超时归类的逻辑照常成立。
func (e *Error) Timeout() bool   { return e.timeout }
func (e *Error) Temporary() bool { return e.timeout }

// Classify 从错误链里取出本包的分类；不是本包产生的错误返回 ok=false。
func Classify(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
