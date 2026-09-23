package agenthost

// Agent远控页「文件」页签的只读文件面：凭访问证书登上主机，以 SSH 用户的身份列一层目录、
// 读一个文件的开头一段（预览）或整份原字节（图片预览与下载）。不写、不改、不删——改动
// 主机是智能体的事，经它的工具走、进操作日志；这里只让管理员看。
//
// 与 exec.go 同一套纪律：每次操作一条连接、做完即断，不需要 devd（受控纳管的主机也能
// 看）；路径经 stdin 交给脚本，不拼进命令行；文件内容与文件名都不进日志（§15.1 同一条线，
// 主机上的文件可能是任何东西）。

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// 文件面的错误码。管理面映射状态码见 internal/admin/hostagentfiles.go。
const (
	CodeInvalidPath      = "invalid_path"
	CodePathNotFound     = "path_not_found"
	CodeNotDirectory     = "not_directory"
	CodeIsDirectory      = "is_directory"
	CodeNotRegularFile   = "not_regular_file"
	CodePermissionDenied = "permission_denied"
	CodeFileTooLarge     = "file_too_large"
)

const (
	// ListLimit 是一次列目录最多带回的条目数；更多的只报 truncated（/usr/lib 这类目录
	// 动辄上万项，一层清单也不该把几 MB 搬回来）。
	ListLimit = 2000
	// PreviewLimit 是文本预览读取的字节上限；超过即截断、页面标明只看了开头。
	PreviewLimit = 1 << 20
	// RawLimit 是整份读取（图片预览、下载）的上限：结果整份在设备内存里过一遍，不给大文件。
	RawLimit = 16 << 20
	// filesTimeout 是一条文件命令的上限（包括读 RawLimit 那么大的文件）。
	filesTimeout = 60 * time.Second
)

// 脚本退出码：与下面几段脚本约定，折成上面的错误码。
const (
	exitNotFound   = 64
	exitNotDir     = 65
	exitDenied     = 66
	exitIsDir      = 67
	exitNotRegular = 68
	exitTooLarge   = 69
)

// listScript 列一层目录。stdin 第一行是绝对路径（空 = 家目录）。stdout 先是 NUL 分隔的
// 表头：实际路径、家目录、是否截断、条目数；再是每个条目一段 NUL 结尾的「d|- + 名字」
// （d = 目录或指向目录的链接）；最后是每个条目一行 `stat -c`（权限串、大小、修改时刻、
// 属主），与名字按顺序对应。名字可以含空格、换行等任何字符，只有 stat 那几列是按行的，
// 列里不会有换行。stat 走 ./ 前缀，免得以 - 开头的名字被当成选项。
var listScript = `# llmgate-host-files list
IFS= read -r p || [ -n "$p" ] || p=
[ -n "$p" ] || p=$HOME
[ -n "$p" ] || p=/
[ -e "$p" ] || exit 64
[ -d "$p" ] || exit 65
cd "$p" 2>/dev/null && [ -r . ] || exit 66
n=0
more=0
set --
for f in * .[!.]* ..?*; do
	[ -e "$f" ] || [ -L "$f" ] || continue
	if [ "$n" -ge ` + strconv.Itoa(ListLimit) + ` ]; then more=1; break; fi
	n=$((n + 1))
	set -- "$@" "./$f"
done
printf '%s\0%s\0%s\0%s\0' "$(pwd)" "$HOME" "$more" "$n"
for f do
	if [ -d "$f" ]; then printf 'd%s\0' "${f#./}"; else printf '%s%s\0' - "${f#./}"; fi
done
[ "$n" -eq 0 ] || LC_ALL=C stat -c '%A %s %Y %U' "$@" 2>/dev/null
`

// readScript 读一个普通文件。stdin 第一行是绝对路径，第二行是读取上限（字节）；stdout
// 首行是 `stat -L -c`（权限串、大小、修改时刻），其后是文件开头至多「上限 + 1」字节
// （多出的那一个字节只用来判断截断）。strict 为 1 时文件大于上限直接退出 69、不读内容。
// /proc、/sys 下的文件大小报 0 但能读，所以截断看读到的字节数，不看 stat 的大小。
const readScript = `# llmgate-host-files read
IFS= read -r p || [ -n "$p" ] || exit 64
IFS= read -r limit || [ -n "$limit" ] || exit 64
IFS= read -r strict || [ -n "$strict" ] || strict=0
[ -e "$p" ] || exit 64
[ -d "$p" ] && exit 67
[ -f "$p" ] || exit 68
[ -r "$p" ] || exit 66
meta=$(LC_ALL=C stat -L -c '%A %s %Y' "$p" 2>/dev/null) || exit 66
if [ "$strict" = 1 ]; then
	size=${meta#* }
	size=${size%% *}
	[ "$size" -le "$limit" ] || exit 69
fi
printf '%s\n' "$meta"
head -c $((limit + 1)) "$p" 2>/dev/null || exit 66
`

// FileEntry 是目录里的一项。Dir 对指向目录的符号链接也为真（点进去就是那个目录）。
type FileEntry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Dir     bool      `json:"dir"`
	Symlink bool      `json:"symlink,omitempty"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	Owner   string    `json:"owner,omitempty"`
	ModTime time.Time `json:"mod_time"`
}

// Listing 是一层目录。Parent 在根目录时为空；Truncated 表示条目超过 ListLimit 只带回了一部分。
type Listing struct {
	Path      string      `json:"path"`
	Parent    string      `json:"parent,omitempty"`
	Home      string      `json:"home,omitempty"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}

// FilePreview 是一个文件的开头一段。Binary 为真时 Content 为空；Truncated 表示只读了
// 前 PreviewLimit 字节。
type FilePreview struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Mode      string    `json:"mode"`
	ModTime   time.Time `json:"mod_time"`
	Binary    bool      `json:"binary"`
	Truncated bool      `json:"truncated"`
	Content   string    `json:"content"`
}

// FileRaw 是一个文件的整份原字节（≤ RawLimit）。
type FileRaw struct {
	Path    string
	ModTime time.Time
	Content []byte
}

// ValidateHostPath 收窄交给文件面的路径：空串（= 家目录，仅列目录可用）或绝对路径，不含
// NUL 与换行（路径按行经 stdin 交给脚本），至多 4096 字节。返回清理过的路径。
func ValidateHostPath(p string, allowEmpty bool) (string, error) {
	if p == "" && allowEmpty {
		return "", nil
	}
	if p == "" || !strings.HasPrefix(p, "/") || len(p) > 4096 || strings.ContainsAny(p, "\x00\n\r") {
		return "", &Error{Code: CodeInvalidPath, Msg: "路径须是主机上的绝对路径"}
	}
	return path.Clean(p), nil
}

// ListDir 列主机上一层目录。p 为空即 SSH 用户的家目录。
func (m *Manager) ListDir(ctx context.Context, id int64, p string) (*Listing, error) {
	p, err := ValidateHostPath(p, true)
	if err != nil {
		return nil, err
	}
	out, err := m.filesRun(ctx, id, listScript, p+"\n", 4<<20)
	if err != nil {
		return nil, err
	}
	return parseListing(out)
}

// PreviewFile 读主机上一个普通文件的开头一段。
func (m *Manager) PreviewFile(ctx context.Context, id int64, p string) (*FilePreview, error) {
	p, err := ValidateHostPath(p, false)
	if err != nil {
		return nil, err
	}
	out, err := m.filesRun(ctx, id, readScript, fmt.Sprintf("%s\n%d\n0\n", p, PreviewLimit), PreviewLimit+4096)
	if err != nil {
		return nil, err
	}
	mode, size, mod, body, err := parseRead(out)
	if err != nil {
		return nil, err
	}
	fp := &FilePreview{Path: p, Size: size, Mode: mode, ModTime: mod}
	if len(body) > PreviewLimit {
		body, fp.Truncated = body[:PreviewLimit], true
	}
	if size < int64(len(body)) {
		fp.Size = int64(len(body))
	}
	if isBinary(body, fp.Truncated) {
		fp.Binary = true
		return fp, nil
	}
	if fp.Truncated {
		// 截在一个多字节字符中间时退回到完整字符，页面不出现半个字。
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	fp.Content = string(body)
	return fp, nil
}

// ReadFile 读主机上一个普通文件的整份原字节；大于 RawLimit 答 CodeFileTooLarge。
func (m *Manager) ReadFile(ctx context.Context, id int64, p string) (*FileRaw, error) {
	p, err := ValidateHostPath(p, false)
	if err != nil {
		return nil, err
	}
	out, err := m.filesRun(ctx, id, readScript, fmt.Sprintf("%s\n%d\n1\n", p, RawLimit), RawLimit+4096)
	if err != nil {
		return nil, err
	}
	_, _, mod, body, err := parseRead(out)
	if err != nil {
		return nil, err
	}
	if len(body) > RawLimit {
		return nil, tooLarge()
	}
	return &FileRaw{Path: p, ModTime: mod, Content: body}, nil
}

// filesRun 登上主机跑一段文件脚本，把约定的退出码折成错误码。
func (m *Manager) filesRun(ctx context.Context, id int64, script, stdin string, limit int) ([]byte, error) {
	c, err := m.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.Run(ctx, RunRequest{Command: shCommand(script), Stdin: stdin, Timeout: filesTimeout, OutputLimit: limit})
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
	case exitNotFound:
		return nil, &Error{Code: CodePathNotFound, Msg: "主机上没有这个路径"}
	case exitNotDir:
		return nil, &Error{Code: CodeNotDirectory, Msg: "这个路径不是目录"}
	case exitDenied:
		return nil, &Error{Code: CodePermissionDenied, Msg: "登录用户没有读取这个路径的权限"}
	case exitIsDir:
		return nil, &Error{Code: CodeIsDirectory, Msg: "这个路径是目录"}
	case exitNotRegular:
		return nil, &Error{Code: CodeNotRegularFile, Msg: "只能预览普通文件（设备文件、管道、套接字不能读）"}
	case exitTooLarge:
		return nil, tooLarge()
	default:
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("读取主机文件失败（退出码 %d）", res.ExitCode)}
	}
	if res.Truncated && len(res.Stdout) >= limit {
		return nil, &Error{Code: CodeCommandFailed, Msg: "主机返回的内容超出上限"}
	}
	return []byte(res.Stdout), nil
}

// shCommand 把脚本整段交给 /bin/sh 而不是登录 shell：登录 shell 可能是 zsh / fish，
// 通配符与 read 的语义不同。脚本里的单引号按 '\'' 转义。
func shCommand(script string) string {
	return "/bin/sh -c '" + strings.ReplaceAll(script, "'", `'\''`) + "'"
}

func tooLarge() error {
	return &Error{Code: CodeFileTooLarge, Msg: fmt.Sprintf("文件超过 %d MB，不能在这里整份读取", RawLimit>>20)}
}

func malformed() error {
	return &Error{Code: CodeCommandFailed, Msg: "主机返回的目录信息无法解析（目录可能正在变化，请刷新）"}
}

// parseListing 解 listScript 的输出。
func parseListing(out []byte) (*Listing, error) {
	fields := func() (string, bool) {
		i := bytes.IndexByte(out, 0)
		if i < 0 {
			return "", false
		}
		f := string(out[:i])
		out = out[i+1:]
		return f, true
	}
	cwd, ok1 := fields()
	home, ok2 := fields()
	more, ok3 := fields()
	countStr, ok4 := fields()
	count, err := strconv.Atoi(countStr)
	if !ok1 || !ok2 || !ok3 || !ok4 || err != nil || count < 0 || !strings.HasPrefix(cwd, "/") {
		return nil, malformed()
	}
	l := &Listing{Path: cwd, Home: home, Truncated: more == "1", Entries: make([]FileEntry, 0, count)}
	if cwd != "/" {
		l.Parent = path.Dir(cwd)
	}
	for range count {
		f, ok := fields()
		if !ok || len(f) < 2 {
			return nil, malformed()
		}
		l.Entries = append(l.Entries, FileEntry{Name: f[1:], Path: path.Join(cwd, f[1:]), Dir: f[0] == 'd'})
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if count == 0 {
		lines = nil
	}
	if len(lines) != count {
		return nil, malformed()
	}
	for i, line := range lines {
		cols := strings.Fields(line)
		if len(cols) < 3 || cols[0] == "" {
			return nil, malformed()
		}
		e := &l.Entries[i]
		e.Mode = cols[0]
		e.Symlink = cols[0][0] == 'l'
		e.Size, _ = strconv.ParseInt(cols[1], 10, 64)
		if sec, err := strconv.ParseInt(cols[2], 10, 64); err == nil {
			e.ModTime = time.Unix(sec, 0).UTC()
		}
		if len(cols) > 3 {
			e.Owner = cols[3]
		}
	}
	// 目录在前，再按名字（不分大小写）排。
	sort.SliceStable(l.Entries, func(i, j int) bool {
		a, b := l.Entries[i], l.Entries[j]
		if a.Dir != b.Dir {
			return a.Dir
		}
		la, lb := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})
	return l, nil
}

// parseRead 解 readScript 的输出：首行元数据，其余是内容。
func parseRead(out []byte) (mode string, size int64, mod time.Time, body []byte, err error) {
	i := bytes.IndexByte(out, '\n')
	if i < 0 {
		return "", 0, time.Time{}, nil, &Error{Code: CodeCommandFailed, Msg: "主机返回的文件信息无法解析"}
	}
	cols := strings.Fields(string(out[:i]))
	if len(cols) < 3 {
		return "", 0, time.Time{}, nil, &Error{Code: CodeCommandFailed, Msg: "主机返回的文件信息无法解析"}
	}
	size, _ = strconv.ParseInt(cols[1], 10, 64)
	if sec, e := strconv.ParseInt(cols[2], 10, 64); e == nil {
		mod = time.Unix(sec, 0).UTC()
	}
	return cols[0], size, mod, out[i+1:], nil
}

// isBinary 判断内容是不是文本：含 NUL 或不是合法 UTF-8 即二进制。被截断时容许末尾
// 最多 3 字节的半个字符。
func isBinary(b []byte, truncated bool) bool {
	head := b
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	if utf8.Valid(b) {
		return false
	}
	if truncated {
		for cut := 1; cut <= 3 && cut <= len(b); cut++ {
			if utf8.Valid(b[:len(b)-cut]) {
				return false
			}
		}
	}
	return true
}
