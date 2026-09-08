package admin

// 固件升级端点（docs/firmware-update.md；全部仅 admin，withSession 默认拒绝
// 已覆盖）：
//
//	GET    /admin/v1/system/firmware          聚合快照（当前版本 / advisory / staged / 引擎）
//	POST   /admin/v1/system/firmware/check    即时读取官网稳定通道索引
//	POST   /admin/v1/system/firmware/download 下载 advisory 的 latest 到本机（
//	                                          synchronous-but-detached，60s 内完成就把结局带回）
//	POST   /admin/v1/system/firmware/upload   手动上传固件包（body 为 application/octet-stream
//	                                          裸字节——withCSRF 对这一条路径放行非 JSON，CSRF 头照旧）
//	POST   /admin/v1/system/firmware/install  请求升级引擎安装已就绪的固件包（202 受理；
//	                                          随后引擎会停掉本进程，页面短暂不可用）
//	POST   /admin/v1/system/firmware/rollback 请求引擎回退到上一版本
//	DELETE /admin/v1/system/firmware/staged   丢弃已就绪的固件包
//
// 边界：在线检查/下载匿名读取 LLM Gate官网；手动上传恒可用，官网不是运行依赖。
// 引擎 socket 不可达答 503 engine_unavailable，提示按 docs-board/deployment.md
// 手工替换——那条老路是引擎自身出问题时的兜底，不废除。
//
// §15.1：审计 detail 只带版本号与来源，不带 URL 与摘要；固件包内容不进任何
// 日志。升级链路没有设备身份或密钥物料过手。

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/buildinfo"
	"github.com/llm-net/llm-gate/firmware/internal/fwimage"
	"github.com/llm-net/llm-gate/firmware/internal/officialsite"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/update"
	"github.com/llm-net/llm-gate/firmware/internal/updated"
)

// 审计事件名。
const (
	EventFirmwareCheck    = "system.firmware_check"
	EventFirmwareDownload = "system.firmware_download"
	EventFirmwareUpload   = "system.firmware_upload"
	EventFirmwareInstall  = "system.firmware_install"
	EventFirmwareRollback = "system.firmware_rollback"
	EventFirmwareDiscard  = "system.firmware_discard"
)

// firmwareUploadCap 是手动上传的请求体上限（与 update 包的读上限同数量级）。
const firmwareUploadCap = 256 << 20

// FirmwareUploadPath 是 withCSRF 放行 octet-stream 的那条路径（中间件与
// 路由表共用同一常量，防两处漂移）。
const FirmwareUploadPath = "/admin/v1/system/firmware/upload"

// SetFirmwareUpdater 注入升级管理器（gatewayd 装配期调用一次）。不注入时
// 固件升级端点答 503 firmware_unavailable（只可能出现在测试路径）。
func (s *Server) SetFirmwareUpdater(m *update.Manager) { s.fw = m }

// firmwareEngineJSON 是引擎状态的界面读法。
type firmwareEngineJSON struct {
	Available        bool            `json:"available"`
	Busy             bool            `json:"busy,omitempty"`
	Phase            string          `json:"phase,omitempty"`
	InstalledVersion string          `json:"installed_version,omitempty"`
	PrevVersion      string          `json:"prev_version,omitempty"`
	LastResult       *updated.Result `json:"last_result,omitempty"`
}

// firmwareStatusJSON 是聚合快照。current_version 来自 buildinfo，是固件唯一的
// 版本名称——界面上的「固件版本」、二进制自述标记、官网索引里的 version 全是
// 这一串。
type firmwareStatusJSON struct {
	CurrentVersion string                 `json:"current_version"`
	Advisory       *update.Advisory       `json:"advisory,omitempty"`
	Downloading    bool                   `json:"downloading"`
	DownloadError  string                 `json:"download_error,omitempty"`
	Staged         *update.StagedManifest `json:"staged,omitempty"`
	Engine         firmwareEngineJSON     `json:"engine"`
}

// requireFirmware 是未注入升级管理器时的统一拒绝。
func (s *Server) requireFirmware(w http.ResponseWriter) bool {
	if s.fw == nil {
		writeError(w, http.StatusServiceUnavailable, "firmware_unavailable", "本进程未接入升级管理器")
		return false
	}
	return true
}

// firmwareSnapshot 组装聚合快照；引擎读数带 2s 预算（socket 不可达要快速
// 降级成 available:false，不能拖住整页）。
func (s *Server) firmwareSnapshot(ctx context.Context) firmwareStatusJSON {
	out := firmwareStatusJSON{
		CurrentVersion: buildinfo.Version,
		Advisory:       s.fw.Advisory(),
	}
	out.Downloading, out.DownloadError = s.fw.Downloading()

	ectx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if st, err := s.fw.EngineStatus(ectx); err == nil {
		// 上次安装成功装出去的 staged 包看到即清（引擎不动本侧目录）。
		s.fw.Reconcile(st)
		out.Engine = firmwareEngineJSON{
			Available:        true,
			Busy:             st.Busy,
			Phase:            st.Phase,
			InstalledVersion: st.InstalledVersion,
			LastResult:       st.LastResult,
		}
		if st.Prev != nil {
			out.Engine.PrevVersion = st.Prev.Version
		}
	}
	if man, ok := s.fw.Staged(); ok {
		out.Staged = man
	}
	return out
}

func (s *Server) handleFirmwareStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareCheck(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	adv, err := s.fw.Check(r.Context())
	if err != nil {
		s.firmwareError(w, r, err)
		return
	}
	detail := "官网没有可升级版本"
	if adv.UpgradeAvailable && adv.Latest != nil {
		detail = "官网可升级版本 " + adv.Latest.Version
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareCheck, Entity: "system:firmware", Detail: detail,
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	adv := s.fw.Advisory()
	if err := s.fw.Download(r.Context()); err != nil {
		s.firmwareError(w, r, err)
		return
	}
	detail := "下载固件包"
	if adv != nil && adv.Latest != nil {
		detail = "下载固件包 " + adv.Latest.Version
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareDownload, Entity: "system:firmware", Detail: detail,
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, firmwareUploadCap+(1<<20))
	man, err := s.fw.Upload(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "firmware_too_large", "固件包超过大小上限")
			return
		}
		s.firmwareError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareUpload, Entity: "system:firmware",
		Detail:   "上传固件包 " + man.Version,
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	man, ok := s.fw.Staged()
	if !ok {
		s.firmwareError(w, r, update.ErrNoStaged)
		return
	}
	// 审计先行：引擎受理后 ~1.5s 就会停掉本进程，此后没有落审计的机会。
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareInstall, Entity: "system:firmware",
		Detail:   "安装固件 " + man.Version + "（来源 " + man.Source + "）",
		RemoteIP: remoteIP(r),
	})
	if _, err := s.fw.Install(r.Context()); err != nil {
		s.firmwareError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareRollback(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareRollback, Entity: "system:firmware",
		Detail:   "回退到上一版本",
		RemoteIP: remoteIP(r),
	})
	if _, err := s.fw.Rollback(r.Context()); err != nil {
		s.firmwareError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.firmwareSnapshot(r.Context()))
}

func (s *Server) handleFirmwareDiscard(w http.ResponseWriter, r *http.Request) {
	if !s.requireFirmware(w) {
		return
	}
	man, ok := s.fw.Staged()
	if !ok {
		writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
		return
	}
	if err := s.fw.Discard(); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: EventFirmwareDiscard, Entity: "system:firmware",
		Detail:   "丢弃固件包 " + man.Version,
		RemoteIP: remoteIP(r),
	})
	writeJSON(w, http.StatusOK, s.firmwareSnapshot(r.Context()))
}

// firmwareError 把升级链路的哨兵错误映射到状态码。
func (s *Server) firmwareError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, updated.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "engine_unavailable",
			"升级引擎不可用（llmgate-updated 服务未运行）；可按部署文档手工替换二进制")
	case errors.Is(err, updated.ErrBusy):
		writeError(w, http.StatusConflict, "engine_busy", "已有一次安装或回退正在进行")
	case errors.Is(err, update.ErrDownloadBusy):
		writeError(w, http.StatusConflict, "download_busy", "固件包正在下载中")
	case errors.Is(err, update.ErrNoOffer):
		writeError(w, http.StatusConflict, "no_update_offer", "当前没有可下载的新版本，请先检查更新")
	case errors.Is(err, update.ErrNoStaged):
		writeError(w, http.StatusConflict, "no_staged_firmware", "没有已就绪的固件包")
	case errors.Is(err, update.ErrNoVersion), errors.Is(err, fwimage.ErrNotFirmware):
		writeError(w, http.StatusBadRequest, "invalid_firmware", err.Error())
	default:
		var ce *officialsite.HTTPError
		if errors.As(err, &ce) {
			writeError(w, http.StatusBadGateway, "website_error", err.Error())
			return
		}
		// 下载/staging 的其余失败是面向管理员的一句话（摘要不符、包为空…）。
		writeError(w, http.StatusBadRequest, "firmware_error", err.Error())
	}
}
