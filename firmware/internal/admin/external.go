package admin

// 网络/域名/代理——公网接入方式（仅管理员）：
//
//	GET /admin/v1/system/external   读当前方式与两套设置
//	PUT /admin/v1/system/external   选择方式并保存外网映射地址
//
// 设备对外公布的公网基址只有两种来源，**二选一**（external_access.mode）：
//
//   - manual（外网映射）：管理员在自己的网络边界（反向代理、端口映射、DDNS 等）
//     为这台设备配置的公开基址。设备不建立、不探测也不维护映射，只记录管理员
//     确认可用的地址（external_access.manual_url）。
//   - cloudflare（Cloudflare Tunnel）：设备经 cloudflared 主动连出到用户自己的
//     Cloudflare 账号，公网基址恒为 https://<hostname>（cloudflare.go）。
//
// 两种设置分别保留，切换时不要求用户重复输入；生效的地址由 mode 派生，随
// GET /admin/v1/endpoints 返回，供「API调用」和「开发工具接入」生成设备地址。
// Tunnel 启用中不允许把方式切走：先停用 Tunnel，再改方式。
//
// 地址是管理员主动公布的访问地址，不是凭据；但仍拒绝 userinfo、查询串和片段，
// 避免把口令或一次性参数误存进设置、审计与接入指引（§15.1）。

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	settingExternalMode      = "external_access.mode"
	settingExternalManualURL = "external_access.manual_url"

	ExternalModeNone       = "none"
	ExternalModeManual     = "manual"
	ExternalModeCloudflare = "cloudflare"

	EventExternalUpdate = "system.external_update"
	maxExternalURLLen   = 200
)

// externalAccessJSON 是公网接入方式的读数。ExternalURL 是按 mode 派生的当前生效
// 基址（none 或 Tunnel 未启用时缺席）。
type externalAccessJSON struct {
	Mode        string `json:"mode"`
	ManualURL   string `json:"manual_url"`
	ExternalURL string `json:"external_url,omitempty"`
	// CloudflareEnabled 报告 Tunnel 是否已启用：界面据此知道「切走方式」会被拒。
	CloudflareEnabled bool `json:"cloudflare_enabled"`
}

// normalizeExternalURL 校验并规范化外网映射基址：scheme 与 host 转小写，去掉
// 路径末尾斜杠；可保留反向代理使用的路径前缀。客户端会在结果后直接拼接 API
// 路径，因此查询串和片段没有合法语义。
func normalizeExternalURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxExternalURLLen {
		return "", fmt.Errorf("地址过长（上限 %d 字符）", maxExternalURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("不是合法的网址")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("缺少主机名，例如 https://device.example.com")
	}
	if u.User != nil {
		return "", fmt.Errorf("不能包含用户名或口令")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("请只填基址，不要带 ? 查询串或 # 片段")
	}
	if u.RawPath != "" {
		return "", fmt.Errorf("路径包含无法安全规范化的转义字符")
	}
	host := strings.ToLower(u.Host)
	path := strings.TrimRight(u.EscapedPath(), "/")
	return scheme + "://" + host + path, nil
}

// externalMode 读当前方式；从未设置视为 none。
func (s *Server) externalMode(ctx context.Context) (string, error) {
	mode, err := s.st.GetSetting(ctx, settingExternalMode)
	if err != nil {
		return "", err
	}
	switch mode {
	case ExternalModeManual, ExternalModeCloudflare:
		return mode, nil
	}
	return ExternalModeNone, nil
}

// cloudflareEnabled 报告 Tunnel 是否已启用（管理器未注入时恒为 false）。
func (s *Server) cloudflareEnabled(ctx context.Context) bool {
	if s.cf == nil {
		return false
	}
	enabled, err := s.cf.Enabled(ctx)
	return err == nil && enabled
}

// effectiveExternalURL 按 mode 派生当前生效的公网基址。读数失败只降级为空串
// ——接入指引不能因为一项设置读不出而整页打不开。
func (s *Server) effectiveExternalURL(ctx context.Context) string {
	mode, err := s.externalMode(ctx)
	if err != nil {
		s.log.Warn("读取公网接入方式失败，接入地址暂不含它", "err", err.Error())
		return ""
	}
	switch mode {
	case ExternalModeManual:
		u, err := s.st.GetSetting(ctx, settingExternalManualURL)
		if err != nil {
			s.log.Warn("读取外网映射地址失败，接入地址暂不含它", "err", err.Error())
			return ""
		}
		return u
	case ExternalModeCloudflare:
		if !s.cloudflareEnabled(ctx) {
			return ""
		}
		cfg, err := s.cf.Config(ctx)
		if err != nil {
			return ""
		}
		return cfg.ExternalURL()
	}
	return ""
}

func (s *Server) externalAccess(ctx context.Context) (externalAccessJSON, error) {
	mode, err := s.externalMode(ctx)
	if err != nil {
		return externalAccessJSON{}, err
	}
	manual, err := s.st.GetSetting(ctx, settingExternalManualURL)
	if err != nil {
		return externalAccessJSON{}, err
	}
	return externalAccessJSON{
		Mode:              mode,
		ManualURL:         manual,
		ExternalURL:       s.effectiveExternalURL(ctx),
		CloudflareEnabled: s.cloudflareEnabled(ctx),
	}, nil
}

func (s *Server) writeExternal(w http.ResponseWriter, r *http.Request, status int) {
	ext, err := s.externalAccess(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, status, struct {
		External externalAccessJSON `json:"external"`
	}{External: ext})
}

func (s *Server) handleGetExternal(w http.ResponseWriter, r *http.Request) {
	s.writeExternal(w, r, http.StatusOK)
}

// handleSetExternal 选择方式并保存外网映射地址。manual 模式要求地址非空；
// 任何模式下都保存 manual_url（空串即清除），切换时不丢设置。Tunnel 启用中
// 只能保持 cloudflare 模式。
func (s *Server) handleSetExternal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode      string `json:"mode"`
		ManualURL string `json:"manual_url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Mode {
	case ExternalModeNone, ExternalModeManual, ExternalModeCloudflare:
	default:
		writeError(w, http.StatusBadRequest, "invalid_external_mode", "公网接入方式只能是 none、manual 或 cloudflare")
		return
	}
	normalized, err := normalizeExternalURL(req.ManualURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_external_url", "外网映射地址无效："+err.Error())
		return
	}
	if req.Mode == ExternalModeManual && normalized == "" {
		writeError(w, http.StatusBadRequest, "invalid_external_url", "选择外网映射时必须填写已验证可用的外部地址")
		return
	}
	if req.Mode != ExternalModeCloudflare && s.cloudflareEnabled(r.Context()) {
		writeError(w, http.StatusConflict, "cloudflare_tunnel_active", "Cloudflare Tunnel 正在启用中：先停用 Tunnel，再切换公网接入方式")
		return
	}
	if err := s.st.SetSetting(r.Context(), settingExternalManualURL, normalized); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.st.SetSetting(r.Context(), settingExternalMode, req.Mode); err != nil {
		s.internalError(w, r, err)
		return
	}
	detail := externalAuditDetail(req.Mode, normalized)
	s.audit(r.Context(), store.AuditEvent{
		Event: EventExternalUpdate, Entity: "system:external", Detail: detail, RemoteIP: remoteIP(r),
	})
	s.writeExternal(w, r, http.StatusOK)
}

func externalAuditDetail(mode, manualURL string) string {
	switch mode {
	case ExternalModeManual:
		return "公网接入改为外网映射：" + manualURL
	case ExternalModeCloudflare:
		return "公网接入改为 Cloudflare Tunnel"
	}
	if manualURL != "" {
		return "关闭公网接入（保留外网映射地址 " + manualURL + "）"
	}
	return "关闭公网接入"
}

// setExternalModeCloudflare 是 Tunnel 启用成功后的收口：公网接入方式随之切到
// cloudflare（二选一）。
func (s *Server) setExternalModeCloudflare(ctx context.Context) error {
	return s.st.SetSetting(ctx, settingExternalMode, ExternalModeCloudflare)
}

// clearExternalModeIfCloudflare 是删除本机 Tunnel 配置后的收口：方式回到 none。
func (s *Server) clearExternalModeIfCloudflare(ctx context.Context) error {
	mode, err := s.externalMode(ctx)
	if err != nil {
		return err
	}
	if mode != ExternalModeCloudflare {
		return nil
	}
	return s.st.SetSetting(ctx, settingExternalMode, ExternalModeNone)
}
