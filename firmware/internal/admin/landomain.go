package admin

// 网络/域名/代理——内网域名（internal/landomain）：
//
//	GET  /admin/v1/system/lan-domain                    读数：提供方、账号关联、域名、证书、HTTPS 监听
//	PUT  /admin/v1/system/lan-domain                    选择提供方式 / 改 HTTPS 监听地址      （LAN）
//	POST /admin/v1/system/lan-domain/link/start         向官网申请关联码                       （LAN）
//	POST /admin/v1/system/lan-domain/link/wait          同步陪等关联结果（≤ waitLinkFor）      （LAN）
//	POST /admin/v1/system/lan-domain/link/cancel        丢弃进行中的关联                       （LAN）
//	POST /admin/v1/system/lan-domain/unlink             解除账号关联（官网侧释放域名）         （LAN）
//	POST /admin/v1/system/lan-domain/claim              申领托管域名并开始签发（陪等 ≤ waitIssueFor）（LAN）
//	POST /admin/v1/system/lan-domain/register           登记自有域名（不签发：先由管理员设 DNS）  （LAN）
//	POST /admin/v1/system/lan-domain/dns-check          请官网核对自有域名的 A / CNAME / CAA     （LAN）
//	POST /admin/v1/system/lan-domain/certificate        立即签发/续期（陪等 ≤ waitIssueFor）   （LAN）
//	POST /admin/v1/system/lan-domain/certificate/wait   陪等进行中的签发（阶段一变即返回）     （LAN）
//	PUT  /admin/v1/system/lan-domain/target             只改解析地址                           （LAN）
//	POST /admin/v1/system/lan-domain/release            释放域名并删除本机证书                 （LAN）
//
// 写入端点恒 LANOnly（改的是这台设备对外的名字与监听）；读数 Admin 档。陪等端点都是
// 同步语义但不绑请求生命周期：管理器用自己的超时跑完整流程，本端点最多陪等一个上限，
// 签发的陪等还在阶段变化时提前返回（读数 issue_stage），界面拿到一帧就显示、再来陪等
// 下一帧，直到 issuing=false——与关联对话框同属「由人发起的陪等」，零轮询约定不破。
//
// 审计 detail 只含域名、解析地址与提供方（本就要进公共 DNS 的公开值）；设备令牌与
// 私钥不经过本文件（§15.1）。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/landomain"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventLanDomainProvider = "system.lan_domain_provider"
	EventLanDomainLink     = "system.lan_domain_link"
	EventLanDomainUnlink   = "system.lan_domain_unlink"
	EventLanDomainClaim    = "system.lan_domain_claim"
	EventLanDomainRegister = "system.lan_domain_register"
	EventLanDomainIssue    = "system.lan_domain_issue"
	EventLanDomainRelease  = "system.lan_domain_release"
)

const (
	// waitLinkFor 是关联陪等的上限：账号持有人在另一台设备上确认，界面每次陪等一轮。
	waitLinkFor = 25 * time.Second
	// waitIssueFor 是一轮签发陪等的上限：官网侧 DNS-01 全程通常 30–90 秒，等 TXT 传播那一段
	// 最长；阶段一变就提前返回，界面循环陪等直到终态。
	waitIssueFor = 30 * time.Second
)

// SetLanDomain 注入内网域名管理器（gatewayd 装配期调用一次）。
func (s *Server) SetLanDomain(m *landomain.Manager) { s.lan = m }

func (s *Server) requireLanDomain(w http.ResponseWriter) bool {
	if s.lan == nil {
		writeError(w, http.StatusServiceUnavailable, "lan_domain_unavailable", "本进程未接入内网域名管理器")
		return false
	}
	return true
}

type lanProviderJSON struct {
	ID        string `json:"id"`
	Label     string `json:"label" i18n:"text"`
	Available bool   `json:"available"`
}

type lanLinkJSON struct {
	Linked  bool   `json:"linked"`
	Account string `json:"account,omitempty"`
	// Pending 是进行中的关联会话（关联码与官网页面地址）。
	Pending *lanPendingJSON `json:"pending,omitempty"`
}

type lanPendingJSON struct {
	UserCode                string `json:"user_code"`
	VerificationURL         string `json:"verification_url"`
	VerificationURLComplete string `json:"verification_url_complete"`
	ExpiresAt               string `json:"expires_at"`
	Status                  string `json:"status"`
	Error                   string `json:"error,omitempty" i18n:"text"`
}

type lanCertJSON struct {
	NotBefore      string   `json:"not_before"`
	NotAfter       string   `json:"not_after"`
	Issuer         string   `json:"issuer"`
	SANs           []string `json:"sans"`
	ExpiringSoon   bool     `json:"expiring_soon"`
	CoversHostname bool     `json:"covers_hostname"`
}

// lanDNSRecordJSON 是自有域名持有人要在自己的 DNS 服务商设置的一条记录。
type lanDNSRecordJSON struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// lanDNSCheckJSON 是官网用公共解析器核对自有域名三条记录的结果（POST …/dns-check）。
type lanDNSCheckJSON struct {
	Hostname     string `json:"hostname"`
	AcmeDelegate string `json:"acme_delegate"`
	TargetIP     string `json:"target_ip"`
	A            struct {
		Status    string   `json:"status"` // ok | mismatch | missing | error
		Addresses []string `json:"addresses"`
	} `json:"a"`
	Challenge struct {
		Status string `json:"status"` // ok | mismatch | missing | error
		Target string `json:"target,omitempty"`
	} `json:"challenge"`
	CAA struct {
		Status    string   `json:"status"` // ok | blocked | none | error
		FoundAt   string   `json:"found_at,omitempty"`
		Records   []string `json:"records"`
		Permitted []string `json:"permitted"`
	} `json:"caa"`
	Ready     bool   `json:"ready"`
	CheckedAt string `json:"checked_at"`
}

// lanDomainJSON 是「内网域名」读数。
type lanDomainJSON struct {
	Provider  string            `json:"provider"`
	Providers []lanProviderJSON `json:"providers"`
	// SiteConfigured：本进程接了官网客户端（生产恒 true）。
	SiteConfigured bool        `json:"site_configured"`
	Suffix         string      `json:"suffix"`
	Link           lanLinkJSON `json:"link"`
	Claimed        bool        `json:"claimed"`
	// Kind：managed（官网申领的 <label>.<suffix>）/ custom（自有域名）；未选提供方式时空。
	Kind     string `json:"kind,omitempty"`
	Label    string `json:"label,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	TargetIP string `json:"target_ip,omitempty"`
	// AcmeDelegate 与 DNSRecords 只在自有域名登记后出现：持有人要设的 A 与 _acme-challenge CNAME。
	AcmeDelegate string             `json:"acme_delegate,omitempty"`
	DNSRecords   []lanDNSRecordJSON `json:"dns_records,omitempty"`
	// URL 是证书就绪且 HTTPS 监听在跑时的访问地址。
	URL     string `json:"url,omitempty"`
	Issuing bool   `json:"issuing"`
	// IssueStage 是进行中签发的阶段：submitting | pending | authorizing | challenging | validating |
	// finalizing | installing（landomain.Stage*）；不在签发时缺席。
	IssueStage   string       `json:"issue_stage,omitempty"`
	Cert         *lanCertJSON `json:"cert,omitempty"`
	LastCA       string       `json:"last_ca,omitempty"`
	LastIssuedAt string       `json:"last_issued_at,omitempty"`
	LastError    string       `json:"last_error,omitempty" i18n:"text"`
	LastErrorAt  string       `json:"last_error_at,omitempty"`
	HTTPSListen  string       `json:"https_listen"`
	HTTPSActive  bool         `json:"https_active"`
	HTTPSAddr    string       `json:"https_addr,omitempty"`
	// Addresses 是本机可选的解析目标（与接入读数同一口径）。
	Addresses []accessAddress `json:"addresses"`
}

func (s *Server) lanDomainJSON(r *http.Request) lanDomainJSON {
	st := s.lan.Status(r.Context())
	out := lanDomainJSON{
		Provider: st.State.Provider,
		Providers: []lanProviderJSON{
			{ID: landomain.ProviderOfficialSite, Label: "LLM Gate官网", Available: st.SiteConfigured},
			{ID: landomain.ProviderOwnDomain, Label: "自有域名", Available: st.SiteConfigured},
		},
		SiteConfigured: st.SiteConfigured,
		Suffix:         st.State.SuffixOrDefault(),
		Link:           lanLinkJSON{Linked: st.State.Linked, Account: st.State.Account},
		Claimed:        st.State.Claimed(),
		Kind:           st.State.Kind(),
		Label:          st.State.Label(),
		Hostname:       st.State.Hostname,
		TargetIP:       st.State.TargetIP,
		AcmeDelegate:   st.State.AcmeDelegate,
		Issuing:        st.Issuing,
		IssueStage:     st.IssueStage,
		LastCA:         st.State.LastCA,
		LastIssuedAt:   fmtRFC3339(st.State.LastIssuedAt),
		LastError:      st.State.LastError,
		LastErrorAt:    fmtRFC3339(st.State.LastErrorAt),
		HTTPSListen:    st.State.HTTPSListen,
		HTTPSActive:    st.HTTPSAddr != "",
		HTTPSAddr:      st.HTTPSAddr,
		Addresses:      localIPv4s(servedIP(r)),
	}
	for _, rec := range st.State.DNSRecords() {
		out.DNSRecords = append(out.DNSRecords, lanDNSRecordJSON{Type: rec.Type, Name: rec.Name, Value: rec.Value})
	}
	if st.Link != nil {
		out.Link.Pending = &lanPendingJSON{
			UserCode:                st.Link.UserCode,
			VerificationURL:         st.Link.VerificationURL,
			VerificationURLComplete: st.Link.VerificationURLComplete,
			ExpiresAt:               fmtRFC3339(st.Link.ExpiresAt),
			Status:                  st.Link.Status,
			Error:                   st.Link.Error,
		}
	}
	if st.Cert != nil {
		out.Cert = &lanCertJSON{
			NotBefore:      fmtRFC3339(st.Cert.NotBefore),
			NotAfter:       fmtRFC3339(st.Cert.NotAfter),
			Issuer:         st.Cert.Issuer,
			SANs:           st.Cert.SANs,
			ExpiringSoon:   st.Cert.ExpiringSoon,
			CoversHostname: st.Cert.CoversHostname,
		}
		if st.Cert.CoversHostname && st.HTTPSAddr != "" {
			out.URL = s.lanDomainURL(st)
		}
	}
	return out
}

// lanDomainURL 拼 https://<hostname>[:port]；443 省略端口。
func (s *Server) lanDomainURL(st landomain.Status) string {
	port := listenPort(st.HTTPSAddr)
	if port == 0 {
		port = listenPort(st.State.HTTPSListen)
	}
	if port == 0 || port == 443 {
		return "https://" + st.State.Hostname
	}
	return fmt.Sprintf("https://%s:%d", st.State.Hostname, port)
}

// effectiveLanDomainURL 给接入读数：证书覆盖域名且监听在跑时才公布。
func (s *Server) effectiveLanDomainURL(r *http.Request) string {
	if s.lan == nil {
		return ""
	}
	st := s.lan.Status(r.Context())
	if st.Cert == nil || !st.Cert.CoversHostname || st.HTTPSAddr == "" {
		return ""
	}
	return s.lanDomainURL(st)
}

func fmtRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *Server) writeLanDomain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		LanDomain lanDomainJSON `json:"lan_domain"`
	}{LanDomain: s.lanDomainJSON(r)})
}

func (s *Server) handleLanDomainStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	s.writeLanDomain(w, r)
}

// handleLanDomainUpdate 选择提供方式与/或修改 HTTPS 监听地址（字段缺席 = 不改）。
func (s *Server) handleLanDomainUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct {
		Provider    *string `json:"provider"`
		HTTPSListen *string `json:"https_listen"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Provider != nil {
		if err := s.lan.SetProvider(r.Context(), strings.TrimSpace(*req.Provider)); err != nil {
			s.writeLanDomainError(w, r, err)
			return
		}
		s.audit(r.Context(), store.AuditEvent{
			Event: EventLanDomainProvider, Entity: "system:lan_domain",
			Detail: "内网域名提供方式：" + providerLabel(*req.Provider), RemoteIP: remoteIP(r),
		})
	}
	if req.HTTPSListen != nil {
		if err := s.lan.SetHTTPSListen(r.Context(), *req.HTTPSListen); err != nil {
			s.writeLanDomainError(w, r, err)
			return
		}
	}
	s.writeLanDomain(w, r)
}

func providerLabel(p string) string {
	switch p {
	case landomain.ProviderOfficialSite:
		return "LLM Gate官网"
	case landomain.ProviderOwnDomain:
		return "自有域名"
	}
	return "不使用"
}

func (s *Server) handleLanDomainLinkStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	if _, err := s.lan.StartLink(r.Context()); err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainLink, Entity: "system:lan_domain", Detail: "发起 LLM Gate官网账号关联", RemoteIP: remoteIP(r),
	})
	s.writeLanDomain(w, r)
}

// handleLanDomainLinkWait 同步陪等关联结果。approved 后令牌已入库，读数里 linked=true。
func (s *Server) handleLanDomainLinkWait(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	sess, err := s.lan.WaitLink(r.Context(), waitLinkFor)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		s.writeLanDomainError(w, r, err)
		return
	}
	if sess.Status == "approved" {
		s.audit(r.Context(), store.AuditEvent{
			Event: EventLanDomainLink, Entity: "system:lan_domain",
			Detail: "已关联 LLM Gate官网账号：" + sess.Account, RemoteIP: remoteIP(r),
		})
	}
	s.writeLanDomain(w, r)
}

func (s *Server) handleLanDomainLinkCancel(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	s.lan.CancelLink()
	s.writeLanDomain(w, r)
}

func (s *Server) handleLanDomainUnlink(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	prior := s.lan.Status(r.Context()).State
	if err := s.lan.Unlink(r.Context()); err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	detail := "解除 LLM Gate官网账号关联"
	if prior.Hostname != "" {
		detail += "，释放 " + prior.Hostname
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainUnlink, Entity: "system:lan_domain", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeLanDomain(w, r)
}

// handleLanDomainClaim 申领域名（或改解析地址）并随即开始签发；陪等 ≤ waitIssueFor。
func (s *Server) handleLanDomainClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct {
		Label    string `json:"label"`
		TargetIP string `json:"target_ip"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := s.lan.Claim(r.Context(), req.Label, req.TargetIP)
	if err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainClaim, Entity: "system:lan_domain",
		Detail: fmt.Sprintf("%s → %s", st.Hostname, st.TargetIP), RemoteIP: remoteIP(r),
	})
	if _, err := s.lan.StartIssue(); err != nil {
		if errors.Is(err, landomain.ErrIssueBusy) {
			s.writeLanDomain(w, r)
			return
		}
		s.writeLanDomainError(w, r, err)
		return
	}
	s.waitIssue(w, r, landomain.StageSubmitting)
}

// handleLanDomainRegister 登记自有域名（或改期望解析地址）。不随即签发：管理员得先在自己的
// DNS 服务商把读数里 dns_records 那两条设好；签发走 …/certificate。
func (s *Server) handleLanDomainRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct {
		Hostname string `json:"hostname"`
		TargetIP string `json:"target_ip"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := s.lan.Register(r.Context(), req.Hostname, req.TargetIP)
	if err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainRegister, Entity: "system:lan_domain",
		Detail: fmt.Sprintf("登记自有域名 %s → %s", st.Hostname, st.TargetIP), RemoteIP: remoteIP(r),
	})
	s.writeLanDomain(w, r)
}

// handleLanDomainDNSCheck 请官网核对自有域名的公开 DNS 记录，连同读数一起返回。只读。
func (s *Server) handleLanDomainDNSCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	check, err := s.lan.DNSCheck(r.Context())
	if err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	out := lanDNSCheckJSON{Hostname: check.Hostname, AcmeDelegate: check.AcmeDelegate, TargetIP: check.TargetIP, Ready: check.Ready}
	out.A.Status, out.A.Addresses = check.A.Status, nonNil(check.A.Addresses)
	out.Challenge.Status, out.Challenge.Target = check.Challenge.Status, check.Challenge.Target
	out.CAA.Status, out.CAA.FoundAt = check.CAA.Status, check.CAA.FoundAt
	out.CAA.Records, out.CAA.Permitted = nonNil(check.CAA.Records), nonNil(check.CAA.Permitted)
	if check.CheckedAt > 0 {
		out.CheckedAt = time.Unix(check.CheckedAt, 0).UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, struct {
		LanDomain lanDomainJSON   `json:"lan_domain"`
		DNSCheck  lanDNSCheckJSON `json:"dns_check"`
	}{LanDomain: s.lanDomainJSON(r), DNSCheck: out})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Server) handleLanDomainIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.lan.Status(r.Context()).State.Claimed() {
		s.writeLanDomainError(w, r, landomain.ErrNotClaimed)
		return
	}
	if _, err := s.lan.StartIssue(); err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainIssue, Entity: "system:lan_domain", Detail: "发起证书签发", RemoteIP: remoteIP(r),
	})
	s.waitIssue(w, r, landomain.StageSubmitting)
}

// handleLanDomainIssueWait 陪等进行中的签发：`since` 是界面已经看到的阶段，阶段变了或签发
// 结束就返回；没有签发在进行时立即返回当前读数。
func (s *Server) handleLanDomainIssueWait(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct {
		Since string `json:"since"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.lan.Issuing() {
		s.writeLanDomain(w, r)
		return
	}
	s.waitIssue(w, r, req.Since)
}

// waitIssue 陪等到签发结束、阶段离开 since 或 waitIssueFor 耗尽。结束且失败答 502（管理器已把
// 失败写进状态，message 直接给管理员看）；其余情况回快照——issuing=true 时界面继续陪等。
func (s *Server) waitIssue(w http.ResponseWriter, r *http.Request, since string) {
	finished, err := s.lan.WaitIssue(r.Context(), waitIssueFor, since)
	if r.Context().Err() != nil {
		// 管理员关了页面：签发不中断，结果落状态。
		return
	}
	if finished && err != nil {
		writeError(w, http.StatusBadGateway, "issue_failed", "证书签发失败——"+err.Error())
		return
	}
	s.writeLanDomain(w, r)
}

func (s *Server) handleLanDomainTarget(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct {
		TargetIP string `json:"target_ip"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := s.lan.SetTarget(r.Context(), req.TargetIP)
	if err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainClaim, Entity: "system:lan_domain",
		Detail: fmt.Sprintf("%s → %s", st.Hostname, st.TargetIP), RemoteIP: remoteIP(r),
	})
	s.writeLanDomain(w, r)
}

func (s *Server) handleLanDomainRelease(w http.ResponseWriter, r *http.Request) {
	if !s.requireLanDomain(w) {
		return
	}
	var req struct{}
	if !decodeJSON(w, r, &req) {
		return
	}
	prior := s.lan.Status(r.Context()).State
	if err := s.lan.Release(r.Context()); err != nil {
		s.writeLanDomainError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventLanDomainRelease, Entity: "system:lan_domain",
		Detail: "释放 " + prior.Hostname, RemoteIP: remoteIP(r),
	})
	s.writeLanDomain(w, r)
}

// writeLanDomainError 把 landomain 的哨兵与官网拒绝映射为统一错误体。
func (s *Server) writeLanDomainError(w http.ResponseWriter, r *http.Request, err error) {
	if se, ok := landomain.IsSiteError(err); ok {
		s.writeSiteError(w, se)
		return
	}
	var verr *landomain.ValidationError
	switch {
	case errors.As(err, &verr):
		// 设备侧预检（前缀、域名、地址、提供方式取值）没过：管理员改输入即可。
		writeError(w, http.StatusBadRequest, "invalid_lan_domain", err.Error())
	case errors.Is(err, landomain.ErrProviderNotSet):
		writeError(w, http.StatusConflict, "provider_not_set", err.Error())
	case errors.Is(err, landomain.ErrWrongProvider):
		writeError(w, http.StatusConflict, "provider_mismatch", err.Error())
	case errors.Is(err, landomain.ErrSiteUnavailable):
		writeError(w, http.StatusServiceUnavailable, "site_unavailable", err.Error())
	case errors.Is(err, landomain.ErrNotLinked):
		writeError(w, http.StatusConflict, "not_linked", err.Error())
	case errors.Is(err, landomain.ErrTokenUnreadable):
		writeError(w, http.StatusConflict, "link_token_unreadable", err.Error())
	case errors.Is(err, landomain.ErrNoLinkSession):
		writeError(w, http.StatusConflict, "no_link_session", err.Error())
	case errors.Is(err, landomain.ErrNotClaimed):
		writeError(w, http.StatusConflict, "domain_not_claimed", err.Error())
	case errors.Is(err, landomain.ErrDomainClaimed):
		writeError(w, http.StatusConflict, "domain_claimed", err.Error())
	case errors.Is(err, landomain.ErrIssueBusy):
		writeError(w, http.StatusConflict, "issue_busy", err.Error())
	case errors.Is(err, landomain.ErrInvalidListen):
		writeError(w, http.StatusBadRequest, "invalid_listen", err.Error())
	default:
		msg := err.Error()
		if strings.Contains(msg, "LLM Gate官网") {
			writeError(w, http.StatusBadGateway, "site_unreachable", msg)
			return
		}
		writeError(w, http.StatusBadGateway, "lan_domain_failed", msg)
	}
}

// writeSiteError 把官网的业务拒绝原样转给管理员：官网的 message 已是中文，code 沿用。
func (s *Server) writeSiteError(w http.ResponseWriter, se *landomain.SiteError) {
	status := se.Status
	switch {
	case status == http.StatusUnauthorized:
		// 令牌被官网撤销（账号持有人解除了关联）：设备侧要重新关联。
		writeError(w, http.StatusConflict, "link_revoked", "LLM Gate官网已不再接受这台设备的关联令牌，请重新关联账号")
		return
	case status < 400 || status >= 600:
		status = http.StatusBadGateway
	}
	code := se.Code
	if code == "" {
		code = "site_error"
	}
	msg := se.Message
	if msg == "" {
		msg = fmt.Sprintf("LLM Gate官网请求失败（HTTP %d）", se.Status)
	}
	writeError(w, status, code, msg)
}
