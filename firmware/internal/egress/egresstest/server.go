// Package egresstest 提供离线测试用的最小 SOCKS5 服务器：记录每一次 CONNECT 请求的
// 目标（原样保留域名 / IP 形态，用来断言设备没有在本地预解析），可选用户名/口令认证，
// 并能按需注入协议错误、拒绝与挂死。不依赖公网或真实代理。
package egresstest

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Options 配置假代理的行为。
type Options struct {
	// Username / Password 非空时要求 RFC 1929 认证。
	Username, Password string
	// Reply 非 0 时对每个 CONNECT 回该 REP 码（RFC 1928 §6）而不连接目标。
	Reply byte
	// Hang 让服务器在读到第一个字节后不再应答（模拟握手挂死）。
	Hang bool
	// BadVersion 让方法协商应答写成 SOCKS4 版本号。
	BadVersion bool
	// CloseOnGreeting 在收到方法协商后直接断开。
	CloseOnGreeting bool
	// Resolve 把 CONNECT 里的目标主机映射到真正要拨的地址（缺省原样）；测试用它把
	// example.com 指到本地 httptest。
	Resolve func(host string) string
}

// Connect 是一次被记录的 CONNECT 请求。
type Connect struct {
	// ATYP：1 IPv4 / 3 域名 / 4 IPv6。
	ATYP byte
	Host string
	Port int
	// Authenticated 报告这条连接是否走完了用户名/口令认证。
	Authenticated bool
}

// Server 是运行中的假 SOCKS5 服务器。
type Server struct {
	ln   net.Listener
	opt  Options
	mu   sync.Mutex
	conn []Connect
	// tcpAccepts 是 TCP 层接受的连接数（含未完成握手的）。
	tcpAccepts int
	wg         sync.WaitGroup
	closed     chan struct{}
}

// New 启动一台假代理并在测试结束时关闭。
func New(t testing.TB, opt Options) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("egresstest: listen: %v", err)
	}
	s := &Server{ln: ln, opt: opt, closed: make(chan struct{})}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Addr 是 host:port 形式的代理地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 停止监听并等待在途连接处理协程退出。
func (s *Server) Close() {
	select {
	case <-s.closed:
		return
	default:
		close(s.closed)
	}
	s.ln.Close()
	s.wg.Wait()
}

// Connects 返回已记录的 CONNECT 请求副本。
func (s *Server) Connects() []Connect {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Connect(nil), s.conn...)
}

// TCPAccepts 返回 TCP 层接受过的连接数。
func (s *Server) TCPAccepts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tcpAccepts
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.tcpAccepts++
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(c)
		}()
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c, head[:]); err != nil || head[0] != 0x05 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if s.opt.Hang {
		<-s.closed
		return
	}
	if s.opt.CloseOnGreeting {
		return
	}
	if s.opt.BadVersion {
		c.Write([]byte{0x04, 0x00})
		return
	}
	wantAuth := s.opt.Username != ""
	offered := map[byte]bool{}
	for _, m := range methods {
		offered[m] = true
	}
	authed := false
	switch {
	case wantAuth && offered[0x02]:
		c.Write([]byte{0x05, 0x02})
		var v [2]byte
		if _, err := io.ReadFull(c, v[:]); err != nil || v[0] != 0x01 {
			return
		}
		user := make([]byte, int(v[1]))
		if _, err := io.ReadFull(c, user); err != nil {
			return
		}
		var pl [1]byte
		if _, err := io.ReadFull(c, pl[:]); err != nil {
			return
		}
		pass := make([]byte, int(pl[0]))
		if _, err := io.ReadFull(c, pass); err != nil {
			return
		}
		if string(user) != s.opt.Username || string(pass) != s.opt.Password {
			c.Write([]byte{0x01, 0x01})
			return
		}
		c.Write([]byte{0x01, 0x00})
		authed = true
	case wantAuth:
		c.Write([]byte{0x05, 0xFF})
		return
	case offered[0x00]:
		c.Write([]byte{0x05, 0x00})
	default:
		c.Write([]byte{0x05, 0xFF})
		return
	}

	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 0x05 || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		var ip [4]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	case 0x04:
		var ip [16]byte
		if _, err := io.ReadFull(c, ip[:]); err != nil {
			return
		}
		host = net.IP(ip[:]).String()
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		name := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, name); err != nil {
			return
		}
		host = string(name)
	default:
		c.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(c, portBuf[:]); err != nil {
		return
	}
	port := int(binary.BigEndian.Uint16(portBuf[:]))
	s.mu.Lock()
	s.conn = append(s.conn, Connect{ATYP: req[3], Host: host, Port: port, Authenticated: authed})
	s.mu.Unlock()

	if s.opt.Reply != 0 {
		c.Write([]byte{0x05, s.opt.Reply, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	dialHost := host
	if s.opt.Resolve != nil {
		dialHost = s.opt.Resolve(host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(dialHost, strconv.Itoa(port)))
	if err != nil {
		c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
	_ = c.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() { io.Copy(target, c); target.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(c, target); done <- struct{}{} }()
	select {
	case <-done:
	case <-s.closed:
	}
}
