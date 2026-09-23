package modeld

// Codex app-server 的 stdio JSON-RPC 客户端（与 internal/codexappserver/jsonrpc.go 同一套协议：
// 每行一条 JSON；客户端 → 服务端有请求（带 id）与通知；服务端 → 客户端有应答、通知与反向请求）。
// 守护进程二进制不能把 codexappserver 整包（连同官网清单、组件引擎）拖进来，所以这里保留一份
// 只含标准库的最小实现。
//
// §15.1：本文件不写日志；消息正文只在内存里交给调用方。

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

const rpcMaxLine = 64 << 20

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("app-server 错误 %d: %s", e.Code, e.Message) }

var errRPCClosed = errors.New("app-server 连接已关闭")

type rpcWireIn struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

type rpcClient struct {
	w      io.Writer
	wMu    sync.Mutex
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	closed  bool

	incoming chan rpcMessage
	done     chan struct{}
}

func newRPCClient(r io.Reader, w io.Writer, buf int) *rpcClient {
	c := &rpcClient{w: w, pending: map[int64]chan rpcResponse{}, incoming: make(chan rpcMessage, buf), done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

func (c *rpcClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), rpcMaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var in rpcWireIn
		if err := json.Unmarshal(line, &in); err != nil {
			continue
		}
		switch {
		case in.Method != "":
			msg := rpcMessage{ID: in.ID, Method: in.Method, Params: in.Params}
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
				ch <- rpcResponse{result: in.Result, err: in.Error}
			}
		}
	}
	c.shutdown()
}

func (c *rpcClient) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()
	close(c.done)
	for _, ch := range pending {
		ch <- rpcResponse{err: &rpcError{Code: -1, Message: errRPCClosed.Error()}}
	}
}

func (c *rpcClient) Close()                      { c.shutdown() }
func (c *rpcClient) Done() <-chan struct{}       { return c.done }
func (c *rpcClient) Incoming() <-chan rpcMessage { return c.incoming }

func (c *rpcClient) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.wMu.Lock()
	defer c.wMu.Unlock()
	select {
	case <-c.done:
		return errRPCClosed
	default:
	}
	_, err = c.w.Write(raw)
	return err
}

func (c *rpcClient) Notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}

func (c *rpcClient) Reply(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *rpcClient) Call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errRPCClosed
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
			if resp.err.Code == -1 && resp.err.Message == errRPCClosed.Error() {
				return errRPCClosed
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
