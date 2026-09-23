// Package devhost 给「智能体 → 主机/SoC」里的每台主机装守护进程——工作节点装 devd
// （llmgate-devd），模型服务节点装 modeld（llmgate-modeld，daemon.go 的 daemonSpec 收着两者的
// 差异）——并让设备能连上它：经这台主机已有的 SSH 免密连接安装 / 卸载守护进程，以及对守护进程
// 的透传（devd：文件 / Git / 终端；modeld：算力服务器 / 任务队列 / 模型缓存 / 引擎会话）。
//
// 边界：主机行、SSH 连接与访问证书都归 internal/agenthost——本包不拨 SSH，只从
// agenthost.Manager.Open 借一条凭证书登好的连接：安装脚本在上面跑，透传的每条请求
// 在上面开一条转发通道连到守护进程的本机 Unix socket。守护进程没有自己的端口、证书或
// 身份：鉴权与对端身份全由那条 SSH 连接承担（访问证书 + 钉死的主机公钥），主机上除了
// 访问证书的公钥没有第二套凭据。守护进程的观测值写回同一行的 devd_* 列
// （store.AgentHost.Devd）；文件、Git、终端的实际工作全在主机上的 internal/devd 里做。
// 设备不常驻连接主机——每次界面动作一条 SSH 连接，用完即断，终端会话开着才有长连接。
//
// 纪律（§15.1）：安装 / 卸载可能要 sudo 口令（该用户没配免密 sudo 时）：口令只在那
// 一次请求的内存里，经 sudo -S 的 stdin 交给对端，不入库、不进 argv、不进日志、不进
// 审计 detail、不回响应。
package devhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 错误码。管理面按它映射 HTTP 状态（见 internal/admin/devconsole.go）。与 agenthost
// 同名的那几个含义相同：SSH 那一段的失败原样带着 agenthost 的码过来。
const (
	CodeInvalidHost     = "invalid_host"
	CodeUnreachable     = "host_unreachable"
	CodeAuthFailed      = "host_auth_failed"
	CodeHostKeyChanged  = "host_key_changed"
	CodeCommandFailed   = "host_command_failed"
	CodeSudoFailed      = "sudo_failed"
	CodeUnsupportedArch = "unsupported_arch"
	CodeAssetMissing    = "daemon_asset_missing"
	CodeForwardDenied   = agenthost.CodeForwardDenied
	CodeNotInstalled    = "devd_not_installed"
	CodeKindNotAllowed  = "host_kind_not_allowed"
	CodeUnavailable     = "dev_hosts_unavailable"
)

// Error 是本包对外的错误形态。Status 非零时是守护进程透传回来的 HTTP 状态。
type Error struct {
	Code   string
	Msg    string
	Status int
}

func (e *Error) Error() string { return e.Msg }

// Options 是 New 的入参。
type Options struct {
	Store  *store.Store
	Logger *slog.Logger
	// Hosts 提供主机行与凭访问证书登好的 SSH 连接（安装、卸载与透传都走它）。
	Hosts *agenthost.Manager
	// Binaries 提供各架构的 devd 二进制；nil = 内嵌制品。
	Binaries BinaryProvider
	// ModelBinaries 提供各架构的 modeld 二进制；nil = 内嵌制品（Binaries 给的是非内嵌的假提供者时
	// 沿用它：测试里两种守护进程的假二进制不必分开给）。
	ModelBinaries BinaryProvider
	Now           func() time.Time
}

// Manager 是本包的入口。方法并发安全：状态全在 store 里。
type Manager struct {
	st       *store.Store
	log      *slog.Logger
	hosts    *agenthost.Manager
	binaries BinaryProvider
	modelBin BinaryProvider
	now      func() time.Time

	// jobs 是各主机的开发工具动作队列（devtooljobs.go），按需建。
	jobsMu  sync.Mutex
	jobs    map[int64]*devToolQueue
	jobSeq  int64
	jobHook DevToolHook
}

// New 装配 Manager。
func New(o Options) *Manager {
	m := &Manager{st: o.Store, log: o.Logger, hosts: o.Hosts, binaries: o.Binaries, modelBin: o.ModelBinaries, now: o.Now}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.binaries == nil {
		m.binaries = &EmbeddedBinaries{BinName: devdSpec.BinName}
	}
	if m.modelBin == nil {
		if _, embedded := m.binaries.(*EmbeddedBinaries); embedded {
			m.modelBin = &EmbeddedBinaries{BinName: modeldSpec.BinName}
		} else {
			m.modelBin = m.binaries
		}
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m
}

// Binaries 暴露 devd 的二进制提供者。
func (m *Manager) Binaries() BinaryProvider { return m.binaries }

// binariesFor 是这种守护进程的二进制提供者。
func (m *Manager) binariesFor(spec daemonSpec) BinaryProvider {
	if spec.Name == modeldSpec.Name {
		return m.modelBin
	}
	return m.binaries
}

// DaemonVersion 是内嵌 devd 的版本（没有制品为空——界面据此禁用安装）。
func (m *Manager) DaemonVersion() string { return m.versionOf(devdSpec) }

// ModelDaemonVersion 是内嵌 modeld 的版本（没有制品为空）。
func (m *Manager) ModelDaemonVersion() string { return m.versionOf(modeldSpec) }

// DaemonVersionFor 是这台主机该装的那种守护进程的内嵌版本。
func (m *Manager) DaemonVersionFor(host *store.AgentHost) string { return m.versionOf(specFor(host)) }

func (m *Manager) versionOf(spec daemonSpec) string {
	if eb, ok := m.binariesFor(spec).(*EmbeddedBinaries); ok {
		return eb.Version()
	}
	return "test"
}

const (
	hostTimeout = 4 * time.Minute
	// daemonStartWait 是安装后等守护进程起来的上限。
	daemonStartWait = 20 * time.Second
	// uploadTimeout 是上传二进制那一条命令的上限：十几 MB 走 Wi-Fi 到慢一点的板子。
	uploadTimeout = 3 * time.Minute
	// commandTimeout 是其余远端命令的上限（systemctl 起服务在内）。
	commandTimeout = 90 * time.Second
	outputLimit    = 8 << 10
)

// ---- 安装 / 卸载 ----

// InstallRequest 是「安装 / 重新安装守护进程」的入参。
type InstallRequest struct {
	// ID 是主机行。
	ID int64
	// Password 只在该用户没有免密 sudo 时需要：经 sudo -S 的 stdin 用一次即弃。
	// root 或已免密 sudo 的主机留空。
	Password string
}

// Install 经这台主机已有的免密 SSH 连接把守护进程装上去：上传对应架构的二进制，以 root
// 调用 `llmgate-devd install` / `llmgate-modeld install`（守护进程以这个 SSH 用户的身份运行、
// 管理面只听本机 socket），再在同一条 SSH 连接上连守护进程验收。装哪一种由主机类型决定。
func (m *Manager) Install(ctx context.Context, req InstallRequest) (*store.AgentHost, error) {
	host, err := m.hostRow(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	// 受控纳管的主机没有守护进程：类型在纳管时选定、不能切换，不碰主机也不动行。
	if !host.AllowsDaemon() {
		return nil, &Error{Code: CodeKindNotAllowed, Msg: "受控纳管的主机不能安装守护进程，只能使用 Agent远控。"}
	}
	spec := specFor(host)
	// 没有免密 sudo 又没给口令：连都不必连，先把补救动作说清楚。
	if err := needRoot(host, req.Password, true); err != nil {
		return nil, err
	}
	// 固件里没有守护进程制品就别去碰主机。
	if m.versionOf(spec) == "" {
		return nil, &Error{Code: CodeAssetMissing, Msg: "本固件没有内嵌守护进程 " + spec.BinName + " 的制品（构建时未执行 make devd-assets）"}
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	state := m.stateOf(host)

	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return m.fail(ctx, state, fromAgent(err))
	}
	defer c.Close()

	// 架构 → 二进制。
	out, _, code, err := m.run(ctx, c, archCommand, "", commandTimeout)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	arch := goArch(out)
	if code != 0 || arch == "" {
		return m.fail(ctx, state, &Error{Code: CodeUnsupportedArch, Msg: fmt.Sprintf("主机架构 %q 不受支持（只支持 x86_64 与 aarch64 的 Linux）", strings.TrimSpace(out))})
	}
	binary, _, err := m.binariesFor(spec).Binary(arch)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	// 上传二进制到临时文件（用户自己的权限、0600）。
	binTmp := "/tmp/" + spec.BinName + "-" + tmpSuffix()
	if _, stderr, code, err := m.run(ctx, c, uploadCommand(binTmp), string(binary), uploadTimeout); err != nil {
		return m.fail(ctx, state, err)
	} else if code != 0 {
		return m.fail(ctx, state, &Error{Code: CodeCommandFailed, Msg: "上传守护进程到主机失败" + tail(stderr)})
	}
	// 以 root 安装并起服务。
	command, stdin, viaSudo := privileged(host.Username, installScript(spec, binTmp, host.Username), req.Password)
	out, stderr, code, err := m.run(ctx, c, command, stdin, commandTimeout)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	if code != 0 {
		if viaSudo {
			return m.fail(ctx, state, sudoFailure(code, stderr, "在主机上安装守护进程失败"))
		}
		return m.fail(ctx, state, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("在主机上安装守护进程失败（退出码 %d）%s", code, tail(stderr))})
	}
	if parseInstallOutput(out) == "" {
		return m.fail(ctx, state, &Error{Code: CodeCommandFailed, Msg: "守护进程安装输出里没有 socket 路径" + tail(stderr)})
	}
	return m.verifyWithRetry(ctx, c, spec, state)
}

// UninstallRequest 是「卸载守护进程」的入参。Password 同 InstallRequest。
type UninstallRequest struct {
	ID       int64
	Password string
}

// Uninstall 经免密 SSH 在主机上停服务、删配置与二进制，然后把这一行的守护进程观测值
// 归零。主机够不着时返回错误、行保持原样（管理员可以稍后重试，或解除纳管时一并放弃）。
func (m *Manager) Uninstall(ctx context.Context, req UninstallRequest) (*store.AgentHost, error) {
	host, err := m.installedHost(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if err := needRoot(host, req.Password, false); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	state := m.stateOf(host)
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return m.fail(ctx, state, fromAgent(err))
	}
	defer c.Close()
	command, stdin, viaSudo := privileged(host.Username, uninstallScript(specFor(host)), req.Password)
	_, stderr, code, err := m.run(ctx, c, command, stdin, commandTimeout)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	if code != 0 {
		if viaSudo {
			return m.fail(ctx, state, sudoFailure(code, stderr, "卸载守护进程失败"))
		}
		return m.fail(ctx, state, &Error{Code: CodeCommandFailed, Msg: "卸载守护进程失败" + tail(stderr)})
	}
	return m.st.ClearAgentHostDevd(ctx, host.ID)
}

// needRoot 在动主机之前把「要 root 却没免密 sudo 又没口令」说清楚。
func needRoot(host *store.AgentHost, password string, install bool) error {
	if isRoot(host.Username) || host.SudoNoPasswd || password != "" {
		return nil
	}
	if install {
		return &Error{Code: CodeSudoFailed, Msg: fmt.Sprintf(
			"安装守护进程需要 root 权限，而 %s 在这台主机上没有免密 sudo：请填写它的口令（只用这一次），或先在「重新纳管」里配置免密 sudo。", host.Username)}
	}
	return &Error{Code: CodeSudoFailed, Msg: fmt.Sprintf(
		"卸载守护进程需要 root 权限，而 %s 在这台主机上没有免密 sudo：请填写它的口令（只用这一次）。", host.Username)}
}

// run 在借来的连接上跑一条命令。非零退出码不是 error。
func (m *Manager) run(ctx context.Context, c *agenthost.Conn, command, stdin string, timeout time.Duration) (stdout, stderr string, code int, err error) {
	res, err := c.Run(ctx, agenthost.RunRequest{Command: command, Stdin: stdin, Timeout: timeout, OutputLimit: outputLimit})
	if err != nil {
		return "", "", 0, fromAgent(err)
	}
	return res.Stdout, res.Stderr, res.ExitCode, nil
}

// fromAgent 把 agenthost 的错误折成本包形态（码相同、原因照转）。
func fromAgent(err error) error {
	var he *agenthost.Error
	if errors.As(err, &he) {
		return &Error{Code: he.Code, Msg: he.Msg}
	}
	return err
}

func errCode(err error) string {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// ---- 检查 ----

// Check 经 SSH 连一次守护进程，刷新自述与状态。它不改主机上的任何东西。
func (m *Manager) Check(ctx context.Context, id int64) (*store.AgentHost, error) {
	host, err := m.installedHost(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	state := m.stateOf(host)
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return m.fail(ctx, state, fromAgent(err))
	}
	defer c.Close()
	info, err := newDaemonClient(c, specFor(host)).info(ctx)
	if err != nil {
		return m.fail(ctx, state, err)
	}
	return m.record(ctx, state, info)
}

func (m *Manager) hostRow(ctx context.Context, id int64) (*store.AgentHost, error) {
	if m.hosts == nil {
		return nil, &Error{Code: CodeUnavailable, Msg: "本进程未接入主机纳管管理器"}
	}
	return m.st.GetAgentHost(ctx, id)
}

func (m *Manager) installedHost(ctx context.Context, id int64) (*store.AgentHost, error) {
	host, err := m.hostRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !host.Devd.Installed() {
		name := specFor(host).Name
		return nil, &Error{Code: CodeNotInstalled, Msg: fmt.Sprintf("这台主机上没有装守护进程 %s：先「安装 %s」。", name, name)}
	}
	return host, nil
}

// ProxyModelService 同 Proxy，但要求这台主机是模型服务节点（透传给 modeld）。
func (m *Manager) ProxyModelService(ctx context.Context, id int64, method, path string, query url.Values, body io.Reader, contentType string) (*ProxyResponse, error) {
	host, err := m.hostRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !host.IsModelService() {
		return nil, &Error{Code: CodeKindNotAllowed, Msg: "只有模型服务节点才有模型服务页。"}
	}
	return m.Proxy(ctx, id, method, path, query, body, contentType)
}

// ---- devd 透传 ----

// ProxyResponse 是转给界面的守护进程响应。
type ProxyResponse struct {
	Status int
	Body   []byte
}

// Proxy 把一条界面请求转给主机的守护进程（path 不含 /v1 前缀）：开一条 SSH 连接、在上面
// 开通道说 HTTP、用完即断。守护进程的 JSON 响应（含错误体）原样带回，由界面展示。
// 连得上连不上一并记进行里（note），免得工作空间一直报错、主机表还写着「已安装」。
func (m *Manager) Proxy(ctx context.Context, id int64, method, path string, query url.Values, body io.Reader, contentType string) (*ProxyResponse, error) {
	host, err := m.installedHost(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		err = fromAgent(err)
		m.note(ctx, host, err)
		return nil, err
	}
	defer c.Close()
	resp, err := newDaemonClient(c, specFor(host)).do(ctx, method, "/v1/"+strings.TrimPrefix(path, "/"), query, body, contentType)
	if err != nil {
		m.note(ctx, host, err)
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, daemonProxyMax))
	if err != nil {
		err = &Error{Code: CodeUnreachable, Msg: "读取守护进程响应失败：" + err.Error()}
		m.note(ctx, host, err)
		return nil, err
	}
	m.note(ctx, host, nil)
	return &ProxyResponse{Status: resp.StatusCode, Body: raw}, nil
}

// ProxyStream 把一条请求转给守护进程并**原样流回**响应（path 不含 /v1 前缀）：给创作工作空间
// 搬运图像 / 视频用——请求体流着送上去、响应体流着拿回来，不整份落内存；headers 是要带上的
// 额外请求头（Range 之类）。那条 SSH 连接随响应体活着，调用方关掉 Body 时一并收掉。可达性
// 同样记进行里。
func (m *Manager) ProxyStream(ctx context.Context, id int64, method, path string, query url.Values, body io.Reader, contentType string, headers http.Header) (*http.Response, error) {
	host, err := m.installedHost(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		err = fromAgent(err)
		m.note(ctx, host, err)
		return nil, err
	}
	resp, err := newDaemonClient(c, specFor(host)).stream(ctx, method, "/v1/"+strings.TrimPrefix(path, "/"), query, body, contentType, headers)
	if err != nil {
		c.Close()
		m.note(ctx, host, err)
		return nil, err
	}
	m.note(ctx, host, nil)
	resp.Body = &sshBackedBody{ReadCloser: resp.Body, ssh: c}
	return resp, nil
}

// sshBackedBody 关掉响应体时连底下那条 SSH 连接一起关。
type sshBackedBody struct {
	io.ReadCloser
	ssh *agenthost.Conn
}

func (b *sshBackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.ssh.Close()
	return err
}

// Attach 打开一条终端帧流（wire 帧）：那条 SSH 连接随帧流一起活着，调用方 Close 时一并收掉。
// 与 Proxy 一样把可达性记进行里。
func (m *Manager) Attach(ctx context.Context, id int64, name, dir string, cols, rows int) (net.Conn, error) {
	host, err := m.installedHost(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		err = fromAgent(err)
		m.note(ctx, host, err)
		return nil, err
	}
	conn, err := newDaemonClient(c, specFor(host)).attach(ctx, name, dir, cols, rows)
	if err != nil {
		c.Close()
		m.note(ctx, host, err)
		return nil, err
	}
	m.note(ctx, host, nil)
	return &sshBackedConn{Conn: conn, ssh: c}, nil
}

// sshBackedConn 关掉帧流时连底下那条 SSH 连接一起关。
type sshBackedConn struct {
	net.Conn
	ssh *agenthost.Conn
}

func (c *sshBackedConn) Close() error {
	err := c.Conn.Close()
	c.ssh.Close()
	return err
}

// ---- 验收 ----

// verifyWithRetry 在安装后于同一条 SSH 连接上等守护进程起来：socket 还没就绪就隔半秒
// 再试，最多 daemonStartWait。
func (m *Manager) verifyWithRetry(ctx context.Context, c *agenthost.Conn, spec daemonSpec, state store.AgentHostDevdState) (*store.AgentHost, error) {
	deadline := time.Now().Add(daemonStartWait)
	client := newDaemonClient(c, spec)
	for {
		info, err := client.info(ctx)
		if err == nil {
			return m.record(ctx, state, info)
		}
		if errCode(err) != CodeUnreachable || time.Now().After(deadline) {
			return m.fail(ctx, state, err)
		}
		select {
		case <-ctx.Done():
			return m.fail(ctx, state, err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// record 把守护进程自述写回行里。
func (m *Manager) record(ctx context.Context, state store.AgentHostDevdState, info *devd.Info) (*store.AgentHost, error) {
	state.Version = info.Version
	state.Home = info.Home
	state.Tmux = info.Tmux
	state.Status = store.DevdStatusReady
	state.LastError = ""
	return m.st.SetAgentHostDevd(ctx, state)
}

// note 把 devd 透传（Proxy / Attach）这一次的可达性记回行里。它不是「检查」：透传
// 一屏点几十次，读数与行里已有的一样就不写，只有由好转坏、由坏转好才落库。
//
//   - cause == nil：这一次连上了，把上一次留下的异常清掉。
//   - 守护进程自己答出来的错（daemonAnswered：tmux 没装、路径不合法……）同样算「连上了」：
//     它活着，不该把整行标成异常。
//   - 调用方的 ctx 已经完了（界面走开、上游取消）就什么都不写：那不是主机的问题。
func (m *Manager) note(ctx context.Context, host *store.AgentHost, cause error) {
	if ctx.Err() != nil {
		return
	}
	if daemonAnswered(cause) {
		cause = nil
	}
	state := m.stateOf(host)
	switch {
	case cause == nil:
		if host.Devd.Status == store.DevdStatusReady && host.Devd.LastError == "" {
			return
		}
		state.Status = store.DevdStatusReady
		state.LastError = ""
	default:
		if host.Devd.Status == store.DevdStatusError && host.Devd.LastError == cause.Error() {
			return
		}
		state.Status = store.DevdStatusError
		state.LastError = cause.Error()
	}
	if _, err := m.st.SetAgentHostDevd(context.WithoutCancel(ctx), state); err != nil {
		m.log.Error("写回守护进程状态失败", "host_id", state.ID, "error", err.Error())
	}
}

// daemonAnswered 报告这个错误是守护进程自己答出来的（带 HTTP 状态的错误体只可能来自
// 它），而不是路上出的事。
func daemonAnswered(err error) bool {
	var de *Error
	return errors.As(err, &de) && de.Status != 0
}

func (m *Manager) fail(ctx context.Context, state store.AgentHostDevdState, cause error) (*store.AgentHost, error) {
	state.Status = store.DevdStatusError
	state.LastError = cause.Error()
	if _, err := m.st.SetAgentHostDevd(context.WithoutCancel(ctx), state); err != nil {
		m.log.Error("写回守护进程状态失败", "host_id", state.ID, "error", err.Error())
	}
	return nil, cause
}

func (m *Manager) stateOf(h *store.AgentHost) store.AgentHostDevdState {
	d := h.Devd
	return store.AgentHostDevdState{
		ID: h.ID, Version: d.Version, Home: d.Home, Tmux: d.Tmux,
		Status: store.DevdStatusError, CheckedAt: m.now(),
	}
}

// StatusFromError 把本包错误折成 HTTP 状态（管理面用）。
func StatusFromError(err error) (code string, status int, ok bool) {
	var de *Error
	if !errors.As(err, &de) {
		return "", 0, false
	}
	if de.Status != 0 {
		return de.Code, de.Status, true
	}
	status = http.StatusBadGateway
	switch de.Code {
	case CodeInvalidHost, CodeUnsupportedArch:
		status = http.StatusBadRequest
	case CodeHostKeyChanged, CodeNotInstalled, CodeForwardDenied, CodeKindNotAllowed, CodeToolMissing, CodeWorkspaceDirExists:
		status = http.StatusConflict
	case CodeRepoAuthFailed, CodeRepoNotFound, CodeBranchNotFound:
		status = http.StatusBadRequest
	case CodeAuthFailed:
		status = http.StatusUnauthorized
	case CodeAssetMissing:
		status = http.StatusNotImplemented
	case CodeUnavailable:
		status = http.StatusServiceUnavailable
	}
	return de.Code, status, true
}
