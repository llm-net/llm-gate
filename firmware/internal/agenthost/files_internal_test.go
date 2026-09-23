package agenthost

// 文件面脚本的语义验收：把 listScript / readScript 经 shCommand 包好，交本机 /bin/sh 跑，
// 再用生产里的解析函数解输出——验的是怪名字（空格、换行、以 - 开头、隐藏文件）、指向目录
// 的链接、断链、截断与各种退出码。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func runFiles(t *testing.T, script, home, stdin string) ([]byte, int) {
	t.Helper()
	out, code := runSh(t, shCommand(script), home, stdin)
	return []byte(out), code
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListScript(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "d")
	if err := os.MkdirAll(filepath.Join(dir, "sub dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "b.txt"), "hello")
	mustWrite(t, filepath.Join(dir, "A.md"), "# x")
	mustWrite(t, filepath.Join(dir, ".hidden"), "")
	mustWrite(t, filepath.Join(dir, "..dots"), "")
	mustWrite(t, filepath.Join(dir, "-rf"), "x")
	mustWrite(t, filepath.Join(dir, "line\nbreak"), "x")
	mustWrite(t, filepath.Join(dir, "it's"), "x")
	if err := os.Symlink(filepath.Join(dir, "sub dir"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "broken")); err != nil {
		t.Fatal(err)
	}

	out, code := runFiles(t, listScript, home, dir+"\n")
	if code != 0 {
		t.Fatalf("退出码 = %d，输出 %q", code, out)
	}
	l, err := parseListing(out)
	if err != nil {
		t.Fatalf("解析: %v（输出 %q）", err, out)
	}
	if l.Path != dir || l.Parent != home || l.Home != home || l.Truncated {
		t.Fatalf("表头 = %+v", l)
	}
	var names []string
	byName := map[string]FileEntry{}
	for _, e := range l.Entries {
		names = append(names, e.Name)
		byName[e.Name] = e
	}
	want := []string{"link", "sub dir", "-rf", "..dots", ".hidden", "A.md", "b.txt", "broken", "it's", "line\nbreak"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("条目 = %q，要 %q", names, want)
	}
	if e := byName["b.txt"]; e.Dir || e.Size != 5 || e.Mode != "-rw-r--r--" || e.Path != filepath.Join(dir, "b.txt") || e.ModTime.IsZero() || e.Owner == "" {
		t.Fatalf("b.txt = %+v", e)
	}
	if e := byName["link"]; !e.Dir || !e.Symlink {
		t.Fatalf("link = %+v", e)
	}
	if e := byName["broken"]; e.Dir || !e.Symlink {
		t.Fatalf("broken = %+v", e)
	}
	if e := byName["sub dir"]; !e.Dir || e.Symlink || !strings.HasPrefix(e.Mode, "d") {
		t.Fatalf("sub dir = %+v", e)
	}

	// 空路径 = 家目录；空目录没有 stat 行。
	empty := filepath.Join(home, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = runFiles(t, listScript, home, "\n")
	if l, err := parseListing(out); code != 0 || err != nil || l.Path != home {
		t.Fatalf("家目录: code=%d err=%v", code, err)
	}
	out, code = runFiles(t, listScript, home, empty+"\n")
	if l, err := parseListing(out); code != 0 || err != nil || len(l.Entries) != 0 {
		t.Fatalf("空目录: code=%d err=%v", code, err)
	}
	// 根目录没有上一级。
	out, _ = runFiles(t, listScript, home, "/\n")
	if l, err := parseListing(out); err != nil || l.Path != "/" || l.Parent != "" {
		t.Fatalf("根目录: %+v %v", l, err)
	}

	for _, c := range []struct {
		path string
		code int
	}{
		{filepath.Join(home, "nope"), exitNotFound},
		{filepath.Join(dir, "b.txt"), exitNotDir},
	} {
		if _, code := runFiles(t, listScript, home, c.path+"\n"); code != c.code {
			t.Errorf("%s 退出码 = %d，要 %d", c.path, code, c.code)
		}
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(home, "locked")
		if err := os.Mkdir(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(locked, 0o755)
		if _, code := runFiles(t, listScript, home, locked+"\n"); code != exitDenied {
			t.Errorf("无权限目录退出码 = %d", code)
		}
	}
}

func TestListScriptTruncates(t *testing.T) {
	home := t.TempDir()
	for i := range ListLimit + 5 {
		mustWrite(t, filepath.Join(home, fmt.Sprintf("f%05d", i)), "")
	}
	out, code := runFiles(t, listScript, home, home+"\n")
	l, err := parseListing(out)
	if code != 0 || err != nil {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !l.Truncated || len(l.Entries) != ListLimit {
		t.Fatalf("truncated=%v n=%d", l.Truncated, len(l.Entries))
	}
}

func TestReadScript(t *testing.T) {
	home := t.TempDir()
	text := filepath.Join(home, "a b.txt")
	mustWrite(t, text, "你好，世界\n")
	bin := filepath.Join(home, "x.bin")
	mustWrite(t, bin, "ab\x00cd")

	out, code := runFiles(t, readScript, home, text+"\n100\n0\n")
	mode, size, mod, body, err := parseRead(out)
	if code != 0 || err != nil || mode != "-rw-r--r--" || size != int64(len("你好，世界\n")) || mod.IsZero() || string(body) != "你好，世界\n" {
		t.Fatalf("读文本: code=%d err=%v mode=%q size=%d body=%q", code, err, mode, size, body)
	}
	// 上限 4 字节：读到 5 字节（多一个判截断），且停在「好」字中间。
	out, _ = runFiles(t, readScript, home, text+"\n4\n0\n")
	if _, _, _, body, _ := parseRead(out); len(body) != 5 || isBinary(body[:4], true) {
		t.Fatalf("截断读: %q", body)
	}
	out, _ = runFiles(t, readScript, home, bin+"\n100\n0\n")
	if _, _, _, body, _ := parseRead(out); !isBinary(body, false) {
		t.Fatalf("二进制判错: %q", body)
	}
	// strict：大于上限直接拒。
	if _, code := runFiles(t, readScript, home, text+"\n4\n1\n"); code != exitTooLarge {
		t.Fatalf("strict 退出码 = %d", code)
	}
	if _, code := runFiles(t, readScript, home, text+"\n100\n1\n"); code != 0 {
		t.Fatalf("strict 未超限退出码 = %d", code)
	}

	fifo := filepath.Join(home, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil && !errors.Is(err, syscall.EPERM) {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		code int
	}{
		{filepath.Join(home, "nope"), exitNotFound},
		{home, exitIsDir},
		{fifo, exitNotRegular},
	} {
		if _, code := runFiles(t, readScript, home, c.path+"\n100\n0\n"); code != c.code {
			t.Errorf("%s 退出码 = %d，要 %d", c.path, code, c.code)
		}
	}
}

func TestValidateHostPath(t *testing.T) {
	for _, c := range []struct {
		in    string
		empty bool
		want  string
		ok    bool
	}{
		{"", true, "", true},
		{"", false, "", false},
		{"relative", true, "", false},
		{"/a/../b/", false, "/b", true},
		{"/a\nb", false, "", false},
		{"/a\x00b", false, "", false},
		{"/" + strings.Repeat("a", 4096), false, "", false},
	} {
		got, err := ValidateHostPath(c.in, c.empty)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ValidateHostPath(%q, %v) = %q, %v", c.in, c.empty, got, err)
		}
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("plain\n"), false) || !isBinary([]byte{0xff, 0xfe, 'a'}, false) {
		t.Fatal("基本判定错")
	}
	half := []byte("好")[:2]
	if isBinary(append([]byte("ok"), half...), false) != true || isBinary(append([]byte("ok"), half...), true) {
		t.Fatal("截断时末尾半个字符应容许")
	}
}
