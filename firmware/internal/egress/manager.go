package egress

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Router 给每个出站客户端一个绑定 scope 的 RoundTripper。生产实现是 *Manager；
// nil *Manager 也是合法的 Router（恒直连，测试与未装配路径用）。
type Router interface {
	Transport(scope Scope, base *http.Transport) http.RoundTripper
}

// TransportFor 是 Router 为 nil 时的兜底：直接用 base（直连）。
func TransportFor(r Router, scope Scope, base *http.Transport) http.RoundTripper {
	if r == nil {
		return base
	}
	return r.Transport(scope, base)
}

// Settings 是本包对单值配置存储的全部依赖（生产 *store.Store）。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	GetSealedSetting(ctx context.Context, key string) (string, error)
	// SetSettingsAtomic 在一个事务里写入若干明文项与若干密封项。
	SetSettingsAtomic(ctx context.Context, plain, sealed map[string]string) error
}

// settings 表里的键。用户名与口令密封（device-key），其余明文。
const (
	settingProvider = "egress.provider"
	settingAddress  = "egress.address"
	settingUsername = "egress.username"
	settingPassword = "egress.password"
	settingRoutes   = "egress.routes"
)

// DefaultTestTarget 是显式连通性测试的固定 HTTPS 目标：LLM Gate官网。测试只做
// TCP / SOCKS5 / TLS / HTTP HEAD 四层诊断，不发提示词、模型请求或任何上游凭据。
const DefaultTestTarget = "https://llm.net/"

// Options 装配 Manager。Settings 可为 nil（不持久化，只在内存里生效——测试用）。
type Options struct {
	Settings Settings
	Logger   *slog.Logger
	// TestTarget 覆盖显式测试的目标 URL（必须是 https://）；空取 DefaultTestTarget。
	TestTarget string
	// HandshakeTimeout 覆盖 SOCKS5 握手时限；零取 DefaultHandshakeTimeout。
	HandshakeTimeout time.Duration
	// TestRootCAs 只给测试注入本地 httptest 证书的信任根；生产恒 nil（系统信任链）。
	// 不是「关闭校验」的口子——它只能加信任根。
	TestRootCAs *x509.CertPool
	Now         func() time.Time
}

// Manager 持有当前出站策略快照，并协调各客户端 Transport 的原子切换。
type Manager struct {
	opt Options
	log *slog.Logger
	// snap 是当前策略快照（immutable，原子指针发布）。
	snap atomic.Pointer[snapshot]
	// mu 串行化 Load / Update / Apply，并保护下面的运行期状态。
	mu         sync.Mutex
	gen        uint64
	transports []*transport
	lastTest   *TestResult
	lastErrors map[Scope]PassiveError
}

// snapshot 是一份 immutable 的策略快照。dialer 只在 profile 可用时非 nil；
// ready=false 时 proxy 路由失败关闭（reason 不含秘密）。
type snapshot struct {
	gen    uint64
	policy Policy
	ready  bool
	reason string
	dialer func(scope Scope, dial dialFunc) *dialer
}

// PassiveError 是运行期某一类流量最近一次经代理失败的类别（被动记录，不轮询）。
type PassiveError struct {
	Category Category  `json:"category"`
	Message  string    `json:"message" i18n:"text"`
	At       time.Time `json:"at"`
}

// NewManager 构造 Manager；启动时先 Load 再接线各客户端。零策略（全直连）即刻可用。
func NewManager(opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.TestTarget == "" {
		opt.TestTarget = DefaultTestTarget
	}
	m := &Manager{opt: opt, log: opt.Logger.With("srv", "egress"), lastErrors: map[Scope]PassiveError{}}
	m.snap.Store(m.newSnapshot(Policy{}, true, ""))
	return m
}

func (m *Manager) newSnapshot(pol Policy, ready bool, reason string) *snapshot {
	m.gen++
	snap := &snapshot{gen: m.gen, policy: pol, ready: ready && pol.Profile.Configured(), reason: reason}
	if snap.ready {
		profile := pol.Profile
		timeout := m.opt.HandshakeTimeout
		snap.dialer = func(scope Scope, dial dialFunc) *dialer { return newDialer(scope, profile, dial, timeout) }
	}
	return snap
}

// Load 从设置存储重建快照。用户名/口令解封失败时保留地址与路由但标记不可用：选了
// proxy 的流量失败关闭，绝不降级成直连；错误只记类别，不含密文。
func (m *Manager) Load(ctx context.Context) error {
	if m == nil || m.opt.Settings == nil {
		return nil
	}
	pol, reason, err := m.load(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishLocked(pol, reason == "", reason)
	if reason != "" {
		m.log.Warn("出站代理凭据不可用，已选择经代理的流量将失败关闭", "reason", reason)
	}
	return nil
}

func (m *Manager) load(ctx context.Context) (Policy, string, error) {
	st := m.opt.Settings
	var pol Policy
	provider, err := st.GetSetting(ctx, settingProvider)
	if err != nil {
		return pol, "", err
	}
	if pol.Profile.Provider, err = ParseProvider(provider); err != nil {
		// 库里出现未知方式（更新的固件写的？）：当作未配置，让管理员重新选择。
		pol.Profile.Provider = ProviderNone
	}
	if pol.Profile.Address, err = st.GetSetting(ctx, settingAddress); err != nil {
		return pol, "", err
	}
	rawRoutes, err := st.GetSetting(ctx, settingRoutes)
	if err != nil {
		return pol, "", err
	}
	pol.Routes = parseRoutes(rawRoutes)
	reason := ""
	if pol.Profile.Username, err = st.GetSealedSetting(ctx, settingUsername); err != nil {
		reason = "SOCKS5 用户名解封失败（设备密钥被替换或密文损坏），请重新填写代理认证"
	}
	if pol.Profile.Password, err = st.GetSealedSetting(ctx, settingPassword); err != nil {
		reason = "SOCKS5 口令解封失败（设备密钥被替换或密文损坏），请重新填写代理认证"
	}
	if reason != "" {
		pol.Profile.Username, pol.Profile.Password = "", ""
	}
	return pol, reason, nil
}

func parseRoutes(raw string) Routes {
	out := Routes{}
	if raw == "" {
		return out
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return out
	}
	for k, v := range stored {
		if Scope(k).Valid() && Route(v) == RouteProxy {
			out[Scope(k)] = RouteProxy
		}
	}
	return out
}

// publishLocked 发布新快照并让每个客户端 Transport 退役旧代理连接池（关闭空闲连接；
// 在飞请求持有旧 *http.Transport 的引用，按原路径完成）。调用方持 m.mu。
func (m *Manager) publishLocked(pol Policy, ready bool, reason string) {
	pol.Routes = pol.Routes.Normalized()
	snap := m.newSnapshot(pol, ready, reason)
	m.snap.Store(snap)
	for _, t := range m.transports {
		t.retire()
	}
	if !snap.policy.Profile.Configured() {
		m.lastTest = nil
	}
}

// Apply 直接发布一份策略（不持久化）。测试与不带存储的装配用；生产走 Update。
func (m *Manager) Apply(pol Policy) error {
	if err := pol.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishLocked(pol, true, "")
	return nil
}

// Policy 返回当前生效策略（含凭据明文，只供本包与测试使用；管理面读 Status）。
func (m *Manager) Policy() Policy {
	if m == nil {
		return Policy{Routes: Routes{}.Normalized()}
	}
	return m.snap.Load().policy
}

// ---- 管理面读数与更新 ----

// Status 是管理 API 的安全读数：不含用户名与口令明文。
type Status struct {
	Provider    Provider `json:"provider"`
	Address     string   `json:"address"`
	UsernameSet bool     `json:"username_set"`
	PasswordSet bool     `json:"password_set"`
	// Configured：已声明代理端点；Available：凭据可用（解封成功）。
	Configured        bool                   `json:"configured"`
	Available         bool                   `json:"available"`
	UnavailableReason string                 `json:"unavailable_reason,omitempty" i18n:"text"`
	Routes            map[Scope]Route        `json:"routes"`
	LastTest          *TestResult            `json:"last_test,omitempty"`
	LastErrors        map[Scope]PassiveError `json:"last_errors,omitempty"`
}

// Status 组装读数。
func (m *Manager) Status() Status {
	if m == nil {
		return Status{Routes: Routes{}.Normalized()}
	}
	snap := m.snap.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Provider:    snap.policy.Profile.Provider,
		Address:     snap.policy.Profile.Address,
		UsernameSet: snap.policy.Profile.Username != "",
		PasswordSet: snap.policy.Profile.Password != "",
		Configured:  snap.policy.Profile.Configured(),
		Available:   snap.ready,
		Routes:      snap.policy.Routes.Normalized(),
		LastTest:    m.lastTest,
	}
	if st.Configured && !snap.ready {
		st.UnavailableReason = snap.reason
	}
	if len(m.lastErrors) > 0 {
		st.LastErrors = make(map[Scope]PassiveError, len(m.lastErrors))
		for k, v := range m.lastErrors {
			st.LastErrors[k] = v
		}
	}
	return st
}

// Change 是管理面的一次更新：省略（nil）即保留；Username / Password 传空串即清除；
// Provider 传空串即清除整个 profile（地址与认证一并清空）。Routes 只覆盖给出的分类。
type Change struct {
	Provider *string
	Address  *string
	Username *string
	Password *string
	Routes   map[Scope]Route
}

// ValidationError 是更新被拒的原因（管理面映射 400）；文本不含凭据。
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

// Update 整组校验并原子落库后发布新快照。校验失败不写库、不换快照。
// 返回值描述本次改动（供审计），不含地址与凭据。
func (m *Manager) Update(ctx context.Context, ch Change) (Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.snap.Load().policy
	next := Policy{Profile: cur.Profile, Routes: cur.Routes.Normalized()}
	var sum Summary
	if ch.Provider != nil {
		p, err := ParseProvider(strings.TrimSpace(*ch.Provider))
		if err != nil {
			return sum, &ValidationError{err.Error()}
		}
		if p != next.Profile.Provider {
			sum.ProviderChanged = true
		}
		next.Profile.Provider = p
		if p == ProviderNone {
			next.Profile.Address, next.Profile.Username, next.Profile.Password = "", "", ""
			sum.AuthCleared = cur.Profile.HasAuth()
		}
	}
	if ch.Provider != nil && next.Profile.Provider.Managed() && !cur.Profile.Provider.Managed() {
		// 切到内置内核：地址与认证一并交给内核管理器，从空开始。
		next.Profile.Address, next.Profile.Username, next.Profile.Password = "", "", ""
		sum.AuthCleared = cur.Profile.HasAuth()
	}
	if next.Profile.Provider.Managed() {
		if ch.Address != nil || ch.Username != nil || ch.Password != nil {
			return sum, &ValidationError{"内置内核的地址与认证由固件管理，不能在这里填写"}
		}
	}
	if ch.Address != nil && next.Profile.Provider != ProviderNone {
		addr := strings.TrimSpace(*ch.Address)
		if addr != next.Profile.Address {
			sum.AddressChanged = true
		}
		next.Profile.Address = addr
	}
	if next.Profile.Provider != ProviderNone && !next.Profile.Provider.Managed() {
		if ch.Username != nil {
			if *ch.Username == "" {
				sum.AuthCleared = sum.AuthCleared || next.Profile.Username != ""
			} else {
				sum.AuthReplaced = true
			}
			next.Profile.Username = *ch.Username
		}
		if ch.Password != nil {
			if *ch.Password == "" {
				sum.AuthCleared = sum.AuthCleared || next.Profile.Password != ""
			} else {
				sum.AuthReplaced = true
			}
			next.Profile.Password = *ch.Password
		}
	}
	for k, v := range ch.Routes {
		if !k.Valid() {
			return sum, &ValidationError{fmt.Sprintf("未知的流量分类 %q", string(k))}
		}
		if _, err := ParseRoute(string(v)); err != nil {
			return sum, &ValidationError{err.Error()}
		}
		if next.Routes[k] != v {
			sum.RoutesChanged = true
		}
		next.Routes[k] = v
	}
	if err := next.Validate(); err != nil {
		return sum, &ValidationError{err.Error()}
	}
	if m.opt.Settings != nil {
		routes, _ := json.Marshal(next.Routes)
		plain := map[string]string{
			settingProvider: string(next.Profile.Provider),
			settingAddress:  next.Profile.Address,
			settingRoutes:   string(routes),
		}
		sealed := map[string]string{
			settingUsername: next.Profile.Username,
			settingPassword: next.Profile.Password,
		}
		if err := m.opt.Settings.SetSettingsAtomic(ctx, plain, sealed); err != nil {
			return sum, fmt.Errorf("保存出站代理设置: %w", err)
		}
	}
	m.publishLocked(next, true, "")
	sum.Provider = next.Profile.Provider
	sum.Routes = next.Routes
	m.log.Info("出站代理设置已更新", "provider", string(next.Profile.Provider),
		"proxy_scopes", scopeNames(next.Routes.ProxyScopes()), "auth", next.Profile.HasAuth())
	return sum, nil
}

// ProviderIsCore 报告当前代理方式是不是内置内核（mihomo 管理器据此拒绝在别的方式下启用）。
func (m *Manager) ProviderIsCore() bool {
	if m == nil {
		return false
	}
	return m.snap.Load().policy.Profile.Provider.Managed()
}

// SetCoreAddress 由内置内核管理器调用：内核起来后把 profile 地址写成本机回环地址、停用时
// 清空。当前方式不是 mihomo 时是空操作（管理员已经换了方式）。清空时若仍有分类选 proxy，
// forceDirect 为真则同一事务把它们改回 direct，否则拒绝。
func (m *Manager) SetCoreAddress(ctx context.Context, addr string, forceDirect bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.snap.Load().policy
	if !cur.Profile.Provider.Managed() {
		return nil
	}
	next := Policy{Profile: cur.Profile, Routes: cur.Routes.Normalized()}
	next.Profile.Address = strings.TrimSpace(addr)
	if next.Profile.Address == "" && len(next.Routes.ProxyScopes()) > 0 {
		if !forceDirect {
			return &ValidationError{"仍有流量选择经代理，停用内置内核前请先把它们改回直连"}
		}
		for _, s := range Scopes {
			next.Routes[s] = RouteDirect
		}
	}
	if err := next.Validate(); err != nil {
		return &ValidationError{err.Error()}
	}
	if m.opt.Settings != nil {
		routes, _ := json.Marshal(next.Routes)
		plain := map[string]string{settingAddress: next.Profile.Address, settingRoutes: string(routes)}
		if err := m.opt.Settings.SetSettingsAtomic(ctx, plain, nil); err != nil {
			return fmt.Errorf("保存出站代理设置: %w", err)
		}
	}
	m.publishLocked(next, true, "")
	m.log.Info("内置内核地址已更新", "configured", next.Profile.Configured(),
		"proxy_scopes", scopeNames(next.Routes.ProxyScopes()))
	return nil
}

// Summary 描述一次 Update 改了什么（审计用；不含地址与凭据）。
type Summary struct {
	Provider        Provider
	Routes          Routes
	ProviderChanged bool
	AddressChanged  bool
	AuthReplaced    bool
	AuthCleared     bool
	RoutesChanged   bool
}

func scopeNames(scopes []Scope) string {
	names := make([]string, 0, len(scopes))
	for _, s := range scopes {
		names = append(names, string(s))
	}
	return strings.Join(names, ",")
}

// ---- 客户端 Transport ----

// Transport 实现 Router：返回绑定 scope 的 RoundTripper。base 是该客户端自己的直连
// Transport（超时、重定向、压缩与连接池按调用方原样保留）；代理侧按快照克隆 base 并换成
// SOCKS5 拨号器，direct / proxy 两个连接池永不混用。
func (m *Manager) Transport(scope Scope, base *http.Transport) http.RoundTripper {
	if m == nil {
		return base
	}
	t := &transport{m: m, scope: scope, base: base}
	m.mu.Lock()
	m.transports = append(m.transports, t)
	m.mu.Unlock()
	return t
}

type transport struct {
	m     *Manager
	scope Scope
	base  *http.Transport

	mu    sync.Mutex
	gen   uint64
	proxy *http.Transport
}

// RoundTrip 每次只读一次快照并固定本次尝试的路径。
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	snap := t.m.snap.Load()
	route := snap.policy.RouteFor(t.scope, ModeFrom(req.Context()))
	if route == RouteDirect {
		return t.base.RoundTrip(req)
	}
	if !snap.ready {
		return nil, t.fail(&Error{Scope: t.scope, Category: CategoryUnconfigured, Detail: snap.reason, err: errors.New("egress: proxy unavailable")})
	}
	if req.URL == nil || !strings.EqualFold(req.URL.Scheme, "https") {
		return nil, t.fail(&Error{Scope: t.scope, Category: CategoryPlaintextRefused, err: errors.New("egress: plaintext target")})
	}
	resp, err := t.proxyFor(snap).RoundTrip(req)
	if err != nil {
		return nil, t.classify(err)
	}
	return resp, nil
}

// proxyFor 取（必要时按当前快照重建）本客户端的代理 Transport。
func (t *transport) proxyFor(snap *snapshot) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proxy != nil && t.gen == snap.gen {
		return t.proxy
	}
	old := t.proxy
	pt := t.base.Clone()
	pt.Proxy = nil
	pt.DialTLSContext = nil
	d := snap.dialer(t.scope, t.base.DialContext)
	pt.DialContext = d.DialContext
	t.proxy, t.gen = pt, snap.gen
	if old != nil {
		old.CloseIdleConnections()
	}
	return pt
}

// retire 在快照更换后丢掉旧代理连接池：关闭空闲连接，在飞请求不受影响。
func (t *transport) retire() {
	t.mu.Lock()
	old := t.proxy
	t.proxy = nil
	t.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

// CloseIdleConnections 让 http.Client.CloseIdleConnections 对两个连接池都生效。
func (t *transport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
	t.mu.Lock()
	p := t.proxy
	t.mu.Unlock()
	if p != nil {
		p.CloseIdleConnections()
	}
}

// classify 给经代理的失败归类：拨号器已归类的原样返回；隧道建立后的目标 TLS 失败记为
// tls_failed；其余（响应头超时、传输中断等）是普通上游错误，原样透传。
func (t *transport) classify(err error) error {
	if e, ok := Classify(err); ok {
		return t.fail(e)
	}
	if isTLSError(err) {
		return t.fail(&Error{Scope: t.scope, Category: CategoryTLSFailed, err: err})
	}
	return err
}

func (t *transport) fail(e *Error) error {
	t.m.notePassive(e)
	return e
}

func isTLSError(err error) bool {
	var (
		certErr    *tls.CertificateVerificationError
		recordErr  tls.RecordHeaderError
		alertErr   tls.AlertError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		invalidErr x509.CertificateInvalidError
	)
	return errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) ||
		errors.As(err, &unknownCA) || errors.As(err, &hostErr) || errors.As(err, &invalidErr)
}

// notePassive 记下某一类流量最近一次经代理失败的类别（管理界面展示，不轮询）。
func (m *Manager) notePassive(e *Error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastErrors[e.Scope] = PassiveError{Category: e.Category, Message: e.Category.Text(), At: m.opt.Now()}
}

// ---- 显式连通性测试 ----

// TestStage 是分层诊断里的一层。
type TestStage struct {
	Name      string `json:"name"` // proxy_tcp | socks5 | tls | https
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail,omitempty" i18n:"text"`
}

// TestResult 是一次显式测试的结果；Target 只有主机名。
type TestResult struct {
	OK       bool        `json:"ok"`
	At       time.Time   `json:"at"`
	Target   string      `json:"target"`
	Stages   []TestStage `json:"stages"`
	Category Category    `json:"category,omitempty"`
	Message  string      `json:"message,omitempty" i18n:"text"`
}

// ErrNotConfigured 表示测试时没有可用的代理。
var ErrNotConfigured = errors.New("尚未配置代理端点")

// Test 用当前快照对固定 HTTPS 目标做一次 TCP → SOCKS5 → TLS → HTTP HEAD 的分层诊断。
// 不发提示词、模型请求或上游凭据；结果记入 Status.LastTest。
func (m *Manager) Test(ctx context.Context) (TestResult, error) {
	if m == nil {
		return TestResult{}, ErrNotConfigured
	}
	snap := m.snap.Load()
	if !snap.policy.Profile.Configured() {
		return TestResult{}, ErrNotConfigured
	}
	res := TestResult{At: m.opt.Now()}
	target, err := url.Parse(m.opt.TestTarget)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" {
		return res, errors.New("测试目标必须是 https:// 地址")
	}
	res.Target = target.Hostname()
	if !snap.ready {
		res.Category, res.Message = CategoryUnconfigured, snap.reason
		m.recordTest(res)
		return res, nil
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	base := &net.Dialer{Timeout: 10 * time.Second}
	d := snap.dialer(ScopeOfficialSite, base.DialContext)
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 1. TCP 到代理。
	start := m.opt.Now()
	conn, err := base.DialContext(tctx, "tcp", d.proxy)
	if err != nil {
		e, _ := d.classifyDial(err).(*Error)
		res.Stages = append(res.Stages, TestStage{Name: "proxy_tcp", LatencyMS: sinceMS(m.opt.Now(), start), Detail: e.Category.Text()})
		res.Category, res.Message = e.Category, e.Category.Text()
		m.recordTest(res)
		return res, nil
	}
	defer conn.Close()
	res.Stages = append(res.Stages, TestStage{Name: "proxy_tcp", OK: true, LatencyMS: sinceMS(m.opt.Now(), start)})

	// 2. SOCKS5 握手 + CONNECT。
	start = m.opt.Now()
	portNum, _ := strconv.Atoi(port)
	if err := d.handshake(tctx, conn, target.Hostname(), portNum); err != nil {
		e, _ := Classify(err)
		res.Stages = append(res.Stages, TestStage{Name: "socks5", LatencyMS: sinceMS(m.opt.Now(), start), Detail: e.Category.Text() + detailSuffix(e)})
		res.Category, res.Message = e.Category, e.Category.Text()+detailSuffix(e)
		m.recordTest(res)
		return res, nil
	}
	res.Stages = append(res.Stages, TestStage{Name: "socks5", OK: true, LatencyMS: sinceMS(m.opt.Now(), start)})

	// 3. 目标 TLS（系统信任链，SNI 为目标主机名）。
	start = m.opt.Now()
	tconn := tls.Client(conn, &tls.Config{ServerName: target.Hostname(), MinVersion: tls.VersionTLS12, RootCAs: m.opt.TestRootCAs})
	if err := tconn.HandshakeContext(tctx); err != nil {
		res.Stages = append(res.Stages, TestStage{Name: "tls", LatencyMS: sinceMS(m.opt.Now(), start), Detail: CategoryTLSFailed.Text()})
		res.Category, res.Message = CategoryTLSFailed, CategoryTLSFailed.Text()
		m.recordTest(res)
		return res, nil
	}
	res.Stages = append(res.Stages, TestStage{Name: "tls", OK: true, LatencyMS: sinceMS(m.opt.Now(), start)})
	tconn.Close()

	// 4. 完整 HTTP HEAD（新建一条经代理的连接，走与生产相同的 Transport 路径）。
	start = m.opt.Now()
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          1,
	}
	if m.opt.TestRootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: m.opt.TestRootCAs, MinVersion: tls.VersionTLS12}
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(tctx, http.MethodHead, target.String(), nil)
	if err != nil {
		return res, errors.New("构造测试请求失败")
	}
	req.Header.Set("User-Agent", "")
	resp, err := hc.Do(req)
	if err != nil {
		cat, msg := CategoryTargetFailed, "HTTP 请求失败"
		if e, ok := Classify(err); ok {
			cat, msg = e.Category, e.Category.Text()
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			cat, msg = CategoryTimeout, "HTTP 响应超时"
		}
		res.Stages = append(res.Stages, TestStage{Name: "https", LatencyMS: sinceMS(m.opt.Now(), start), Detail: msg})
		res.Category, res.Message = cat, msg
		m.recordTest(res)
		return res, nil
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	res.Stages = append(res.Stages, TestStage{Name: "https", OK: true, LatencyMS: sinceMS(m.opt.Now(), start),
		Detail: fmt.Sprintf("HTTP %d", resp.StatusCode)})
	res.OK = true
	res.Message = "代理可用：目标 TLS 与 HTTPS 请求均成功"
	m.recordTest(res)
	return res, nil
}

func detailSuffix(e *Error) string {
	if e == nil || e.Detail == "" {
		return ""
	}
	return "（" + e.Detail + "）"
}

func (m *Manager) recordTest(res TestResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := res
	m.lastTest = &r
	m.log.Info("出站代理连通性测试", "ok", res.OK, "category", string(res.Category))
}

func sinceMS(now, start time.Time) int64 {
	if ms := now.Sub(start).Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}
