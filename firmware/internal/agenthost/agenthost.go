// Package agenthost 管理「智能体 → 主机/SoC」纳管的主机：设备自己的访问证书
// （一把 ed25519 SSH 密钥对）、把公钥装到主机上、轮换证书并下发到全部主机，
// 以及日常的连通性与免密检查。
//
// 边界写明白：本包只负责**让智能体能免密登录这些主机**——装公钥、配免密 sudo、
// 记下连接事实。它不在主机上安装任何东西、不下发脚本、不常驻连接，也不代表
// 智能体去执行任务；每次操作都是管理员在界面上按一下，做完即断开。
//
// 纪律（§15.1）：
//   - 主机口令只在那一次请求的内存里，经 SSH 会话的 stdin 交给对端，**不入库、
//     不进 argv、不进日志、不进审计 detail、不回响应**；设备也不执行 ssh /
//     ssh-copy-id 等外部命令（那是把口令送进 argv 的最短路径）。
//   - 证书私钥用设备密钥封存在 settings，永不出本包；公钥是公开值，可下载。
//   - 主机公钥首次纳管即钉死，之后逐字比对，不一致即拒绝连接；没有「忽略主机
//     公钥」的开关（SSH 面上与「禁止关闭 TLS 校验」同一条纪律）。
package agenthost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 错误码。管理面按它映射 HTTP 状态与界面提示（见 internal/admin/agenthosts.go）。
const (
	CodeInvalidHost           = "invalid_host"
	CodeHostExists            = "host_exists"
	CodeCertificateMissing    = "certificate_missing"
	CodeCertificateExists     = "certificate_exists"
	CodeCertificateUnreadable = "certificate_unreadable"
	CodeUnreachable           = "host_unreachable"
	CodeAuthFailed            = "host_auth_failed"
	CodeHostKeyChanged        = "host_key_changed"
	CodeCommandFailed         = "host_command_failed"
	CodeSudoFailed            = "sudo_failed"
)

// Error 是本包对外的错误形态：Code 给管理面映射状态码，Msg 是可直接展示给
// 管理员的中文原因（不含口令、私钥或密文）。
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Options 是 New 的入参。
type Options struct {
	// Store 是唯一持久层：主机行 + 证书所在的 settings。
	Store *store.Store
	// Logger 为 nil 时不记日志（测试）。
	Logger *slog.Logger
	// Dial 为 nil 时用标准库拨号；测试注入进程内监听地址。
	Dial DialFunc
	// Now 为 nil 时取 time.Now（测试注入固定时刻）。
	Now func() time.Time
}

// Manager 是本包的入口。方法并发安全：状态全在 store 里，本结构只持无状态依赖。
type Manager struct {
	st     *store.Store
	log    *slog.Logger
	dialFn DialFunc
	now    func() time.Time
}

// New 装配 Manager。
func New(o Options) *Manager {
	m := &Manager{st: o.Store, log: o.Logger, dialFn: o.Dial, now: o.Now}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m
}

func (m *Manager) dialer() DialFunc {
	if m.dialFn != nil {
		return m.dialFn
	}
	d := &net.Dialer{}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return d.DialContext(ctx, network, address)
	}
}

// hostTimeout 是「一台主机上的一整套动作」的上限（拨号 + 认证 + 三四条命令）。
const hostTimeout = 90 * time.Second

// rotateParallel 是轮换证书时同时下发的主机数：内网里这几条连接互不相干，
// 但也不必为十几台主机同时开一堆会话。
const rotateParallel = 4

// ---- 证书 ----

// Certificate 读当前访问证书的公开面；没生成过返回 nil。
func (m *Manager) CurrentCertificate(ctx context.Context) (*Certificate, error) {
	k, err := m.loadCert(ctx)
	if err != nil || k == nil {
		return nil, err
	}
	return k.describe(), nil
}

// GenerateCertificate 首次生成访问证书。已有证书时答 CodeCertificateExists——
// 换新证书是「轮换证书」那条路（要把新公钥下发到已纳管的主机），不能在这里
// 悄悄把旧的盖掉：那会让全部主机立刻失联。
func (m *Manager) GenerateCertificate(ctx context.Context) (*Certificate, error) {
	cur, err := m.loadCert(ctx)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		return nil, &Error{Code: CodeCertificateExists, Msg: "设备上已有访问证书；要换新的请用「轮换证书」，它会把新公钥下发到已纳管的主机。"}
	}
	next, err := newCertKey(m.now())
	if err != nil {
		return nil, err
	}
	if err := m.saveCert(ctx, next, nil); err != nil {
		return nil, err
	}
	m.log.Info("已生成智能体访问证书", "fingerprint", next.fingerprint())
	return next.describe(), nil
}

// HostResult 是轮换证书时单台主机的下发结果。
type HostResult struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	OK      bool   `json:"ok"`
	// Error 是失败原因（成功时为空），可直接展示。
	Error string `json:"error,omitempty" i18n:"text"`
}

// RotateCertificate 生成新证书并下发到全部已纳管主机：新证书立刻成为当前证书，
// 旧证书退到「上一把」槽位——新公钥还没装上去之前，只能用旧的登录进去换。
//
// 某台主机当时不在线只让它那一条失败：它的行仍记着旧指纹，界面显示「证书待更新」，
// 管理员之后单独「补发证书」即可（仍用上一把连上，不必再要口令）。
func (m *Manager) RotateCertificate(ctx context.Context) (*Certificate, []HostResult, error) {
	cur, err := m.loadCert(ctx)
	if err != nil {
		return nil, nil, err
	}
	if cur == nil {
		return nil, nil, errNoCertificate()
	}
	next, err := newCertKey(m.now())
	if err != nil {
		return nil, nil, err
	}
	if err := m.saveCert(ctx, next, cur); err != nil {
		return nil, nil, err
	}
	m.log.Info("已更新智能体访问证书", "fingerprint", next.fingerprint())

	hosts, err := m.st.ListAgentHosts(ctx)
	if err != nil {
		return next.describe(), nil, err
	}
	results := make([]HostResult, len(hosts))
	sem := make(chan struct{}, rotateParallel)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h store.AgentHost) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = HostResult{ID: h.ID, Name: h.Name, Address: h.Address}
			if _, err := m.pushTo(ctx, h, next, cur); err != nil {
				results[i].Error = err.Error()
				return
			}
			results[i].OK = true
		}(i, h)
	}
	wg.Wait()
	return next.describe(), results, nil
}

func errNoCertificate() error {
	return &Error{Code: CodeCertificateMissing, Msg: "设备还没有访问证书：请先生成证书，再添加主机。"}
}

// ---- 主机 ----

// List 列出全部纳管主机。
func (m *Manager) List(ctx context.Context) ([]store.AgentHost, error) {
	return m.st.ListAgentHosts(ctx)
}

// Get 取一行主机（管理面在删除前要拿它的连接三元组写审计）。
func (m *Manager) Get(ctx context.Context, id int64) (*store.AgentHost, error) {
	return m.st.GetAgentHost(ctx, id)
}

// FingerprintOf 把 authorized_keys 单行折成 SHA256 指纹（解析不了或空串回空串）。
// 管理面用它把钉死的主机公钥变成管理员能与 ssh-keygen -lf 核对的那一串。
func FingerprintOf(line string) string {
	if strings.TrimSpace(line) == "" {
		return ""
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// Rename 只改展示名称。
func (m *Manager) Rename(ctx context.Context, id int64, name string) (*store.AgentHost, error) {
	name, err := normalizeName(name)
	if err != nil {
		return nil, err
	}
	return m.st.RenameAgentHost(ctx, id, name)
}

// EnrollRequest 是「添加主机」与「重新纳管」的入参。
type EnrollRequest struct {
	// ID 非零 = 对已有行重新纳管（连接三元组与类型取行里的值，不从这里改）。
	ID   int64
	Name string
	// Kind 是新主机的类型（store.AgentHostKindManaged | store.AgentHostKindWorker），
	// 添加时必填、之后不改；重新纳管忽略。
	Kind     string
	Address  string
	Port     int
	Username string
	// Password 是主机上这个用户的登录口令，只用于这一次装公钥与配免密 sudo，
	// 用完即弃（不入库、不进日志、不回响应）。
	Password string
	// ConfigureSudo 为真时在主机上写 /etc/sudoers.d/ 条目，让该用户免密 sudo；
	// root 用户本就不需要，恒跳过。
	ConfigureSudo bool
	// AcceptNewHostKey 为真时接受与已钉死的主机公钥不同的那一把（主机重装过）。
	AcceptNewHostKey bool
}

// Enroll 用用户名口令登录主机，把当前访问证书的公钥装进 authorized_keys（可选
// 同时配好免密 sudo），再用证书重连一次验收。成功之后这台主机就是免密可达的，
// 口令不再需要。
func (m *Manager) Enroll(ctx context.Context, req EnrollRequest) (*store.AgentHost, error) {
	cert, prev, err := m.certPair(ctx)
	if err != nil {
		return nil, err
	}
	host, err := m.enrollRow(ctx, req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()

	state := store.AgentHostState{ID: host.ID, HostKey: host.HostKey, KeyFingerprint: host.KeyFingerprint,
		Status: store.AgentHostStatusError, CheckedAt: m.now()}
	c, err := m.dial(ctx, target{
		Address: host.Address, Port: host.Port, Username: host.Username,
		HostKey: host.HostKey, AcceptNewHostKey: req.AcceptNewHostKey,
	}, []ssh.AuthMethod{ssh.Password(req.Password)})
	if err != nil {
		return m.fail(ctx, state, err)
	}
	defer c.Close()
	state.HostKey = c.hostKey

	prevLine := ""
	if prev != nil {
		prevLine = prev.public
	}
	if err := installKey(ctx, c, cert.public, prevLine); err != nil {
		return m.fail(ctx, state, err)
	}
	state.KeyFingerprint = cert.fingerprint()
	if req.ConfigureSudo && host.Username != rootUser {
		if err := configureSudo(ctx, c, host.Username, req.Password); err != nil {
			return m.fail(ctx, state, err)
		}
	}
	return m.verify(ctx, *host, state, cert, nil, nil)
}

// Push 用已装好的证书（必要时退回上一把）重新下发当前证书，不需要口令。
// 「证书待更新」的主机、以及轮换证书时错过的主机走这条路补上。
func (m *Manager) Push(ctx context.Context, id int64) (*store.AgentHost, error) {
	cert, prev, err := m.certPair(ctx)
	if err != nil {
		return nil, err
	}
	host, err := m.st.GetAgentHost(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.pushTo(ctx, *host, cert, prev)
}

func (m *Manager) pushTo(ctx context.Context, host store.AgentHost, cert, prev *certKey) (*store.AgentHost, error) {
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	state := store.AgentHostState{ID: host.ID, HostKey: host.HostKey, KeyFingerprint: host.KeyFingerprint,
		Status: store.AgentHostStatusError, CheckedAt: m.now()}
	c, err := m.connectWithCert(ctx, host, cert, prev)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	defer c.Close()
	state.HostKey = c.hostKey
	prevLine := ""
	if prev != nil {
		prevLine = prev.public
	}
	if err := installKey(ctx, c, cert.public, prevLine); err != nil {
		return m.fail(ctx, state, err)
	}
	state.KeyFingerprint = cert.fingerprint()
	return m.verify(ctx, host, state, cert, nil, nil)
}

// Check 用当前证书连一次，核对公钥是否真在 authorized_keys 里、免密 sudo 是否
// 还有效，并刷新系统自述。它不改主机上的任何东西。
func (m *Manager) Check(ctx context.Context, id int64) (*store.AgentHost, error) {
	cert, prev, err := m.certPair(ctx)
	if err != nil {
		return nil, err
	}
	host, err := m.st.GetAgentHost(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	state := store.AgentHostState{ID: host.ID, HostKey: host.HostKey, KeyFingerprint: host.KeyFingerprint,
		Status: store.AgentHostStatusError, CheckedAt: m.now()}
	c, err := m.connectWithCert(ctx, *host, cert, prev)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	defer c.Close()
	state.HostKey = c.hostKey
	// 这条连接可能是用上一把证书登上去的（这台主机错过了那次更新）：查出来
	// 当前证书不在位时要说清补救动作，而不是报「写不进 authorized_keys」。
	return m.verify(ctx, *host, state, cert, c, &Error{
		Code: CodeCommandFailed,
		Msg:  "主机上没有当前访问证书的公钥（多半是错过了一次证书轮换），请用「补发证书」下发。",
	})
}

// Remove 删除一台主机。removeKey 为真时先尽力把设备公钥（当前与上一把）从主机的
// authorized_keys 里摘掉、并删掉本包写过的 sudoers 条目；主机连不上不拦删除——
// 设备侧的行必须删得掉，够不着的主机由管理员自己清理。
func (m *Manager) Remove(ctx context.Context, id int64, removeKey bool) (cleaned bool, err error) {
	host, err := m.st.GetAgentHost(ctx, id)
	if err != nil {
		return false, err
	}
	if removeKey {
		cleaned = m.cleanup(ctx, *host) == nil
	}
	if err := m.st.DeleteAgentHost(ctx, id); err != nil {
		return cleaned, err
	}
	return cleaned, nil
}

func (m *Manager) cleanup(ctx context.Context, host store.AgentHost) error {
	cert, prev, err := m.certPair(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	c, err := m.connectWithCert(ctx, host, cert, prev)
	if err != nil {
		return err
	}
	defer c.Close()
	keys := cert.public
	if prev != nil {
		keys += "\n" + prev.public
	}
	if _, _, code, err := c.run(ctx, removeKeyScript, keys+"\n"); err != nil {
		return err
	} else if code != 0 {
		return &Error{Code: CodeCommandFailed, Msg: "从主机上移除公钥失败"}
	}
	if host.Username != rootUser {
		// 免密 sudo 还在才删得掉；删不掉不算失败（公钥已经摘了，没有免密 sudo
		// 的那台主机本来就要人工清理）。
		c.run(ctx, sudoersRemoveCommand(host.Username), "")
	}
	return nil
}

// certPair 取当前证书与上一把证书；没有当前证书即错。
func (m *Manager) certPair(ctx context.Context) (cert, prev *certKey, err error) {
	cert, err = m.loadCert(ctx)
	if err != nil {
		return nil, nil, err
	}
	if cert == nil {
		return nil, nil, errNoCertificate()
	}
	prev, err = m.loadPrevCert(ctx)
	if err != nil {
		return nil, nil, err
	}
	return cert, prev, nil
}

// connectWithCert 用当前证书登录，失败时退回上一把（轮换证书时错过下发的主机
// 还认着旧公钥）。
func (m *Manager) connectWithCert(ctx context.Context, host store.AgentHost, cert, prev *certKey) (*conn, error) {
	signers := []ssh.Signer{cert.signer}
	if prev != nil {
		signers = append(signers, prev.signer)
	}
	return m.dial(ctx, target{
		Address: host.Address, Port: host.Port, Username: host.Username, HostKey: host.HostKey,
	}, []ssh.AuthMethod{ssh.PublicKeys(signers...)})
}

// verify 用证书重连一次（c 非 nil 时复用那条连接）确认免密登录真的可用，
// 顺手读回系统自述与免密 sudo 状态，然后整组写回。
func (m *Manager) verify(ctx context.Context, host store.AgentHost, state store.AgentHostState, cert *certKey, c *conn, missing *Error) (*store.AgentHost, error) {
	if c == nil {
		fresh, err := m.connectWithCert(ctx, store.AgentHost{
			Address: host.Address, Port: host.Port, Username: host.Username, HostKey: state.HostKey,
		}, cert, nil)
		if err != nil {
			return m.fail(ctx, state, err)
		}
		defer fresh.Close()
		c = fresh
	}
	out, _, code, err := c.run(ctx, probeScript, cert.public+"\n")
	if err != nil {
		return m.fail(ctx, state, err)
	}
	if code != 0 {
		return m.fail(ctx, state, &Error{Code: CodeCommandFailed, Msg: "主机上的自述命令未能执行"})
	}
	installed, system := parseProbe(out)
	if !installed {
		if missing == nil {
			missing = &Error{Code: CodeCommandFailed, Msg: "公钥没能写进主机的 authorized_keys，请检查该用户的家目录权限。"}
		}
		return m.fail(ctx, state, missing)
	}
	state.System = system
	state.SudoNoPasswd = host.Username == rootUser || sudoReady(ctx, c)
	state.KeyFingerprint = cert.fingerprint()
	state.Status = store.AgentHostStatusReady
	state.LastError = ""
	return m.st.SetAgentHostState(ctx, state)
}

// fail 把失败原因写进行里再把错误交回调用方：界面下一次刷新看得到同一句话。
func (m *Manager) fail(ctx context.Context, state store.AgentHostState, cause error) (*store.AgentHost, error) {
	state.Status = store.AgentHostStatusError
	state.LastError = cause.Error()
	state.SudoNoPasswd = false
	// 写回失败不掩盖真正的原因：仍然返回 cause。
	if _, err := m.st.SetAgentHostState(context.WithoutCancel(ctx), state); err != nil {
		m.log.Error("写回主机状态失败", "host_id", state.ID, "error", err.Error())
	}
	return nil, cause
}

// enrollRow 取（或建）纳管目标的行。
func (m *Manager) enrollRow(ctx context.Context, req EnrollRequest) (*store.AgentHost, error) {
	if req.ID != 0 {
		host, err := m.st.GetAgentHost(ctx, req.ID)
		if err != nil {
			return nil, err
		}
		return host, nil
	}
	address, err := normalizeAddress(req.Address)
	if err != nil {
		return nil, err
	}
	username, err := normalizeUsername(req.Username)
	if err != nil {
		return nil, err
	}
	name, err := normalizeName(req.Name)
	if err != nil {
		return nil, err
	}
	port := req.Port
	if port == 0 {
		port = DefaultPort
	}
	if port < 1 || port > 65535 {
		return nil, &Error{Code: CodeInvalidHost, Msg: "SSH 端口须在 1–65535 之间"}
	}
	if !store.ValidAgentHostKind(req.Kind) {
		return nil, &Error{Code: CodeInvalidHost, Msg: "请选择主机类型：受控纳管或工作节点"}
	}
	if name == "" {
		name = address
	}
	host, err := m.st.CreateAgentHost(ctx, store.NewAgentHost{
		Name: name, Kind: req.Kind, Address: address, Port: port, Username: username, CreatedAt: m.now(),
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, &Error{Code: CodeHostExists, Msg: fmt.Sprintf("%s@%s:%d 已经在列表里了；要换口令重装公钥，请用那一行的「重新纳管」。", username, address, port)}
	}
	if err != nil {
		return nil, err
	}
	return host, nil
}

// DefaultPort 是缺省 SSH 端口。
const DefaultPort = 22

// rootUser 不需要配免密 sudo：它本来就是 root。
const rootUser = "root"

// 主机名/地址：点分与连字符的常见形状，或 IPv4 / IPv6 字面量。不收空白与控制
// 字符——这个值要进 net.JoinHostPort 与错误提示。
var addressRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._:-]{0,253}[A-Za-z0-9])?$`)

// POSIX 可移植用户名。它要拼进 sudoers 条目，收窄到这张表才谈得上安全。
var usernameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func normalizeAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return s, nil
	}
	if !addressRE.MatchString(s) {
		return "", &Error{Code: CodeInvalidHost, Msg: "主机地址须是 IP 或主机名（字母、数字、点、连字符）"}
	}
	return s, nil
}

func normalizeUsername(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !usernameRE.MatchString(s) {
		return "", &Error{Code: CodeInvalidHost, Msg: "用户名须是 1–32 位小写字母、数字、下划线或连字符，且不以数字开头"}
	}
	return s, nil
}

func normalizeName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 32 {
		return "", &Error{Code: CodeInvalidHost, Msg: "名称最多 32 个字符"}
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", &Error{Code: CodeInvalidHost, Msg: "名称不能包含控制字符"}
		}
	}
	return s, nil
}
