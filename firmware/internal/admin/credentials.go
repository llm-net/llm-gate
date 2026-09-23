package admin

// 智能体——凭证管理（store/credentials.go）：
//
//	GET    /admin/v1/credentials        读数：全部凭证的公开面 + 可选的类型与站点词汇表
//	POST   /admin/v1/credentials        添加一份凭证（带令牌）                          （LAN）
//	PATCH  /admin/v1/credentials/{id}   改名称 / 站点 / 账号 / 令牌（令牌留空即不改）      （LAN）
//	DELETE /admin/v1/credentials/{id}   删除                                            （LAN）
//
// 这些凭证是交给智能体使用的第三方凭据（目前只有 git 托管站点的账号 + 令牌）。读数是
// Admin 档，只回公开面（类型、站点、账号、令牌末 4 位）；写端点带令牌明文，恒 LANOnly。
//
// §15.1：令牌只在请求体里出现一次，交给 store 封存即弃；不回响应、不进日志、不进审计
// detail。审计只记类型、站点、账号与「是否换了令牌」。

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventCredentialCreate = "credential.create"
	EventCredentialUpdate = "credential.update"
	EventCredentialDelete = "credential.delete"
)

// credentialJSON 是一份凭证的公开面：没有令牌。
type credentialJSON struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Username string `json:"username"`
	// SecretHint 是令牌末 4 位（短令牌留空），只作辨认。
	SecretHint string `json:"secret_hint,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// credentialKindJSON 是一种凭证类型的词汇表：界面的类型与站点下拉都从这里取。
type credentialKindJSON struct {
	Kind  string   `json:"kind"`
	Hosts []string `json:"hosts"`
}

type credentialsJSON struct {
	Credentials []credentialJSON     `json:"credentials"`
	Kinds       []credentialKindJSON `json:"kinds"`
}

type credentialOneJSON struct {
	Credential credentialJSON `json:"credential"`
}

// credentialRequest 是添加与修改共用的入参。修改时缺席的字段保持原值；Secret 留空在
// 修改时表示不换令牌，添加时必填。Kind 只在添加时生效。
type credentialRequest struct {
	Kind     *string `json:"kind"`
	Name     *string `json:"name"`
	Host     *string `json:"host"`
	Username *string `json:"username"`
	Secret   *string `json:"secret"`
}

func credJSON(c store.Credential) credentialJSON {
	return credentialJSON{
		ID: c.ID, Kind: c.Kind, Name: c.Name, Host: c.Host, Username: c.Username, SecretHint: c.SecretHint,
		CreatedAt: fmtRFC3339(c.CreatedAt), UpdatedAt: fmtRFC3339(c.UpdatedAt),
	}
}

func credentialKinds() []credentialKindJSON {
	return []credentialKindJSON{{Kind: store.CredentialKindGit, Hosts: append([]string(nil), store.GitCredentialHosts...)}}
}

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListCredentials(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := credentialsJSON{Credentials: make([]credentialJSON, 0, len(rows)), Kinds: credentialKinds()}
	for _, c := range rows {
		out.Credentials = append(out.Credentials, credJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateCredential(w http.ResponseWriter, r *http.Request) {
	var req credentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	nc := store.NewCredential{Kind: deref(req.Kind), Name: deref(req.Name), Host: deref(req.Host), Username: deref(req.Username), Secret: deref(req.Secret)}
	c, err := s.st.CreateCredential(r.Context(), nc)
	if err != nil {
		s.writeCredentialError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCredentialCreate, Entity: credentialEntity(c.ID),
		Detail: fmt.Sprintf("添加 %s 凭证 %s@%s%s", c.Kind, c.Username, c.Host, credentialNameSuffix(c.Name)), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusCreated, credentialOneJSON{Credential: credJSON(*c)})
}

func (s *Server) handlePatchCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req credentialRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := s.st.UpdateCredential(r.Context(), id, store.CredentialPatch{
		Name: req.Name, Host: req.Host, Username: req.Username, Secret: req.Secret,
	})
	if err != nil {
		s.writeCredentialError(w, r, err)
		return
	}
	secretNote := ""
	if req.Secret != nil && *req.Secret != "" {
		secretNote = "，已更换令牌"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCredentialUpdate, Entity: credentialEntity(c.ID),
		Detail: fmt.Sprintf("修改 %s 凭证 %s@%s%s%s", c.Kind, c.Username, c.Host, credentialNameSuffix(c.Name), secretNote), RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, credentialOneJSON{Credential: credJSON(*c)})
}

func (s *Server) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.st.GetCredential(r.Context(), id)
	if err != nil {
		s.writeCredentialError(w, r, err)
		return
	}
	if err := s.st.DeleteCredential(r.Context(), id); err != nil {
		s.writeCredentialError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCredentialDelete, Entity: credentialEntity(c.ID),
		Detail: fmt.Sprintf("删除 %s 凭证 %s@%s%s", c.Kind, c.Username, c.Host, credentialNameSuffix(c.Name)), RemoteIP: remoteIP(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// writeCredentialError 把本域的错误映射成统一错误体：入参不合法 400（原因随错误文本），
// 重复 409，不存在 404，其余 500。错误文本不含令牌（store 保证）。
func (s *Server) writeCredentialError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "credential_not_found", "凭证不存在")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "credential_exists", "同一站点、同一账号的凭证已存在")
	case errors.Is(err, store.ErrInvalidCredential):
		writeError(w, http.StatusBadRequest, "invalid_credential", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

func credentialEntity(id string) string { return "credential:" + id }

func credentialNameSuffix(name string) string {
	if name == "" {
		return ""
	}
	return "（" + name + "）"
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
