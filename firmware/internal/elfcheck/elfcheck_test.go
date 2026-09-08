package elfcheck

import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// MinimalELF 造一个只有 64 字节文件头的 ELF64：debug/elf 解析它只需要头部自洽
// （无节区、无程序头）。测试专用，不是能运行的程序。
func MinimalELF(machine elf.Machine, typ elf.Type) []byte {
	b := make([]byte, 64)
	copy(b[:4], []byte{0x7f, 'E', 'L', 'F'})
	b[4] = byte(elf.ELFCLASS64)
	b[5] = byte(elf.ELFDATA2LSB)
	b[6] = byte(elf.EV_CURRENT)
	binary.LittleEndian.PutUint16(b[16:], uint16(typ))
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(b[52:], 64) // e_ehsize
	binary.LittleEndian.PutUint16(b[54:], 56) // e_phentsize
	binary.LittleEndian.PutUint16(b[58:], 64) // e_shentsize
	return b
}

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerifyMatchesHostArch(t *testing.T) {
	arm := write(t, "arm64", MinimalELF(elf.EM_AARCH64, elf.ET_EXEC))
	x86 := write(t, "amd64", MinimalELF(elf.EM_X86_64, elf.ET_DYN))
	if err := verifyFor(arm, "arm64"); err != nil {
		t.Fatalf("arm64 制品在 arm64 上应通过: %v", err)
	}
	if err := verifyFor(x86, "amd64"); err != nil {
		t.Fatalf("amd64 制品在 amd64 上应通过: %v", err)
	}
	if err := verifyFor(arm, "amd64"); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("跨架构应被拒: %v", err)
	}
	if err := verifyFor(x86, "arm64"); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("跨架构应被拒: %v", err)
	}
}

func TestVerifyRejectsNonELFAndNonExecutable(t *testing.T) {
	text := write(t, "script", []byte("#!/bin/sh\necho hi\n"))
	if err := verifyFor(text, "arm64"); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("脚本应被拒: %v", err)
	}
	rel := write(t, "rel", MinimalELF(elf.EM_AARCH64, elf.ET_REL))
	if err := verifyFor(rel, "arm64"); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("目标文件（ET_REL）应被拒: %v", err)
	}
	if err := verifyFor(filepath.Join(t.TempDir(), "missing"), "arm64"); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("不存在的文件应报 ErrNotExecutable: %v", err)
	}
}
