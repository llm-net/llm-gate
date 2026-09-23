package admin

// Codex App Server 组件（internal/codexappserver）——「组件管理」页的第三张卡：
//
//	GET    /admin/v1/system/components/codex-app-server           组件读数
//	POST   /admin/v1/system/components/codex-app-server/check     匿名取官网签名清单             （LAN）
//	POST   /admin/v1/system/components/codex-app-server/manifest  离线导入清单与签名             （LAN）
//	POST   /admin/v1/system/components/codex-app-server/download  从官方 release 下载并就绪       （LAN）
//	POST   /admin/v1/system/components/codex-app-server/upload    上传官方 tar.gz（octet-stream） （LAN）
//	POST   /admin/v1/system/components/codex-app-server/install   把就绪制品装进非活动 slot       （LAN）
//	POST   /admin/v1/system/components/codex-app-server/rollback  切回上一 slot                  （LAN）
//	POST   /admin/v1/system/components/codex-app-server/selfcheck 拉起一次做握手自检（无凭据）    （LAN）
//	DELETE /admin/v1/system/components/codex-app-server/staged    丢弃就绪制品                   （LAN）
//	DELETE /admin/v1/system/components/codex-app-server           卸载组件                       （LAN）
//
// 该组件没有常驻 unit、没有「启用中」的功能开关：安装/回退只切 current 软链。唯一的 409
// component_in_use 来自本进程正在运行的 app-server 实例（codexappserver.Manager.Launch）——
// 安装与卸载会整目录删槽位，运行中的实例随后就找不到 code-mode host。审计只记动作、版本、
// 来源与结果类别（§15.1）；自检读数不含凭据，也不发模型请求。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// CodexAppServerUploadPath 是 withCSRF 放行 octet-stream 的制品上传路径。
const CodexAppServerUploadPath = "/admin/v1/system/components/codex-app-server/upload"

const codexAppServerEntity = "component:codex-app-server"

// SetCodexAppServer 注入组件管理器（gatewayd 装配期调用一次）。
func (s *Server) SetCodexAppServer(m *codexappserver.Manager) { s.codexAppServer = m }

func (s *Server) requireCodexAppServer(w http.ResponseWriter) bool {
	if s.codexAppServer == nil {
		writeError(w, http.StatusServiceUnavailable, "codex_app_server_unavailable", "本进程未接入 Codex App Server 组件管理器")
		return false
	}
	return true
}

func (s *Server) writeCodexAppServer(w http.ResponseWriter, r *http.Request, status int) {
	writeJSON(w, status, struct {
		Component codexappserver.ComponentStatus `json:"component"`
	}{Component: s.codexAppServer.Status(r.Context())})
}

func (s *Server) handleCodexAppServerStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func codexAppServerAdvisoryDetail(adv *codexappserver.Advisory, source string) string {
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

func (s *Server) handleCodexAppServerCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	adv, err := s.codexAppServer.Check(r.Context())
	if err != nil {
		s.codexAppServerError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: codexAppServerEntity, Detail: codexAppServerAdvisoryDetail(adv, "官网"), RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerManifest(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	var req struct {
		Index     string `json:"index"`
		Signature string `json:"signature"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	adv, err := s.codexAppServer.ImportManifest(r.Context(), []byte(req.Index), []byte(req.Signature))
	if err != nil {
		s.codexAppServerError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentCheck, Entity: codexAppServerEntity, Detail: codexAppServerAdvisoryDetail(adv, "离线导入"), RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	adv := s.codexAppServer.Advisory(r.Context())
	if err := s.codexAppServer.Download(r.Context()); err != nil {
		s.codexAppServerError(w, r, err)
		return
	}
	detail := "下载 Codex App Server 官方制品"
	if adv != nil && adv.Latest != nil {
		detail += " " + adv.Latest.Version
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDownload, Entity: codexAppServerEntity, Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, codexappserver.UploadCap+(1<<20))
	staged, err := s.codexAppServer.Upload(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "component_too_large", "组件制品超过大小上限")
			return
		}
		s.codexAppServerError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentUpload, Entity: codexAppServerEntity, Detail: "上传 Codex App Server 官方制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	staged, ok := s.codexAppServer.StagedManifest()
	if !ok {
		s.codexAppServerError(w, r, codexappserver.ErrNoStaged)
		return
	}
	comp, err := s.codexAppServer.Install(r.Context())
	detail := "安装 Codex App Server " + staged.Version + "（来源 " + staged.Source + "）"
	if err != nil {
		detail += "：失败"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentInstall, Entity: codexAppServerEntity, Detail: detail, RemoteIP: remoteIP(r),
	})
	if err != nil {
		if comp != nil {
			writeError(w, http.StatusConflict, "component_not_ready", err.Error())
			return
		}
		s.codexAppServerError(w, r, err)
		return
	}
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerRollback(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	comp, err := s.codexAppServer.Rollback(r.Context())
	if err != nil {
		s.codexAppServerError(w, r, err)
		return
	}
	detail := "Codex App Server 回退到上一 slot"
	if comp != nil && comp.Current != nil {
		detail += "（" + comp.Current.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRollback, Entity: codexAppServerEntity, Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	staged, ok := s.codexAppServer.StagedManifest()
	if !ok {
		s.writeCodexAppServer(w, r, http.StatusOK)
		return
	}
	if err := s.codexAppServer.Discard(); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentDiscard, Entity: codexAppServerEntity, Detail: "丢弃 Codex App Server 制品 " + staged.Version, RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

func (s *Server) handleCodexAppServerRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	before := s.codexAppServer.Status(r.Context())
	if err := s.codexAppServer.Remove(r.Context()); err != nil {
		s.codexAppServerError(w, r, err)
		return
	}
	detail := "卸载 Codex App Server 组件"
	if before.Installed && before.Version != "" {
		detail += "（" + before.Version + "）"
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentRemove, Entity: codexAppServerEntity, Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeCodexAppServer(w, r, http.StatusOK)
}

// EventComponentSelfCheck 是组件自检的审计事件。
const EventComponentSelfCheck = "system.component_selfcheck"

// codexAppServerSelfCheckTimeout 是一次自检的总预算（入口 --version、捆绑 bwrap、拉起与握手）。
const codexAppServerSelfCheckTimeout = 60 * time.Second

func (s *Server) handleCodexAppServerSelfCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireCodexAppServer(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), codexAppServerSelfCheckTimeout)
	defer cancel()
	res := s.codexAppServer.SelfCheck(ctx)
	detail := "Codex App Server 自检：失败"
	if res.OK {
		detail = fmt.Sprintf("Codex App Server 自检：通过（%s）", res.Version)
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventComponentSelfCheck, Entity: codexAppServerEntity, Detail: detail, RemoteIP: remoteIP(r),
	})
	status := http.StatusOK
	if !res.OK && strings.Contains(res.Error, codexappserver.ErrComponentMissing.Error()) {
		status = http.StatusConflict
	}
	writeJSON(w, status, struct {
		SelfCheck codexappserver.SelfCheckResult `json:"selfcheck"`
		Component codexappserver.ComponentStatus `json:"component"`
	}{SelfCheck: res, Component: s.codexAppServer.Status(r.Context())})
}

// codexAppServerError 把管理器、官网客户端与引擎的哨兵错误映射到状态码与机读 code。官网答非
// 2xx（清单尚未发布 = 404）或连不上都是 502 website_error，不是内部错误。
func (s *Server) codexAppServerError(w http.ResponseWriter, r *http.Request, err error) {
	var me *codexappserver.ManifestError
	var ee *updated.EngineError
	var he *officialsite.HTTPError
	switch {
	case errors.As(err, &he):
		writeError(w, http.StatusBadGateway, "website_error", err.Error())
	case strings.HasPrefix(err.Error(), "无法连接 LLM Gate官网"), strings.HasPrefix(err.Error(), "连接 LLM Gate官网超时"):
		writeError(w, http.StatusBadGateway, "website_error", err.Error())
	case errors.As(err, &me), errors.Is(err, codexappserver.ErrManifestStale):
		writeError(w, http.StatusBadRequest, "component_manifest_rejected", err.Error())
	case errors.Is(err, codexappserver.ErrNoManifest), errors.Is(err, codexappserver.ErrNoAdvisory), errors.Is(err, codexappserver.ErrNoStaged):
		writeError(w, http.StatusConflict, "component_not_ready", err.Error())
	case errors.Is(err, codexappserver.ErrDownloadBusy):
		writeError(w, http.StatusConflict, "component_download_busy", err.Error())
	case errors.Is(err, codexappserver.ErrUploadNotListed):
		writeError(w, http.StatusBadRequest, "component_not_listed", err.Error())
	case errors.Is(err, codexappserver.ErrComponentMissing):
		writeError(w, http.StatusConflict, "component_missing", err.Error())
	case errors.Is(err, codexappserver.ErrComponentInUse):
		writeError(w, http.StatusConflict, "component_in_use", err.Error())
	case errors.Is(err, updated.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable", "升级引擎不可达，无法操作组件")
	case errors.As(err, &ee):
		status := http.StatusBadRequest
		if ee.Status == http.StatusConflict {
			status = http.StatusConflict
		}
		writeError(w, status, "engine_rejected", ee.Message)
	default:
		s.internalError(w, r, err)
	}
}
