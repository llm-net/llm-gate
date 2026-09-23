package agenthost

// 给主机智能体（internal/hostagent）用的执行面：一条凭证书登录、可复用的 SSH 连接，
// 在上面跑智能体要求的命令。与本包其余部分同一套纪律——主机公钥逐字比对、私钥不出
// 本包、命令与 stdin 都不进日志——只是命令内容由智能体决定，超时与输出上限由调用方
// 按任务给（apt 装包动辄几分钟，装公钥那 30 秒的上限不适用）。
//
// 本文件不写日志：命令与输出可能含主机上的任何内容（§15.1 同一条线），留痕由
// hostagent 的操作日志（数据库行）承担，那是产品功能，不是进程日志。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Conn 是一条给智能体用的已认证连接：Open 用当前证书（必要时退回上一把）登上去，
// 之后每次 Run 各开一个 SSH 会话，命令之间不共享 shell 状态。用完 Close。
type Conn struct {
	c    *conn
	host store.AgentHost
}

// Host 是这条连接所属的主机行（打开时的快照）。
func (c *Conn) Host() store.AgentHost { return c.host }

// Close 断开连接；可重复调用。
func (c *Conn) Close() {
	if c != nil && c.c != nil {
		c.c.Close()
	}
}

// Open 凭证书连上一台已纳管主机。主机公钥仍按首次纳管钉死的那把逐字比对，不一致
// 即 CodeHostKeyChanged；没有证书或主机不存在按各自的错误回。
func (m *Manager) Open(ctx context.Context, id int64) (*Conn, error) {
	cert, prev, err := m.certPair(ctx)
	if err != nil {
		return nil, err
	}
	host, err := m.st.GetAgentHost(ctx, id)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout+handshakeTimeout)
	defer cancel()
	c, err := m.connectWithCert(dialCtx, *host, cert, prev)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c, host: *host}, nil
}

// RunRequest 是一条要在主机上跑的命令。
type RunRequest struct {
	// Command 交给对端的登录 shell 执行（与 ssh 客户端的 exec 请求同义）。
	Command string
	// Stdin 原样喂给命令；写文件走这里而不是拼进命令行。
	Stdin string
	// Timeout 是这条命令的上限；零值取 DefaultRunTimeout，上限 MaxRunTimeout。
	Timeout time.Duration
	// OutputLimit 是 stdout / stderr 各自截取的字节上限；零值取 DefaultOutputLimit。
	OutputLimit int
	// Observe 非空时，stdout / stderr 每到一段都同步抄送一份给它（进度观察，与截取上限
	// 无关）；它的错误被吞掉，不影响命令。写入发生在读取 goroutine 上，须自行加锁。
	Observe io.Writer
}

// RunResult 是命令的结果。非零退出码不是 error。
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Truncated 报告 stdout 或 stderr 有一侧超过了 OutputLimit 被截断。
	Truncated bool
	Duration  time.Duration
}

const (
	// DefaultRunTimeout 是智能体命令的缺省上限；MaxRunTimeout 是调用方能要到的最大值。
	DefaultRunTimeout = 2 * time.Minute
	MaxRunTimeout     = 15 * time.Minute
	// DefaultOutputLimit 是智能体命令 stdout / stderr 各自的缺省截取上限。
	DefaultOutputLimit = 64 << 10
)

// Run 在这条连接上跑一条命令。ctx 取消或超时即关掉会话（对端命令被 SIGHUP）。
func (c *Conn) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}
	if timeout > MaxRunTimeout {
		timeout = MaxRunTimeout
	}
	limit := req.OutputLimit
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	start := time.Now()
	out, errOut, code, truncated, err := c.c.runWith(ctx, req.Command, req.Stdin, timeout, limit, req.Observe)
	if err != nil {
		return RunResult{Duration: time.Since(start)}, err
	}
	return RunResult{Stdout: out, Stderr: errOut, ExitCode: code, Truncated: truncated, Duration: time.Since(start)}, nil
}

// CodeForwardDenied：对端明说转发被禁（administratively prohibited）。**OpenSSH 不会
// 这么说**：它的 server_input_channel_open 对 direct-tcpip / direct-streamlocal 的一切
// 失败都回同一句 SSH2_OPEN_CONNECT_FAILED + "open failed"——AllowStreamLocalForwarding
// 关着、socket 不存在、没人监听、权限不够，客户端拿到的字节一模一样（已在测试服务器
// 上逐种实测）。所以这个码只有非 OpenSSH 的对端才会命中，CodeUnreachable 才是常态，
// 调用方不能把它当成「服务没起来」的同义词。
const CodeForwardDenied = "forward_denied"

// Dial 经这条 SSH 连接打开一条到主机上某个地址的转发通道（TCP 或 Unix socket，对应
// SSH 的 direct-tcpip / direct-streamlocal）：devd 透传就靠它连到主机上守护进程的
// socket。返回的连接关掉只关这一条通道，SSH 连接本身仍由 Close 收。失败原因按上面
// 那条：对端多半只给一句 open failed，本包如实转述，不替它猜。
func (c *Conn) Dial(network, address string) (net.Conn, error) {
	conn, err := c.c.client.Dial(network, address)
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) && oce.Reason == ssh.Prohibited {
			return nil, &Error{Code: CodeForwardDenied, Msg: fmt.Sprintf("主机 sshd 禁止转发到 %s（AllowStreamLocalForwarding / AllowTcpForwarding 关着）", address)}
		}
		return nil, &Error{Code: CodeUnreachable, Msg: fmt.Sprintf("经 SSH 连接主机上的 %s 失败：%s", address, err)}
	}
	return conn, nil
}

// Listen 请主机的 sshd 在它自己的回环上开一个 TCP 端口（SSH 远程转发 tcpip-forward）：主机上
// 连到这个端口的连接经这条 SSH 连接回到设备，由返回的 Listener 交出。addr 形如
// "127.0.0.1:0"（端口 0 = 由 sshd 挑一个空闲端口，Addr() 报实际端口）。节点端智能体引擎靠它
// 访问设备：主机不必按局域网地址连得到设备，也不用信任设备的证书。Listener 关掉即撤销转发；
// SSH 连接断开时一并失效。sshd 关了 AllowTcpForwarding 时答 CodeForwardDenied。
func (c *Conn) Listen(addr string) (net.Listener, error) {
	ln, err := c.c.client.Listen("tcp", addr)
	if err != nil {
		return nil, &Error{Code: CodeForwardDenied, Msg: fmt.Sprintf(
			"主机的 sshd 拒绝在 %s 上开远程转发（AllowTcpForwarding / PermitListen 关着？）：%s", addr, err)}
	}
	return ln, nil
}

// streamStderrLimit 是 Stream 留下的 stderr 尾段上限（只用来说明启动失败的原因）。
const streamStderrLimit = 4 << 10

// Stream 是主机上一条长时间运行、双向流式的命令：Stdin / Stdout 就是它的标准输入输出（节点端
// 智能体引擎把它们当协议通道），stderr 只留末尾一段。与 Run 不同，它没有超时也不截输出，由
// 调用方 Close 收尾。本类型不写日志：流里是提示词与模型输出（§15.1）。
type Stream struct {
	sess   *ssh.Session
	Stdin  io.WriteCloser
	Stdout io.Reader
	stderr *tailBuffer

	done      chan struct{}
	exitCode  int
	exited    bool
	closeOnce sync.Once
}

// Start 在这条连接上拉起一条流式命令（交给对端的登录 shell 执行，同 Run）。
func (c *Conn) Start(command string) (*Stream, error) {
	sess, err := c.c.client.NewSession()
	if err != nil {
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("打开会话失败：%s", err)}
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("打开会话失败：%s", err)}
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("打开会话失败：%s", err)}
	}
	s := &Stream{sess: sess, Stdin: stdin, Stdout: stdout, stderr: &tailBuffer{limit: streamStderrLimit}, done: make(chan struct{})}
	sess.Stderr = s.stderr
	if err := sess.Start(command); err != nil {
		sess.Close()
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("远端命令未能执行：%s", err)}
	}
	go func() {
		err := sess.Wait()
		var exit *ssh.ExitError
		switch {
		case err == nil:
			s.exited = true
		case errors.As(err, &exit):
			s.exited, s.exitCode = true, exit.ExitStatus()
		}
		close(s.done)
	}()
	return s, nil
}

// Done 在远端命令结束（或会话断开）后关闭。
func (s *Stream) Done() <-chan struct{} { return s.done }

// ExitCode 是远端命令的退出码；ok 为假表示还没结束或会话断开时没拿到退出状态。
func (s *Stream) ExitCode() (code int, ok bool) {
	select {
	case <-s.done:
		return s.exitCode, s.exited
	default:
		return 0, false
	}
}

// StderrTail 是远端 stderr 的末尾一段（至多 4 KiB）。
func (s *Stream) StderrTail() string { return s.stderr.String() }

// Close 先关 stdin 让远端命令按 EOF 自然退出，grace 内没结束就发 KILL 信号并关掉会话。可重复调用。
func (s *Stream) Close(grace time.Duration) {
	s.closeOnce.Do(func() {
		_ = s.Stdin.Close()
		select {
		case <-s.done:
		case <-time.After(grace):
			_ = s.sess.Signal(ssh.SIGKILL)
			_ = s.sess.Close()
			select {
			case <-s.done:
			case <-time.After(grace):
			}
		}
		_ = s.sess.Close()
	})
}

// tailBuffer 只留最后 limit 字节。并发安全（stderr 由 ssh 包的 goroutine 写入）。
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
