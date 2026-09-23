package codexappserver

// app-server 的 stdio JSON-RPC 客户端：每行一条 JSON（换行分隔），客户端 → 服务端有请求
//（带 id）与通知（无 id）；服务端 → 客户端有应答（id + result/error）、通知与**反向请求**
//（带 id 与 method，例如审批询问），反向请求由调用方经 Incoming 取走并用 Reply 作答。
//
// §15.1：本文件不写日志。消息正文（可能含提示词、模型输出、凭据相关结果）只在内存里
// 交给调用方；调用方也不得把正文写进日志或审计。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// maxLineBytes 是单条 JSON-RPC 消息的上限（agentMessage/delta 之类通知很小，整段 diff 也
// 远小于此）。
const maxLineBytes = 64 << 20

// Message 是一条服务端来的通知或反向请求（服务端应答不经它，直接回到 Call）。
type Message struct {
	// ID 非空表示反向请求，需用 Reply 应答；为空是通知。
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCError 是服务端应答里的 error 对象。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("app-server 错误 %d: %s", e.Code, e.Message) }

// ErrClientClosed 表示连接已断（服务端退出或 Close）。
var ErrClientClosed = errors.New("app-server 连接已关闭")

type wireIn struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    *RPCError
}

// Client 在一对 stdio 管道上跑协议。并发安全；Incoming 只有一个消费者时语义最清楚。
type Client struct {
	w      io.Writer
	wMu    sync.Mutex
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan response
	closed  bool
	readErr error

	incoming chan Message
	done     chan struct{}
}

// NewClient 在 r（服务端 stdout）/ w（服务端 stdin）上启动读循环。incomingBuf 是通知缓冲，
// 满了读循环会阻塞——调用方要持续消费 Incoming 或用 DrainIncoming。
func NewClient(r io.Reader, w io.Writer, incomingBuf int) *Client {
	c := &Client{w: w, pending: map[int64]chan response{}, incoming: make(chan Message, incomingBuf), done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var in wireIn
		if err := json.Unmarshal(line, &in); err != nil {
			continue // 非 JSON 行（不该出现）：跳过，不记录内容
		}
		switch {
		case in.Method != "":
			msg := Message{ID: in.ID, Method: in.Method, Params: in.Params}
			select {
			case c.incoming <- msg:
			case <-c.done:
				return
			}
		case len(in.ID) > 0:
			var id int64
			if err := json.Unmarshal(in.ID, &id); err != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- response{result: in.Result, err: in.Error}
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.shutdown(err)
}

func (c *Client) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.readErr = err
	pending := c.pending
	c.pending = map[int64]chan response{}
	c.mu.Unlock()
	close(c.done)
	for _, ch := range pending {
		ch <- response{err: &RPCError{Code: -1, Message: ErrClientClosed.Error()}}
	}
}

// Close 停掉读循环并让所有在等的 Call 以 ErrClientClosed 返回。不关闭底层管道（由进程持有者关）。
func (c *Client) Close() { c.shutdown(ErrClientClosed) }

// Done 在连接断开后关闭。
func (c *Client) Done() <-chan struct{} { return c.done }

// Incoming 是服务端通知与反向请求的通道。
func (c *Client) Incoming() <-chan Message { return c.incoming }

func (c *Client) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wMu.Lock()
	defer c.wMu.Unlock()
	select {
	case <-c.done:
		return ErrClientClosed
	default:
	}
	_, err = c.w.Write(raw)
	return err
}

// Notify 发一条客户端通知（无 id）。
func (c *Client) Notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}

// Reply 应答一条服务端反向请求。
func (c *Client) Reply(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// ReplyError 用 JSON-RPC 错误明确拒绝不支持的反向请求。
func (c *Client) ReplyError(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": RPCError{Code: code, Message: message}})
}

// Call 发请求并等应答；result 非 nil 时把 result 解进去。服务端 error 以 *RPCError 返回。
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClientClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	select {
	case resp := <-ch:
		if resp.err != nil {
			if resp.err.Code == -1 && resp.err.Message == ErrClientClosed.Error() {
				return ErrClientClosed
			}
			return resp.err
		}
		if result != nil && len(resp.result) > 0 {
			return json.Unmarshal(resp.result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}
