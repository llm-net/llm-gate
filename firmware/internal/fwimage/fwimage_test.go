package fwimage

// 形状闸门的可执行验收：测试二进制自己就是「主模块 = 本固件 module、无
// -ldflags 版本」的真实样本，垃圾文件与空文件是反例。全程离线、不执行任何
// 被校验文件。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyAcceptsOwnTestBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	info, err := Verify(self)
	if err != nil {
		t.Fatalf("Verify(测试二进制) = %v，应当通过（同架构、同 module）", err)
	}
	// go test 不注入 -ldflags，版本应为空——正是 staging 侧要拒绝的 dev 形态。
	if info.Version != "" {
		t.Fatalf("测试二进制不应带 ldflags 版本，得到 %q", info.Version)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "not-elf")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho nope\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p); !errors.Is(err, ErrNotFirmware) {
		t.Fatalf("Verify(脚本) = %v，应为 ErrNotFirmware", err)
	}
	if _, err := Verify(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("Verify(不存在的文件) 应当报错")
	}
}

// marker 由运行期拼接构造（与 markerNeedle 同一理由：不能让完整字面量以
// 测试二进制自身的 rodata 出现，那会让「无标记」用例被自己的源码污染）。
//
// 第二字段（竖线之后）恒空；用例仍然两种都造，钉住「第二字段有内容的标记
// 照样读得出版本」。
func makeMarker(version, second string) []byte {
	return []byte("lgf" + "w1{" + version + "|" + second + "}lgf" + "w1")
}

func TestVerifyReadsAppendedMarker(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	base, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("读取测试二进制: %v", err)
	}
	p := filepath.Join(t.TempDir(), "marked")
	// 先塞一个「针命中但尾不匹配」的碎片，再放真标记：扫描器必须跳过前者。
	blob := append(append([]byte(nil), base...), []byte("lgf")...)
	blob = append(blob, []byte("w1{not-a-marker-no-terminator ")...)
	blob = append(blob, makeMarker("2608221732-2d81", "")...)
	if err := os.WriteFile(p, blob, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := Verify(p)
	if err != nil {
		t.Fatalf("Verify = %v", err)
	}
	if info.Version != "2608221732-2d81" {
		t.Fatalf("标记解析不对: %+v", info)
	}
}

func TestMarkerCrossesChunkBoundary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	base, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("读取测试二进制: %v", err)
	}
	// 把标记铺在 1 MiB 块边界上（针在块尾、体在下一块），重叠逻辑必须接住。
	pad := (1 << 20) - (len(base) % (1 << 20)) - 3
	if pad < 0 {
		pad += 1 << 20
	}
	blob := append(append([]byte(nil), base...), bytes.Repeat([]byte{0}, pad)...)
	blob = append(blob, makeMarker("2608221803-4f0c", "")...)
	p := filepath.Join(t.TempDir(), "boundary")
	if err := os.WriteFile(p, blob, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := Verify(p)
	if err != nil {
		t.Fatalf("Verify = %v", err)
	}
	if info.Version != "2608221803-4f0c" {
		t.Fatalf("跨块标记解析不对: %+v", info)
	}
}
