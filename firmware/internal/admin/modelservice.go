package admin

// 智能体——模型服务节点（internal/modeld 守护进程 modeld）的管理端点。守护进程的安装 / 检查 /
// 卸载沿用 devconsole.go 的 /devd 端点（按主机类型装 modeld），这里只有透传：
//
//	GET    /admin/v1/agent-hosts/{id}/model                    一屏读数（守护进程 /v1/summary）        （LAN）
//	*      /admin/v1/agent-hosts/{id}/model/{path...}          透传：经 SSH 转给 modeld /v1/{path}     （LAN）
//	POST   /admin/v1/agent-hosts/{id}/model/engines/sessions   起引擎会话：设备解封所选密钥后再转     （LAN）
//
// 只有 model_service 类型的主机走得通（其余答 409 host_kind_not_allowed）。写方法的透传记一条审计
// `agent_host.model_service`，只记方法与路径、不记请求体（§15.1：任务入参、提示词与密钥明文都在体里）。
// 起引擎会话时 API 密钥的明文由设备解封（另记 key.reveal 审计）、经 SSH 会话交给守护进程，守护进程
// 只把它落在 0600 的实例目录里；明文不回响应、不进日志。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventModelService       = "agent_host.model_service"
	EventModelEngineSession = "agent_host.model_engine_session"
)

// modelBodyMax 是转给守护进程的请求体上限（任务入参封顶 8 MiB）。
const modelBodyMax = 8<<20 + 64<<10

func (s *Server) handleModelSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	s.proxyModel(w, r, id, http.MethodGet, "summary", nil, "")
}

// handleModelProxy 把界面请求原样转给 modeld：方法、路径、查询串、JSON 体都透传。
func (s *Server) handleModelProxy(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	path := r.PathValue("path")
	if path == "" || strings.Contains(path, "..") {
		writeError(w, http.StatusNotFound, "not_found", "路径不存在")
		return
	}
	var body io.Reader
	ct := ""
	if r.Method != http.MethodGet {
		body = io.LimitReader(r.Body, modelBodyMax)
		ct = "application/json"
	}
	if r.Method != http.MethodGet {
		s.audit(r.Context(), store.AuditEvent{
			Event: EventModelService, Entity: agentHostEntity(id),
			Detail: fmt.Sprintf("%s %s", r.Method, path), RemoteIP: remoteIP(r),
		})
	}
	s.proxyModel(w, r, id, r.Method, path, body, ct)
}

// modelSessionRequest 是起引擎会话的入参：密钥按 key_id 由设备解封，base_url 是节点访问本设备的地址
// （与安装 gate 时同一份接入地址选项）。
type modelSessionRequest struct {
	Engine       string `json:"engine"`
	KeyID        int64  `json:"key_id"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Workdir      string `json:"workdir"`
	Instructions string `json:"instructions"`
	Binary       string `json:"binary"`
	Title        string `json:"title"`
}

func (s *Server) handleModelEngineSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireDevHosts(w) {
		return
	}
	id, ok := agentHostID(w, r)
	if !ok {
		return
	}
	var req modelSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Engine != "codex" && req.Engine != "claude" {
		writeError(w, http.StatusBadRequest, "invalid_host", "引擎须是 codex 或 claude")
		return
	}
	base, ok := siteRoot(req.BaseURL)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_host", "节点访问本设备的地址须是 http:// 或 https:// 开头的站点根地址")
		return
	}
	if req.KeyID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_host", "请选择一把 API 密钥：引擎的模型调用以它的名义经本设备结算")
		return
	}
	// 先确认这台主机是模型服务节点、装了守护进程，再解封密钥——别为一台走不通的主机泄一次明文。
	host, err := s.st.GetAgentHost(r.Context(), id)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	if !host.IsModelService() {
		writeError(w, http.StatusConflict, "host_kind_not_allowed", "只有模型服务节点才能运行引擎会话。")
		return
	}
	if !host.Devd.Installed() {
		writeError(w, http.StatusConflict, "devd_not_installed", "这台主机上没有装守护进程 modeld：先「安装 modeld」。")
		return
	}
	k, plaintext, ok := s.revealKeyForHost(w, r, req.KeyID, id, "运行引擎会话")
	if !ok {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"engine": req.Engine, "binary": req.Binary, "model": req.Model, "effort": req.Effort, "workdir": req.Workdir,
		"instructions": req.Instructions, "title": req.Title, "api_base": base, "api_key": plaintext,
	})
	s.audit(r.Context(), store.AuditEvent{
		Event: EventModelEngineSession, Entity: agentHostEntity(id),
		Detail:   fmt.Sprintf("在 %s@%s:%d 起 %s 引擎会话（密钥 %s，模型 %s）", host.Username, host.Address, host.Port, req.Engine, k.Label, req.Model),
		RemoteIP: remoteIP(r),
	})
	s.proxyModel(w, r, id, http.MethodPost, "engines/sessions", bytes.NewReader(payload), "application/json")
}

// proxyModel 转一条请求给模型服务节点的守护进程并把响应原样带回（错误体本地化）。
func (s *Server) proxyModel(w http.ResponseWriter, r *http.Request, id int64, method, path string, body io.Reader, ct string) {
	resp, err := s.devHosts.ProxyModelService(r.Context(), id, method, path, r.URL.Query(), body, ct)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
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

// siteRoot 收一个 http / https 站点根地址（路径为空或 /、没有查询串与片段），回去掉尾部斜杠的形态。
func siteRoot(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}
