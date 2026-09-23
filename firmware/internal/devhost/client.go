package devhost

// 与主机上 llmgate-devd 之间的客户端：HTTP 跑在一条经 SSH 转发到守护进程 Unix socket
// 的通道上（agenthost.Conn.Dial("unix", …)）。没有 TLS、没有证书、没有身份指纹——
// 鉴权与对端身份都是那条 SSH 连接的事（访问证书登录 + 钉死的主机公钥），这里只是
// 在通道上说 HTTP。每次操作一条通道，不复用。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
)

const (
	daemonReqTimeout = 60 * time.Second
	// daemonProxyMax 是转发给界面的响应体上限（读文件本身封顶 2 MiB，diff 4 MiB）。
	daemonProxyMax = 8 << 20
	// daemonHost 是请求里的 Host 头：socket 上没有主机名可言，给个固定值。
	daemonHost = "llmgate-devd"
)

// daemonClient 是对一台主机守护进程的一次性客户端：借一条已登好的 SSH 连接，
// 每次请求在上面开一条到 socket 的转发通道。
type daemonClient struct {
	conn *agenthost.Conn
	spec daemonSpec
}

func newDaemonClient(conn *agenthost.Conn, spec daemonSpec) *daemonClient {
	return &daemonClient{conn: conn, spec: spec}
}

// dial 开一条到守护进程 socket 的通道。
//
// 失败原因这里**不猜**：OpenSSH 对 direct-streamlocal 的一切失败都只回一句
// "open failed"（见 agenthost.CodeForwardDenied 的注释），服务没起来、主机上装的是旧版
// 守护进程（老版本监听 TCP 端口，根本没有这个 socket）、sshd 关了转发、socket 权限不对
// 在客户端看来完全相同。所以这条消息把四种可能一次列全，管理员照着一条条查即可；对端
// 若如实回了 administratively prohibited（非 OpenSSH），仍单独折成 CodeForwardDenied。
func (c *daemonClient) dial() (net.Conn, error) {
	conn, err := c.conn.Dial("unix", c.spec.Socket)
	if err == nil {
		return conn, nil
	}
	var he *agenthost.Error
	if errors.As(err, &he) && he.Code == agenthost.CodeForwardDenied {
		return nil, &Error{Code: CodeForwardDenied, Msg: he.Msg + "：设备经 SSH 转发连到守护进程的 socket，请在主机的 sshd_config 里放开 AllowStreamLocalForwarding。"}
	}
	if !errors.As(err, &he) {
		return nil, &Error{Code: CodeUnreachable, Msg: "连接守护进程失败：" + err.Error()}
	}
	return nil, &Error{Code: CodeUnreachable, Msg: fmt.Sprintf(
		"%s。主机 sshd 对这一步的失败只回一句 open failed，看不出是哪一种，请依次查："+
			"① 守护进程没起来（主机上 systemctl status %s）；"+
			"② 主机上装的是旧版守护进程，没有 %s：卸载 %s 后重新安装；"+
			"③ 主机 sshd 关了转发（AllowStreamLocalForwarding / AllowTcpForwarding / DisableForwarding）；"+
			"④ socket 在，但登录用户连不上它（属主或权限不对）。",
		he.Msg, c.spec.UnitName, c.spec.Socket, c.spec.Name)}
}

// httpClient 是给 JSON 请求用的客户端：每条请求一条 SSH 通道，不复用、不走出站代理
// （目标是另一台机器上的 Unix socket，只经 SSH 可达）。
func (c *daemonClient) httpClient() *http.Client {
	return &http.Client{
		Timeout: daemonReqTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return c.dial()
			},
			DisableKeepAlives: true,
		},
	}
}

// streamClient 是给原字节搬运用的客户端：整体不设超时（一份视频走 Wi-Fi 要多久由 ctx 说了算），
// 同样每条请求一条 SSH 通道。
func (c *daemonClient) streamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return c.dial()
			},
			DisableKeepAlives: true,
		},
	}
}

// do 发一条请求并返回响应（调用方负责关 Body）。path 已含 /v1 前缀。
func (c *daemonClient) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	return c.send(ctx, c.httpClient(), method, path, query, body, contentType, nil)
}

// stream 同 do，但不设整体超时、可带额外请求头（Range 之类）。
func (c *daemonClient) stream(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, headers http.Header) (*http.Response, error) {
	return c.send(ctx, c.streamClient(), method, path, query, body, contentType, headers)
}

func (c *daemonClient) send(ctx context.Context, client *http.Client, method, path string, query url.Values, body io.Reader, contentType string, headers http.Header) (*http.Response, error) {
	u := "http://" + daemonHost + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		var de *Error
		if errors.As(err, &de) {
			return nil, de
		}
		return nil, &Error{Code: CodeUnreachable, Msg: fmt.Sprintf("请求守护进程失败：%s", netReason(unwrapURL(err)))}
	}
	return resp, nil
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// netReason 把连接错误折成一句人话。
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

// getJSON 取一个 JSON 响应；非 2xx 折成 Error（带守护进程给的 code/message）。
func (c *daemonClient) getJSON(ctx context.Context, method, path string, query url.Values, body any, dst any) error {
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
		ct = "application/json"
	}
	resp, err := c.do(ctx, method, path, query, rd, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, daemonProxyMax))
	if err != nil {
		return &Error{Code: CodeUnreachable, Msg: "读取守护进程响应失败：" + err.Error()}
	}
	if resp.StatusCode/100 != 2 {
		return daemonError(resp.StatusCode, raw)
	}
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &Error{Code: CodeCommandFailed, Msg: "守护进程响应不是合法 JSON"}
	}
	return nil
}

// daemonError 把守护进程的错误体折成本包 Error。
func daemonError(status int, raw []byte) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(raw, &body)
	if body.Error.Code == "" {
		return &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("守护进程答 HTTP %d", status)}
	}
	return &Error{Code: body.Error.Code, Msg: body.Error.Message, Status: status}
}

// info 读守护进程自述。
func (c *daemonClient) info(ctx context.Context) (*devd.Info, error) {
	var info devd.Info
	if err := c.getJSON(ctx, http.MethodGet, "/v1/info", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// attach 走 Upgrade 握手，回一条 wire 帧流（跑在那条 SSH 通道上）。
func (c *daemonClient) attach(ctx context.Context, name, dir string, cols, rows int) (net.Conn, error) {
	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	q := url.Values{"name": {name}, "dir": {dir}, "cols": {strconv.Itoa(cols)}, "rows": {strconv.Itoa(rows)}}
	req := "GET /v1/tmux/attach?" + q.Encode() + " HTTP/1.1\r\nHost: " + daemonHost + "\r\nUpgrade: " + devd.UpgradeProtocol + "\r\nConnection: Upgrade\r\n\r\n"
	conn.SetDeadline(time.Now().Add(daemonReqTimeout))
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, &Error{Code: CodeUnreachable, Msg: "向守护进程发起终端会话失败：" + err.Error()}
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, &Error{Code: CodeUnreachable, Msg: "守护进程没有应答终端会话：" + err.Error()}
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		conn.Close()
		return nil, daemonError(resp.StatusCode, raw)
	}
	conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		// 101 之后可能已经跟着终端字节：把缓冲里的先吐出来。
		buffered, _ := br.Peek(br.Buffered())
		return &prefixedConn{Conn: conn, prefix: append([]byte(nil), buffered...)}, nil
	}
	return conn, nil
}

// prefixedConn 先读完握手时多读进缓冲的字节，再读底层连接。
type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixedConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}
