package agenthost

// 与被纳管主机之间的 SSH 传输层：拨号、主机公钥钉死、跑一条命令。
//
// 只用 golang.org/x/crypto/ssh（纯 Go，满足 CGO_ENABLED=0）。设备**不执行**
// ssh/ssh-copy-id 等外部命令：板上未必装得有，且拼命令行是把口令送进 argv 的
// 最短路径（§15.1 明确禁止）。口令与私钥一律只经会话的 stdin 或内存里的
// signer 传递。
//
// 主机公钥不做 TOFU 之外的信任：首次纳管时记下那一把，之后每次连接逐字比对，
// 不一致即 ErrHostKeyChanged——这是 SSH 面上与「禁止关闭 TLS 校验」同一条纪律，
// 没有「忽略主机公钥」的开关。管理员确认主机确实重装过，可在界面上重新纳管并
// 显式接受新的主机公钥。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	// dialTimeout 是 TCP 拨号上限；handshakeTimeout 覆盖版本交换、密钥交换与认证。
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 15 * time.Second
	// commandTimeout 是单条远端命令的上限：装公钥、写 sudoers、读自述都是毫秒级，
	// 超过这个数只可能是对端卡住。
	commandTimeout = 30 * time.Second
	// outputLimit 是单次命令 stdout/stderr 各自的截取上限——远端可以吐任意长度，
	// 设备只取够写错误提示的那一段。
	outputLimit = 8 << 10
)

// DialFunc 是拨号入口（测试注入进程内监听地址）。
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// target 是一次连接所需的全部连接事实。
type target struct {
	Address  string
	Port     int
	Username string
	// HostKey 是已钉死的主机公钥（authorized_keys 单行）。空 = 尚未钉死。
	HostKey string
	// AcceptNewHostKey 允许这一次连接接受与 HostKey 不同的主机公钥（重新纳管时
	// 由管理员显式勾选）。
	AcceptNewHostKey bool
}

func (t target) addr() string { return net.JoinHostPort(t.Address, strconv.Itoa(t.Port)) }

// conn 是一条已建立的 SSH 连接。
type conn struct {
	client *ssh.Client
	// hostKey 是本次连接实际见到的主机公钥（authorized_keys 单行）。
	hostKey string
}

func (c *conn) Close() { c.client.Close() }

// dial 建一条 SSH 连接。auth 是按顺序尝试的认证方式（口令或证书）。
func (m *Manager) dial(ctx context.Context, t target, auth []ssh.AuthMethod) (*conn, error) {
	seen := ""
	callback := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		seen = authorizedLine(key)
		if t.HostKey == "" || t.AcceptNewHostKey || seen == t.HostKey {
			return nil
		}
		return &Error{Code: CodeHostKeyChanged, Msg: fmt.Sprintf(
			"主机公钥与首次纳管时记下的不一致（指纹 %s）：这台主机可能重装过系统，也可能连接被劫持。确认无误后在界面上重新纳管并接受新的主机公钥。",
			ssh.FingerprintSHA256(key))}
	}
	dialer := m.dialer()
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := dialer(dialCtx, "tcp", t.addr())
	if err != nil {
		return nil, &Error{Code: CodeUnreachable, Msg: fmt.Sprintf("连接 %s 失败：%s", t.addr(), netReason(err))}
	}
	if err := raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		raw.Close()
		return nil, &Error{Code: CodeUnreachable, Msg: fmt.Sprintf("连接 %s 失败：%s", t.addr(), netReason(err))}
	}
	cfg := &ssh.ClientConfig{
		User:            t.Username,
		Auth:            auth,
		HostKeyCallback: callback,
		Timeout:         handshakeTimeout,
	}
	sc, chans, reqs, err := ssh.NewClientConn(raw, t.addr(), cfg)
	if err != nil {
		raw.Close()
		var pinned *Error
		if errors.As(err, &pinned) {
			return nil, pinned
		}
		return nil, &Error{Code: CodeAuthFailed, Msg: fmt.Sprintf("登录 %s@%s 失败：%s", t.Username, t.addr(), authReason(err))}
	}
	// 握手完成后撤掉整条连接的截止时间：之后每条命令各自受 commandTimeout 约束。
	raw.SetDeadline(time.Time{})
	return &conn{client: ssh.NewClient(sc, chans, reqs), hostKey: seen}, nil
}

// run 在这条连接上跑一条命令，stdin 原样喂给它，回收截断后的 stdout/stderr。
// 非零退出码不是 error：调用方按退出码给各自的判断（免密 sudo 探测就靠它）。
func (c *conn) run(ctx context.Context, command, stdin string) (stdout, stderr string, code int, err error) {
	stdout, stderr, code, _, err = c.runWith(ctx, command, stdin, commandTimeout, outputLimit, nil)
	return stdout, stderr, code, err
}

// runWith 是 run 的通用形态：超时与两侧输出的截取上限由调用方给（智能体命令用）。
// truncated 报告有一侧超过上限被截断。observe 非空时，stdout / stderr 每到一段都同步
// 抄送一份给它（进度观察，不受截取上限影响）；它的返回值与错误都不管。
func (c *conn) runWith(ctx context.Context, command, stdin string, timeout time.Duration, limit int, observe io.Writer) (stdout, stderr string, code int, truncated bool, err error) {
	session, err := c.client.NewSession()
	if err != nil {
		return "", "", 0, false, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("打开会话失败：%s", err)}
	}
	defer session.Close()
	var out, errBuf bytes.Buffer
	outCap := &capped{buf: &out, limit: limit}
	errCap := &capped{buf: &errBuf, limit: limit}
	session.Stdout = outCap
	session.Stderr = errCap
	if observe != nil {
		session.Stdout = io.MultiWriter(outCap, observed{observe})
		session.Stderr = io.MultiWriter(errCap, observed{observe})
	}
	session.Stdin = strings.NewReader(stdin)

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err = <-done:
	case <-runCtx.Done():
		session.Close()
		<-done
		if ctx.Err() != nil {
			return "", "", 0, false, &Error{Code: CodeCommandFailed, Msg: "远端命令已中止"}
		}
		return "", "", 0, false, &Error{Code: CodeCommandFailed, Msg: "远端命令超时未返回"}
	}
	stdout, stderr = out.String(), errBuf.String()
	truncated = outCap.truncated || errCap.truncated
	if err == nil {
		return stdout, stderr, 0, truncated, nil
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return stdout, stderr, exit.ExitStatus(), truncated, nil
	}
	return stdout, stderr, 0, truncated, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("远端命令未能执行：%s", err)}
}

// observed 把输出抄送给观察者，吞掉它的错误：观察只是旁路，不能反过来打断命令。
type observed struct{ w io.Writer }

func (o observed) Write(p []byte) (int, error) {
	_, _ = o.w.Write(p)
	return len(p), nil
}

// capped 截断写入：远端可以吐任意长度，设备只留够写错误提示的那一段。
type capped struct {
	buf       *bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	limit := c.limit
	if limit <= 0 {
		limit = outputLimit
	}
	if room := limit - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return len(p), nil
}

// authorizedLine 把公钥规格化成 authorized_keys 单行（无换行、无注释）。
// 钉死的主机公钥与已装证书公钥都按这个形状逐字比对。
func authorizedLine(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// netReason 把拨号错误折成一句人话，不回显对端地址以外的内部细节。
func netReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "连接超时"
	case errors.Is(err, context.Canceled):
		return "连接已取消"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err.Error()
	}
	return err.Error()
}

// authReason 区分「口令/证书被拒」与「握手就没成」。SSH 的认证失败信息里不含
// 口令，可原样转述。
func authReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unable to authenticate"), strings.Contains(msg, "no supported methods"):
		return "用户名或口令不对，或该主机不允许这种登录方式"
	case errors.Is(err, io.EOF):
		return "对端在认证过程中断开"
	}
	return msg
}
