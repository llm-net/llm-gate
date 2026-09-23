package wsock

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		defer c.Close(CloseNormal, "")
		for {
			op, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(op, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server) string { return "ws://" + strings.TrimPrefix(ts.URL, "http://") + "/" }

func TestEcho(t *testing.T) {
	ts := echoServer(t)
	c, _, err := Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, size := range []int{0, 10, 200, 70000} {
		payload := bytes.Repeat([]byte("z"), size)
		if err := c.WriteMessage(OpBinary, payload); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != OpBinary || !bytes.Equal(got, payload) {
			t.Fatalf("size %d: op=%d len=%d err=%v", size, op, len(got), err)
		}
	}
	if err := c.WriteMessage(OpText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if op, got, err := c.ReadMessage(); err != nil || op != OpText || string(got) != "hi" {
		t.Fatalf("text: %d %q %v", op, got, err)
	}
	// ping 得到 pong。
	if err := c.WriteMessage(OpPing, []byte("p")); err != nil {
		t.Fatal(err)
	}
	if op, got, err := c.ReadMessage(); err != nil || op != OpPong || string(got) != "p" {
		t.Fatalf("pong: %d %q %v", op, got, err)
	}
	// close 得到 close。
	if err := c.WriteMessage(OpClose, []byte{0x03, 0xe8}); err != nil {
		t.Fatal(err)
	}
	if op, _, err := c.ReadMessage(); err != nil || op != OpClose {
		t.Fatalf("close: %d %v", op, err)
	}
}

func TestRejectsNonUpgrade(t *testing.T) {
	ts := echoServer(t)
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("状态 = %d", resp.StatusCode)
	}
}

func TestRejectsUnmaskedClientFrame(t *testing.T) {
	ts := echoServer(t)
	c, _, err := Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 直接写一个未加掩码的帧：服务端须以协议错误关闭。
	if _, err := c.raw.Write([]byte{0x82, 0x01, 'x'}); err != nil {
		t.Fatal(err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil || op != OpClose || len(payload) < 2 || int(payload[0])<<8|int(payload[1]) != CloseProtocolError {
		t.Fatalf("期望协议错误关闭，得到 op=%d payload=%v err=%v", op, payload, err)
	}
}

func TestFragmentedMessage(t *testing.T) {
	ts := echoServer(t)
	c, _, err := Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	write := func(fin bool, op int, data []byte) {
		hdr := byte(op)
		if fin {
			hdr |= 0x80
		}
		frame := []byte{hdr, 0x80 | byte(len(data)), 1, 2, 3, 4}
		for i, b := range data {
			frame = append(frame, b^byte(i%4+1))
		}
		if _, err := c.raw.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	write(false, OpBinary, []byte("ab"))
	write(false, OpContinuation, []byte("cd"))
	write(true, OpContinuation, []byte("e"))
	op, got, err := c.ReadMessage()
	if err != nil || op != OpBinary || string(got) != "abcde" {
		t.Fatalf("分片重组 = %d %q %v", op, got, err)
	}
}
