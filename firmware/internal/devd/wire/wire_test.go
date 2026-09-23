package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Data, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, Resize, ResizePayload(120, 40)); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, Exit, nil); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&buf)
	f, err := r.Next()
	if err != nil || f.Type != Data || string(f.Payload) != "hello" {
		t.Fatalf("帧 1 = %+v, %v", f, err)
	}
	f, err = r.Next()
	if err != nil || f.Type != Resize {
		t.Fatalf("帧 2 = %+v, %v", f, err)
	}
	cols, rows, ok := ParseResize(f.Payload)
	if !ok || cols != 120 || rows != 40 {
		t.Fatalf("resize = %d×%d ok=%v", cols, rows, ok)
	}
	f, err = r.Next()
	if err != nil || f.Type != Exit || len(f.Payload) != 0 {
		t.Fatalf("帧 3 = %+v, %v", f, err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("流尾 = %v", err)
	}
}

func TestSplitsLargeData(t *testing.T) {
	var buf bytes.Buffer
	big := bytes.Repeat([]byte("x"), MaxPayload+10)
	if err := Write(&buf, Data, big); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&buf)
	f1, err := r.Next()
	if err != nil || len(f1.Payload) != MaxPayload {
		t.Fatalf("第一帧 = %d, %v", len(f1.Payload), err)
	}
	f2, err := r.Next()
	if err != nil || len(f2.Payload) != 10 {
		t.Fatalf("第二帧 = %d, %v", len(f2.Payload), err)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	if _, err := Decode([]byte{0, 0}); err == nil {
		t.Fatal("短帧应报错")
	}
	b := Encode(Data, []byte("ab"))
	if _, err := Decode(b[:len(b)-1]); err == nil {
		t.Fatal("长度不符应报错")
	}
	if _, _, ok := ParseResize([]byte{1, 2, 3}); ok {
		t.Fatal("resize 载荷形状不对应拒绝")
	}
	f, err := Decode(b)
	if err != nil || string(f.Payload) != "ab" {
		t.Fatalf("Decode = %+v, %v", f, err)
	}
}
