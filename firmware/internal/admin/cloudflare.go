package admin

// Cloudflare Tunnel 公网接入端点（docs-dev/firmware-cloudflare-tunnel.md §6.2；全部仅
// admin，withSession 默认拒绝已覆盖）：
//
//	GET    /admin/v1/system/cloudflare-tunnel            四层状态 + 配置（不含 token）
//	PUT    /admin/v1/system/cloudflare-tunnel            原子写 hostname/exposure/auto_update/token
//	                                                     （token 省略即保留、clear_token 才销毁）
//	POST   /admin/v1/system/cloudflare-tunnel/enable     前置检查 + origin socket + connector
//	POST   /admin/v1/system/cloudflare-tunnel/disable    停止本机入口（不声称已删 Cloudflare 侧资源）
//	POST   /admin/v1/system/cloudflare-tunnel/test       本地分层自检，可选一次公网 HTTPS 探测
//	POST   /admin/v1/system/cloudflare-tunnel/delete     销毁密封 token 与设置，可选卸载组件
//	GET    /admin/v1/system/components/cloudflared       组件状态
//	POST   /admin/v1/system/components/cloudflared/check      读官网签名清单
//	POST   /admin/v1/system/components/cloudflared/manifest   离线导入签名清单
//	POST   /admin/v1/system/components/cloudflared/download   官方 release 直下（同步陪等 ≤60s）
//	POST   /admin/v1/system/components/cloudflared/upload     手动上传官方制品（octet-stream，只限 LAN）
//	POST   /admin/v1/system/components/cloudflared/install    经升级引擎装进非活动 slot
//	POST   /admin/v1/system/components/cloudflared/rollback   切回上一 slot
//	DELETE /admin/v1/system/components/cloudflared/staged     丢弃已就绪制品
//	DELETE /admin/v1/system/components/cloudflared            卸载组件（Tunnel 启用中 409；只限 LAN）
//
// 组件的安装/升级/回退/卸载统一在界面「第三方组件」页操作；「公网接入」分区只展示组件
// 状态并链接过去。卸载从不替管理员停用功能：Tunnel 启用中答 409 component_in_use。
//
// 边界：设备只持有 tunnel-scoped token，不代用户登录 Cloudflare、不建 zone、不改 DNS。
// 任何响应都不回显 token；审计只记 enable/disable、hostname、exposure、组件版本、
// token replaced/cleared 与结果类别，不记 token、安装命令、account/tunnel ID 或
// 上游错误原文（§15.1）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/cloudflared"
	"github.com/llm-net/llm-gate/firmware/internal/elfcheck"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// 审计事件名。
const (
	EventCloudflareUpdate  = "system.cloudflare_update"
	EventCloudflareEnable  = "system.cloudflare_enable"
	EventCloudflareDisable = "system.cloudflare_disable"
	EventCloudflareTest    = "system.cloudflare_test"
	EventCloudflareDelete  = "system.cloudflare_delete"
	EventComponentCheck    = "system.component_check"
	EventComponentDownload = "system.component_download"
	EventComponentUpload   = "system.component_upload"
	EventComponentInstall  = "system.component_install"
	EventComponentRollback = "system.component_rollback"
	EventComponentDiscard  = "system.component_discard"
	EventComponentRemove   = "system.component_remove"
)

// ComponentUploadPath 是 withCSRF 放行 octet-stream 的组件制品上传路径。
const ComponentUploadPath = "/admin/v1/system/components/cloudflared/upload"

// componentUploadCap 是手动上传的请求体上限（cloudflared 约 37 MiB）。
const componentUploadCap = 128 << 20

// SetCloudflareTunnel 注入 Tunnel 管理器（gatewayd 装配期调用一次）。
func (s *Server) SetCloudflareTunnel(m *cloudflared.Manager) { s.cf = m }

func (s *Server) requireCloudflare(w http.ResponseWriter) bool {
	if s.cf == nil {
		writeError(w, http.StatusServiceUnavailable, "cloudflare_unavailable", "本进程未接入 Cloudflare Tunnel 管理器")
		return false
	}
	return true
}

// cloudflareStatusJSON 是状态读数：管理器的四层状态外加公网接入方式与固定的
// Service URL（界面把它逐字展示给管理员填进 Cloudflare Dashboard）。
type cloudflareStatusJSON struct {
	cloudflared.Status
	Mode       string `json:"mode"`
	ServiceURL string `json:"service_url"`
}

func (s *Server) cloudflareSnapshot(ctx context.Context) cloudflareStatusJSON {
	mode, _ := s.externalMode(ctx)
	return cloudflareStatusJSON{
		Status:     s.cf.Status(ctx),
		Mode:       mode,
		ServiceURL: "unix:" + s.cf.Socket(),
	}
}

func (s *Server) writeCloudflare(w http.ResponseWriter, r *http.Request, status int) {
	writeJSON(w, status, struct {
		Tunnel cloudflareStatusJSON `json:"tunnel"`
	}{Tunnel: s.cloudflareSnapshot(r.Context())})
}

func (s *Server) handleCloudflareStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	s.writeCloudflare(w, r, http.StatusOK)
}

// cloudflareUpdateReq 的三态：字段缺席 = 不改；token 缺席 = 保留已密封的那把。
type cloudflareUpdateReq struct {
	Hostname   *string `json:"hostname"`
	Exposure   *string `json:"exposure"`
	AutoUpdate *bool   `json:"auto_update"`
	Token      *string `json:"token"`
	ClearToken bool    `json:"clear_token"`
}

func (s *Server) handleCloudflareUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req cloudflareUpdateReq
	if !decodeJSON(w, r, &req) {
		return
	}
	change, err := s.cf.UpdateConfig(r.Context(), cloudflared.ConfigPatch{
		Hostname: req.Hostname, Exposure: req.Exposure, AutoUpdate: req.AutoUpdate,
		Token: req.Token, ClearToken: req.ClearToken,
	})
	if change != nil {
		// 落库已成功（可能只是 connector 重启失败）：先记审计。
		s.audit(r.Context(), store.AuditEvent{
			Event: EventCloudflareUpdate, Entity: "system:cloudflare", Detail: cloudflareChangeDetail(change), RemoteIP: remoteIP(r),
		})
	}
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	s.writeCloudflare(w, r, http.StatusOK)
}

func cloudflareChangeDetail(c *cloudflared.ConfigChange) string {
	auto := "关"
	if c.Config.AutoUpdate {
		auto = "开"
	}
	parts := []string{fmt.Sprintf("hostname %s、exposure %s、自动更新 %s", orDash(c.Config.Hostname), c.Config.Exposure, auto)}
	if c.TokenReplaced {
		parts = append(parts, "token 已替换")
	}
	if c.TokenCleared {
		parts = append(parts, "token 已清除")
	}
	if c.Restarted {
		parts = append(parts, "connector 已按新 token 重启")
	}
	return "Cloudflare Tunnel 设置：" + strings.Join(parts, "；")
}

func orDash(v string) string {
	if v == "" {
		return "（空）"
	}
	return v
}

func (s *Server) handleCloudflareEnable(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req struct {
		AcceptThirdParty     bool `json:"accept_third_party"`
		ConfirmAdminExposure bool `json:"confirm_admin_exposure"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.cf.Enable(r.Context(), cloudflared.EnableRequest{
		AcceptThirdParty: req.AcceptThirdParty, ConfirmAdminExposure: req.ConfirmAdminExposure,
	}); err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	// 二选一：Tunnel 起来了，公网接入方式随之切到 cloudflare。
	if err := s.setExternalModeCloudflare(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	}
	cfg, _ := s.cf.Config(r.Context())
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCloudflareEnable, Entity: "system:cloudflare",
		Detail:   fmt.Sprintf("启用 Cloudflare Tunnel：%s（exposure %s）", cfg.Hostname, cfg.Exposure),
		RemoteIP: remoteIP(r),
	})
	s.writeCloudflare(w, r, http.StatusOK)
}

func (s *Server) handleCloudflareDisable(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req struct {
		KeepToken bool `json:"keep_token"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	err := s.cf.Disable(r.Context(), req.KeepToken)
	detail := "停用 Cloudflare Tunnel"
	if req.KeepToken {
		detail += "（保留密封 token）"
	} else {
		detail += "（token 已清除）"
	}
	if err != nil {
		detail += "；部分步骤失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCloudflareDisable, Entity: "system:cloudflare", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	s.writeCloudflare(w, r, http.StatusOK)
}

func (s *Server) handleCloudflareTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req struct {
		Public bool `json:"public"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	rep := s.cf.Test(r.Context(), req.Public)
	outcome := "通过"
	if !rep.OK {
		var failed []string
		for _, c := range rep.Checks {
			if !c.OK {
				failed = append(failed, c.Layer)
			}
		}
		outcome = "未通过（" + strings.Join(failed, "、") + "）"
	}
	scope := "本地自检"
	if req.Public {
		scope = "本地自检 + 公网探测"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCloudflareTest, Entity: "system:cloudflare", Detail: scope + "：" + outcome, RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, struct {
		Report *cloudflared.TestReport `json:"report"`
		Tunnel cloudflareStatusJSON    `json:"tunnel"`
	}{Report: rep, Tunnel: s.cloudflareSnapshot(r.Context())})
}

func (s *Server) handleCloudflareDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req struct {
		RemoveComponent bool `json:"remove_component"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	err := s.cf.Delete(r.Context(), req.RemoveComponent)
	if merr := s.clearExternalModeIfCloudflare(r.Context()); merr != nil {
		err = errors.Join(err, merr)
	}
	detail := "删除本机 Cloudflare Tunnel 配置（token 已销毁）"
	if req.RemoveComponent {
		detail += "，并卸载 cloudflared 组件"
	}
	if err != nil {
		detail += "；部分步骤失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventCloudflareDelete, Entity: "system:cloudflare", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	s.writeCloudflare(w, r, http.StatusOK)
}

// ---- 组件 ----

func (s *Server) writeComponent(w http.ResponseWriter, r *http.Request, status int) {
	writeJSON(w, status, struct {
		Component cloudflared.ComponentStatus `json:"component"`
	}{Component: s.cf.Status(r.Context()).Component})
}

func (s *Server) handleComponentStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	adv, err := s.cf.Check(r.Context())
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: "component:cloudflared", Detail: advisoryDetail(adv, "官网"), RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func advisoryDetail(adv *cloudflared.Advisory, source string) string {
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

func (s *Server) handleComponentManifest(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	var req struct {
		Index     string `json:"index"`
		Signature string `json:"signature"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	adv, err := s.cf.ImportManifest(r.Context(), []byte(req.Index), []byte(req.Signature))
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: "component:cloudflared", Detail: advisoryDetail(adv, "离线导入"), RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	adv := s.cf.Advisory(r.Context())
	if err := s.cf.Download(r.Context()); err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	detail := "下载 cloudflared 官方制品"
	if adv != nil && adv.Latest != nil {
		detail += " " + adv.Latest.Version
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDownload, Entity: "component:cloudflared", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, componentUploadCap+(1<<20))
	staged, err := s.cf.Upload(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "component_too_large", "组件制品超过大小上限")
			return
		}
		s.cloudflareError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentUpload, Entity: "component:cloudflared", Detail: "上传 cloudflared 官方制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	staged, ok := s.cf.StagedManifest()
	if !ok {
		s.cloudflareError(w, r, cloudflared.ErrNoStaged)
		return
	}
	comp, err := s.cf.Install(r.Context())
	detail := "安装 cloudflared " + staged.Version + "（来源 " + staged.Source + "）"
	if err != nil {
		detail += "：失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentInstall, Entity: "component:cloudflared", Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		if comp != nil {
			// 装上了但新版本未就绪、引擎已切回旧版：409 带原因，状态读数照常。
			writeError(w, http.StatusConflict, "component_not_ready", err.Error())
			return
		}
		s.cloudflareError(w, r, err)
		return
	}
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentRollback(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	comp, err := s.cf.Rollback(r.Context())
	if err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	detail := "cloudflared 回退到上一 slot"
	if comp != nil && comp.Current != nil {
		detail += "（" + comp.Current.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRollback, Entity: "component:cloudflared", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	staged, ok := s.cf.StagedManifest()
	if !ok {
		s.writeComponent(w, r, http.StatusOK)
		return
	}
	if err := s.cf.Discard(); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDiscard, Entity: "component:cloudflared", Detail: "丢弃 cloudflared 制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

func (s *Server) handleComponentRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireCloudflare(w) {
		return
	}
	before := s.cf.Status(r.Context()).Component
	if err := s.cf.RemoveComponent(r.Context()); err != nil {
		s.cloudflareError(w, r, err)
		return
	}
	detail := "卸载 cloudflared 组件"
	if before.Installed && before.Version != "" {
		detail += "（" + before.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRemove, Entity: "component:cloudflared", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeComponent(w, r, http.StatusOK)
}

// cloudflareError 把管理器与引擎的哨兵错误映射到状态码与机读 code。错误文本
// 全部面向管理员（zh-CN、不含 token、不含上游原文）。
func (s *Server) cloudflareError(w http.ResponseWriter, r *http.Request, err error) {
	code, status := "cloudflare_error", http.StatusBadRequest
	switch {
	case errors.Is(err, cloudflared.ErrInvalidConfig), errors.Is(err, cloudflared.ErrInvalidToken):
		code = "invalid_cloudflare_config"
	case errors.Is(err, cloudflared.ErrHostnameRequired):
		code, status = "hostname_required", http.StatusConflict
	case errors.Is(err, cloudflared.ErrTokenRequired):
		code, status = "token_required", http.StatusConflict
	case errors.Is(err, cloudflared.ErrTokenUnreadable):
		code, status = "token_unreadable", http.StatusConflict
	case errors.Is(err, cloudflared.ErrTokenInUse):
		code, status = "token_in_use", http.StatusConflict
	case errors.Is(err, cloudflared.ErrComponentMissing):
		code, status = "component_missing", http.StatusConflict
	case errors.Is(err, cloudflared.ErrComponentInUse):
		code, status = "component_in_use", http.StatusConflict
	case errors.Is(err, cloudflared.ErrAdminGate):
		code, status = "default_password", http.StatusConflict
	case errors.Is(err, cloudflared.ErrThirdPartyConsent), errors.Is(err, cloudflared.ErrAdminConsent):
		code, status = "confirmation_required", http.StatusConflict
	case errors.Is(err, cloudflared.ErrNoAdvisory), errors.Is(err, cloudflared.ErrNoManifest):
		code, status = "no_component_offer", http.StatusConflict
	case errors.Is(err, cloudflared.ErrNoStaged):
		code, status = "no_staged_component", http.StatusConflict
	case errors.Is(err, cloudflared.ErrDownloadBusy):
		code, status = "download_busy", http.StatusConflict
	case errors.Is(err, cloudflared.ErrUploadNotListed):
		code, status = "component_not_listed", http.StatusConflict
	case errors.Is(err, cloudflared.ErrManifestStale):
		code, status = "manifest_stale", http.StatusConflict
	case errors.Is(err, elfcheck.ErrNotExecutable):
		code = "invalid_component"
	case errors.Is(err, updated.ErrUnavailable):
		code, status = "engine_unavailable", http.StatusServiceUnavailable
	default:
		var me *cloudflared.ManifestError
		var ee *updated.EngineError
		var he *officialsite.HTTPError
		switch {
		case errors.As(err, &me):
			code = "manifest_rejected"
		case errors.As(err, &ee):
			code, status = "engine_rejected", http.StatusConflict
		case errors.As(err, &he):
			code, status = "website_error", http.StatusBadGateway
		}
	}
	writeError(w, status, code, err.Error())
}
