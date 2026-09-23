package studio

// 目录的两种落点，同一套操作（Storage）：
//   - deviceStorage：设备自己数据目录下的目录，直接读写文件系统；
//   - hostStorage：一台工作节点上的目录（~/workspaces/<name>），设备经那台主机的守护进程
//     devd（internal/devhost 透传，SSH 转发通道到本机 socket）列目录、按原字节读写、删、改名。
//     每次操作一条 SSH 连接，用完即断；大文件流着走，不整份落内存。
//
// 文件以「子目录/文件名」的路径引用（files.go 的 ValidatePath）；List 按子目录列，缺失的子目录
// 两种落点都顺手建出来（主机上经守护进程的 fs/mkdir）。缩略图不在这里：两种落点都把缩略图
// 缓存在设备上（files.go）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/devhost"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// Entry 是子目录里的一个普通文件（Name 不带目录）。
type Entry struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Object 是打开的一个文件：设备上的给 Content（可 Seek，http.ServeContent 用），主机上的给
// Body（只能顺序读）。用完 Close。
type Object struct {
	Size    int64
	ModTime time.Time
	Content io.ReadSeekCloser
	Body    io.ReadCloser
}

// Reader 是可读的那一侧。
func (o *Object) Reader() io.Reader {
	if o.Content != nil {
		return o.Content
	}
	return o.Body
}

// Close 释放底层文件或流。
func (o *Object) Close() error {
	if o.Content != nil {
		return o.Content.Close()
	}
	if o.Body != nil {
		return o.Body.Close()
	}
	return nil
}

// Storage 是一个工作空间目录的操作集。路径都已经过 ValidatePath（「子目录/文件名」），dir 已经过
// ValidDir。
type Storage interface {
	// List 列出子目录 dir 里的普通文件（不含以 . 开头的与再下一层的目录）；子目录不存在即建出来。
	List(ctx context.Context, dir string) ([]Entry, error)
	Open(ctx context.Context, p string) (*Object, error)
	// Put 把 r 写成文件（同名覆盖，先临时文件再改名）；回落盘后的大小与修改时刻（Name 为空）。
	Put(ctx context.Context, p string, r io.Reader) (Entry, error)
	Delete(ctx context.Context, p string) error
	Rename(ctx context.Context, from, to string) error
	Exists(ctx context.Context, p string) (bool, error)
}

// errTooLarge 由 capReader 在超过上限时报出：设备这一侧据此把上传判成 CodeFileTooLarge。
var errTooLarge = errors.New("studio: file exceeds the size limit")

// capReader 读到 limit 字节之后再有数据就报 errTooLarge（上传经它流向落点，超限即中断）。
type capReader struct {
	r     io.Reader
	left  int64
	over  bool
	limit int64
}

func newCapReader(r io.Reader, limit int64) *capReader {
	return &capReader{r: r, left: limit, limit: limit}
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.over {
		return 0, errTooLarge
	}
	if c.left <= 0 {
		// 再探一个字节：还有数据就是超限。
		var probe [1]byte
		n, err := c.r.Read(probe[:])
		if n > 0 {
			c.over = true
			return 0, errTooLarge
		}
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// ---- 设备上的目录 ----

type deviceStorage struct {
	dir string
}

// full 是路径 p 在设备文件系统上的绝对位置。
func (d deviceStorage) full(p string) string { return filepath.Join(d.dir, filepath.FromSlash(p)) }

func (d deviceStorage) List(_ context.Context, dir string) ([]Entry, error) {
	entries, err := os.ReadDir(d.full(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(d.full(dir), 0o700); err != nil {
				return nil, fmt.Errorf("创建工作空间目录: %w", err)
			}
			return []Entry{}, nil
		}
		return nil, fmt.Errorf("读取工作空间目录: %w", err)
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		// 与主机落点同一条规则：隐藏文件、非普通文件与名字不合 ValidateFileName 的条目都不进清单
		//（后者无法按名字经工具或端点操作，列出来也用不了）。
		if strings.HasPrefix(name, ".") || !e.Type().IsRegular() || ValidateFileName(name) != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{Name: name, Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

func (d deviceStorage) Open(_ context.Context, p string) (*Object, error) {
	f, err := os.Open(d.full(p))
	if err != nil {
		return nil, &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
	}
	return &Object{Size: info.Size(), ModTime: info.ModTime(), Content: f}, nil
}

func (d deviceStorage) Put(_ context.Context, p string, r io.Reader) (Entry, error) {
	target := d.full(p)
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Entry{}, fmt.Errorf("创建工作空间目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		return Entry{}, fmt.Errorf("写文件: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, r)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return Entry{}, err
		}
		return Entry{}, fmt.Errorf("写文件: %w", err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return Entry{}, fmt.Errorf("写文件: %w", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return Entry{}, fmt.Errorf("写文件: %w", err)
	}
	return Entry{Size: info.Size(), ModTime: info.ModTime()}, nil
}

func (d deviceStorage) Delete(_ context.Context, p string) error {
	if err := os.Remove(d.full(p)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
		}
		return fmt.Errorf("删文件: %w", err)
	}
	return nil
}

func (d deviceStorage) Rename(_ context.Context, from, to string) error {
	if _, err := os.Stat(d.full(from)); err != nil {
		return &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
	}
	if _, err := os.Stat(d.full(to)); err == nil {
		return &Error{Code: CodeFileExists, Msg: "目标文件名已被占用"}
	}
	if err := os.MkdirAll(filepath.Dir(d.full(to)), 0o700); err != nil {
		return fmt.Errorf("改名: %w", err)
	}
	if err := os.Rename(d.full(from), d.full(to)); err != nil {
		return fmt.Errorf("改名: %w", err)
	}
	return nil
}

func (d deviceStorage) Exists(_ context.Context, p string) (bool, error) {
	_, err := os.Stat(d.full(p))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ---- 工作节点上的目录（经 devd）----

type hostStorage struct {
	dev    *devhost.Manager
	hostID int64
	dir    string
}

// full 是路径 p（或子目录名）在主机上的绝对位置。
func (h hostStorage) full(p string) string { return path.Join(h.dir, p) }

// isNotFound 报告 err 是不是守护进程答的「不存在」。
func isFileNotFound(err error) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == CodeFileNotFound
}

// mkdir 在主机上建子目录（守护进程的 fs/mkdir 是 mkdir -p 语义）。
func (h hostStorage) mkdir(ctx context.Context, dir string) error {
	body, _ := json.Marshal(map[string]any{"path": h.full(dir)})
	resp, err := h.dev.Proxy(ctx, h.hostID, http.MethodPost, "fs/mkdir", nil, strings.NewReader(string(body)), "application/json")
	if err != nil {
		return daemonErr("创建主机上的工作空间子目录", err)
	}
	if resp.Status/100 != 2 {
		return daemonErr("创建主机上的工作空间子目录", proxyErr(resp.Status, resp.Body))
	}
	return nil
}

// daemonErr 把守护进程 / 透传层的错误折成本包错误：守护进程答的 not_found 是文件不在，
// 其余原样带上可读原因（主机连不上、没装守护进程…）。
func daemonErr(action string, err error) error {
	var de *devhost.Error
	if errors.As(err, &de) {
		switch de.Code {
		case devd.CodeNotFound:
			return &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
		}
		return &Error{Code: de.Code, Msg: action + "：" + de.Msg}
	}
	return fmt.Errorf("%s: %w", action, err)
}

// proxyErr 把非 2xx 的守护进程响应折成 devhost.Error（带守护进程给的 code / message）。
func proxyErr(status int, raw []byte) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error.Code != "" {
		return &devhost.Error{Code: body.Error.Code, Msg: body.Error.Message, Status: status}
	}
	return &devhost.Error{Code: devhost.CodeCommandFailed, Msg: fmt.Sprintf("守护进程答 HTTP %d", status), Status: status}
}

func (h hostStorage) List(ctx context.Context, dir string) ([]Entry, error) {
	resp, err := h.dev.Proxy(ctx, h.hostID, http.MethodGet, "fs/list", url.Values{"path": {h.full(dir)}}, nil, "")
	if err != nil {
		return nil, daemonErr("读取主机上的工作空间目录", err)
	}
	if resp.Status/100 != 2 {
		err := daemonErr("读取主机上的工作空间目录", proxyErr(resp.Status, resp.Body))
		if isFileNotFound(err) {
			// 子目录还没有（空间是旧版建的，或被主机上的命令删了）：建出来，当空目录。
			if err := h.mkdir(ctx, dir); err != nil {
				return nil, err
			}
			return []Entry{}, nil
		}
		return nil, err
	}
	var listing devd.Listing
	if err := json.Unmarshal(resp.Body, &listing); err != nil {
		return nil, fmt.Errorf("读取主机上的工作空间目录: 响应不是合法 JSON")
	}
	out := make([]Entry, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		if e.Dir || strings.HasPrefix(e.Name, ".") || ValidateFileName(e.Name) != nil {
			continue
		}
		out = append(out, Entry{Name: e.Name, Size: e.Size, ModTime: e.ModTime})
	}
	return out, nil
}

func (h hostStorage) Open(ctx context.Context, p string) (*Object, error) {
	resp, err := h.dev.ProxyStream(ctx, h.hostID, http.MethodGet, "fs/raw", url.Values{"path": {h.full(p)}}, nil, "", nil)
	if err != nil {
		return nil, daemonErr("读取主机上的文件", err)
	}
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, daemonErr("读取主机上的文件", proxyErr(resp.StatusCode, raw))
	}
	obj := &Object{Body: resp.Body, Size: -1}
	if n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil {
		obj.Size = n
	}
	if t, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		obj.ModTime = t
	}
	return obj, nil
}

func (h hostStorage) Put(ctx context.Context, p string, r io.Reader) (Entry, error) {
	// 子目录不在时守护进程写不了临时文件而答不存在，请求体又只能读一遍、不能失败后重放：
	// 先用一条 mkdir -p 语义的请求确保子目录在（多一条 SSH 连接，换来不缓冲正文）。
	if dir, _ := SplitPath(p); dir != "" {
		if err := h.mkdir(ctx, dir); err != nil {
			return Entry{}, err
		}
	}
	resp, err := h.dev.ProxyStream(ctx, h.hostID, http.MethodPut, "fs/raw", url.Values{"path": {h.full(p)}}, r, "application/octet-stream", nil)
	if err != nil {
		// 上传经 capReader 超限时是我们自己中断了请求体。
		if errors.Is(err, errTooLarge) || strings.Contains(err.Error(), errTooLarge.Error()) {
			return Entry{}, errTooLarge
		}
		return Entry{}, daemonErr("写入主机上的文件", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return Entry{}, daemonErr("写入主机上的文件", proxyErr(resp.StatusCode, raw))
	}
	var out devd.RawFile
	if err := json.Unmarshal(raw, &out); err != nil {
		return Entry{}, fmt.Errorf("写入主机上的文件: 响应不是合法 JSON")
	}
	return Entry{Size: out.Size, ModTime: out.ModTime}, nil
}

func (h hostStorage) Delete(ctx context.Context, p string) error {
	body, _ := json.Marshal(map[string]any{"path": h.full(p)})
	resp, err := h.dev.Proxy(ctx, h.hostID, http.MethodPost, "fs/delete", nil, strings.NewReader(string(body)), "application/json")
	if err != nil {
		return daemonErr("删除主机上的文件", err)
	}
	if resp.Status/100 != 2 {
		return daemonErr("删除主机上的文件", proxyErr(resp.Status, resp.Body))
	}
	return nil
}

func (h hostStorage) Rename(ctx context.Context, from, to string) error {
	if ok, err := h.Exists(ctx, from); err != nil {
		return err
	} else if !ok {
		return &Error{Code: CodeFileNotFound, Msg: "文件不存在"}
	}
	if ok, err := h.Exists(ctx, to); err != nil {
		return err
	} else if ok {
		return &Error{Code: CodeFileExists, Msg: "目标文件名已被占用"}
	}
	if toDir, _ := SplitPath(to); toDir != "" {
		if err := h.mkdir(ctx, toDir); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(map[string]any{"from": h.full(from), "to": h.full(to)})
	resp, err := h.dev.Proxy(ctx, h.hostID, http.MethodPost, "fs/rename", nil, strings.NewReader(string(body)), "application/json")
	if err != nil {
		return daemonErr("改名主机上的文件", err)
	}
	if resp.Status/100 != 2 {
		return daemonErr("改名主机上的文件", proxyErr(resp.Status, resp.Body))
	}
	return nil
}

func (h hostStorage) Exists(ctx context.Context, p string) (bool, error) {
	dir, name := SplitPath(p)
	entries, err := h.List(ctx, dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// storage 按工作空间的落点选实现。
func (m *Manager) storage(ws *store.Workspace) (Storage, error) {
	if !ws.OnHost() {
		return deviceStorage{dir: m.Dir(ws)}, nil
	}
	if m.devHosts == nil {
		return nil, &Error{Code: devhost.CodeUnavailable, Msg: "本进程未接入工作节点的守护进程透传"}
	}
	if ws.Path == "" || !strings.HasPrefix(ws.Path, "/") {
		return nil, &Error{Code: CodeInvalidInput, Msg: "工作空间在主机上的路径形态异常"}
	}
	return hostStorage{dev: m.devHosts, hostID: ws.HostID, dir: ws.Path}, nil
}
