package egress

// SOCKS5 客户端拨号器（RFC 1928 CONNECT + RFC 1929 用户名/口令）。
//
// 为什么不用 net/http.Transport 自带的 socks5 Proxy 支持：那条路的 SOCKS 握手只受请求
// context 约束，没有独立的握手超时（订阅长流的客户端整体超时是 10 分钟，代理挂死就是
// 10 分钟的空转）；错误也只是字符串，归类要靠比对文本。这里 150 行标准库代码换来：
// 握手有自己的时限、错误天然带 [Category]、目标域名恒以 DOMAINNAME 交给代理解析、
// 用户名/口令只活在本结构体里（不拼进任何 URL）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// DefaultHandshakeTimeout 是 SOCKS5 握手（方法协商 + 认证 + CONNECT 应答）的缺省时限，
// 从与代理的 TCP 建连成功起算。
const DefaultHandshakeTimeout = 10 * time.Second

const (
	socksVersion      = 0x05
	socksAuthNone     = 0x00
	socksAuthUserPass = 0x02
	socksAuthNoAccept = 0xFF
	socksCmdConnect   = 0x01
	socksATYPIPv4     = 0x01
	socksATYPDomain   = 0x03
	socksATYPIPv6     = 0x04
	userPassVersion   = 0x01
)

// dialFunc 是到代理端点的底层 TCP 拨号（来自各客户端自己的 net.Dialer，带各自的建连超时）。
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// dialer 是一份快照的 SOCKS5 拨号器：字段构造后只读，可并发使用。
type dialer struct {
	scope    Scope
	proxy    string
	username string
	password string
	dial     dialFunc
	timeout  time.Duration
}

func newDialer(scope Scope, p Profile, dial dialFunc, timeout time.Duration) *dialer {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	return &dialer{scope: scope, proxy: p.Address, username: p.Username, password: p.Password, dial: dial, timeout: timeout}
}

// DialContext 经代理建立到 addr（host:port，host 可以是域名、IPv4 或 IPv6）的 TCP 隧道。
// 返回的连接已完成 SOCKS5 CONNECT，调用方（net/http）在其上做目标 TLS。
func (d *dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, &Error{Scope: d.scope, Category: CategoryTargetFailed, Detail: "只支持 TCP", err: errors.New("unsupported network " + network)}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &Error{Scope: d.scope, Category: CategoryTargetFailed, Detail: "目标地址不合法", err: err}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, &Error{Scope: d.scope, Category: CategoryTargetFailed, Detail: "目标端口不合法", err: errors.New("bad port")}
	}
	conn, err := d.dial(ctx, "tcp", d.proxy)
	if err != nil {
		return nil, d.classifyDial(err)
	}
	if err := d.handshake(ctx, conn, host, port); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// classifyDial 把到代理端点的建连错误归类：超时 / 取消 / 其余不可达。
func (d *dialer) classifyDial(err error) error {
	if errors.Is(err, context.Canceled) {
		return &Error{Scope: d.scope, Category: CategoryUnreachable, Detail: "请求已取消", err: err}
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Scope: d.scope, Category: CategoryTimeout, err: err, timeout: true}
	}
	return &Error{Scope: d.scope, Category: CategoryUnreachable, err: err}
}

// handshake 在 conn 上完成方法协商、认证与 CONNECT。整个握手受 d.timeout 与 ctx 双重
// 约束；成功后清掉连接上的时限，把连接交给上层。
func (d *dialer) handshake(ctx context.Context, conn net.Conn, host string, port int) (retErr error) {
	deadline := time.Now().Add(d.timeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd
	}
	_ = conn.SetDeadline(deadline)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Unix(1, 0)) // 立即让在途读写返回
		case <-done:
		}
	}()
	defer func() {
		if retErr == nil {
			_ = conn.SetDeadline(time.Time{})
		}
	}()

	// 方法协商。
	methods := []byte{socksAuthNone}
	if d.username != "" {
		methods = []byte{socksAuthNone, socksAuthUserPass}
	}
	greeting := append([]byte{socksVersion, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return d.classifyIO(ctx, err, "发送方法协商失败")
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return d.classifyIO(ctx, err, "代理没有应答方法协商")
	}
	if reply[0] != socksVersion {
		return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: fmt.Sprintf("应答版本 %d", reply[0]), err: errors.New("socks: bad version")}
	}
	switch reply[1] {
	case socksAuthNone:
	case socksAuthUserPass:
		if d.username == "" {
			return &Error{Scope: d.scope, Category: CategoryAuthFailed, Detail: "代理要求用户名/口令认证", err: errors.New("socks: auth required")}
		}
		if err := d.authenticate(ctx, conn); err != nil {
			return err
		}
	case socksAuthNoAccept:
		if d.username == "" {
			return &Error{Scope: d.scope, Category: CategoryAuthFailed, Detail: "代理不接受无认证连接", err: errors.New("socks: no acceptable methods")}
		}
		return &Error{Scope: d.scope, Category: CategoryAuthFailed, Detail: "代理不接受用户名/口令认证", err: errors.New("socks: no acceptable methods")}
	default:
		return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: fmt.Sprintf("代理选择了不支持的认证方法 %d", reply[1]), err: errors.New("socks: unsupported method")}
	}

	// CONNECT：域名恒以 DOMAINNAME 交给代理解析，设备侧不做目标 DNS。
	req := []byte{socksVersion, socksCmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, socksATYPIPv4)
			req = append(req, ip4...)
		} else {
			req = append(req, socksATYPIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return &Error{Scope: d.scope, Category: CategoryTargetFailed, Detail: "目标域名过长", err: errors.New("socks: FQDN too long")}
		}
		req = append(req, socksATYPDomain, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return d.classifyIO(ctx, err, "发送 CONNECT 失败")
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return d.classifyIO(ctx, err, "代理没有应答 CONNECT")
	}
	if head[0] != socksVersion {
		return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: fmt.Sprintf("应答版本 %d", head[0]), err: errors.New("socks: bad version")}
	}
	if head[1] != 0x00 {
		return &Error{Scope: d.scope, Category: CategoryTargetFailed, Detail: replyText(head[1]), err: fmt.Errorf("socks: reply %d", head[1])}
	}
	// 读掉 BND.ADDR / BND.PORT。
	var bindLen int
	switch head[3] {
	case socksATYPIPv4:
		bindLen = 4 + 2
	case socksATYPIPv6:
		bindLen = 16 + 2
	case socksATYPDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return d.classifyIO(ctx, err, "CONNECT 应答不完整")
		}
		bindLen = int(l[0]) + 2
	default:
		return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: fmt.Sprintf("未知地址类型 %d", head[3]), err: errors.New("socks: bad atyp")}
	}
	if _, err := io.CopyN(io.Discard, conn, int64(bindLen)); err != nil {
		return d.classifyIO(ctx, err, "CONNECT 应答不完整")
	}
	return nil
}

// authenticate 执行 RFC 1929 子协商。用户名/口令只在这里写进连接，不进任何错误值。
func (d *dialer) authenticate(ctx context.Context, conn net.Conn) error {
	if len(d.username) > maxAuthLen || len(d.password) > maxAuthLen || len(d.username) == 0 {
		return &Error{Scope: d.scope, Category: CategoryAuthFailed, Detail: "用户名/口令长度不合法", err: errors.New("socks: bad credentials length")}
	}
	msg := make([]byte, 0, 3+len(d.username)+len(d.password))
	msg = append(msg, userPassVersion, byte(len(d.username)))
	msg = append(msg, d.username...)
	msg = append(msg, byte(len(d.password)))
	msg = append(msg, d.password...)
	if _, err := conn.Write(msg); err != nil {
		return d.classifyIO(ctx, err, "发送认证失败")
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return d.classifyIO(ctx, err, "代理没有应答认证")
	}
	if reply[0] != userPassVersion {
		return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: "认证应答版本错误", err: errors.New("socks: bad auth version")}
	}
	if reply[1] != 0x00 {
		return &Error{Scope: d.scope, Category: CategoryAuthFailed, Detail: "用户名或口令被拒绝", err: errors.New("socks: auth rejected")}
	}
	return nil
}

// classifyIO 把握手期间的读写错误归类：超时 → proxy_timeout；取消 → unreachable（带说明）；
// 其余（EOF、RST）→ proxy_protocol。
func (d *dialer) classifyIO(ctx context.Context, err error, detail string) error {
	if ctx.Err() != nil {
		return &Error{Scope: d.scope, Category: CategoryUnreachable, Detail: "请求已取消", err: ctx.Err()}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &Error{Scope: d.scope, Category: CategoryTimeout, Detail: detail, err: err, timeout: true}
	}
	return &Error{Scope: d.scope, Category: CategoryProtocol, Detail: detail, err: err}
}

// replyText 是 RFC 1928 §6 REP 字段的中文含义。
func replyText(rep byte) string {
	switch rep {
	case 0x01:
		return "代理内部错误"
	case 0x02:
		return "被代理规则拒绝"
	case 0x03:
		return "网络不可达"
	case 0x04:
		return "主机不可达"
	case 0x05:
		return "目标拒绝连接"
	case 0x06:
		return "TTL 超时"
	case 0x07:
		return "代理不支持 CONNECT"
	case 0x08:
		return "代理不支持该地址类型"
	}
	return fmt.Sprintf("应答码 %d", rep)
}
