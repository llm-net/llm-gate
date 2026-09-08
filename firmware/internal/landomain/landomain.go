// Package landomain 给设备一个内网域名，并为它签发、续期、热加载一张公网可信的 HTTPS 证书。
//
// 提供方式二选一（lan_domain.provider）：
//
//   - official_site：通过 LLM Gate官网申领 <label>.llm.net。官网把 A 记录指到设备自报的
//     内网 IPv4，DNS-01 的 TXT 直接写在该名字下。
//   - own_domain：设备管理员自己的域名（box.example.com）。官网不碰它的解析：管理员自己把 A 记录
//     指到设备内网 IP，再把 _acme-challenge.<域名> CNAME 到官网分配的委托名 <id>.acme.llm.net；
//     官网把 DNS-01 的 TXT 写在委托名上，CA 顺着 CNAME 验证。两条记录设一次，续期全自动。
//
// 两种方式都先关联官网账号（证书由官网以该账号名义向 CA 申请）。分工：
//
//   - 官网（website/functions/_lib/lan-domain.ts）：确认设备与官网账号的关联、（托管域名）写 A 记录、
//     按配额选择 CA（Google Trust Services / Let's Encrypt）并用 DNS-01 完成验证、把证书链交回设备；
//     自有域名另提供只读的 DNS 检查（A / 委托 CNAME / CAA 逐条核对）。
//   - 设备（本包）：发起关联并保管设备令牌（device-key 密封）、生成证书私钥与 CSR、
//     轮询订单、落盘证书、给网关的 HTTPS 监听供证书、每半天同步解析地址（托管域名）并在到期前续期。
//
// 证书私钥永远不离开设备；官网只见到 CSR。域名解析到的是内网地址，CA 从公网打不到设备，
// 所以只能 DNS-01——那条 TXT 记录由官网代发，设备不接触 Cloudflare 或管理员的 DNS 服务商。
//
// 边界（根 AGENTS.md）：域名恒为可选便利。官网不可达、令牌失效、签发失败只让 HTTPS
// 入口不可用；纯 IP 网关、本地管理台、固件上传与回退不受影响。本包不进健康前置条件。
//
// 落盘（<data_dir>/lan-domain/，目录 0700、文件 0600）：
//
//	private-key.pem          当前证书的私钥（EC P-256）——§15.1 密钥物料
//	private-key.pending.pem  正在签发的私钥（订单在飞时与官网侧 CSR 配对）
//	certificate.pem          证书链（公开数据）
//
// 其余状态（提供方、链接令牌、域名、解析地址、委托名、监听地址、最近结果）在 settings 表，
// 令牌走 SetSealedSetting。§15.1：令牌与私钥不进日志、审计与 API 响应。
package landomain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 提供方式。
const (
	ProviderNone         = ""
	ProviderOfficialSite = "official_site"
	ProviderOwnDomain    = "own_domain"
)

// 域名种类（官网 lan_domains.kind 的同一口径）：托管 <label>.llm.net / 自有域名。
const (
	KindManaged = "managed"
	KindCustom  = "custom"
)

// DefaultSuffix 是官网签发的内网域名后缀；官网 link 读数会带回实际值。
const DefaultSuffix = "llm.net"

// DefaultHTTPSListen 是域名 HTTPS 监听的缺省地址。gatewayd 的 systemd unit 带
// CAP_NET_BIND_SERVICE，非 root 也能绑 443。
const DefaultHTTPSListen = ":443"

// settings 键。
const (
	settingProvider     = "lan_domain.provider"
	settingLinkToken    = "lan_domain.link_token" // sealed
	settingLinkID       = "lan_domain.link_id"
	settingLinkAccount  = "lan_domain.link_account"
	settingSuffix       = "lan_domain.suffix"
	settingHostname     = "lan_domain.hostname"
	settingTargetIP     = "lan_domain.target_ip"
	settingAcmeDelegate = "lan_domain.acme_delegate"
	settingHTTPSListen  = "lan_domain.https_listen"
	settingLastIssuedAt = "lan_domain.last_issued_at"
	settingLastCA       = "lan_domain.last_ca"
	settingLastError    = "lan_domain.last_error"
	settingLastErrorAt  = "lan_domain.last_error_at"
)

// 签发阶段（Status.IssueStage / 管理面 issue_stage）。前四个与官网订单状态同名；
// validating 是 challenging 且挑战已交给 CA；submitting / installing 是设备侧的两头。
const (
	StageSubmitting  = "submitting"
	StagePending     = "pending"
	StageAuthorizing = "authorizing"
	StageChallenging = "challenging"
	StageValidating  = "validating"
	StageFinalizing  = "finalizing"
	StageInstalling  = "installing"
)

// 落盘文件名。
const (
	certKeyFile        = "private-key.pem"
	certPendingKeyFile = "private-key.pending.pem"
	certChainFile      = "certificate.pem"
)

// 时序。
const (
	// renewBefore：剩余有效期短于它即续期。90 天证书配 30 天窗口，容得下几十次失败重试。
	renewBefore = 30 * 24 * time.Hour
	// issueTimeout 覆盖一次签发全程（官网侧 ACME 订单 + 轮询）。
	issueTimeout = 8 * time.Minute
	// maintenanceStartDelay / maintenanceInterval：维护协程的启动延迟与周期。
	maintenanceStartDelay = time.Minute
	maintenanceInterval   = 12 * time.Hour
	// defaultPollInterval 是订单与关联轮询的缺省间隔（官网给的 interval/retryAfter 更长时以官网为准）。
	defaultPollInterval = 5 * time.Second
)

// 哨兵错误（管理面按它们映射状态码）。
var (
	ErrProviderNotSet  = errors.New("请先选择内网域名的提供方式")
	ErrWrongProvider   = errors.New("当前的提供方式不支持这个操作")
	ErrNotLinked       = errors.New("尚未关联 LLM Gate官网账号")
	ErrNoLinkSession   = errors.New("没有进行中的账号关联")
	ErrNotClaimed      = errors.New("尚未申领内网域名")
	ErrDomainClaimed   = errors.New("已申领内网域名：请先释放域名再改变提供方式")
	ErrIssueBusy       = errors.New("已有一次证书签发在进行中")
	ErrTokenUnreadable = errors.New("官网关联令牌解封失败（设备密钥被替换或密文损坏），请重新关联账号")
	ErrInvalidListen   = errors.New("HTTPS 监听地址不是合法的 host:port")
	ErrSiteUnavailable = errors.New("本进程未接入 LLM Gate官网")
)

// ValidationError 是管理员输入（前缀、域名、地址）没过设备侧预检；管理面据此答 400。
type ValidationError struct{ msg string }

func (e *ValidationError) Error() string { return e.msg }

func invalid(msg string) error { return &ValidationError{msg: msg} }

// Settings 是本包对单值配置的全部依赖（生产 *store.Store）。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	GetSealedSetting(ctx context.Context, key string) (string, error)
	SetSealedSetting(ctx context.Context, key, value string) error
}

// Listener 是本包对网关域名 HTTPS 监听的全部依赖（生产 *gateway.Server）。
type Listener interface {
	StartDomainTLS(addr string) error
	StopDomainTLS() error
	DomainTLSAddr() string
}

// Options 装配 Manager。Settings 必填；Site 为 nil 时所有出网动作答 ErrSiteUnavailable；
// Listener 为 nil 时只管证书不起监听（测试）。
type Options struct {
	DataDir  string
	Settings Settings
	Site     *SiteClient
	Listener Listener
	Logger   *slog.Logger
	// Model / DeviceName 是发起关联时自报给账号持有人辨认的展示信息，不是设备身份。
	Model      string
	DeviceName string
	// Version 是固件版本名称，随关联自报。
	Version string
	// Now / PollInterval 是测试注入点。
	Now          func() time.Time
	PollInterval time.Duration
}

// LinkSession 是一次进行中的账号关联（进程内，不落盘：关联码 15 分钟即失效）。
type LinkSession struct {
	UserCode                string
	VerificationURL         string
	VerificationURLComplete string
	ExpiresAt               time.Time
	// Status：pending / approved / denied / expired / failed。
	Status  string
	Account string
	Error   string

	deviceCode string
	interval   time.Duration
}

// String / GoString：会话里带 device code（等价于一次性凭据），格式化输出一律遮掉。
func (s LinkSession) String() string {
	return fmt.Sprintf("LinkSession{user_code=%s status=%s}", s.UserCode, s.Status)
}
func (s LinkSession) GoString() string { return s.String() }
func (s LinkSession) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user_code", s.UserCode), slog.String("status", s.Status))
}

// State 是持久化的域名状态（全部可公开：域名与 IP 本就要进公共 DNS）。
type State struct {
	Provider    string
	Linked      bool
	LinkID      string
	Account     string
	Suffix      string
	Hostname    string
	TargetIP    string
	HTTPSListen string
	// AcmeDelegate 是自有域名的 DNS-01 委托名（_acme-challenge.<域名> 须 CNAME 到它）；托管域名为空。
	AcmeDelegate string

	LastIssuedAt time.Time
	LastCA       string
	LastError    string
	LastErrorAt  time.Time
}

// Label 返回托管域名的前缀（hostname 去掉后缀）；自有域名没有前缀。
func (s State) Label() string {
	if s.Hostname == "" || s.Provider != ProviderOfficialSite {
		return ""
	}
	return strings.TrimSuffix(s.Hostname, "."+s.SuffixOrDefault())
}

// Kind 返回域名种类；未选提供方式时空串。
func (s State) Kind() string {
	switch s.Provider {
	case ProviderOfficialSite:
		return KindManaged
	case ProviderOwnDomain:
		return KindCustom
	}
	return ""
}

// DNSRecord 是自有域名持有人须在自己的 DNS 服务商设置的一条记录。
type DNSRecord struct {
	Type  string
	Name  string
	Value string
}

// DNSRecords 列出自有域名要设的记录：A 指到设备内网 IP、_acme-challenge 委托到官网。托管域名为空。
func (s State) DNSRecords() []DNSRecord {
	if s.Provider != ProviderOwnDomain || s.Hostname == "" {
		return nil
	}
	out := []DNSRecord{{Type: "A", Name: s.Hostname, Value: s.TargetIP}}
	if s.AcmeDelegate != "" {
		out = append(out, DNSRecord{Type: "CNAME", Name: "_acme-challenge." + s.Hostname, Value: s.AcmeDelegate})
	}
	return out
}

// SuffixOrDefault 返回域名后缀；官网还没告知时取缺省。
func (s State) SuffixOrDefault() string {
	if s.Suffix == "" {
		return DefaultSuffix
	}
	return s.Suffix
}

// Claimed 报告是否已申领域名。
func (s State) Claimed() bool { return s.Hostname != "" }

// CertStatus 是当前证书的读数。
type CertStatus struct {
	NotBefore      time.Time
	NotAfter       time.Time
	Issuer         string
	SANs           []string
	ExpiringSoon   bool
	CoversHostname bool
}

// Status 是管理面读数的完整快照。
type Status struct {
	State   State
	Link    *LinkSession
	Issuing bool
	// IssueStage 是进行中签发的当前阶段（Stage* 常量）；不在签发时空串。
	IssueStage string
	Cert       *CertStatus
	HTTPSAddr  string
	// SiteConfigured 报告本进程是否接了官网客户端。
	SiteConfigured bool
}

// Manager 驱动关联、申领、签发、续期与证书热加载。零值不可用，经 [New] 构造。
type Manager struct {
	opt Options
	dir string
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	link    *LinkSession
	issuing bool
	// issueStage / issueNotify / issueErr：进行中签发的阶段、阶段变化或结束时关闭的通知通道、
	// 最近一次签发的结果（StartIssue 时清零）。WaitIssue 靠它们做「阶段一变就返回」的陪等。
	issueStage  string
	issueNotify chan struct{}
	issueErr    error
	cert        atomic.Pointer[tls.Certificate]
}

// New 装配 Manager 并从磁盘恢复证书。恢复失败只降级：证书是增强能力，坏文件不该让
// 网关起不来。
func New(opt Options) *Manager {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.PollInterval <= 0 {
		opt.PollInterval = defaultPollInterval
	}
	m := &Manager{
		opt: opt,
		dir: filepath.Join(opt.DataDir, "lan-domain"),
		log: opt.Logger.With("srv", "landomain"),
		now: opt.Now,
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		m.log.Error("创建内网域名数据目录失败，证书功能不可用", "err", err.Error())
		return m
	}
	m.loadCertificate()
	return m
}

func (m *Manager) path(name string) string { return filepath.Join(m.dir, name) }

// ---- 状态读写 ----

func (m *Manager) get(ctx context.Context, key string) string {
	v, err := m.opt.Settings.GetSetting(ctx, key)
	if err != nil {
		m.log.Warn("读取内网域名设置失败", "key", key, "err", err.Error())
		return ""
	}
	return v
}

func (m *Manager) set(ctx context.Context, key, value string) error {
	return m.opt.Settings.SetSetting(ctx, key, value)
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// LoadState 读持久化状态。令牌不在其中（Linked 只报告有没有）。
func (m *Manager) LoadState(ctx context.Context) State {
	st := State{
		Provider:     m.get(ctx, settingProvider),
		LinkID:       m.get(ctx, settingLinkID),
		Account:      m.get(ctx, settingLinkAccount),
		Suffix:       m.get(ctx, settingSuffix),
		Hostname:     m.get(ctx, settingHostname),
		TargetIP:     m.get(ctx, settingTargetIP),
		HTTPSListen:  m.get(ctx, settingHTTPSListen),
		AcmeDelegate: m.get(ctx, settingAcmeDelegate),
		LastIssuedAt: parseTime(m.get(ctx, settingLastIssuedAt)),
		LastCA:       m.get(ctx, settingLastCA),
		LastError:    m.get(ctx, settingLastError),
		LastErrorAt:  parseTime(m.get(ctx, settingLastErrorAt)),
		Linked:       m.get(ctx, settingLinkToken) != "",
	}
	if st.HTTPSListen == "" {
		st.HTTPSListen = DefaultHTTPSListen
	}
	switch st.Provider {
	case ProviderOfficialSite, ProviderOwnDomain:
	default:
		st.Provider = ProviderNone
	}
	return st
}

// token 解封设备令牌；未关联返回 ErrNotLinked，解不开返回 ErrTokenUnreadable。
func (m *Manager) token(ctx context.Context) (string, error) {
	tok, err := m.opt.Settings.GetSealedSetting(ctx, settingLinkToken)
	if err != nil {
		return "", ErrTokenUnreadable
	}
	if tok == "" {
		return "", ErrNotLinked
	}
	return tok, nil
}

func (m *Manager) site() (*SiteClient, error) {
	if m.opt.Site == nil {
		return nil, ErrSiteUnavailable
	}
	return m.opt.Site, nil
}

func (m *Manager) recordError(ctx context.Context, step string, err error) error {
	full := step + "：" + err.Error()
	if err := m.set(ctx, settingLastError, full); err != nil {
		m.log.Warn("记录内网域名错误失败", "err", err.Error())
	}
	_ = m.set(ctx, settingLastErrorAt, fmtTime(m.now()))
	m.log.Warn("内网域名操作失败", "step", step, "err", err.Error())
	return errors.New(full)
}

func (m *Manager) clearError(ctx context.Context) {
	_ = m.set(ctx, settingLastError, "")
	_ = m.set(ctx, settingLastErrorAt, "")
}

// Status 返回当前快照。
func (m *Manager) Status(ctx context.Context) Status {
	st := m.LoadState(ctx)
	m.mu.Lock()
	var link *LinkSession
	if m.link != nil {
		copy := *m.link
		copy.deviceCode = ""
		if copy.Status == "pending" && m.now().After(copy.ExpiresAt) {
			copy.Status = "expired"
		}
		link = &copy
	}
	issuing, stage := m.issuing, m.issueStage
	m.mu.Unlock()
	out := Status{State: st, Link: link, Issuing: issuing, IssueStage: stage, Cert: m.certStatus(st.Hostname), SiteConfigured: m.opt.Site != nil}
	if m.opt.Listener != nil {
		out.HTTPSAddr = m.opt.Listener.DomainTLSAddr()
	}
	return out
}

func (m *Manager) certStatus(hostname string) *CertStatus {
	cert := m.cert.Load()
	if cert == nil {
		return nil
	}
	leaf := cert.Leaf
	if leaf == nil && len(cert.Certificate) > 0 {
		var err error
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil
		}
	}
	if leaf == nil {
		return nil
	}
	return &CertStatus{
		NotBefore:      leaf.NotBefore,
		NotAfter:       leaf.NotAfter,
		Issuer:         leaf.Issuer.CommonName,
		SANs:           leaf.DNSNames,
		ExpiringSoon:   m.now().After(leaf.NotAfter.Add(-renewBefore)),
		CoversHostname: hostname != "" && leaf.VerifyHostname(hostname) == nil,
	}
}

// ---- 提供方 ----

// SetProvider 选择提供方式。已申领/登记域名时不能换（先释放；关联保留——令牌只用于域名，
// 留着无害，管理员可另行解除）。
func (m *Manager) SetProvider(ctx context.Context, provider string) error {
	switch provider {
	case ProviderNone, ProviderOfficialSite, ProviderOwnDomain:
	default:
		return invalid(fmt.Sprintf("未知的提供方式 %q", provider))
	}
	st := m.LoadState(ctx)
	if provider != st.Provider && st.Claimed() {
		return ErrDomainClaimed
	}
	return m.set(ctx, settingProvider, provider)
}

// SetHTTPSListen 修改域名 HTTPS 监听地址；已有证书时立即按新地址重启监听。
func (m *Manager) SetHTTPSListen(ctx context.Context, addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = DefaultHTTPSListen
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ErrInvalidListen
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return ErrInvalidListen
	}
	if host != "" && net.ParseIP(host) == nil {
		return ErrInvalidListen
	}
	if err := m.set(ctx, settingHTTPSListen, addr); err != nil {
		return err
	}
	if m.HasCertificate() && m.opt.Listener != nil {
		if err := m.opt.Listener.StartDomainTLS(addr); err != nil {
			return m.recordError(ctx, "启动 HTTPS 监听", err)
		}
	}
	return nil
}

// ---- 账号关联 ----

// StartLink 向官网申请一枚关联码。已有进行中的关联（未过期、未终态）时幂等返回它——
// 同一个管理员开两次对话框、或界面在开发模式下重复挂载，都不该再烧一枚关联码。
func (m *Manager) StartLink(ctx context.Context) (*LinkSession, error) {
	site, err := m.site()
	if err != nil {
		return nil, err
	}
	st := m.LoadState(ctx)
	if st.Provider == ProviderNone {
		return nil, ErrProviderNotSet
	}
	m.mu.Lock()
	if m.link != nil && m.link.Status == "pending" && m.now().Before(m.link.ExpiresAt) {
		out := *m.link
		out.deviceCode = ""
		m.mu.Unlock()
		return &out, nil
	}
	m.mu.Unlock()

	name := m.opt.DeviceName
	if name == "" {
		name, _ = os.Hostname()
	}
	res, err := site.LinkStart(ctx, LinkStartRequest{Model: m.opt.Model, Name: name, FirmwareVersion: m.opt.Version})
	if err != nil {
		return nil, err
	}
	interval := time.Duration(res.Interval) * time.Second
	if interval < m.opt.PollInterval {
		interval = m.opt.PollInterval
	}
	expires := m.now().Add(time.Duration(res.ExpiresIn) * time.Second)
	if res.ExpiresIn <= 0 {
		expires = m.now().Add(15 * time.Minute)
	}
	sess := &LinkSession{
		UserCode:                res.UserCode,
		VerificationURL:         res.VerificationURL,
		VerificationURLComplete: res.VerificationURLComplete,
		ExpiresAt:               expires,
		Status:                  "pending",
		deviceCode:              res.DeviceCode,
		interval:                interval,
	}
	m.mu.Lock()
	m.link = sess
	m.mu.Unlock()
	m.log.Info("已向官网申请账号关联码", "user_code", res.UserCode)
	out := *sess
	out.deviceCode = ""
	return &out, nil
}

// WaitLink 轮询官网直到关联到达终态或 wait 耗尽，返回会话快照。approved 时令牌已密封
// 入库、账号名已记录。管理面用它做「同步陪等」（≤ 一个请求周期），零轮询约定不破。
func (m *Manager) WaitLink(ctx context.Context, wait time.Duration) (*LinkSession, error) {
	site, err := m.site()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	sess := m.link
	m.mu.Unlock()
	if sess == nil {
		return nil, ErrNoLinkSession
	}
	deadline := m.now().Add(wait)
	for {
		m.mu.Lock()
		cur := *sess
		m.mu.Unlock()
		if cur.Status != "pending" {
			cur.deviceCode = ""
			return &cur, nil
		}
		if m.now().After(cur.ExpiresAt) {
			m.finishLink("expired", "", "", "")
			cur.Status = "expired"
			cur.deviceCode = ""
			return &cur, nil
		}
		res, err := site.LinkPoll(ctx, cur.deviceCode)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 一次轮询失败不算终态：官网抖动时下一次陪等继续。
			m.log.Warn("轮询账号关联失败", "err", err.Error())
		} else {
			switch res.Status {
			case "approved":
				if err := m.storeLink(ctx, res); err != nil {
					m.finishLink("failed", "", "", err.Error())
				} else {
					m.finishLink("approved", res.LinkID, res.Account.DisplayName, "")
				}
				continue
			case "denied":
				m.finishLink("denied", "", "", "")
				continue
			case "expired", "consumed":
				m.finishLink("expired", "", "", "")
				continue
			}
		}
		if !m.now().Add(cur.interval).Before(deadline) {
			cur.deviceCode = ""
			return &cur, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cur.interval):
		}
	}
}

func (m *Manager) finishLink(status, linkID, account, errText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.link == nil {
		return
	}
	m.link.Status = status
	m.link.Error = errText
	if status == "approved" {
		m.link.Account = account
	}
	_ = linkID
}

func (m *Manager) storeLink(ctx context.Context, res *LinkPollResponse) error {
	if res.Token == "" {
		return errors.New("官网未返回设备令牌")
	}
	if err := m.opt.Settings.SetSealedSetting(ctx, settingLinkToken, res.Token); err != nil {
		return fmt.Errorf("保存设备令牌: %w", err)
	}
	_ = m.set(ctx, settingLinkID, res.LinkID)
	_ = m.set(ctx, settingLinkAccount, res.Account.DisplayName)
	m.clearError(ctx)
	// 顺手读一次官网读数，把后缀记下来（缺省 llm.net）。
	if status, err := m.opt.Site.LinkStatus(ctx, res.Token); err == nil && status.LanDomain.Suffix != "" {
		_ = m.set(ctx, settingSuffix, status.LanDomain.Suffix)
	}
	m.log.Info("已关联 LLM Gate官网账号")
	return nil
}

// CancelLink 丢弃进行中的关联会话（官网侧关联码到期自然失效）。
func (m *Manager) CancelLink() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.link = nil
}

// Unlink 解除关联：先向官网撤销令牌（官网同时释放域名与解析记录；失败只记日志——本地
// 撤销必须成功，官网侧残留由账号持有人在官网解除），再清空本地令牌、域名与证书。
func (m *Manager) Unlink(ctx context.Context) error {
	m.mu.Lock()
	if m.issuing {
		m.mu.Unlock()
		return ErrIssueBusy
	}
	m.mu.Unlock()
	tok, err := m.token(ctx)
	if err != nil && !errors.Is(err, ErrTokenUnreadable) {
		return err
	}
	if err == nil && m.opt.Site != nil {
		if uerr := m.opt.Site.Unlink(ctx, tok); uerr != nil {
			m.log.Warn("官网侧解除关联失败，本地照常清除", "err", uerr.Error())
		}
	}
	if err := m.opt.Settings.SetSealedSetting(ctx, settingLinkToken, ""); err != nil {
		return err
	}
	_ = m.set(ctx, settingLinkID, "")
	_ = m.set(ctx, settingLinkAccount, "")
	m.CancelLink()
	m.clearLocalDomain(ctx)
	m.log.Info("已解除 LLM Gate官网账号关联")
	return nil
}

// ---- 域名 ----

// ValidateLabel 是设备侧预检（官网为准）：5–32 位小写字母/数字/连字符，不以连字符开头结尾。
func ValidateLabel(label string) error {
	if len(label) < 5 || len(label) > 32 {
		return invalid("域名前缀长度须为 5–32 个字符")
	}
	for i := range len(label) {
		c := label[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
		if !ok {
			return invalid("域名前缀只能包含小写字母、数字与连字符")
		}
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return invalid("域名前缀不能以连字符开头或结尾")
	}
	if strings.Contains(label, "--") {
		return invalid("域名前缀不能包含连续的连字符")
	}
	return nil
}

// ValidateHostname 是自有域名的设备侧预检（官网为准）：小写、至少两级、每级 1–63 位字母数字
// 连字符且不以连字符开头结尾、整体 ≤ 253；不接受通配符、IP 形状与 llmgate.* 下的名字。
func ValidateHostname(hostname string) error {
	if hostname == "" {
		return invalid("请填写你自己的域名，例如 box.example.com")
	}
	if len(hostname) > 253 || len("_acme-challenge."+hostname) > 253 {
		return invalid("域名长度不合法")
	}
	if strings.Contains(hostname, "*") {
		return invalid("不支持通配符域名")
	}
	labels := strings.Split(hostname, ".")
	if len(labels) < 2 {
		return invalid("请填写完整域名（至少两级，例如 box.example.com）")
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid("域名只能包含小写字母、数字与连字符，各级不能以连字符开头或结尾")
		}
		for i := range len(label) {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return invalid("域名只能包含小写字母、数字与连字符，各级不能以连字符开头或结尾")
			}
		}
	}
	if net.ParseIP(hostname) != nil {
		return invalid("域名不能是 IP 地址")
	}
	tld := labels[len(labels)-1]
	if strings.Trim(tld, "0123456789") == "" {
		return invalid("域名不能是 IP 地址")
	}
	for _, zone := range []string{DefaultSuffix, "pages.dev", "workers.dev"} {
		if hostname == zone || strings.HasSuffix(hostname, "."+zone) {
			return invalid(zone + " 下的名字不能作为自有域名登记")
		}
	}
	return nil
}

// Claim 申领域名（或更新解析地址）。label 为空表示保持现有域名；targetIP 为空时自动挑
// 本机地址。申领成功后自动启动一次签发（已有覆盖该域名且未进续期窗口的证书除外）。
func (m *Manager) Claim(ctx context.Context, label, targetIP string) (State, error) {
	site, err := m.site()
	if err != nil {
		return State{}, err
	}
	st := m.LoadState(ctx)
	if err := requireProvider(st, ProviderOfficialSite); err != nil {
		return State{}, err
	}
	tok, err := m.token(ctx)
	if err != nil {
		return State{}, err
	}
	label = strings.ToLower(strings.TrimSpace(label))
	if label == "" {
		label = st.Label()
	}
	if label == "" {
		return State{}, invalid("请填写域名前缀")
	}
	if err := ValidateLabel(label); err != nil {
		return State{}, err
	}
	targetIP, err = normalizeTargetIP(targetIP, st.TargetIP)
	if err != nil {
		return State{}, err
	}
	dom, err := site.Claim(ctx, tok, label, targetIP)
	if err != nil {
		return State{}, m.recordError(ctx, "申领域名", err)
	}
	if err := m.set(ctx, settingHostname, dom.Hostname); err != nil {
		return State{}, err
	}
	_ = m.set(ctx, settingTargetIP, dom.TargetIP)
	_ = m.set(ctx, settingAcmeDelegate, "")
	if suffix := strings.TrimPrefix(dom.Hostname, dom.Label+"."); suffix != "" && suffix != dom.Hostname {
		_ = m.set(ctx, settingSuffix, suffix)
	}
	m.clearError(ctx)
	m.log.Info("内网域名申领成功", "hostname", dom.Hostname, "target_ip", dom.TargetIP)
	return m.LoadState(ctx), nil
}

// requireProvider 检查当前提供方式：未选答 ErrProviderNotSet，选了别的答 ErrWrongProvider。
func requireProvider(st State, want string) error {
	switch st.Provider {
	case want:
		return nil
	case ProviderNone:
		return ErrProviderNotSet
	}
	return ErrWrongProvider
}

// normalizeTargetIP 取管理员指定的本机 IPv4；空串时按 fallback → 字典序最小自动挑。
func normalizeTargetIP(targetIP, fallback string) (string, error) {
	targetIP = strings.TrimSpace(targetIP)
	if targetIP == "" {
		targetIP = pickLocalIPv4("", fallback)
	}
	if ip := net.ParseIP(targetIP); ip == nil || ip.To4() == nil {
		return "", invalid("解析地址须为本机的 IPv4 地址")
	}
	return targetIP, nil
}

// Register 登记自有域名（或更新期望解析地址）：官网只落库并分配 DNS-01 委托名，不写任何解析记录；
// 之后由管理员在自己的 DNS 服务商设好 A 与 _acme-challenge CNAME，再签发证书。hostname 为空表示
// 保持现有域名。
func (m *Manager) Register(ctx context.Context, hostname, targetIP string) (State, error) {
	site, err := m.site()
	if err != nil {
		return State{}, err
	}
	st := m.LoadState(ctx)
	if err := requireProvider(st, ProviderOwnDomain); err != nil {
		return State{}, err
	}
	tok, err := m.token(ctx)
	if err != nil {
		return State{}, err
	}
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "" {
		hostname = st.Hostname
	}
	if err := ValidateHostname(hostname); err != nil {
		return State{}, err
	}
	targetIP, err = normalizeTargetIP(targetIP, st.TargetIP)
	if err != nil {
		return State{}, err
	}
	dom, err := site.RegisterCustom(ctx, tok, hostname, targetIP)
	if err != nil {
		return State{}, m.recordError(ctx, "登记域名", err)
	}
	if dom.AcmeDelegate == "" {
		return State{}, m.recordError(ctx, "登记域名", errors.New("官网未返回证书验证的委托名"))
	}
	if err := m.set(ctx, settingHostname, dom.Hostname); err != nil {
		return State{}, err
	}
	_ = m.set(ctx, settingTargetIP, dom.TargetIP)
	_ = m.set(ctx, settingAcmeDelegate, dom.AcmeDelegate)
	m.clearError(ctx)
	m.log.Info("自有域名已登记", "hostname", dom.Hostname, "target_ip", dom.TargetIP, "acme_delegate", dom.AcmeDelegate)
	return m.LoadState(ctx), nil
}

// DNSCheck 请官网用公共解析器核对自有域名的三条记录（A / 委托 CNAME / CAA）。只读，不改状态。
func (m *Manager) DNSCheck(ctx context.Context) (*DNSCheck, error) {
	site, err := m.site()
	if err != nil {
		return nil, err
	}
	st := m.LoadState(ctx)
	if err := requireProvider(st, ProviderOwnDomain); err != nil {
		return nil, err
	}
	if !st.Claimed() {
		return nil, ErrNotClaimed
	}
	tok, err := m.token(ctx)
	if err != nil {
		return nil, err
	}
	return site.DNSCheck(ctx, tok, st.TargetIP)
}

// SetTarget 只更新解析地址。
func (m *Manager) SetTarget(ctx context.Context, targetIP string) (State, error) {
	site, err := m.site()
	if err != nil {
		return State{}, err
	}
	st := m.LoadState(ctx)
	if !st.Claimed() {
		return State{}, ErrNotClaimed
	}
	tok, err := m.token(ctx)
	if err != nil {
		return State{}, err
	}
	targetIP, err = normalizeTargetIP(targetIP, "")
	if err != nil {
		return State{}, err
	}
	dom, err := site.SetTarget(ctx, tok, targetIP)
	if err != nil {
		return State{}, m.recordError(ctx, "更新解析地址", err)
	}
	_ = m.set(ctx, settingTargetIP, dom.TargetIP)
	m.clearError(ctx)
	return m.LoadState(ctx), nil
}

// Release 释放域名：官网删除记录后，本地清空域名并删除证书与私钥、停止 HTTPS 监听。
func (m *Manager) Release(ctx context.Context) error {
	m.mu.Lock()
	if m.issuing {
		m.mu.Unlock()
		return ErrIssueBusy
	}
	m.mu.Unlock()
	site, err := m.site()
	if err != nil {
		return err
	}
	st := m.LoadState(ctx)
	if !st.Claimed() {
		return ErrNotClaimed
	}
	tok, err := m.token(ctx)
	if err != nil {
		return err
	}
	if err := site.Release(ctx, tok); err != nil {
		return m.recordError(ctx, "释放域名", err)
	}
	m.clearLocalDomain(ctx)
	m.log.Info("内网域名已释放", "hostname", st.Hostname)
	return nil
}

func (m *Manager) clearLocalDomain(ctx context.Context) {
	_ = m.set(ctx, settingHostname, "")
	_ = m.set(ctx, settingTargetIP, "")
	_ = m.set(ctx, settingAcmeDelegate, "")
	_ = m.set(ctx, settingLastIssuedAt, "")
	_ = m.set(ctx, settingLastCA, "")
	m.clearError(ctx)
	for _, name := range []string{certChainFile, certKeyFile, certPendingKeyFile} {
		if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("删除域名证书文件失败", "path", m.path(name), "err", err.Error())
		}
	}
	m.cert.Store(nil)
	if m.opt.Listener != nil {
		if err := m.opt.Listener.StopDomainTLS(); err != nil {
			m.log.Warn("停止域名 HTTPS 监听失败", "err", err.Error())
		}
	}
}

// ---- 签发 ----

// StartIssue 启动一次异步签发/续期，返回一次性结果通道（缓冲 1，不取也不泄漏）。签发
// 用自己的超时而不是请求上下文：管理员关掉页面不该把签发半途掐死。
func (m *Manager) StartIssue() (<-chan error, error) {
	if _, err := m.site(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.issuing {
		m.mu.Unlock()
		return nil, ErrIssueBusy
	}
	m.issuing = true
	m.issueStage = StageSubmitting
	m.issueErr = nil
	m.notifyIssueLocked()
	m.mu.Unlock()

	ch := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), issueTimeout)
		defer cancel()
		err := m.issue(ctx)
		m.mu.Lock()
		m.issuing = false
		m.issueStage = ""
		m.issueErr = err
		m.notifyIssueLocked()
		m.mu.Unlock()
		ch <- err
	}()
	return ch, nil
}

// notifyIssueLocked 唤醒所有 WaitIssue（关闭旧通道、换一条新的）。调用方持 mu。
func (m *Manager) notifyIssueLocked() {
	if m.issueNotify != nil {
		close(m.issueNotify)
	}
	m.issueNotify = make(chan struct{})
}

// setStage 记录签发阶段；变化时唤醒陪等者。
func (m *Manager) setStage(stage string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.issuing || m.issueStage == stage {
		return
	}
	m.issueStage = stage
	m.notifyIssueLocked()
}

// WaitIssue 陪等进行中的签发：签发结束（finished=true，err 是它的结果）、阶段不再是 since、
// timeout 耗尽或 ctx 取消时返回。没有签发在进行时立即返回 finished=true 与最近一次的结果。
// 管理面用它把「一次陪等」切成「阶段一变就回一帧」，界面才有东西可以显示。
func (m *Manager) WaitIssue(ctx context.Context, timeout time.Duration, since string) (finished bool, err error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		issuing, stage, notify, last := m.issuing, m.issueStage, m.issueNotify, m.issueErr
		m.mu.Unlock()
		if !issuing {
			return true, last
		}
		if stage != since {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline.C:
			return false, nil
		case <-notify:
		}
	}
}

// Issuing 报告是否有签发在进行中。
func (m *Manager) Issuing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issuing
}

func (m *Manager) issue(ctx context.Context) error {
	st := m.LoadState(ctx)
	if !st.Claimed() {
		return ErrNotClaimed
	}
	tok, err := m.token(ctx)
	if err != nil {
		return err
	}
	key, keyPEM, err := m.pendingKey()
	if err != nil {
		return m.recordError(ctx, "生成证书私钥", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: st.Hostname},
		DNSNames: []string{st.Hostname},
	}, key)
	if err != nil {
		return m.recordError(ctx, "生成证书请求", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	order, err := m.opt.Site.OrderCertificate(ctx, tok, csrPEM)
	if err != nil {
		return m.recordError(ctx, "提交证书订单", err)
	}
	if order.Hostname != "" && order.Hostname != st.Hostname {
		return m.recordError(ctx, "提交证书订单", fmt.Errorf("官网订单域名 %s 与本机 %s 不一致", order.Hostname, st.Hostname))
	}
	m.setStage(orderStage(order))
	for !order.Terminal() {
		wait := m.opt.PollInterval
		if ra := time.Duration(order.RetryAfter) * time.Second; ra > wait {
			wait = ra
		}
		select {
		case <-ctx.Done():
			return m.recordError(ctx, "等待证书签发", errors.New("签发超时，请稍后重试"))
		case <-time.After(wait):
		}
		next, err := m.opt.Site.CertificateStatus(ctx, tok)
		if err != nil {
			if ctx.Err() != nil {
				return m.recordError(ctx, "等待证书签发", errors.New("签发超时，请稍后重试"))
			}
			m.log.Warn("轮询证书订单失败，稍后重试", "err", err.Error())
			continue
		}
		order = next
		m.setStage(orderStage(order))
	}
	if order.Status == "failed" {
		msg := order.Error
		if msg == "" {
			msg = "官网未给出失败原因"
		}
		return m.recordError(ctx, "证书签发", errors.New(msg))
	}
	m.setStage(StageInstalling)
	cert, err := tls.X509KeyPair([]byte(order.CertificatePEM), keyPEM)
	if err != nil {
		return m.recordError(ctx, "校验证书", errors.New("官网返回的证书与本机私钥不匹配"))
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || leaf.VerifyHostname(st.Hostname) != nil {
		return m.recordError(ctx, "校验证书", errors.New("官网返回的证书未覆盖本机域名"))
	}
	cert.Leaf = leaf
	if err := atomicWrite(m.path(certKeyFile), keyPEM, 0o600); err != nil {
		return m.recordError(ctx, "保存私钥", err)
	}
	if err := atomicWrite(m.path(certChainFile), []byte(order.CertificatePEM), 0o600); err != nil {
		return m.recordError(ctx, "保存证书", err)
	}
	_ = os.Remove(m.path(certPendingKeyFile))
	m.cert.Store(&cert)
	_ = m.set(ctx, settingLastIssuedAt, fmtTime(m.now()))
	_ = m.set(ctx, settingLastCA, order.CA)
	m.clearError(ctx)
	m.log.Info("内网域名证书已签发", "hostname", st.Hostname, "ca", order.CA, "not_after", leaf.NotAfter.UTC().Format(time.RFC3339))
	m.startListener(ctx)
	return nil
}

// orderStage 把官网订单状态映射成签发阶段；challenging 且挑战已交 CA 记为 validating。
func orderStage(o *OrderInfo) string {
	switch o.Status {
	case "pending":
		return StagePending
	case "authorizing":
		return StageAuthorizing
	case "challenging":
		if o.ChallengeTriggered {
			return StageValidating
		}
		return StageChallenging
	case "finalizing":
		return StageFinalizing
	}
	return StageInstalling
}

// pendingKey 取正在签发的私钥：有 pending 文件就复用（官网可能还留着上一次的在飞订单，
// CSR 必须配同一把钥匙），没有就新生成并先落盘。
func (m *Manager) pendingKey() (*ecdsa.PrivateKey, []byte, error) {
	if raw, err := os.ReadFile(m.path(certPendingKeyFile)); err == nil {
		if key, err := parseECKey(raw); err == nil {
			return key, raw, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := atomicWrite(m.path(certPendingKeyFile), raw, 0o600); err != nil {
		return nil, nil, err
	}
	return key, raw, nil
}

func parseECKey(raw []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("不是 PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

func (m *Manager) startListener(ctx context.Context) {
	if m.opt.Listener == nil || !m.HasCertificate() {
		return
	}
	addr := m.get(ctx, settingHTTPSListen)
	if addr == "" {
		addr = DefaultHTTPSListen
	}
	if err := m.opt.Listener.StartDomainTLS(addr); err != nil {
		_ = m.recordError(ctx, "启动 HTTPS 监听", err)
	}
}

// needsIssue：已申领且（没有证书 / 证书进入续期窗口 / 证书对不上现域名）。
func (m *Manager) needsIssue(ctx context.Context) bool {
	st := m.LoadState(ctx)
	if !st.Claimed() || !st.Linked {
		return false
	}
	cs := m.certStatus(st.Hostname)
	return cs == nil || cs.ExpiringSoon || !cs.CoversHostname
}

// SyncIP 用当前最合适的本机地址重申领（官网 upsert A 记录），修复托管域名的 IP 漂移。
// 自有域名的 A 记录在管理员自己手里，设备改不了，不同步。
func (m *Manager) SyncIP(ctx context.Context) {
	st := m.LoadState(ctx)
	if st.Provider != ProviderOfficialSite || !st.Claimed() || !st.Linked {
		return
	}
	ip := pickLocalIPv4(st.TargetIP, "")
	if ip == "" || ip == st.TargetIP {
		return
	}
	if _, err := m.SetTarget(ctx, ip); err != nil {
		m.log.Warn("同步域名解析地址失败（下轮维护重试）", "hostname", st.Hostname, "err", err.Error())
	}
}

// Run 是维护协程：启动时恢复 HTTPS 监听；延迟后与此后每 12 小时同步解析地址并在需要时
// 续期。ctx 取消即返，fire-and-forget。
func (m *Manager) Run(ctx context.Context) {
	m.startListener(ctx)
	t := time.NewTimer(maintenanceStartDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	tick := func() {
		syncCtx, cancel := context.WithTimeout(ctx, time.Minute)
		m.SyncIP(syncCtx)
		cancel()
		if !m.needsIssue(ctx) {
			return
		}
		ch, err := m.StartIssue()
		if err != nil {
			return
		}
		select {
		case err := <-ch:
			if err != nil {
				m.log.Warn("证书自动续期失败（12 小时后重试）", "err", err.Error())
			}
		case <-ctx.Done():
		}
	}
	tick()
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// ---- TLS 供给 ----

// GetCertificate 实现 tls.Config.GetCertificate：恒返回当前证书，不看 SNI——单域名设备
// 没有第二张脸。
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := m.cert.Load()
	if cert == nil {
		return nil, errors.New("设备尚未签发 HTTPS 证书")
	}
	return cert, nil
}

// HasCertificate 报告是否已有可服务的证书。
func (m *Manager) HasCertificate() bool { return m.cert.Load() != nil }

func (m *Manager) loadCertificate() {
	certPath, keyPath := m.path(certChainFile), m.path(certKeyFile)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		return
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		m.log.Warn("加载既有域名证书失败，按未签发处理（可重新签发覆盖）", "err", err.Error())
		return
	}
	if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
		cert.Leaf = leaf
	}
	m.cert.Store(&cert)
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ---- 本机地址 ----

// LocalIPv4s 枚举本机 up 且非回环网卡上的全局单播 IPv4（口径与管理面接入读数一致），
// 字典序稳定。
func LocalIPv4s() []string {
	set := localIPv4Set()
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// pickLocalIPv4 依次尝试 preferred、fallback、字典序最小的本机地址。
func pickLocalIPv4(preferred, fallback string) string {
	locals := localIPv4Set()
	if len(locals) == 0 {
		return ""
	}
	for _, cand := range []string{preferred, fallback} {
		if cand != "" && locals[cand] {
			return cand
		}
	}
	best := ""
	for ip := range locals {
		if best == "" || ip < best {
			best = ip
		}
	}
	return best
}

func localIPv4Set() map[string]bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make(map[string]bool)
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil && ip4.IsGlobalUnicast() {
				out[ip4.String()] = true
			}
		}
	}
	return out
}
