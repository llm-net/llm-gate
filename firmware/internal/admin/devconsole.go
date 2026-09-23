package admin

// 智能体——主机/SoC 上的守护进程与透传（internal/devhost）。守护进程按主机类型二选一：工作节点
// 是 devd（llmgate-devd），模型服务节点是 modeld（llmgate-modeld）；端点路径沿用 /devd：
//
//	POST   /admin/v1/agent-hosts/{id}/devd/install         经免密 SSH 安装 / 重装守护进程            （LAN）
//	POST   /admin/v1/agent-hosts/{id}/devd/check           经 SSH 连一次守护进程刷新自述            （LAN）
//	DELETE /admin/v1/agent-hosts/{id}/devd                 卸载守护进程                            （LAN）
//	*      /admin/v1/agent-hosts/{id}/console/{path...}    devd 透传：经 SSH 转给守护进程 /v1/{path}   （LAN）
//	GET    /admin/v1/agent-hosts/{id}/terminal             工作空间终端：WebSocket ↔ 守护进程帧流     （LAN）
//
// 读数没有单独端点：守护进程的状态是主机行的一部分（`GET /admin/v1/agent-hosts` 每行的
// `devd`）。守护进程没有自己的证书或端口——设备凭访问证书登录主机、经 SSH 转发通道连到
// 它的本机 socket。全部端点恒 LANOnly：它们要么带着 sudo 口令，要么是另一台机器上的
// 文件、Git 与终端——这类能力不经公网 Tunnel 到达。
//
// §15.1：口令只在请求体里出现一次，交给 internal/devhost 用掉即弃；不入库、不进日志、
// 不进审计 detail、不回响应。透传的文件内容、终端字节不记日志（访问日志只有
// 长度与哈希前缀，与其余管理面一致）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/devd/wire"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/i18n"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/wsock"
)

// 审计事件名。
const (
	EventDevdInstall   = "agent_host.devd_install"
	EventDevdUninstall = "agent_host.devd_uninstall"
	EventDevdPush      = "agent_host.devd_push"
	EventDevdAcceptID  = "agent_host.devd_accept_identity"
	EventDevdTerminal  = "agent_host.devd_terminal"
	EventDevCertRotate = "agent_host.dev_certificate_rotate"
)

// SetDevHosts 注入devd 管理器（gatewayd 装配期调用一次）。
func (s *Server) SetDevHosts(m *devhost.Manager) {
	s.devHosts = m
	if m != nil {
		m.SetDevToolHook(s.auditDevToolJob)
	}
}

func (s *Server) requireDevHosts(w http.ResponseWriter) bool {
	if !s.requireAgentHosts(w) {
		return false
	}
	if s.devHosts == nil {
		writeError(w, http.StatusServiceUnavailable, "dev_hosts_unavailable", "本进程未接入 devd 管理器")
		return false
	}
	return true
}

// agentHostDevdJSON 是一台主机上守护进程的读数；主机行没装守护进程时整个字段缺席。
type agentHostDevdJSON struct {
	Status        string `json:"status"`
	Version       string `json:"version,omitempty"`
	Home          string `json:"home,omitempty"`
	Tmux          bool   `json:"tmux"`
	LastError     string `json:"last_error,omitempty" i18n:"text"`
	LastCheckedAt string `json:"last_checked_at,omitempty"`
}

func devdJSON(d store.AgentHostDevd) *agentHostDevdJSON {
	if !d.Installed() {
		return nil
	}
	j := &agentHostDevdJSON{Status: d.Status, Version: d.Version, Home: d.Home, Tmux: d.Tmux, LastError: d.LastError}
	if !d.CheckedAt.IsZero() {
		j.LastCheckedAt = fmtRFC3339(d.CheckedAt)
	}
	return j
}

// devdRequest 是安装 / 卸载守护进程的入参。Password 只在该用户没有免密 sudo 时需要，用完即弃。
type devdRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleDevdInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req devdRequest
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	host, err := s.devHosts.Install(r.Context(), devhost.InstallRequest{ID: id, Password: req.Password})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventDevdInstall, Entity: agentHostEntity(host.ID),
		Detail:   fmt.Sprintf("经 %s@%s:%d 安装守护进程 %s（版本 %s）", host.Username, host.Address, host.Port, host.DaemonName(), host.Devd.Version),
		RemoteIP: remoteIP(r),
	})
	s.writeAgentHost(w, r, host)
}

func (s *Server) handleDevdUninstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req devdRequest
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	host, err := s.devHosts.Uninstall(r.Context(), devhost.UninstallRequest{ID: id, Password: req.Password})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventDevdUninstall, Entity: agentHostEntity(host.ID),
		Detail: fmt.Sprintf("从 %s@%s:%d 卸载守护进程 %s", host.Username, host.Address, host.Port, host.DaemonName()), RemoteIP: remoteIP(r),
	})
	s.writeAgentHost(w, r, host)
}

func (s *Server) handleDevdCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	host, err := s.devHosts.Check(r.Context(), id)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	s.writeAgentHost(w, r, host)
}

// consoleBodyMax 是转给守护进程的请求体上限（写文件封顶 8 MiB）。
const consoleBodyMax = 8<<20 + 64<<10

// handleDevConsole 把界面请求原样转给守护进程：方法、路径、查询串、JSON 体都透传，
// 响应状态与体原样带回（守护进程的错误体与管理面同形，界面直接展示）。
func (s *Server) handleDevConsole(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	path := r.PathValue("path")
	if path == "" || strings.Contains(path, "..") || path == "trust" {
		writeError(w, http.StatusNotFound, "not_found", "路径不存在")
		return
	}
	var body io.Reader
	ct := ""
	if r.Method != http.MethodGet {
		body = io.LimitReader(r.Body, consoleBodyMax)
		ct = "application/json"
	}
	resp, err := s.devHosts.Proxy(r.Context(), id, r.Method, path, r.URL.Query(), body, ct)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	// 守护进程的错误体 message 是中文源文本：按本次请求的语言本地化后再交给界面。
	if resp.Status/100 != 2 {
		if code, msg, ok := parseErrorBody(resp.Body); ok {
			writeError(w, resp.Status, code, msg)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}

// handleDevTerminal 把浏览器的 WebSocket 与守护进程的终端帧流对接：一条 WebSocket
// 二进制消息 = 一帧 wire 帧，设备只搬运不解释。先完成 WebSocket 握手再连守护进程，
// 连不上时以 Exit 帧把原因交给界面（握手前的 HTTP 错误浏览器拿不到内容）。
func (s *Server) handleDevTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	name := q.Get("name")
	if name == "" {
		name = "main"
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	ws, err := wsock.Accept(w, r)
	if err != nil {
		return
	}
	defer ws.Close(wsock.CloseNormal, "")
	conn, err := s.devHosts.Attach(r.Context(), id, name, q.Get("dir"), cols, rows)
	if err != nil {
		msg := err.Error()
		if _, _, ok := devhost.StatusFromError(err); !ok {
			msg = "连接守护进程失败"
		}
		ws.WriteMessage(wsock.OpBinary, wire.Encode(wire.Exit, []byte(localizeText(r, msg))))
		return
	}
	defer conn.Close()
	s.audit(r.Context(), store.AuditEvent{
		Event: EventDevdTerminal, Entity: agentHostEntity(id),
		Detail: "打开终端会话 " + name, RemoteIP: remoteIP(r),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		fr := wire.NewReader(conn)
		for {
			f, err := fr.Next()
			if err != nil {
				return
			}
			if err := ws.WriteMessage(wsock.OpBinary, wire.Encode(f.Type, f.Payload)); err != nil {
				return
			}
			if f.Type == wire.Exit {
				return
			}
		}
	}()
	for {
		op, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}
		if op != wsock.OpBinary {
			continue
		}
		if _, err := wire.Decode(msg); err != nil {
			break
		}
		if _, err := conn.Write(msg); err != nil {
			break
		}
	}
	conn.Close()
	<-done
}

// writeDevHostError 把本域错误映射成统一错误体。连接类失败是「那台主机的事」，
// 带可读原因回 4xx/502，不落 ERROR 日志。
func (s *Server) writeDevHostError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "host_not_found", "主机不存在")
		return
	}
	code, status, ok := devhost.StatusFromError(err)
	if !ok {
		s.internalError(w, r, err)
		return
	}
	writeError(w, status, code, err.Error())
}

// parseErrorBody 解守护进程的错误体 {"error":{"code","message"}}。
func parseErrorBody(raw []byte) (code, message string, ok bool) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error.Code == "" {
		return "", "", false
	}
	return body.Error.Code, body.Error.Message, true
}

// localizeText 按本次请求的语言本地化一句中文文案（给 WebSocket 里的 Exit 帧用：
// 那条路不经 writeError）。
func localizeText(r *http.Request, msg string) string {
	return i18n.T(i18n.Negotiate(r.Header.Get("Accept-Language")), msg)
}
