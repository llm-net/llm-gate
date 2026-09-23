package admin

// 智能体——工作空间（store/workspaces.go + internal/devhost/workspace.go）：
//
//	GET    /admin/v1/workspaces        读数：全部工作空间 + 可选的工作节点与 git 凭证
//	POST   /admin/v1/workspaces        创建一个工作空间：开发（可选克隆仓库）或创作，都落在一台工作节点上（LAN）
//	GET    /admin/v1/workspaces/{id}   一个工作空间（打开页用）
//	DELETE /admin/v1/workspaces/{id}   删除记录，开发空间可选连同主机上的目录；创作空间连同设备上的目录 （LAN）
//
// 开发工作空间是一台工作节点上 ~/workspaces/<name> 那个目录。创建时设备经 SSH 在主机上先
// 校验仓库地址、分支与凭证（git ls-remote），任一不通就不建目录也不落行；通过后建目录、
// 克隆，再把行写进设备的 workspaces 表。打开工作空间走 devd 的透传端点
// （/admin/v1/agent-hosts/{id}/console/* 与 /terminal），本文件不另起一套。
// 创作工作空间（kind = studio，internal/studio）同样是工作节点上 ~/workspaces/<name> 那个目录，
// 不克隆仓库；早先的创作空间也可能落在设备数据目录下（host_* 全空），那种不再新建、只能管理
// 文件。它的文件与智能体对话端点在 studio.go。
//
// §15.1：凭证令牌由设备解封一次、经 SSH 会话的 stdin 交给主机上的脚本，不回响应、不进
// 日志、不进审计 detail（审计只记凭证的账号@站点）。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/studio"
)

// 审计事件名。
const (
	EventWorkspaceCreate = "workspace.create"
	EventWorkspaceDelete = "workspace.delete"
)

// workspaceJSON 是一个工作空间的读数，连同它所在主机与所用凭证的展示信息。
type workspaceJSON struct {
	ID string `json:"id"`
	// Kind 是类型：dev（工作节点上的目录）或 studio（设备上的创作工作空间，host_* 全空）。
	Kind        string `json:"kind"`
	HostID      int64  `json:"host_id"`
	HostName    string `json:"host_name"`
	HostAddress string `json:"host_address"`
	// HostReady 报告这台主机的 devd 当前可用（装了 devd 且最近一次连得上）：打开工作空间
	// 要靠它。
	HostReady bool   `json:"host_ready"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	RepoURL   string `json:"repo_url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// CredentialID / CredentialLabel 是克隆时用的凭证；凭证已删除时两者都缺席。
	CredentialID    string `json:"credential_id,omitempty"`
	CredentialLabel string `json:"credential_label,omitempty"`
	// Template / TemplateName 是创作工作空间的创作类型（studio.Template）：创建时选定一次、之后不改；
	// 开发工作空间两者都缺席。
	Template     string `json:"template,omitempty"`
	TemplateName string `json:"template_name,omitempty" i18n:"text"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// workspaceHostJSON 是创建对话框可选的工作节点。
type workspaceHostJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Username string `json:"username"`
	// DevdReady 报告 devd 可用；没装 devd 也能创建，只是打不开。
	DevdReady bool `json:"devd_ready"`
}

// workspaceCredentialJSON 是创建对话框可选的 git 凭证。
type workspaceCredentialJSON struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type workspacesJSON struct {
	Workspaces  []workspaceJSON           `json:"workspaces"`
	Hosts       []workspaceHostJSON       `json:"hosts"`
	Credentials []workspaceCredentialJSON `json:"credentials"`
	// StudioTemplates 是创建对话框可选的创作类型（按展示顺序，首项是缺省）。
	StudioTemplates []studio.Template `json:"studio_templates"`
}

type workspaceOneJSON struct {
	Workspace workspaceJSON `json:"workspace"`
}

// workspaceRequest 是创建的入参。Kind 空即 dev；Template 只对 studio 有意义，空即 studio.DefaultTemplate。
type workspaceRequest struct {
	Kind         string `json:"kind"`
	HostID       int64  `json:"host_id"`
	Name         string `json:"name"`
	RepoURL      string `json:"repo_url"`
	Branch       string `json:"branch"`
	CredentialID string `json:"credential_id"`
	Template     string `json:"template"`
}

// workspaceDeleteRequest 是删除的可选入参。
type workspaceDeleteRequest struct {
	// RemoveDir 为真时连主机上的目录一起删；主机够不着即整个删除失败、行保留。
	RemoveDir bool `json:"remove_dir"`
}

func credentialLabel(c store.Credential) string {
	label := c.Username + "@" + c.Host
	if c.Name != "" {
		label = c.Name + "（" + label + "）"
	}
	return label
}

func workspaceView(w store.Workspace, hosts map[int64]store.AgentHost, creds map[string]store.Credential) workspaceJSON {
	kind := w.Kind
	if kind == "" {
		kind = store.WorkspaceKindDev
	}
	j := workspaceJSON{
		ID: w.ID, Kind: kind, HostID: w.HostID, Name: w.Name, Path: w.Path, RepoURL: w.RepoURL, Branch: w.Branch,
		CreatedAt: fmtRFC3339(w.CreatedAt), UpdatedAt: fmtRFC3339(w.UpdatedAt),
	}
	if h, ok := hosts[w.HostID]; ok {
		j.HostName, j.HostAddress = h.Name, h.Address
		j.HostReady = h.Devd.Installed() && h.Devd.Status == store.DevdStatusReady
	}
	if c, ok := creds[w.CredentialID]; ok && w.CredentialID != "" {
		j.CredentialID, j.CredentialLabel = c.ID, credentialLabel(c)
	}
	if w.IsStudio() {
		if tpl, ok := studio.TemplateByID(w.Template); ok {
			j.Template, j.TemplateName = tpl.ID, tpl.Name
		} else {
			j.Template = w.Template
		}
	}
	return j
}

// workspaceContext 一次读出主机表与凭证表，供读数拼装。
func (s *Server) workspaceContext(r *http.Request) (map[int64]store.AgentHost, map[string]store.Credential, error) {
	hostRows, err := s.st.ListAgentHosts(r.Context())
	if err != nil {
		return nil, nil, err
	}
	credRows, err := s.st.ListCredentials(r.Context())
	if err != nil {
		return nil, nil, err
	}
	hosts := make(map[int64]store.AgentHost, len(hostRows))
	for _, h := range hostRows {
		hosts[h.ID] = h
	}
	creds := make(map[string]store.Credential, len(credRows))
	for _, c := range credRows {
		creds[c.ID] = c
	}
	return hosts, creds, nil
}

func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListWorkspaces(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	hosts, creds, err := s.workspaceContext(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := workspacesJSON{Workspaces: make([]workspaceJSON, 0, len(rows)), Hosts: []workspaceHostJSON{}, Credentials: []workspaceCredentialJSON{},
		StudioTemplates: studio.Templates()}
	for _, ws := range rows {
		out.Workspaces = append(out.Workspaces, workspaceView(ws, hosts, creds))
	}
	hostRows, _ := s.st.ListAgentHosts(r.Context())
	for _, h := range hostRows {
		if !h.AllowsDevd() {
			continue
		}
		out.Hosts = append(out.Hosts, workspaceHostJSON{ID: h.ID, Name: h.Name, Address: h.Address, Username: h.Username,
			DevdReady: h.Devd.Installed() && h.Devd.Status == store.DevdStatusReady})
	}
	credRows, _ := s.st.ListCredentials(r.Context())
	for _, c := range credRows {
		if c.Kind == store.CredentialKindGit {
			out.Credentials = append(out.Credentials, workspaceCredentialJSON{ID: c.ID, Label: credentialLabel(c)})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.st.GetWorkspace(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	hosts, creds, err := s.workspaceContext(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceOneJSON{Workspace: workspaceView(*ws, hosts, creds)})
}

func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req workspaceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind == store.WorkspaceKindStudio {
		s.createStudioWorkspace(w, r, req)
		return
	}
	if req.Kind != "" && req.Kind != store.WorkspaceKindDev {
		writeError(w, http.StatusBadRequest, "invalid_workspace", "未知的工作空间类型")
		return
	}
	if !s.requireDevHosts(w) {
		return
	}
	name, err := store.NormalizeWorkspaceName(req.Name)
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	repo, err := store.NormalizeRepoURL(req.RepoURL)
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	branch, err := store.NormalizeBranch(req.Branch)
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	if repo == "" && (branch != "" || req.CredentialID != "") {
		writeError(w, http.StatusBadRequest, "invalid_workspace", "没有仓库地址时不能指定分支或凭证")
		return
	}
	host, err := s.st.GetAgentHost(r.Context(), req.HostID)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	if _, err := s.st.GetWorkspaceByName(r.Context(), host.ID, name); err == nil {
		writeError(w, http.StatusConflict, "workspace_exists", "这台主机上已有同名工作空间")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	var (
		cred      *store.CredentialSecret
		credLabel string
	)
	if req.CredentialID != "" {
		secret, err := s.st.GetCredentialSecret(r.Context(), req.CredentialID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "credential_not_found", "凭证不存在")
			} else {
				writeError(w, http.StatusConflict, "credential_unreadable", err.Error())
			}
			return
		}
		if secret.Kind != store.CredentialKindGit {
			writeError(w, http.StatusBadRequest, "invalid_workspace", "只能选 git 凭证")
			return
		}
		cred = &secret
		credLabel = credentialLabel(secret.Credential)
	}
	res, err := s.devHosts.CreateWorkspace(r.Context(), devhost.WorkspaceRequest{
		HostID: host.ID, Name: name, RepoURL: repo, Branch: branch, Credential: cred,
	})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	ws, err := s.st.CreateWorkspace(r.Context(), store.NewWorkspace{
		HostID: host.ID, Name: name, Path: res.Path, RepoURL: repo, Branch: branch, CredentialID: req.CredentialID,
	})
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	detail := fmt.Sprintf("在 %s@%s:%d 创建工作空间 %s（%s）", host.Username, host.Address, host.Port, ws.Name, ws.Path)
	if repo != "" {
		detail += "，克隆 " + repo
		if branch != "" {
			detail += " 分支 " + branch
		}
		if credLabel != "" {
			detail += "，使用凭证 " + credLabel
		}
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceCreate, Entity: workspaceEntity(ws.ID), Detail: detail, RemoteIP: remoteIP(r)})
	hosts, creds, err := s.workspaceContext(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, workspaceOneJSON{Workspace: workspaceView(*ws, hosts, creds)})
}

// createStudioWorkspace 建一个创作工作空间：落在 host_id 那台工作节点上（经 SSH 建
// ~/workspaces/<name>，同开发工作空间，不克隆仓库）。智能体在节点上运行，设备上不再新建。
func (s *Server) createStudioWorkspace(w http.ResponseWriter, r *http.Request, req workspaceRequest) {
	if !s.requireStudio(w) {
		return
	}
	name, err := store.NormalizeWorkspaceName(req.Name)
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	if req.RepoURL != "" || req.Branch != "" || req.CredentialID != "" {
		writeError(w, http.StatusBadRequest, "invalid_workspace", "创作工作空间不能指定仓库、分支或凭证")
		return
	}
	if req.HostID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_workspace", "创作工作空间须落在一台工作节点上：智能体在节点上运行，设备上不再新建创作空间")
		return
	}
	tpl, ok := studio.TemplateByID(strings.TrimSpace(req.Template))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_workspace", "未知的创作类型")
		return
	}
	s.createStudioWorkspaceOnHost(w, r, req.HostID, name, tpl)
}

func (s *Server) createStudioWorkspaceOnHost(w http.ResponseWriter, r *http.Request, hostID int64, name string, tpl studio.Template) {
	if !s.requireDevHosts(w) {
		return
	}
	host, err := s.st.GetAgentHost(r.Context(), hostID)
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	if _, err := s.st.GetWorkspaceByName(r.Context(), host.ID, name); err == nil {
		writeError(w, http.StatusConflict, "workspace_exists", "这台主机上已有同名工作空间")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	res, err := s.devHosts.CreateWorkspace(r.Context(), devhost.WorkspaceRequest{HostID: host.ID, Name: name})
	if err != nil {
		s.writeDevHostError(w, r, err)
		return
	}
	ws, err := s.st.CreateWorkspace(r.Context(), store.NewWorkspace{Kind: store.WorkspaceKindStudio, HostID: host.ID, Name: name, Path: res.Path, Template: tpl.ID})
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	// 主机上的目录经守护进程预置 agent/PROJECT.md；devd 此刻不可用就先放过（打开空间本来也要它），
	// 首次新建对话时再补。
	if err := s.studio.EnsureBrief(r.Context(), ws); err != nil {
		s.log.Warn("未能在工作节点上预置项目说明", "workspace_id", ws.ID, "error", err.Error())
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceCreate, Entity: workspaceEntity(ws.ID),
		Detail: fmt.Sprintf("在 %s@%s:%d 创建创作工作空间 %s（%s，创作类型 %s）", host.Username, host.Address, host.Port, ws.Name, ws.Path, tpl.ID), RemoteIP: remoteIP(r)})
	s.writeCreatedWorkspace(w, r, ws)
}

func (s *Server) writeCreatedWorkspace(w http.ResponseWriter, r *http.Request, ws *store.Workspace) {
	hosts, creds, err := s.workspaceContext(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, workspaceOneJSON{Workspace: workspaceView(*ws, hosts, creds)})
}

func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	var req workspaceDeleteRequest
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	ws, err := s.st.GetWorkspace(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	detail := "删除工作空间 " + ws.Name + "（" + ws.Path + "）"
	if ws.IsStudio() && !ws.OnHost() {
		// 设备上的创作工作空间：删记录就连目录一起删（留下的文件没有别的入口）。
		if !s.requireStudio(w) {
			return
		}
		s.studio.WorkspaceRemoved(r.Context(), ws.ID)
		if err := s.studio.RemoveDir(ws); err != nil {
			s.internalError(w, r, err)
			return
		}
		detail = "删除创作工作空间 " + ws.Name + " 及设备上的目录 " + ws.Path
	} else if ws.IsStudio() {
		// 主机上的创作工作空间：先收会话与设备上的缩略图缓存，主机上的目录按 remove_dir 决定。
		if !s.requireStudio(w) {
			return
		}
		s.studio.WorkspaceRemoved(r.Context(), ws.ID)
		if req.RemoveDir {
			if !s.requireDevHosts(w) {
				return
			}
			host, err := s.devHosts.RemoveWorkspaceDir(r.Context(), ws)
			if err != nil {
				s.writeDevHostError(w, r, err)
				return
			}
			detail = fmt.Sprintf("删除创作工作空间 %s，并删除 %s@%s:%d 上的目录 %s", ws.Name, host.Username, host.Address, host.Port, ws.Path)
		}
		_ = s.studio.RemoveDir(ws)
	} else if req.RemoveDir {
		if !s.requireDevHosts(w) {
			return
		}
		host, err := s.devHosts.RemoveWorkspaceDir(r.Context(), ws)
		if err != nil {
			s.writeDevHostError(w, r, err)
			return
		}
		detail = fmt.Sprintf("删除工作空间 %s，并删除 %s@%s:%d 上的目录 %s", ws.Name, host.Username, host.Address, host.Port, ws.Path)
	}
	if err := s.st.DeleteWorkspace(r.Context(), ws.ID); err != nil {
		s.writeWorkspaceError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{Event: EventWorkspaceDelete, Entity: workspaceEntity(ws.ID), Detail: detail, RemoteIP: remoteIP(r)})
	w.WriteHeader(http.StatusNoContent)
}

// writeWorkspaceError 把本域错误映射成统一错误体。
func (s *Server) writeWorkspaceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "workspace_not_found", "工作空间不存在")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "workspace_exists", "这台主机上已有同名工作空间")
	case errors.Is(err, store.ErrInvalidWorkspace):
		writeError(w, http.StatusBadRequest, "invalid_workspace", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

func workspaceEntity(id string) string { return "workspace:" + id }
