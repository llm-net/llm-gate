package admin

// 出站代理——Clash 订阅（设备内置内核，internal/mihomo，docs-dev/firmware-egress-proxy.md §8）：
//
//	GET    /admin/v1/system/proxy-core                        读数：组件 / 内核 / 订阅 / 节点 / 选定
//	PUT    /admin/v1/system/proxy-core/subscription           保存订阅地址并立即拉取            （LAN）
//	POST   /admin/v1/system/proxy-core/subscription/refresh   重新拉取订阅                     （LAN）
//	DELETE /admin/v1/system/proxy-core/subscription           清除订阅（内核须已停用）           （LAN）
//	PUT    /admin/v1/system/proxy-core/node                   选定节点（空 = 自动选择）           （LAN）
//	POST   /admin/v1/system/proxy-core/latency                直连各节点测 TCP 连接延迟          （LAN）
//	POST   /admin/v1/system/proxy-core/enable                 确认许可证并启动内核，把出站代理指向它 （LAN）
//	POST   /admin/v1/system/proxy-core/disable                停内核、出站改回直连               （LAN）
//	GET    /admin/v1/system/components/mihomo                 组件读数
//	POST   /admin/v1/system/components/mihomo/check|manifest|download|install|rollback，
//	POST   /admin/v1/system/components/mihomo/upload（octet-stream，LAN），DELETE …/staged
//	DELETE /admin/v1/system/components/mihomo                 卸载组件（内核启用中 409；LAN）
//
// 组件的安装/升级/回退/卸载统一在界面「第三方组件」页操作；「出站代理」标签页只展示组件
// 状态并链接过去。卸载从不替管理员停用功能：内核启用中答 409 component_in_use。
//
// 订阅地址与节点凭据不出任何响应：读数只有订阅主机名、节点名与协议类型。审计只记动作、
// 节点数、版本与结果类别（§15.1）。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/mihomo"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// 审计事件名。
const (
	EventProxyCoreSubscription = "system.proxy_core_subscription"
	EventProxyCoreNode         = "system.proxy_core_node"
	EventProxyCoreLatency      = "system.proxy_core_latency"
	EventProxyCoreEnable       = "system.proxy_core_enable"
	EventProxyCoreDisable      = "system.proxy_core_disable"
)

// MihomoUploadPath 是 withCSRF 放行 octet-stream 的制品上传路径。
const MihomoUploadPath = "/admin/v1/system/components/mihomo/upload"

// mihomoUploadCap 是手动上传的请求体上限（官方 gzip 包约 17 MiB）。
const mihomoUploadCap = 96 << 20

// SetProxyCore 注入内置内核管理器（gatewayd 装配期调用一次）。
func (s *Server) SetProxyCore(m *mihomo.Manager) { s.proxyCore = m }

func (s *Server) requireProxyCore(w http.ResponseWriter) bool {
	if s.proxyCore == nil {
		writeError(w, http.StatusServiceUnavailable, "proxy_core_unavailable", "本进程未接入内置代理内核管理器")
		return false
	}
	return true
}

// proxyCoreEnabled 报告内核是否已启用（管理器未注入时恒 false）。
func (s *Server) proxyCoreEnabled(r *http.Request) bool {
	if s.proxyCore == nil {
		return false
	}
	enabled, err := s.proxyCore.Enabled(r.Context())
	return err == nil && enabled
}

func (s *Server) writeProxyCore(w http.ResponseWriter, r *http.Request, status int) {
	writeJSON(w, status, struct {
		ProxyCore mihomo.Status `json:"proxy_core"`
	}{ProxyCore: s.proxyCore.Status(r.Context())})
}

func (s *Server) handleProxyCoreStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreSetSubscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	parsed, err := s.proxyCore.SetSubscription(r.Context(), req.URL)
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreSubscription, Entity: "system:proxy_core",
		Detail: fmt.Sprintf("订阅已保存并更新：%d 个节点（剔除 %d）", len(parsed.Nodes), parsed.Dropped), RemoteIP: remoteIP(r),
	})
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	parsed, err := s.proxyCore.Refresh(r.Context())
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreSubscription, Entity: "system:proxy_core",
		Detail: fmt.Sprintf("订阅已更新：%d 个节点（剔除 %d）", len(parsed.Nodes), parsed.Dropped), RemoteIP: remoteIP(r),
	})
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreClearSubscription(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	if err := s.proxyCore.ClearSubscription(r.Context()); err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreSubscription, Entity: "system:proxy_core", Detail: "订阅已清除", RemoteIP: remoteIP(r),
	})
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreSelectNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.proxyCore.SelectNode(r.Context(), req.Name); err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	detail := "节点改为自动选择"
	if strings.TrimSpace(req.Name) != "" {
		detail = "选定节点：" + strings.TrimSpace(req.Name)
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreNode, Entity: "system:proxy_core", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreLatency(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	sum, err := s.proxyCore.TestLatency(r.Context())
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreLatency, Entity: "system:proxy_core",
		Detail: fmt.Sprintf("测试节点延迟：%d/%d 可达", sum.Reachable, sum.Total), RemoteIP: remoteIP(r),
	})
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreEnable(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	var req struct {
		AcceptLicense bool `json:"accept_license"`
	}
	if !decodeJSONOptional(w, r, &req) {
		return
	}
	err := s.proxyCore.Enable(r.Context(), req.AcceptLicense)
	detail := "启用内置内核"
	if err != nil {
		detail += "：失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreEnable, Entity: "system:proxy_core", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.writeProxyCore(w, r, http.StatusOK)
}

func (s *Server) handleProxyCoreDisable(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	err := s.proxyCore.Disable(r.Context())
	detail := "停用内置内核（经代理的流量改回直连）"
	if err != nil {
		detail += "：部分失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventProxyCoreDisable, Entity: "system:proxy_core", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.writeProxyCore(w, r, http.StatusOK)
}

// ---- 组件 ----

func (s *Server) writeMihomoComponent(w http.ResponseWriter, r *http.Request, status int) {
	writeJSON(w, status, struct {
		Component mihomo.ComponentStatus `json:"component"`
	}{Component: s.proxyCore.Status(r.Context()).Component})
}

func (s *Server) handleMihomoStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func mihomoAdvisoryDetail(adv *mihomo.Advisory, source string) string {
	if adv == nil {
		return source + "组件清单：无"
	}
	detail := fmt.Sprintf("%s组件清单 revision %d", source, adv.Revision)
	if adv.Latest != nil {
		detail += "，可安装 " + adv.Latest.Version
	} else {
		detail += "，没有可安装的新版本"
	}
	if adv.InstalledBlocked {
		detail += "；已安装版本已被阻断"
	}
	return detail
}

func (s *Server) handleMihomoCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	adv, err := s.proxyCore.Check(r.Context())
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: "component:mihomo", Detail: mihomoAdvisoryDetail(adv, "官网"), RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoManifest(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	var req struct {
		Index     string `json:"index"`
		Signature string `json:"signature"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	adv, err := s.proxyCore.ImportManifest(r.Context(), []byte(req.Index), []byte(req.Signature))
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: "component:mihomo", Detail: mihomoAdvisoryDetail(adv, "离线导入"), RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	adv := s.proxyCore.Advisory(r.Context())
	if err := s.proxyCore.Download(r.Context()); err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	detail := "下载 Mihomo 官方制品"
	if adv != nil && adv.Latest != nil {
		detail += " " + adv.Latest.Version
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDownload, Entity: "component:mihomo", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, mihomoUploadCap+(1<<20))
	staged, err := s.proxyCore.Upload(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "component_too_large", "组件制品超过大小上限")
			return
		}
		s.proxyCoreError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentUpload, Entity: "component:mihomo", Detail: "上传 Mihomo 官方制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	staged, ok := s.proxyCore.StagedManifest()
	if !ok {
		s.proxyCoreError(w, r, mihomo.ErrNoStaged)
		return
	}
	comp, err := s.proxyCore.Install(r.Context())
	detail := "安装 Mihomo " + staged.Version + "（来源 " + staged.Source + "）"
	if err != nil {
		detail += "：失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentInstall, Entity: "component:mihomo", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		if comp != nil {
			writeError(w, http.StatusConflict, "component_not_ready", err.Error())
			return
		}
		s.proxyCoreError(w, r, err)
		return
	}
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoRollback(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	comp, err := s.proxyCore.Rollback(r.Context())
	if err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	detail := "Mihomo 回退到上一 slot"
	if comp != nil && comp.Current != nil {
		detail += "（" + comp.Current.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRollback, Entity: "component:mihomo", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	staged, ok := s.proxyCore.StagedManifest()
	if !ok {
		s.writeMihomoComponent(w, r, http.StatusOK)
		return
	}
	if err := s.proxyCore.Discard(); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDiscard, Entity: "component:mihomo", Detail: "丢弃 Mihomo 制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

func (s *Server) handleMihomoRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireProxyCore(w) {
		return
	}
	before := s.proxyCore.Status(r.Context()).Component
	if err := s.proxyCore.Remove(r.Context()); err != nil {
		s.proxyCoreError(w, r, err)
		return
	}
	detail := "卸载 Mihomo 内核组件"
	if before.Installed && before.Version != "" {
		detail += "（" + before.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRemove, Entity: "component:mihomo", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeMihomoComponent(w, r, http.StatusOK)
}

// proxyCoreError 把管理器与引擎的哨兵错误映射到状态码与机读 code。
func (s *Server) proxyCoreError(w http.ResponseWriter, r *http.Request, err error) {
	var me *mihomo.ManifestError
	var ee *updated.EngineError
	switch {
	case errors.As(err, &me), errors.Is(err, mihomo.ErrManifestStale):
		writeError(w, http.StatusBadRequest, "component_manifest_rejected", err.Error())
	case errors.Is(err, mihomo.ErrNoManifest), errors.Is(err, mihomo.ErrNoAdvisory), errors.Is(err, mihomo.ErrNoStaged):
		writeError(w, http.StatusConflict, "component_not_ready", err.Error())
	case errors.Is(err, mihomo.ErrDownloadBusy):
		writeError(w, http.StatusConflict, "component_download_busy", err.Error())
	case errors.Is(err, mihomo.ErrUploadNotListed):
		writeError(w, http.StatusBadRequest, "component_not_listed", err.Error())
	case errors.Is(err, mihomo.ErrComponentMissing):
		writeError(w, http.StatusConflict, "component_missing", err.Error())
	case errors.Is(err, mihomo.ErrComponentInUse):
		writeError(w, http.StatusConflict, "component_in_use", err.Error())
	case errors.Is(err, mihomo.ErrNoSubscription), errors.Is(err, mihomo.ErrNoNodes), errors.Is(err, mihomo.ErrNotClashConfig):
		writeError(w, http.StatusConflict, "subscription_not_ready", err.Error())
	case errors.Is(err, mihomo.ErrSubscriptionInUse):
		writeError(w, http.StatusConflict, "proxy_core_active", err.Error())
	case errors.Is(err, mihomo.ErrLicenseConsent):
		writeError(w, http.StatusBadRequest, "license_consent_required", err.Error())
	case errors.Is(err, mihomo.ErrProviderNotMihomo):
		writeError(w, http.StatusConflict, "provider_not_core", err.Error())
	case errors.Is(err, mihomo.ErrNodeUnknown):
		writeError(w, http.StatusBadRequest, "node_unknown", err.Error())
	case errors.Is(err, mihomo.ErrLatencyBusy):
		writeError(w, http.StatusConflict, "latency_test_busy", err.Error())
	case errors.Is(err, updated.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "升级引擎不可达，无法操作组件或内核")
	case errors.As(err, &ee):
		status := http.StatusBadRequest
		if ee.Status == http.StatusConflict {
			status = http.StatusConflict
		}
		writeError(w, status, "engine_rejected", ee.Message)
	default:
		if strings.HasPrefix(err.Error(), "订阅") || strings.HasPrefix(err.Error(), "无法连接订阅") || strings.HasPrefix(err.Error(), "拉取订阅") {
			writeError(w, http.StatusBadGateway, "subscription_fetch_failed", err.Error())
			return
		}
		s.internalError(w, r, err)
	}
}
