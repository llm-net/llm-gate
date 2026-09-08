package gateway

// Cloudflare Tunnel 专用可信入口（docs-dev/firmware-cloudflare-tunnel.md §4.1、§4.2、§4.3）。
//
// gatewayd 的第二个 listener：/run/llmgate/cloudflare-origin.sock（0660，属组
// llmgate-tunnel）。它只在 Tunnel 启用时创建、停用后关闭并移除；cloudflared 以
// HTTP over Unix socket 回源到这里。请求进入业务路由前经闸门：
//
//  1. Host 必须与设备保存的规范化 public hostname 完全一致（拒绝空、端口、IP、
//     wildcard、尾点）；
//  2. X-Forwarded-Proto 必须是单值 https，否则拒绝——普通 listener 永远不读它；
//  3. 只有这条入口读取单值 CF-Connecting-IP（netip 严格解析），缺失或非法记
//     tunnelctx.UnknownIP，不回退读 X-Forwarded-For；
//  4. 进入 handler 前删除 X-Forwarded-For / X-Real-IP / Forwarded 等未采用的代理头；
//  5. 路由按 profile 的显式 allowlist 判定，未列出一律 404；Admin 档在管理员口令
//     仍是出厂缺省值时同样 404（管理面暴露自动收窄为 api_only）；
//  6. 通过后把不可由 HTTP 头构造的 tunnelctx.Info 写进 context，再交给与 LAN
//     listener 共用的根 handler——管理面据此给会话 Cookie 加 Secure、审计与登录
//     退避用它记客户端 IP。
//
// LAN/TLS listener 上伪造这些头一概无效：它们的请求永远不经过本闸门。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/tunnelctx"
)

// tunnelListener 是在飞的 Tunnel origin socket。
type tunnelListener struct {
	socket string
	ln     net.Listener
	hs     *http.Server
	done   chan struct{}
}

// strippedProxyHeaders 是闸门在进入业务路由前删掉的头：未采用的代理头不能让
// 业务代码各自解释；CF-Connecting-IP 已折进 context，也不再以头的形态存在。
var strippedProxyHeaders = []string{
	"X-Forwarded-For", "X-Real-IP", "Forwarded", "True-Client-IP",
	"X-Forwarded-Host", "X-Forwarded-Port", "CF-Connecting-IP", "CF-Connecting-IPv6",
}

// SetTunnelAdminGate 注入「此刻允不允许经 Tunnel 暴露 Admin 档路由」的判定
// （生产：管理员口令不是出厂缺省值）。未注入时恒不放行。
func (s *Server) SetTunnelAdminGate(gate func() bool) {
	if gate == nil {
		return
	}
	s.tunnelGate.Store(&gate)
}

func (s *Server) tunnelAdminAllowed() bool {
	gate := s.tunnelGate.Load()
	return gate != nil && (*gate)()
}

// StartTunnel 建立 origin socket 并开始服务；已在监听时只更新策略。socket 文件
// 0660，属组尽力 chown 成 group（系统里没有该组——dev 机——则跳过，只记一条警告）。
func (s *Server) StartTunnel(socket, group string, pol tunnelctx.Policy) error {
	if socket == "" {
		return errors.New("缺少 Tunnel socket 路径")
	}
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	p := pol
	s.tunnelPolicy.Store(&p)
	if s.tunnel != nil {
		if s.tunnel.socket == socket {
			return nil
		}
		s.stopTunnelLocked()
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		ln.Close()
		return err
	}
	if group != "" {
		if g, err := user.LookupGroup(group); err == nil {
			if gid, err := strconv.Atoi(g.Gid); err == nil {
				if err := os.Chown(socket, -1, gid); err != nil {
					s.log.Warn("设置 Tunnel socket 属组失败（gatewayd 需在该组内）", "group", group, "err", err.Error())
				}
			}
		} else {
			s.log.Warn("系统里没有 Tunnel socket 属组，跳过 chown", "group", group)
		}
	}
	hs := &http.Server{
		Handler:           s.tunnelHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	t := &tunnelListener{socket: socket, ln: ln, hs: hs, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("Tunnel origin 服务异常退出", "err", err.Error())
		}
	}()
	s.tunnel = t
	s.log.Info("Tunnel origin socket 已监听", "socket", socket, "hostname", pol.Hostname, "profile", string(pol.Profile))
	return nil
}

// StopTunnel 停止服务、关闭并移除 socket 文件；未在监听时无事发生。
func (s *Server) StopTunnel() error {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	return s.stopTunnelLocked()
}

func (s *Server) stopTunnelLocked() error {
	t := s.tunnel
	if t == nil {
		return nil
	}
	s.tunnel = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.hs.Shutdown(ctx); err != nil {
		t.hs.Close()
	}
	<-t.done
	if err := os.Remove(t.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.log.Info("Tunnel origin socket 已关闭", "socket", t.socket)
	return nil
}

// UpdateTunnelPolicy 原子替换策略快照（hostname / profile）；下一个请求即生效。
func (s *Server) UpdateTunnelPolicy(pol tunnelctx.Policy) {
	p := pol
	s.tunnelPolicy.Store(&p)
}

// TunnelListening 报告 origin socket 是否在服务。
func (s *Server) TunnelListening() bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	return s.tunnel != nil
}

// tunnelProbe 是一次公网探测的登记：BeginTunnelProbe 登记 nonce，闸门在 socket 上
// 见到带 tunnelctx.ProbeHeader 的同名 nonce 时标记 seen，EndTunnelProbe 取走结果。
type tunnelProbe struct {
	seen bool
	at   time.Time
}

// tunnelProbeTTL 是登记的有效期：探测超时后残留的登记由下一次 Begin 清掉。
const tunnelProbeTTL = 2 * time.Minute

// BeginTunnelProbe 登记一个探测 nonce 并返回它。管理器把它放进 tunnelctx.ProbeHeader
// 打到公网 hostname；请求若真经 origin socket 回到本机，闸门会标记它。只有登记过
// 的 nonce 才会被记录——公网上任何人塞这个头都不会撑大登记表。取不到随机数时
// 返回空串，管理器据此跳过回执判定。
func (s *Server) BeginTunnelProbe() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(b[:])
	now := time.Now()
	s.tunnelProbeMu.Lock()
	defer s.tunnelProbeMu.Unlock()
	if s.tunnelProbes == nil {
		s.tunnelProbes = map[string]tunnelProbe{}
	}
	for k, p := range s.tunnelProbes {
		if now.Sub(p.at) > tunnelProbeTTL {
			delete(s.tunnelProbes, k)
		}
	}
	s.tunnelProbes[nonce] = tunnelProbe{at: now}
	return nonce
}

// EndTunnelProbe 取走登记并报告闸门有没有在 origin socket 上见过这个 nonce。
// 没登记过、已取走或没到达的 nonce 一律为假。
func (s *Server) EndTunnelProbe(nonce string) bool {
	s.tunnelProbeMu.Lock()
	defer s.tunnelProbeMu.Unlock()
	p, ok := s.tunnelProbes[nonce]
	delete(s.tunnelProbes, nonce)
	return ok && p.seen
}

func (s *Server) markTunnelProbe(nonce string) {
	s.tunnelProbeMu.Lock()
	defer s.tunnelProbeMu.Unlock()
	if p, ok := s.tunnelProbes[nonce]; ok {
		p.seen = true
		s.tunnelProbes[nonce] = p
	}
}

// tunnelHandler 是闸门本体（见文件头六条）。
func (s *Server) tunnelHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 公网探测的回执：请求到了这个 socket 就算数（后面 Host/档位怎么判与它无关），
		// 随即剥离，不让业务路由看到。
		if nonce := r.Header.Get(tunnelctx.ProbeHeader); nonce != "" {
			s.markTunnelProbe(nonce)
			r.Header.Del(tunnelctx.ProbeHeader)
		}
		pol := s.tunnelPolicy.Load()
		if pol == nil || pol.Hostname == "" || !pol.Profile.Valid() {
			writeTunnelError(w, http.StatusServiceUnavailable, "tunnel_not_configured", "Tunnel policy is not configured.")
			return
		}
		if !strings.EqualFold(strings.TrimSpace(r.Host), pol.Hostname) {
			writeTunnelError(w, http.StatusMisdirectedRequest, "host_mismatch", "This hostname is not served by this device.")
			return
		}
		if proto := r.Header.Values("X-Forwarded-Proto"); len(proto) != 1 || strings.ToLower(strings.TrimSpace(proto[0])) != "https" {
			writeTunnelError(w, http.StatusBadRequest, "https_required", "This address only accepts HTTPS.")
			return
		}
		ip := tunnelctx.UnknownIP
		if vals := r.Header.Values("CF-Connecting-IP"); len(vals) == 1 {
			if addr, err := netip.ParseAddr(strings.TrimSpace(vals[0])); err == nil && addr.Zone() == "" {
				ip = addr.String()
			}
		}
		for _, h := range strippedProxyHeaders {
			r.Header.Del(h)
		}
		if s.tunnelMatcher == nil || !s.tunnelMatcher.Allows(r, pol.Profile) {
			writeTunnelError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		if pol.Profile == tunnelctx.ProfileAPIAndAdmin && s.tunnelMatcher.AdminOnly(r) && !s.tunnelAdminAllowed() {
			// 口令仍是出厂缺省：管理面暴露自动收窄为 api_only，不给探测面。
			writeTunnelError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		ctx := tunnelctx.With(r.Context(), tunnelctx.Info{ClientIP: ip, Hostname: pol.Hostname})
		s.root.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeTunnelError 是闸门自己的错误体：统一 {"error":{"code","message"}}，
// 状态码即含义，不带任何设备内部信息。
func writeTunnelError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}
