package wsock

// 最小客户端：只给测试用（浏览器那头是真的 WebSocket 实现）。

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

// ClientConn 是测试用的客户端连接：写帧加掩码，读帧要求服务端不加掩码。
type ClientConn struct {
	raw net.Conn
	br  *bufio.Reader
}

// Dial 对 url（ws://host/path）完成握手。
func Dial(rawURL string, header http.Header) (*ClientConn, *http.Response, error) {
	if !strings.HasPrefix(rawURL, "ws://") {
		return nil, nil, errors.New("wsock: only ws:// is supported")
	}
	rest := strings.TrimPrefix(rawURL, "ws://")
	host, path, _ := strings.Cut(rest, "/")
	path = "/" + path
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, nil, err
	}
	var keyBytes [16]byte
	rand.Read(keyBytes[:])
	key := base64.StdEncoding.EncodeToString(keyBytes[:])
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		conn.Close()
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return nil, resp, errors.New("wsock: handshake failed: " + resp.Status)
	}
	return &ClientConn{raw: conn, br: br}, resp, nil
}

// WriteMessage 写一条加掩码的消息。
func (c *ClientConn) WriteMessage(op int, payload []byte) error {
	hdr := make([]byte, 2, 14)
	hdr[0] = 0x80 | byte(op)
	n := len(payload)
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
	case n <= 0xffff:
		hdr[1] = 0x80 | 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 0x80 | 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := c.raw.Write(hdr); err != nil {
		return err
	}
	_, err := c.raw.Write(masked)
	return err
}

// ReadMessage 读一条消息（不处理分片：测试里服务端不分片）。
func (c *ClientConn) ReadMessage() (int, []byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := int(hdr[0] & 0x0f)
	if hdr[1]&0x80 != 0 {
		return 0, nil, errors.New("wsock: server frame masked")
	}
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// Close 关掉底层连接。
func (c *ClientConn) Close() error { return c.raw.Close() }
