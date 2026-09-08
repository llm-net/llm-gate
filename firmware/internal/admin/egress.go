package admin

// 网络/域名/代理——出站代理（internal/egress，docs-dev/firmware-egress-proxy.md）：
//
//	GET   /admin/v1/system/egress        读数：代理方式、地址、是否设了认证、各分类出口、
//	                                     被覆盖的账号、最近一次显式测试与运行期被动错误类别
//	PATCH /admin/v1/system/egress        整组校验并原子替换 profile 与各分类出口          （LAN）
//	POST  /admin/v1/system/egress/test   对固定 HTTPS 目标做 TCP / SOCKS5 / TLS / HTTPS 分层诊断（LAN）
//
// 设备的全部互联网出口按五类流量各自选 direct / proxy；代理端点只有一个（SOCKS5 或
// Clash / Mihomo 的 SOCKS 端口，两者都只经标准 SOCKS5 通信）。选了代理的流量失败关闭，
// 不回落直连。秘密字段（用户名/口令）省略即保留、空串即清除；任何响应都不回显它们。
//
// 审计只记方式、各分类出口与「认证已替换/已清除」，不记代理地址、用户名、口令、目标
// URL 或连接错误原文（§15.1）。

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/egress"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 审计事件名。
const (
	EventEgressUpdate = "system.egress_update"
	EventEgressTest   = "system.egress_test"
)

// egressScopeJSON 是一类流量的读数：标识、界面名称与当前出口。
type egressScopeJSON struct {
	ID    egress.Scope `json:"id"`
	Label string       `json:"label" i18n:"text"`
	Route egress.Route `json:"route"`
}

// egressOverrideJSON 是显式覆盖了出站方式的上游账号。
type egressOverrideJSON struct {
	UpstreamID int64  `json:"upstream_id"`
	Name       string `json:"name"`
	EgressMode string `json:"egress_mode"`
	Disabled   bool   `json:"disabled"`
}

// egressProviderJSON 是可选代理方式。
type egressProviderJSON struct {
	ID    egress.Provider `json:"id"`
	Label string          `json:"label" i18n:"text"`
}

// egressJSON 是「出站代理」分区的完整读数。
type egressJSON struct {
	egress.Status
	Providers []egressProviderJSON `json:"providers"`
	Scopes    []egressScopeJSON    `json:"scopes"`
	Overrides []egressOverrideJSON `json:"overrides"`
}

func (s *Server) requireEgress(w http.ResponseWriter) bool {
	if s.egress == nil {
		writeError(w, http.StatusServiceUnavailable, "egress_unavailable", "本进程未接入出站代理策略")
		return false
	}
	return true
}

func (s *Server) egressReading(r *http.Request) (egressJSON, error) {
	st := s.egress.Status()
	out := egressJSON{Status: st, Scopes: make([]egressScopeJSON, 0, len(egress.Scopes)), Overrides: []egressOverrideJSON{}}
	for _, p := range []egress.Provider{egress.ProviderNone, egress.ProviderSOCKS5, egress.ProviderClash, egress.ProviderMihomo} {
		out.Providers = append(out.Providers, egressProviderJSON{ID: p, Label: p.Label()})
	}
	for _, sc := range egress.Scopes {
		out.Scopes = append(out.Scopes, egressScopeJSON{ID: sc, Label: sc.Label(), Route: st.Routes[sc]})
	}
	ups, err := s.st.ListUpstreams(r.Context())
	if err != nil {
		return out, err
	}
	for _, u := range ups {
		mode := egressModeOf(u.EgressMode)
		if mode == string(egress.ModeInherit) {
			continue
		}
		out.Overrides = append(out.Overrides, egressOverrideJSON{UpstreamID: u.ID, Name: u.Name, EgressMode: mode, Disabled: u.Disabled})
	}
	sort.Slice(out.Overrides, func(i, j int) bool { return out.Overrides[i].Name < out.Overrides[j].Name })
	return out, nil
}

func (s *Server) writeEgress(w http.ResponseWriter, r *http.Request, status int) {
	out, err := s.egressReading(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, status, struct {
		Egress egressJSON `json:"egress"`
	}{Egress: out})
}

func (s *Server) handleEgressStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgress(w) {
		return
	}
	s.writeEgress(w, r, http.StatusOK)
}

// handleEgressUpdate 整组校验并原子替换。请求体字段全部可选：
//
//	provider  ""|socks5|clash（"" 清除整个 profile）
//	address   host:port
//	username / password  省略保留、空串清除
//	routes    {"<scope>": "direct"|"proxy"}，只覆盖给出的分类
//
// 清除代理端点时仍有账号显式设为经代理 → 409 proxy_in_use，管理员需先把这些账号
// 改回跟随或直连（各分类出口的同类冲突由 egress.Manager 自己拒绝）。
func (s *Server) handleEgressUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgress(w) {
		return
	}
	var req struct {
		Provider *string           `json:"provider"`
		Address  *string           `json:"address"`
		Username *string           `json:"username"`
		Password *string           `json:"password"`
		Routes   map[string]string `json:"routes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ch := egress.Change{Provider: req.Provider, Address: req.Address, Username: req.Username, Password: req.Password}
	if len(req.Routes) > 0 {
		ch.Routes = make(map[egress.Scope]egress.Route, len(req.Routes))
		for k, v := range req.Routes {
			ch.Routes[egress.Scope(k)] = egress.Route(v)
		}
	}
	if req.Provider != nil && strings.TrimSpace(*req.Provider) != string(egress.ProviderMihomo) &&
		s.egress.Status().Provider == egress.ProviderMihomo && s.proxyCoreEnabled(r) {
		writeError(w, http.StatusConflict, "proxy_core_active", "内置内核启用中：先在下方停用内核，再切换代理方式")
		return
	}
	clearing := req.Provider != nil && strings.TrimSpace(*req.Provider) == ""
	if clearing {
		reading, err := s.egressReading(r)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		var names []string
		for _, o := range reading.Overrides {
			if o.EgressMode == string(egress.ModeProxy) {
				names = append(names, o.Name)
			}
		}
		if len(names) > 0 {
			writeError(w, http.StatusConflict, "proxy_in_use",
				"以下账号仍显式设为经代理，不能清除代理端点："+strings.Join(names, "、")+"；请先在「模型接入」页把它们改回跟随设备设置或直连")
			return
		}
	}
	sum, err := s.egress.Update(r.Context(), ch)
	if err != nil {
		var ve *egress.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusBadRequest, "invalid_egress", ve.Error())
			return
		}
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventEgressUpdate, Entity: "system:egress", Detail: egressAuditDetail(sum), RemoteIP: remoteIP(r),
	})
	s.writeEgress(w, r, http.StatusOK)
}

// egressAuditDetail 只记方式、各分类出口与认证的替换/清除事实。
func egressAuditDetail(sum egress.Summary) string {
	parts := []string{"provider=" + string(sum.Provider)}
	routes := make([]string, 0, len(egress.Scopes))
	for _, sc := range egress.Scopes {
		routes = append(routes, string(sc)+":"+string(sum.Routes.Get(sc)))
	}
	parts = append(parts, "routes="+strings.Join(routes, ","))
	if sum.AddressChanged {
		parts = append(parts, "address=changed")
	}
	if sum.AuthReplaced {
		parts = append(parts, "auth=replaced")
	} else if sum.AuthCleared {
		parts = append(parts, "auth=cleared")
	}
	return strings.Join(parts, " ")
}

// handleEgressTest 显式触发一次分层诊断；结果随读数返回（last_test）。审计只记结果类别。
func (s *Server) handleEgressTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireEgress(w) {
		return
	}
	res, err := s.egress.Test(r.Context())
	if err != nil {
		if errors.Is(err, egress.ErrNotConfigured) {
			writeError(w, http.StatusConflict, "proxy_unconfigured", "尚未配置代理端点，无法测试")
			return
		}
		s.internalError(w, r, err)
		return
	}
	outcome := "ok"
	if !res.OK {
		outcome = string(res.Category)
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventEgressTest, Entity: "system:egress", Detail: "result=" + outcome, RemoteIP: remoteIP(r),
	})
	s.writeEgress(w, r, http.StatusOK)
}
