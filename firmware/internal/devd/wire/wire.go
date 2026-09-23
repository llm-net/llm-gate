// Package wire 是工作空间终端流的帧格式：守护进程（llmgate-devd）与设备之间、
// 设备与浏览器之间都用同一种帧，设备只做逐帧搬运，不解释终端字节。
//
// 一帧 = 1 字节类型 + 4 字节大端长度 + 载荷：
//
//	Data   载荷是终端字节流（双向）；
//	Resize 浏览器 → 守护进程：载荷 4 字节，cols、rows 各 uint16 大端；
//	Exit   守护进程 → 浏览器：会话已结束，载荷是一句可展示的原因（可为空）。
//
// 载荷上限 MaxPayload：终端一次吐不出这么多，超过即视为对端出错并断开。
// 本包只有标准库依赖（守护进程二进制要保持最小）。
package wire

import (
	"encoding/binary"
	"errors"
	"io"
)

// 帧类型。
const (
	Data   byte = 0
	Resize byte = 1
	Exit   byte = 2
)

// MaxPayload 是单帧载荷上限（64 KiB）。
const MaxPayload = 64 << 10

// HeaderLen 是帧头长度。
const HeaderLen = 5

// ErrFrameTooLarge 是载荷超过 MaxPayload 的错误。
var ErrFrameTooLarge = errors.New("wire: frame too large")

// Frame 是一帧。
type Frame struct {
	Type    byte
	Payload []byte
}

// Encode 把帧编码成字节。
func Encode(typ byte, payload []byte) []byte {
	buf := make([]byte, HeaderLen+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[HeaderLen:], payload)
	return buf
}

// Decode 从一段完整的字节里解出一帧（浏览器发来的一条 WebSocket 消息就是一帧）。
func Decode(b []byte) (Frame, error) {
	if len(b) < HeaderLen {
		return Frame{}, errors.New("wire: short frame")
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if n > MaxPayload || int(n) != len(b)-HeaderLen {
		return Frame{}, ErrFrameTooLarge
	}
	return Frame{Type: b[0], Payload: b[HeaderLen:]}, nil
}

// ResizePayload 编码 Resize 帧的载荷。
func ResizePayload(cols, rows uint16) []byte {
	var p [4]byte
	binary.BigEndian.PutUint16(p[0:2], cols)
	binary.BigEndian.PutUint16(p[2:4], rows)
	return p[:]
}

// ParseResize 解 Resize 帧的载荷；形状不对返回 ok=false。
func ParseResize(p []byte) (cols, rows uint16, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(p[0:2]), binary.BigEndian.Uint16(p[2:4]), true
}

// Reader 从字节流里逐帧读。
type Reader struct {
	r   io.Reader
	hdr [HeaderLen]byte
}

// NewReader 建一个帧读取器。
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Next 读下一帧；流结束返回 io.EOF。
func (fr *Reader) Next() (Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(fr.hdr[1:5])
	if n > MaxPayload {
		return Frame{}, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Type: fr.hdr[0], Payload: payload}, nil
}

// Write 把一帧写进流。载荷超过上限时按 MaxPayload 切成多帧（只对 Data 有意义）。
func Write(w io.Writer, typ byte, payload []byte) error {
	for {
		chunk := payload
		if len(chunk) > MaxPayload {
			chunk = payload[:MaxPayload]
		}
		if _, err := w.Write(Encode(typ, chunk)); err != nil {
			return err
		}
		payload = payload[len(chunk):]
		if len(payload) == 0 {
			return nil
		}
	}
}
