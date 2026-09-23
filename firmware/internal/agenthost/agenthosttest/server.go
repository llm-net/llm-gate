// Package agenthosttest 是离线的假 SSH 主机：进程内监听、真跑 SSH 协议
// （golang.org/x/crypto/ssh 的服务端），并用一份内存里的 authorized_keys 与
// sudoers 状态模拟被纳管主机的可观察行为。
//
// 它服务两处用例：internal/agenthost 的流程验收，与 internal/admin 的端点验收。
// 全程不出网、不碰本机 SSH 配置，也不起外部进程。
//
// 本文件里的错误串一律用英文：这是测试支撑包（非 _test.go 文件），中文字面量会被
// i18n 提取器收进管理面译文目录，而这些串没有一句给用户看（同 internal/egress/egresstest）。
package agenthosttest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Options 是假主机的初始状态。
type Options struct {
	// Password 是接受的登录口令（空串 = 不接受口令登录）。
	Password string
	// Username 是接受的用户名（空 = 不校验）。
	Username string
	// NoSudo 为真时主机上没有 sudo（sudo 命令答 127）。
	NoSudo bool
	// SudoNoPasswd 是初始的免密 sudo 状态（纳管前通常为 false）。
	SudoNoPasswd bool
	// System 是 uname -srm 的自述（空取一个默认值）。
	System string
	// AuthorizedKeys 是初始已授权的公钥行。
	AuthorizedKeys []string
	// Command 接管设备脚本之外的任意命令（主机智能体的用例）：handled 为假时退回
	// 内建的脚本识别，仍不认识就报测试错误。
	Command func(command, stdin string) (stdout string, code int, handled bool)
	// CommandStderr 同 Command，但还能往 stderr 写东西（先于 Command 查询）。
	CommandStderr func(command, stdin string) (stdout, stderr string, code int, handled bool)
	// Forward 接管转发通道（direct-tcpip / direct-streamlocal）：network 是 "tcp" 或
	// "unix"，address 是设备要连的主机侧地址；nil = 拒绝一切转发。
	Forward func(network, address string) (net.Conn, error)
	// Stream 接管流式命令（节点端智能体引擎）：在读 stdin 之前按命令判断，handled 为真
	// 时 stdin / stdout / stderr 就是会话通道本身，返回的是退出码；为假退回一次性读完
	// stdin 的形态（Command / 内建脚本）。
	Stream func(command string, stdin io.Reader, stdout, stderr io.Writer) (code int, handled bool)
	// NoRemoteForward 让假主机拒绝远程转发（tcpip-forward，同 sshd 的 AllowTcpForwarding no）；
	// 缺省接受：在本机回环上真开一个端口，连进来的连接以 forwarded-tcpip 通道交回设备。
	NoRemoteForward bool
	// ForwardProhibited 让拒绝转发时如实回 administratively prohibited。缺省不这么答：
	// 真正的 OpenSSH 从不这么答（见 agenthost.CodeForwardDenied），本假主机按它来，
	// 免得测试里跑出线上跑不出的那条分支。
	ForwardProhibited bool
}

// Exec 是假主机看见的一条命令。
type Exec struct {
	Command string
	Stdin   string
	// PublicKeyAuth 记录发这条命令的连接是用公钥登录的（而不是口令）。
	PublicKeyAuth bool
}

// Server 是一台假主机。方法并发安全。
type Server struct {
	t        *testing.T
	ln       net.Listener
	cfg      *ssh.ServerConfig
	hostLine string

	mu       sync.Mutex
	opt      Options
	keys     []string
	sudoers  bool
	sudoFile string
	execs    []Exec
	closed   bool
}

// New 起一台假主机，测试结束自动关闭。
func New(t *testing.T, opt Options) *Server {
	t.Helper()
	if opt.System == "" {
		opt.System = "Linux 6.12.11-edge-sunxi64 aarch64"
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成假主机密钥: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("生成假主机密钥: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	s := &Server{t: t, ln: ln, opt: opt, keys: append([]string(nil), opt.AuthorizedKeys...),
		sudoers: opt.SudoNoPasswd, hostLine: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}
	s.cfg = &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if s.opt.Password == "" || string(password) != s.opt.Password {
				return nil, errors.New("password rejected")
			}
			if s.opt.Username != "" && meta.User() != s.opt.Username {
				return nil, errors.New("user rejected")
			}
			return &ssh.Permissions{Extensions: map[string]string{"auth": "password"}}, nil
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if !s.authorized(line) {
				return nil, errors.New("public key not authorized")
			}
			if s.opt.Username != "" && meta.User() != s.opt.Username {
				return nil, errors.New("user rejected")
			}
			return &ssh.Permissions{Extensions: map[string]string{"auth": "publickey"}}, nil
		},
	}
	s.cfg.AddHostKey(signer)
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

// Close 停止监听（重复调用无害）。
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.ln.Close()
}

// HostKeyLine 是这台假主机的公钥（authorized_keys 单行）。
func (s *Server) HostKeyLine() string { return s.hostLine }

// Addr 是监听地址（host, port）。
func (s *Server) Addr() (string, int) {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	n, _ := strconv.Atoi(port)
	return host, n
}

// Dial 是交给 agenthost.Options.Dial 的拨号器：无视目标地址，恒连本台假主机。
func (s *Server) Dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, s.ln.Addr().String())
}

// Keys 是当前 authorized_keys 的全部行。
func (s *Server) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keys...)
}

// HasKey 报告某一行是否在 authorized_keys 里。
func (s *Server) HasKey(line string) bool { return s.authorized(line) }

// SudoNoPasswd 报告该用户现在能否免密 sudo。
func (s *Server) SudoNoPasswd() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sudoers
}

// SudoersFile 是假主机上写下的 sudoers 文件路径（没写过为空）。
func (s *Server) SudoersFile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sudoFile
}

// Execs 是收到过的全部命令（含 stdin）。
func (s *Server) Execs() []Exec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Exec(nil), s.execs...)
}

// authorized 按真 sshd 的口径比对：只看「类型 + base64」两段，行尾注释不参与
// （设备写进去的行带 llmgate-agent 注释，登录时递上来的公钥没有）。
func (s *Server) authorized(line string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := keyBody(line)
	for _, k := range s.keys {
		if keyBody(k) == want {
			return true
		}
	}
	return false
}

func keyBody(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return strings.TrimSpace(line)
	}
	return fields[0] + " " + fields[1]
}

func (s *Server) serve() {
	for {
		raw, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(raw)
	}
}

func (s *Server) handleConn(raw net.Conn) {
	defer raw.Close()
	sc, chans, reqs, err := ssh.NewServerConn(raw, s.cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go s.handleGlobal(sc, reqs)
	byKey := sc.Permissions != nil && sc.Permissions.Extensions["auth"] == "publickey"
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			ch, requests, err := nc.Accept()
			if err != nil {
				return
			}
			go s.handleSession(ch, requests, byKey)
		case "direct-tcpip", "direct-streamlocal@openssh.com":
			go s.handleForward(nc)
		default:
			nc.Reject(ssh.UnknownChannelType, "unsupported channel type")
		}
	}
}

// rejectForward 给出拒绝转发通道的那一句。OpenSSH 的 server_input_channel_open 对
// direct-tcpip / direct-streamlocal 的一切失败（禁转发、目标不存在、没人监听、权限
// 不够）都只回 SSH2_OPEN_CONNECT_FAILED + "open failed"，本假主机照此。
func rejectForward(prohibited bool) (ssh.RejectionReason, string) {
	if prohibited {
		return ssh.Prohibited, "administratively prohibited: forwarding disabled"
	}
	return ssh.ConnectionFailed, "open failed"
}

// handleForward 解转发通道的目标地址，交给 Options.Forward 建连接后双向搬运。
func (s *Server) handleForward(nc ssh.NewChannel) {
	if s.opt.Forward == nil {
		nc.Reject(rejectForward(s.opt.ForwardProhibited))
		return
	}
	network, address := "unix", ""
	if nc.ChannelType() == "direct-tcpip" {
		var req struct {
			Host  string
			Port  uint32
			OHost string
			OPort uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
			nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
			return
		}
		network, address = "tcp", net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))
	} else {
		var req struct {
			Path     string
			Reserved string
			Reserve  uint32
		}
		if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
			nc.Reject(ssh.ConnectionFailed, "bad direct-streamlocal payload")
			return
		}
		address = req.Path
	}
	target, err := s.opt.Forward(network, address)
	if err != nil {
		// 建连接失败的原因不外传：OpenSSH 同样只回这一句，Forward 返回的文字只给
		// 假主机自己看。
		nc.Reject(rejectForward(false))
		return
	}
	ch, requests, err := nc.Accept()
	if err != nil {
		target.Close()
		return
	}
	go ssh.DiscardRequests(requests)
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(ch, target)
		ch.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(target, ch)
		if cw, ok := target.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			target.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	target.Close()
	ch.Close()
}

// handleGlobal 应答全局请求：只认远程转发（tcpip-forward / cancel-tcpip-forward），其余拒绝。
func (s *Server) handleGlobal(sc *ssh.ServerConn, reqs <-chan *ssh.Request) {
	listeners := map[string]net.Listener{}
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}()
	for req := range reqs {
		var fwd struct {
			Addr string
			Port uint32
		}
		switch req.Type {
		case "tcpip-forward":
			if s.opt.NoRemoteForward || ssh.Unmarshal(req.Payload, &fwd) != nil {
				req.Reply(false, nil)
				continue
			}
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(fwd.Port))))
			if err != nil {
				req.Reply(false, nil)
				continue
			}
			port := uint32(ln.Addr().(*net.TCPAddr).Port)
			listeners[net.JoinHostPort(fwd.Addr, strconv.Itoa(int(port)))] = ln
			var reply []byte
			if fwd.Port == 0 {
				reply = ssh.Marshal(struct{ Port uint32 }{port})
			}
			req.Reply(true, reply)
			go s.serveRemoteForward(sc, ln, fwd.Addr, port)
		case "cancel-tcpip-forward":
			if ssh.Unmarshal(req.Payload, &fwd) == nil {
				key := net.JoinHostPort(fwd.Addr, strconv.Itoa(int(fwd.Port)))
				if ln, ok := listeners[key]; ok {
					ln.Close()
					delete(listeners, key)
				}
			}
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// serveRemoteForward 把本机端口上接到的连接逐条以 forwarded-tcpip 通道交回设备。
func (s *Server) serveRemoteForward(sc *ssh.ServerConn, ln net.Listener, addr string, port uint32) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			origin := c.RemoteAddr().(*net.TCPAddr)
			payload := ssh.Marshal(struct {
				Addr       string
				Port       uint32
				OriginAddr string
				OriginPort uint32
			}{addr, port, origin.IP.String(), uint32(origin.Port)})
			ch, reqs, err := sc.OpenChannel("forwarded-tcpip", payload)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(reqs)
			done := make(chan struct{}, 2)
			go func() { io.Copy(ch, c); ch.CloseWrite(); done <- struct{}{} }()
			go func() { io.Copy(c, ch); done <- struct{}{} }()
			<-done
			ch.Close()
			<-done
		}()
	}
}

func (s *Server) handleSession(ch ssh.Channel, requests <-chan *ssh.Request, byKey bool) {
	defer ch.Close()
	for req := range requests {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		req.Reply(true, nil)
		command := payloadString(req.Payload)
		if s.opt.Stream != nil {
			go ssh.DiscardRequests(requests)
			if code, ok := s.opt.Stream(command, ch, ch, ch.Stderr()); ok {
				s.mu.Lock()
				s.execs = append(s.execs, Exec{Command: command, PublicKeyAuth: byKey})
				s.mu.Unlock()
				ch.CloseWrite()
				var status [4]byte
				binary.BigEndian.PutUint32(status[:], uint32(code))
				ch.SendRequest("exit-status", false, status[:])
				return
			}
		}
		stdin, _ := io.ReadAll(ch)
		out, stderr, code := s.execWithStderr(command, string(stdin), byKey)
		io.WriteString(ch, out)
		if stderr != "" {
			io.WriteString(ch.Stderr(), stderr)
		}
		ch.CloseWrite()
		var status [4]byte
		binary.BigEndian.PutUint32(status[:], uint32(code))
		ch.SendRequest("exit-status", false, status[:])
		return
	}
}

// execWithStderr 先问 CommandStderr 钩子，其余交给 exec。
func (s *Server) execWithStderr(command, stdin string, byKey bool) (string, string, int) {
	if s.opt.CommandStderr != nil {
		if out, stderr, code, ok := s.opt.CommandStderr(command, stdin); ok {
			s.mu.Lock()
			s.execs = append(s.execs, Exec{Command: command, Stdin: stdin, PublicKeyAuth: byKey})
			s.mu.Unlock()
			return out, stderr, code
		}
	}
	out, code := s.exec(command, stdin, byKey)
	return out, "", code
}

// exec 解释设备下发的那几段脚本。匹配的是脚本里的特征串，而不是逐字比对整段——
// 脚本改一行注释不该让这台假主机失灵，但它认得出「这是在装公钥还是在探状态」。
func (s *Server) exec(command, stdin string, byKey bool) (string, int) {
	s.mu.Lock()
	s.execs = append(s.execs, Exec{Command: command, Stdin: stdin, PublicKeyAuth: byKey})
	s.mu.Unlock()

	if s.opt.Command != nil {
		if out, code, ok := s.opt.Command(command, stdin); ok {
			return out, code
		}
	}
	lines := strings.Split(stdin, "\n")
	switch {
	case command == "sudo -n true":
		if s.opt.NoSudo {
			return "", 127
		}
		if s.SudoNoPasswd() {
			return "", 0
		}
		return "", 1
	case strings.HasPrefix(command, "sudo -S"):
		return s.sudoWrite(command, lines)
	case strings.HasPrefix(command, "sudo -n"):
		if s.opt.NoSudo || !s.SudoNoPasswd() {
			return "", 1
		}
		s.mu.Lock()
		s.sudoers, s.sudoFile = false, ""
		s.mu.Unlock()
		return "", 0
	case strings.Contains(command, "newkey"):
		return s.install(lines)
	case strings.Contains(command, "llmgate-key:installed"):
		return s.probe(lines)
	case strings.Contains(command, "while IFS="):
		return s.remove(lines)
	}
	s.t.Errorf("fake host got an unrecognized command: %q", command)
	return "", 127
}

func (s *Server) sudoWrite(command string, lines []string) (string, int) {
	if s.opt.NoSudo {
		return "", 127
	}
	if len(lines) == 0 || lines[0] != s.opt.Password {
		return "Sorry, try again.\n", 1
	}
	i := strings.Index(command, "f=/etc/sudoers.d/")
	if i < 0 {
		return "", 1
	}
	path := strings.SplitN(command[i+len("f="):], "\n", 2)[0]
	s.mu.Lock()
	s.sudoers, s.sudoFile = true, path
	s.mu.Unlock()
	return "", 0
}

func (s *Server) install(lines []string) (string, int) {
	if len(lines) == 0 || lines[0] == "" {
		return "", 3
	}
	newKey := lines[0]
	oldKey := ""
	if len(lines) > 1 {
		oldKey = lines[1]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.keys[:0:0]
	for _, k := range s.keys {
		if oldKey != "" && k == oldKey {
			continue
		}
		kept = append(kept, k)
	}
	for _, k := range kept {
		if k == newKey {
			s.keys = kept
			return "", 0
		}
	}
	s.keys = append(kept, newKey)
	return "", 0
}

func (s *Server) probe(lines []string) (string, int) {
	if len(lines) == 0 || lines[0] == "" {
		return "", 3
	}
	marker := "llmgate-key:missing"
	if s.authorized(lines[0]) {
		marker = "llmgate-key:installed"
	}
	return fmt.Sprintf("%s\n%s\n", marker, s.opt.System), 0
}

func (s *Server) remove(lines []string) (string, int) {
	drop := map[string]bool{}
	for _, l := range lines {
		if l != "" {
			drop[l] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.keys[:0:0]
	for _, k := range s.keys {
		if !drop[k] {
			kept = append(kept, k)
		}
	}
	s.keys = kept
	return "", 0
}

// payloadString 解 SSH "exec" 请求的载荷（4 字节长度 + 命令）。
func payloadString(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload)
	if int(n) > len(payload)-4 {
		return ""
	}
	return string(payload[4 : 4+n])
}
