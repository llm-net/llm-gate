package devd

// 文件目录：列目录、读写文本文件、建目录、改名、删除。全部以守护进程所在用户的
// 身份操作，能碰到什么由主机的文件权限决定，本包不另设沙箱——这就是 devd 的
// 用途：让人在浏览器里看到并改动那台主机上自己的文件。
//
// 路径规则：相对路径按家目录解析，之后一律 filepath.Clean 成绝对路径；空串是家目录。
// 读文件上限 maxReadSize，超过只回前面一段并标 truncated；前 8 KiB 里有 NUL 字节
// 视为二进制，不回内容。写文件先写同目录临时文件再 rename，半截内容不会落到原文件。

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxReadSize  = 2 << 20
	binaryProbe  = 8 << 10
	maxWriteSize = 8 << 20
	// maxRawWrite 是按原字节写文件的上限（设备那一侧另有自己的上限）。
	maxRawWrite = 1 << 30
)

// RawFile 是按原字节写文件的结果。
type RawFile struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Entry 是目录里的一项。
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Dir     bool      `json:"dir"`
	Symlink bool      `json:"symlink,omitempty"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
}

// Listing 是一次列目录的结果。
type Listing struct {
	Path    string  `json:"path"`
	Parent  string  `json:"parent,omitempty"`
	Entries []Entry `json:"entries"`
	// GitRoot 是这个目录所在仓库的顶层（不在仓库里为空），界面据此决定 Git 面板
	// 要不要亮。
	GitRoot string `json:"git_root,omitempty"`
}

// FileContent 是读文件的结果。
type FileContent struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Binary    bool   `json:"binary"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
	Mode      string `json:"mode"`
}

// resolvePath 把请求里的路径折成绝对路径。
func (s *Server) resolvePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "~" {
		return s.home, nil
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(s.home, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.home, p)
	}
	if strings.ContainsRune(p, 0) {
		return "", &Error{Code: CodeInvalidPath, Msg: "路径不能包含 NUL 字节"}
	}
	return filepath.Clean(p), nil
}

func (s *Server) listDir(p string) (*Listing, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fsError("读取目录", abs, err)
	}
	out := &Listing{Path: abs, Entries: make([]Entry, 0, len(entries))}
	if parent := filepath.Dir(abs); parent != abs {
		out.Parent = parent
	}
	for _, de := range entries {
		e := Entry{Name: de.Name(), Path: filepath.Join(abs, de.Name())}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			e.Symlink = true
			if target, err := os.Stat(e.Path); err == nil {
				info = target
			}
		}
		e.Dir = info.IsDir()
		e.Size = info.Size()
		e.Mode = info.Mode().String()
		e.ModTime = info.ModTime()
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		return a.Name < b.Name
	})
	out.GitRoot = gitRoot(abs)
	return out, nil
}

func (s *Server) readFile(p string) (*FileContent, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fsError("读取文件", abs, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fsError("读取文件", abs, err)
	}
	if info.IsDir() {
		return nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 是目录", abs)}
	}
	out := &FileContent{Path: abs, Size: info.Size(), Mode: info.Mode().String()}
	buf, err := io.ReadAll(io.LimitReader(f, maxReadSize+1))
	if err != nil {
		return nil, fsError("读取文件", abs, err)
	}
	probe := buf
	if len(probe) > binaryProbe {
		probe = probe[:binaryProbe]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		out.Binary = true
		return out, nil
	}
	if len(buf) > maxReadSize {
		buf = buf[:maxReadSize]
		out.Truncated = true
	}
	out.Content = string(buf)
	return out, nil
}

// writeFile 整份覆盖写：先落同目录临时文件再 rename；已有文件保留权限位。
func (s *Server) writeFile(p, content string) (*FileContent, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	if len(content) > maxWriteSize {
		return nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("文件内容超过 %d 字节上限", maxWriteSize)}
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 是目录", abs)}
		}
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(abs)+".llmgate-*")
	if err != nil {
		return nil, fsError("写入文件", abs, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if _, err := tmp.WriteString(content); err != nil {
		cleanup()
		return nil, fsError("写入文件", abs, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return nil, fsError("写入文件", abs, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fsError("写入文件", abs, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return nil, fsError("写入文件", abs, err)
	}
	return s.readFile(abs)
}

// openRaw 打开一个普通文件按原字节读。
func (s *Server) openRaw(p string) (*os.File, os.FileInfo, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, fsError("读取文件", abs, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fsError("读取文件", abs, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 不是普通文件", abs)}
	}
	return f, info, nil
}

// writeRaw 把 r 的全部字节写成文件：先落同目录临时文件再 rename，读取中断（对端放弃）或超过
// maxRawWrite 都不留半成品；已有文件保留权限位。
func (s *Server) writeRaw(p string, r io.Reader) (*RawFile, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 是目录", abs)}
		}
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(abs)+".llmgate-*")
	if err != nil {
		return nil, fsError("写入文件", abs, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	n, err := io.Copy(tmp, io.LimitReader(r, maxRawWrite+1))
	if err != nil {
		cleanup()
		return nil, &Error{Code: CodeIOError, Msg: fmt.Sprintf("写入文件 %s 失败：%s", abs, err)}
	}
	if n > maxRawWrite {
		cleanup()
		return nil, &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("文件超过 %d 字节上限", maxRawWrite)}
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return nil, fsError("写入文件", abs, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fsError("写入文件", abs, err)
	}
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		return nil, fsError("写入文件", abs, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fsError("写入文件", abs, err)
	}
	return &RawFile{Path: abs, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (s *Server) mkdir(p string) (string, error) {
	abs, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fsError("创建目录", abs, err)
	}
	return abs, nil
}

func (s *Server) rename(from, to string) (string, error) {
	src, err := s.resolvePath(from)
	if err != nil {
		return "", err
	}
	dst, err := s.resolvePath(to)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 已存在", dst)}
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fsError("重命名", src, err)
	}
	return dst, nil
}

// remove 删文件或目录。目录必须显式 recursive 才整棵删——误点一下不该带走一整棵树。
func (s *Server) remove(p string, recursive bool) error {
	abs, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	if abs == "/" || abs == s.home {
		return &Error{Code: CodeInvalidPath, Msg: "不能删除根目录或家目录"}
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return fsError("删除", abs, err)
	}
	if info.IsDir() && recursive {
		if err := os.RemoveAll(abs); err != nil {
			return fsError("删除", abs, err)
		}
		return nil
	}
	if err := os.Remove(abs); err != nil {
		if info.IsDir() {
			return &Error{Code: CodeInvalidPath, Msg: fmt.Sprintf("%s 不是空目录：确认后再整棵删除", abs)}
		}
		return fsError("删除", abs, err)
	}
	return nil
}

func fsError(action, p string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return &Error{Code: CodeNotFound, Msg: fmt.Sprintf("%s 不存在", p)}
	case errors.Is(err, os.ErrPermission):
		return &Error{Code: CodeForbidden, Msg: fmt.Sprintf("%s：没有权限访问 %s", action, p)}
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return &Error{Code: CodeIOError, Msg: fmt.Sprintf("%s %s 失败：%s", action, p, pe.Err)}
	}
	return &Error{Code: CodeIOError, Msg: fmt.Sprintf("%s %s 失败：%s", action, p, err)}
}
