package gateway

// 内网域名 HTTPS 监听：gatewayd 的第三个 listener（前两个是明文 LAN 与可选的客户
// 自管静态证书 TLS）。证书由 internal/landomain 签发并热加载——本文件只管监听的
// 起停与把握手时的取证书交给 provider；域名、私钥与续期不在这里。
//
// 与 LAN listener 共用同一个根 handler 与权限模型：经它进来的请求 r.TLS != nil，
// tunnelctx.Secure 因此给管理面会话 Cookie 加 Secure；它不读任何转发头。
// 绑定失败（端口被占、无权限）只记告警并返回错误给管理器：证书是增强能力，443
// 起不来时设备照常经 80 服务。

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// DomainCertProvider 供给内网域名 HTTPS 监听的证书（生产实现 *landomain.Manager）。
type DomainCertProvider interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
}

type domainListener struct {
	addr string
	ln   net.Listener
	hs   *http.Server
	done chan struct{}
}

// EnableDomainTLS 接入证书供给方；装配期调用一次（Run 之前）。nil 等于不启用。
func (s *Server) EnableDomainTLS(p DomainCertProvider) { s.domainCerts = p }

// StartDomainTLS 在 addr 上启动（或按新地址重启）HTTPS 监听，幂等且并发安全：
// 已在同一地址监听时无事发生。
func (s *Server) StartDomainTLS(addr string) error {
	if s.domainCerts == nil {
		return errors.New("本进程未接入内网域名证书供给")
	}
	if addr == "" {
		return errors.New("缺少 HTTPS 监听地址")
	}
	s.domainMu.Lock()
	defer s.domainMu.Unlock()
	if s.domainTLS != nil {
		if s.domainTLS.addr == addr {
			return nil
		}
		s.stopDomainTLSLocked()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.log.Warn("内网域名 HTTPS 监听启动失败（HTTP 服务不受影响）", "listen", addr, "err", err.Error())
		return err
	}
	hs := &http.Server{
		Handler:           s.root,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		TLSConfig: &tls.Config{
			GetCertificate: s.domainCerts.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		},
	}
	d := &domainListener{addr: addr, ln: ln, hs: hs, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		if err := hs.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("内网域名 HTTPS 服务退出（HTTP 服务不受影响）", "err", err.Error())
		}
	}()
	s.domainTLS = d
	s.log.Info("内网域名 HTTPS 监听已启动", "listen", ln.Addr().String())
	return nil
}

// StopDomainTLS 停止 HTTPS 监听；未在监听时无事发生。已建立的连接 drain 至多 5 秒。
func (s *Server) StopDomainTLS() error {
	s.domainMu.Lock()
	defer s.domainMu.Unlock()
	return s.stopDomainTLSLocked()
}

func (s *Server) stopDomainTLSLocked() error {
	d := s.domainTLS
	if d == nil {
		return nil
	}
	s.domainTLS = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.hs.Shutdown(ctx); err != nil {
		d.hs.Close()
	}
	<-d.done
	s.log.Info("内网域名 HTTPS 监听已关闭", "listen", d.addr)
	return nil
}

// DomainTLSAddr 返回 HTTPS 监听的实际地址；未启动时空串。
func (s *Server) DomainTLSAddr() string {
	s.domainMu.Lock()
	defer s.domainMu.Unlock()
	if s.domainTLS == nil {
		return ""
	}
	return s.domainTLS.ln.Addr().String()
}
