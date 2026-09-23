package admin

// 智能体——主机/SoC（internal/agenthost）：
//
//	GET    /admin/v1/agent-hosts                     读数：访问证书公开面 + 纳管主机清单
//	POST   /admin/v1/agent-hosts/certificate         生成访问证书（已有即 409）          （LAN）
//	POST   /admin/v1/agent-hosts/certificate/rotate  轮换证书并下发到全部已纳管主机       （LAN）
//	POST   /admin/v1/agent-hosts                     添加主机：用用户名口令装公钥         （LAN）
//	POST   /admin/v1/agent-hosts/{id}/enroll         重新纳管（换口令、接受新主机公钥）   （LAN）
//	POST   /admin/v1/agent-hosts/{id}/push           凭现有证书补发当前证书，不要口令     （LAN）
//	POST   /admin/v1/agent-hosts/{id}/check          连一次核对证书与免密 sudo            （LAN）
//	PATCH  /admin/v1/agent-hosts/{id}                只改展示名称                         （LAN）
//	DELETE /admin/v1/agent-hosts/{id}                解除纳管（缺省先摘掉主机上的公钥）   （LAN）
//
// 每台主机上的守护进程 devd（安装 / 检查 / 补发 / 卸载、devd 透传、终端）在
// devconsole.go，路径都挂在 /admin/v1/agent-hosts/{id}/ 下；读数并在主机行里。
//
// 全部写端点恒 LANOnly：它们要么带着主机口令，要么改的是另一台机器上的
// authorized_keys 与 sudoers——这类能力不该经公网 Tunnel 到达。读数是 Admin 档，
// 只回公开事实（公钥、指纹、地址、状态），不回私钥、不回口令。
//
// §15.1：口令只在请求体里出现一次，交给 internal/agenthost 经 SSH 会话 stdin 用掉即弃；
// 不入库、不进日志、不进审计 detail、不回响应。审计只记名称、连接三元组与动作。

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventAgentHostEnroll   = "agent_host.enroll"
	EventAgentHostRemove   = "agent_host.remove"
	EventAgentHostRename   = "agent_host.rename"
	EventAgentHostPush     = "agent_host.push"
	EventAgentCertGenerate = "agent_host.certificate_generate"
	EventAgentCertRotate   = "agent_host.certificate_rotate"
)

// SetAgentHosts 注入主机纳管管理器（gatewayd 装配期调用一次）。
func (s *Server) SetAgentHosts(m *agenthost.Manager) { s.hosts = m }

func (s *Server) requireAgentHosts(w http.ResponseWriter) bool {
	if s.hosts == nil {
		writeError(w, http.StatusServiceUnavailable, "agent_hosts_unavailable", "本进程未接入主机纳管管理器")
		return false
	}
	return true
}

// agentCertJSON 是访问证书的公开面。私钥没有任何字段能导出。
type agentCertJSON struct {
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	CreatedAt   string `json:"created_at"`
	// FileName 是「下载公钥文件」建议的文件名（界面按它存盘）。
	FileName string `json:"file_name"`
}

// agentHostJSON 是一台纳管主机的读数。
type agentHostJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Kind 是纳管时选定的类型：managed（受控纳管，只能用 Agent远控）、worker（工作节点，
	// 还可安装 devd）或 model_service（模型服务节点，装 modeld）；之后不改。
	Kind     string `json:"kind"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Status   string `json:"status"`
	// CertCurrent 报告这台主机上装的是不是当前证书：false = 证书待更新
	// （多半是轮换证书时它不在线），补救动作是「补发证书」。
	CertCurrent bool `json:"cert_current"`
	// KeyFingerprint 是主机上已装的设备公钥指纹；HostKeyFingerprint 是首次纳管时
	// 钉死的主机公钥指纹（管理员可与主机上 ssh-keygen -lf 的输出核对）。
	KeyFingerprint     string `json:"key_fingerprint,omitempty"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	SudoNoPasswd       bool   `json:"sudo_nopasswd"`
	System             string `json:"system,omitempty"`
	LastError          string `json:"last_error,omitempty" i18n:"text"`
	LastCheckedAt      string `json:"last_checked_at,omitempty"`
	CreatedAt          string `json:"created_at"`
	// Devd 是这台主机上守护进程（工作节点 devd / 模型服务节点 modeld）的读数；没装即缺席。
	Devd *agentHostDevdJSON `json:"devd,omitempty"`
}

type agentHostsJSON struct {
	// Certificate 为空 = 还没生成访问证书（界面先请管理员生成）。
	Certificate *agentCertJSON  `json:"certificate,omitempty"`
	Hosts       []agentHostJSON `json:"hosts"`
	// DaemonVersion 是固件内嵌的 devd 版本；空 = 本固件没有制品，界面不给「安装 devd」。
	DaemonVersion string `json:"daemon_version,omitempty"`
	// ModelDaemonVersion 是固件内嵌的 modeld 版本；空 = 没有制品，界面不给「安装 modeld」。
	ModelDaemonVersion string `json:"model_daemon_version,omitempty"`
}

// agentHostJSON 单条响应：改名、纳管、补发、检查都回同一形状的整份读数，
// 界面一次刷新到位。
type agentHostOneJSON struct {
	Host agentHostJSON `json:"host"`
}

type agentRotateJSON struct {
	Certificate *agentCertJSON         `json:"certificate,omitempty"`
	Results     []agenthost.HostResult `json:"results"`
	Hosts       []agentHostJSON        `json:"hosts"`
}

func certJSON(c *agenthost.Certificate) *agentCertJSON {
	if c == nil {
		return nil
	}
	return &agentCertJSON{
		PublicKey:   c.PublicKey,
		Fingerprint: c.Fingerprint,
		KeyType:     c.KeyType,
		CreatedAt:   fmtRFC3339(c.CreatedAt),
		FileName:    "llmgate-agent.pub",
	}
}

func hostJSON(h store.AgentHost, cert *agenthost.Certificate) agentHostJSON {
	j := agentHostJSON{
		ID: h.ID, Name: h.Name, Kind: h.Kind, Address: h.Address, Port: h.Port, Username: h.Username,
		Status: h.Status, KeyFingerprint: h.KeyFingerprint,
		HostKeyFingerprint: agenthost.FingerprintOf(h.HostKey),
		SudoNoPasswd:       h.SudoNoPasswd, System: h.System, LastError: h.LastError,
		CreatedAt: fmtRFC3339(h.CreatedAt),
	}
	if cert != nil && h.KeyFingerprint != "" && h.KeyFingerprint == cert.Fingerprint {
		j.CertCurrent = true
	}
	if !h.LastCheckedAt.IsZero() {
		j.LastCheckedAt = fmtRFC3339(h.LastCheckedAt)
	}
	j.Devd = devdJSON(h.Devd)
	return j
}

// snapshot 装配整份读数（证书 + 全部主机）。
func (s *Server) agentHostsSnapshot(r *http.Request) (*agentHostsJSON, *agenthost.Certificate, error) {
	cert, err := s.hosts.CurrentCertificate(r.Context())
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.hosts.List(r.Context())
	if err != nil {
		return nil, nil, err
	}
	out := &agentHostsJSON{Certificate: certJSON(cert), Hosts: make([]agentHostJSON, 0, len(rows))}
	if s.devHosts != nil {
		out.DaemonVersion = s.devHosts.DaemonVersion()
		out.ModelDaemonVersion = s.devHosts.ModelDaemonVersion()
	}
	for _, h := range rows {
		out.Hosts = append(out.Hosts, hostJSON(h, cert))
	}
	return out, cert, nil
}

func (s *Server) handleAgentHosts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	snapshot, _, err := s.agentHostsSnapshot(r)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleAgentCertGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	cert, err := s.hosts.GenerateCertificate(r.Context())
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentCertGenerate, Entity: "system:agent_hosts",
		Detail: "生成智能体访问证书 " + cert.Fingerprint, RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Certificate *agentCertJSON `json:"certificate"`
	}{Certificate: certJSON(cert)})
}

func (s *Server) handleAgentCertRotate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	cert, results, err := s.hosts.RotateCertificate(r.Context())
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	failed := 0
	for _, res := range results {
		if !res.OK {
			failed++
		}
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentCertRotate, Entity: "system:agent_hosts",
		Detail:   fmt.Sprintf("更新智能体访问证书 %s，下发 %d 台，失败 %d 台", cert.Fingerprint, len(results), failed),
		RemoteIP: remoteIP(r),
	})
	rows, err := s.hosts.List(r.Context())
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	out := agentRotateJSON{Certificate: certJSON(cert), Results: results, Hosts: make([]agentHostJSON, 0, len(rows))}
	if out.Results == nil {
		out.Results = []agenthost.HostResult{}
	}
	for _, h := range rows {
		out.Hosts = append(out.Hosts, hostJSON(h, cert))
	}
	writeJSON(w, http.StatusOK, out)
}

// agentEnrollRequest 是添加主机与重新纳管共用的入参。Password 用完即弃；Kind 只在
// 添加时生效（managed | worker），重新纳管沿用行里的类型。
type agentEnrollRequest struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Address          string `json:"address"`
	Port             int    `json:"port"`
	Username         string `json:"username"`
	Password         string `json:"password"`
	ConfigureSudo    bool   `json:"configure_sudo"`
	AcceptNewHostKey bool   `json:"accept_new_host_key"`
}

func (s *Server) handleAddAgentHost(w http.ResponseWriter, r *http.Request) {
	s.enroll(w, r, 0)
}

func (s *Server) handleEnrollAgentHost(w http.ResponseWriter, r *http.Request) {
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	s.enroll(w, r, id)
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request, id int64) {
	if !s.requireAgentHosts(w) {
		return
	}
	var req agentEnrollRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "invalid_host", "请填写主机上这个用户的登录口令：设备只用它装一次公钥，不会保存。")
		return
	}
	host, err := s.hosts.Enroll(r.Context(), agenthost.EnrollRequest{
		ID: id, Name: req.Name, Kind: req.Kind, Address: req.Address, Port: req.Port, Username: req.Username,
		Password: req.Password, ConfigureSudo: req.ConfigureSudo, AcceptNewHostKey: req.AcceptNewHostKey,
	})
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostEnroll, Entity: agentHostEntity(host.ID),
		Detail:   fmt.Sprintf("纳管 %s@%s:%d（%s，免密 sudo %s）", host.Username, host.Address, host.Port, kindLabel(host.Kind), enabledLabel(host.SudoNoPasswd)),
		RemoteIP: remoteIP(r),
	})
	s.writeAgentHost(w, r, host)
}

func (s *Server) handlePushAgentHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	host, err := s.hosts.Push(r.Context(), id)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostPush, Entity: agentHostEntity(host.ID),
		Detail:   fmt.Sprintf("向 %s@%s:%d 补发访问证书", host.Username, host.Address, host.Port),
		RemoteIP: remoteIP(r),
	})
	s.writeAgentHost(w, r, host)
}

func (s *Server) handleCheckAgentHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	host, err := s.hosts.Check(r.Context(), id)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	s.writeAgentHost(w, r, host)
}

func (s *Server) handlePatchAgentHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	host, err := s.hosts.Rename(r.Context(), id, req.Name)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostRename, Entity: agentHostEntity(host.ID),
		Detail: "主机名称改为 " + host.Name, RemoteIP: remoteIP(r),
	})
	s.writeAgentHost(w, r, host)
}

func (s *Server) handleDeleteAgentHost(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgentHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req struct {
		// RemoveKey 缺省为真：解除纳管时先把设备公钥从主机上摘掉（装了守护进程 devd
		// 的也先卸掉它）。主机够不着也照样删本地行，只是响应里 key_removed /
		// devd_removed 为 false。
		RemoveKey *bool `json:"remove_key"`
		// Password 只在卸载守护进程要 sudo 口令时用（该用户没有免密 sudo），用完即弃。
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	removeKey := req.RemoveKey == nil || *req.RemoveKey
	host, err := s.hosts.Get(r.Context(), id)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	// 先结束这台主机的智能体会话（队列取消、引擎关掉），再卸守护进程、摘公钥、删行：
	// 行删了时间线与档案随之级联删除。
	if s.hostAgent != nil {
		s.hostAgent.HostRemoved(r.Context(), id)
	}
	devdRemoved := false
	if removeKey && host.Devd.Installed() && s.devHosts != nil {
		_, uerr := s.devHosts.Uninstall(r.Context(), devhost.UninstallRequest{ID: id, Password: req.Password})
		devdRemoved = uerr == nil
	}
	cleaned, err := s.hosts.Remove(r.Context(), id, removeKey)
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	detail := fmt.Sprintf("解除纳管 %s@%s:%d（主机上的公钥%s）", host.Username, host.Address, host.Port, removedLabel(removeKey, cleaned))
	if host.Devd.Installed() {
		detail += fmt.Sprintf("（守护进程 devd%s）", uninstalledLabel(removeKey, devdRemoved))
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventAgentHostRemove, Entity: agentHostEntity(id), Detail: detail, RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		KeyRemoved  bool `json:"key_removed"`
		DevdRemoved bool `json:"devd_removed"`
	}{KeyRemoved: cleaned, DevdRemoved: devdRemoved})
}

func (s *Server) writeAgentHost(w http.ResponseWriter, r *http.Request, host *store.AgentHost) {
	cert, err := s.hosts.CurrentCertificate(r.Context())
	if err != nil {
		s.writeAgentHostError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentHostOneJSON{Host: hostJSON(*host, cert)})
}

// writeAgentHostError 把本域的错误映射成统一错误体。连接类失败是「那台主机的
// 事」，不是设备内部错误：一律带可读原因回 4xx/502，不落 ERROR 日志。
func (s *Server) writeAgentHostError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host_not_found", "主机不存在")
		return
	}
	var he *agenthost.Error
	if !errors.As(err, &he) {
		s.internalError(w, r, err)
		return
	}
	status := http.StatusBadGateway
	switch he.Code {
	case agenthost.CodeInvalidHost:
		status = http.StatusBadRequest
	case agenthost.CodeHostExists, agenthost.CodeCertificateExists,
		agenthost.CodeCertificateMissing, agenthost.CodeHostKeyChanged,
		agenthost.CodeCertificateUnreadable:
		status = http.StatusConflict
	case agenthost.CodeAuthFailed:
		status = http.StatusUnauthorized
	}
	writeError(w, status, he.Code, he.Msg)
}

func agentHostID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "host_not_found", "主机不存在")
		return 0, false
	}
	return id, true
}

func agentHostEntity(id int64) string { return "agent_host:" + strconv.FormatInt(id, 10) }

func kindLabel(kind string) string {
	switch kind {
	case store.AgentHostKindManaged:
		return "受控纳管"
	case store.AgentHostKindModelService:
		return "模型服务"
	}
	return "工作节点"
}

func enabledLabel(on bool) string {
	if on {
		return "已配置"
	}
	return "未配置"
}

func removedLabel(requested, done bool) string {
	switch {
	case !requested:
		return "按要求保留"
	case done:
		return "已摘除"
	default:
		return "未能摘除（主机当时不可达）"
	}
}

func uninstalledLabel(requested, done bool) string {
	switch {
	case !requested:
		return "按要求保留"
	case done:
		return "已卸载"
	default:
		return "未能卸载（主机当时不可达或需要 sudo 口令）"
	}
}
