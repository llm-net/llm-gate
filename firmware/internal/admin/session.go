package admin

// 会话端点：login、logout、会话探针 GET /admin/v1/session，以及改密
// POST /admin/v1/password。
//
// 设备只有一个管理员、一个登录口令（出厂 auth.DefaultPassword，
// auth.Service.EnsureDefaultPassword 在启动期播种），管理台第一屏就是登录页，
// 不存在额外的初始化端点。会话 Cookie 语义（决策 3）集中在本文件；令牌明文
// 只在 Set-Cookie 里出现，绝不落日志、落审计 detail。

import (
	"errors"
	"net/http"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/auth"
	"github.com/llm-net/llm-gate/firmware/internal/store"
	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// setSessionCookie 下发会话 Cookie。HTTP 与客户自管 HTTPS 共用 handler；只有
// 实际经 TLS 到达设备、或经可信 Cloudflare Tunnel 入口（闸门只放行单值
// X-Forwarded-Proto: https）的请求才加 Secure——判据是 tunnelctx.Secure，普通
// listener 上的转发头一概不信。
func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(auth.SessionTTL / time.Second),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

// clearSessionCookie 让浏览器丢弃会话 Cookie（登出/会话轮换失败兜底）。
func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
	})
}

// handleLogin 登录：只验一个口令（防爆破退避与审计均在 auth.Login 内）。
// 成功回 204——没有「我是谁」可回，会话本身就是全部答复。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.auth.Login(r.Context(), req.Password, remoteIP(r))
	if err != nil {
		s.writeAuthError(w, r, err)
		return
	}
	setSessionCookie(w, res.Token, tunnelctx.Secure(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout 删除当前会话并清除浏览器 Cookie（logout 审计在 auth.Logout 内）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil { // 位于会话中间件之后，正常不可达；防御分支
		writeError(w, http.StatusUnauthorized, "unauthorized", "未登录或会话已过期")
		return
	}
	if err := s.auth.Logout(r.Context(), c.Value, remoteIP(r)); err != nil {
		s.internalError(w, r, err)
		return
	}
	clearSessionCookie(w, tunnelctx.Secure(r))
	w.WriteHeader(http.StatusNoContent)
}

// handleSession 是界面 boot 的会话探针：走到这里就说明会话中间件已经放行，
// 直接 204；无效会话在中间件那层就已是 401。**刻意不回任何 body**——设备
// 没有「当前用户」这种东西可以回答了。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handlePassword 改登录口令：验旧口令 → 设新口令（auth.SetPassword 清空全部
// 旧会话）→ 为当前浏览器轮换新会话。「改密使全部旧会话失效」与「当前浏览器
// 无感续用」由轮换同时满足。
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ok, err := s.auth.VerifyCurrent(r.Context(), req.OldPassword)
	if err != nil { // 库中哈希串损坏属服务端事故
		s.internalError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_old_password", "旧密码错误")
		return
	}
	if err := s.auth.SetPassword(r.Context(), req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrPasswordPolicy) {
			s.writeAuthError(w, r, err)
		} else {
			s.internalError(w, r, err)
		}
		return
	}
	s.audit(r.Context(), store.AuditEvent{
		Event: auth.EventPasswordChange, Entity: auth.EntityDevicePassword(),
		Detail: "已修改登录口令，全部旧会话已失效", RemoteIP: remoteIP(r),
	})
	token, err := s.auth.IssueSession(r.Context(), remoteIP(r))
	if err != nil {
		// 改密已成功、旧会话已删；轮换失败退化为重新登录，不当作请求失败。
		s.log.Error("改密后会话轮换失败，需重新登录", "error", err.Error())
		clearSessionCookie(w, tunnelctx.Secure(r))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	setSessionCookie(w, token, tunnelctx.Secure(r))
	w.WriteHeader(http.StatusNoContent)
}
