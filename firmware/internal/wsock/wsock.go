// Package wsock 是给设备界面工作空间终端用的最小 WebSocket 服务端（RFC 6455）：
// 握手、二进制/文本帧、分片重组、ping/pong/close。全仓不引第三方 WebSocket 库，
// 这里只实现工作空间终端真正用到的子集，并把攻击面钉死：
//   - 客户端帧必须加掩码（协议要求，未加掩码即断开）；
//   - 单条消息上限 MaxMessage，超过即关闭；
//   - 不协商任何扩展（无压缩），不做子协议。
//
// 设备只在浏览器与守护进程之间搬运 wire 帧，一条 WebSocket 消息 = 一帧。
package wsock

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MaxMessage 是单条消息上限（含分片重组后）。
const MaxMessage = 1 << 20

// 操作码。
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// 关闭码。
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseMessageTooBig = 1009
	CloseInternalError = 1011
)

const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ErrClosed 是连接已关闭。
var ErrClosed = errors.New("websocket: closed")

// Conn 是一条已握手的连接。ReadMessage 只能由一个 goroutine 调用；WriteMessage /
// Close 可并发。
type Conn struct {
	raw    net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed bool
	cmu    sync.Mutex
}

// IsUpgrade 报告请求是不是 WebSocket 握手。
func IsUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// Accept 完成服务端握手并接管连接。失败时已经写好了错误响应。
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if r.Method != http.MethodGet || !IsUpgrade(r) {
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: not an upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusBadRequest)
		return nil, errors.New("websocket: bad version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("websocket: missing key")
	}
	raw, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "connection does not support upgrade", http.StatusInternalServerError)
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	sum := sha1.Sum([]byte(key + magic))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := brw.WriteString(resp); err != nil {
		raw.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		raw.Close()
		return nil, err
	}
	return &Conn{raw: raw, br: brw.Reader}, nil
}

// ReadMessage 读下一条完整的数据消息（文本或二进制），控制帧在内部处理：
// ping 自动回 pong，close 回 close 并返回 ErrClosed。
func (c *Conn) ReadMessage() (opcode int, payload []byte, err error) {
	var msg []byte
	msgOp := 0
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			c.Close(CloseProtocolError, "")
			return 0, nil, err
		}
		switch op {
		case OpPing:
			c.writeFrame(OpPong, data)
			continue
		case OpPong:
			continue
		case OpClose:
			code := CloseNormal
			if len(data) >= 2 {
				code = int(binary.BigEndian.Uint16(data[:2]))
			}
			c.Close(code, "")
			return 0, nil, ErrClosed
		case OpText, OpBinary:
			if msgOp != 0 {
				c.Close(CloseProtocolError, "")
				return 0, nil, errors.New("websocket: unexpected new message during fragmented message")
			}
			msgOp = op
			msg = data
		case OpContinuation:
			if msgOp == 0 {
				c.Close(CloseProtocolError, "")
				return 0, nil, errors.New("websocket: continuation without start")
			}
			msg = append(msg, data...)
		default:
			c.Close(CloseProtocolError, "")
			return 0, nil, errors.New("websocket: unknown opcode")
		}
		if len(msg) > MaxMessage {
			c.Close(CloseMessageTooBig, "")
			return 0, nil, errors.New("websocket: message too big")
		}
		if fin {
			return msgOp, msg, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op int, payload []byte, err error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	op = int(hdr[0] & 0x0f)
	masked := hdr[1]&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("websocket: client frame not masked")
	}
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	if op >= OpClose && (n > 125 || !fin) {
		return false, 0, nil, errors.New("websocket: bad control frame")
	}
	if n > MaxMessage {
		return false, 0, nil, errors.New("websocket: frame too big")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, op, payload, nil
}

// WriteMessage 写一条完整消息（服务端帧不加掩码）。
func (c *Conn) WriteMessage(opcode int, payload []byte) error {
	return c.writeFrame(opcode, payload)
}

func (c *Conn) writeFrame(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.cmu.Lock()
	closed := c.closed
	c.cmu.Unlock()
	if closed {
		return ErrClosed
	}
	hdr := make([]byte, 2, 10)
	hdr[0] = 0x80 | byte(op)
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xffff:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	c.raw.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.raw.Write(hdr); err != nil {
		return err
	}
	_, err := c.raw.Write(payload)
	return err
}

// Close 发 close 帧并关掉底层连接；重复调用无害。
func (c *Conn) Close(code int, reason string) error {
	c.cmu.Lock()
	if c.closed {
		c.cmu.Unlock()
		return nil
	}
	c.cmu.Unlock()
	payload := binary.BigEndian.AppendUint16(nil, uint16(code))
	if len(reason) > 120 {
		reason = reason[:120]
	}
	payload = append(payload, reason...)
	c.writeFrame(OpClose, payload)
	c.cmu.Lock()
	c.closed = true
	c.cmu.Unlock()
	return c.raw.Close()
}
