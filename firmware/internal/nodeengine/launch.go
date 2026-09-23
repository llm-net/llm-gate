package nodeengine

// 在工作节点上拉起一段会话的公共部分：SSH 连接、远程转发与转发服务、准备脚本、包装脚本。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

const (
	// setupTimeout 是准备脚本的上限（建目录、落几个小文件）。
	setupTimeout = 60 * time.Second
	// closeGrace 是关 stdin 后等 CLI 自己退出的时长，超过即 KILL。
	closeGrace = 3 * time.Second
	// diagnoseWait 是启动失败时等远端命令结束、好拿到退出码与 stderr 的时长。
	diagnoseWait = 2 * time.Second
)

// 包装脚本与准备脚本的退出码（与 CLI 自己的退出码错开）。
const (
	exitNoInstanceDir = 90
	exitNoWorkdir     = 95
)

// instanceDirRE 是准备脚本落成的实例目录形状。它会拼进包装脚本与 Claude 的 --settings JSON，
// 所以只收不需要转义的字符（家目录里有空格、引号的节点答明原因）。
var instanceDirRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/llmgate(?:-[0-9]+)?/engine/s\.[A-Za-z0-9]+$`)

// remote 是一段会话在节点上的全部资源。
type remote struct {
	conn   *agenthost.Conn
	ln     net.Listener
	srv    *http.Server
	stream *agenthost.Stream
	// base 是节点上经远程转发可达的设备地址（http://127.0.0.1:<端口>）；dir 是实例目录。
	base string
	dir  string

	closeOnce sync.Once
}

// launchSpec 是一种引擎要落的文件与要跑的命令。两者都依赖转发端口（base），所以按 base 给。
type launchSpec struct {
	// volatile 要求实例目录位于 tmpfs，供会自动保存会话的 CLI 使用。
	volatile bool
	// files 是实例目录下的文件名 → 内容（0600）。名字只用固定的标识符。
	files func(base string) map[string]string
	// command 是包装脚本最后一段：在工作空间目录里拉起 CLI（不要 exec，trap 要收尾）。实例目录
	// 在变量 $d 里。
	command func(base string) string
}

// start 连上节点、开转发、落文件、拉起 CLI。失败时已开的资源全部收掉。
func (o *Options) start(ctx context.Context, req Request, spec launchSpec) (*remote, error) {
	if o.Hosts == nil {
		return nil, notReady("本进程未接入主机连接")
	}
	if !strings.HasPrefix(req.Workdir, "/") || strings.ContainsAny(req.Workdir, "\n\x00") {
		return nil, notReady("工作空间在节点上的路径形态异常：" + req.Workdir)
	}
	conn, err := o.Hosts.Open(ctx, req.HostID)
	if err != nil {
		return nil, notReady("连不上工作节点：" + err.Error())
	}
	r := &remote{conn: conn}
	ok := false
	defer func() {
		if !ok {
			r.Close()
		}
	}()
	if r.ln, err = conn.Listen("127.0.0.1:0"); err != nil {
		return nil, notReady(err.Error())
	}
	port := r.ln.Addr().(*net.TCPAddr).Port
	r.base = "http://127.0.0.1:" + strconv.Itoa(port)
	proxy, err := o.proxy(req.Tools.URL)
	if err != nil {
		return nil, notReady(err.Error())
	}
	r.srv = &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go r.srv.Serve(r.ln)

	script, err := setupScriptFor(spec.files(r.base), spec.volatile)
	if err != nil {
		return nil, notReady(err.Error())
	}
	res, err := conn.Run(ctx, agenthost.RunRequest{Command: "/bin/sh -s", Stdin: script, Timeout: setupTimeout})
	if err != nil {
		return nil, notReady("在工作节点上准备引擎失败：" + err.Error())
	}
	if res.ExitCode != 0 {
		if spec.volatile {
			return nil, notReady("Grok 需要工作节点上可写的 /dev/shm 内存文件系统来保存临时会话")
		}
		return nil, notReady(fmt.Sprintf("在工作节点上准备引擎失败（退出码 %d）：建不了 ~/.cache/llmgate/engine 下的实例目录%s", res.ExitCode, tailNote(res.Stderr)))
	}
	dir := ""
	for _, line := range strings.Split(res.Stdout, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), "dir="); found {
			dir = v
		}
	}
	if !instanceDirRE.MatchString(dir) {
		return nil, notReady("工作节点上的实例目录路径含需要转义的字符（家目录里有空格或引号？），节点端引擎不支持：" + dir)
	}
	r.dir = dir
	if r.stream, err = conn.Start("/bin/sh -c " + shellQuote(wrapperScript(dir, req.Workdir, spec.command(r.base)))); err != nil {
		return nil, notReady("在工作节点上拉起引擎失败：" + err.Error())
	}
	ok = true
	return r, nil
}

// Close 关掉 CLI、删实例目录、撤转发、断连接。可重复调用。
//
// 先关 stdin 让 CLI 按 EOF 自己退出；closeGrace 内没退的，
// 先在同一连接上跑收尾脚本：按实例目录里记的 pid 把包装脚本及其全部后代（CLI、codex 的
// code-mode host、bwrap、模型起的 shell）逐个 KILL——SSH 的 KILL 信号只落在包装脚本上，单
// 靠它会把这些后代留成孤儿；然后删实例目录（trap 没跑到时补一刀）。收尾脚本走 /bin/sh -s，
// 逐个 pid 精确点名，不按名字匹配。
func (r *remote) Close() error {
	r.closeOnce.Do(func() {
		if r.stream != nil {
			_ = r.stream.Stdin.Close()
			select {
			case <-r.stream.Done():
			case <-time.After(closeGrace):
			}
		}
		if r.dir != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = r.conn.Run(ctx, agenthost.RunRequest{Command: "/bin/sh -s", Stdin: teardownScript(r.dir), Timeout: 10 * time.Second})
			cancel()
		}
		if r.stream != nil {
			r.stream.Close(closeGrace)
		}
		if r.srv != nil {
			r.srv.Close()
		}
		if r.ln != nil {
			r.ln.Close()
		}
		r.conn.Close()
	})
	return nil
}

// diagnose 给启动失败补上节点那一侧的原因：等远端命令结束一小会儿，按退出码与 stderr 尾段说明。
func (r *remote) diagnose(what string, err error) error {
	msg := what + "：" + err.Error()
	if r.stream == nil {
		return notReady(msg)
	}
	select {
	case <-r.stream.Done():
	case <-time.After(diagnoseWait):
	}
	if code, exited := r.stream.ExitCode(); exited {
		switch code {
		case exitNoWorkdir:
			msg += "（工作空间目录在节点上不存在）"
		case 126, 127:
			msg += fmt.Sprintf("（节点上找不到或执行不了引擎程序，退出码 %d）", code)
		default:
			msg += fmt.Sprintf("（引擎进程已退出，退出码 %d）", code)
		}
	}
	return notReady(msg + tailNote(r.stream.StderrTail()))
}

// proxy 是转发服务：只放行开发工具接入面与这段会话的 MCP 端点，原样交给设备的回环网关。
// 路径必须已是规范形态（不含 . / .. 段、重复斜杠），免得网关那边清洗后落到别的路由上。
func (o *Options) proxy(mcpPath string) (http.Handler, error) {
	target, err := url.Parse(strings.TrimRight(o.Upstream, "/"))
	if err != nil || target.Host == "" {
		return nil, fmt.Errorf("设备回环地址形态异常：%q", o.Upstream)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		clean := path.Clean(p)
		if strings.HasSuffix(p, "/") && clean != "/" {
			clean += "/"
		}
		allowed := clean == p && (strings.HasPrefix(p, "/agents/codex/") || strings.HasPrefix(p, "/agents/claude/") || strings.HasPrefix(p, "/agents/grok/") || (mcpPath != "" && p == mcpPath))
		if !allowed {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	}), nil
}

// setupScript 是交给节点 /bin/sh -s 的准备脚本：清掉上一次异常退出留下的实例目录（包装脚本记
// 的 pid 已不在；没有 pid 的是刚准备好还没拉起的，一小时后才算遗留），建新的实例目录，
// 以 0600 落文件，最后输出 dir=<路径>。文件内容经 heredoc（带随机定界符）原样落盘。
func setupScript(files map[string]string) (string, error) {
	return setupScriptFor(files, false)
}

func setupScriptFor(files map[string]string, volatile bool) (string, error) {
	var b strings.Builder
	b.WriteString("umask 077\n")
	if volatile {
		// 不回退到磁盘；目录须归当前用户所有且不能是软链接。
		b.WriteString(`[ "$(stat -f -c %T /dev/shm 2>/dev/null)" = tmpfs ] || exit 90
root="/dev/shm/llmgate-$(id -u)"
mkdir -m 700 "$root" 2>/dev/null || true
[ ! -L "$root" ] && [ "$(stat -c %u "$root")" = "$(id -u)" ] && [ "$(stat -c %a "$root")" = 700 ] || exit 90
base="$root/engine"
[ ! -L "$base" ] || exit 90
`)
	} else {
		b.WriteString("base=\"${XDG_CACHE_HOME:-$HOME/.cache}/llmgate/engine\"\n")
	}
	b.WriteString(`
mkdir -p "$base" || exit ` + strconv.Itoa(exitNoInstanceDir) + `
for old in "$base"/s.*; do
  [ -d "$old" ] || continue
  pid=$(cat "$old/pid" 2>/dev/null)
  if [ -n "$pid" ]; then
    kill -0 "$pid" 2>/dev/null && continue
  elif [ -z "$(find "$old" -maxdepth 0 -mmin +60 2>/dev/null)" ]; then
    continue
  fi
  rm -rf "$old"
done
d=$(mktemp -d "$base/s.XXXXXXXX") || exit ` + strconv.Itoa(exitNoInstanceDir) + `
`)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !fileNameRE.MatchString(name) {
			return "", fmt.Errorf("实例文件名形态异常：%q", name)
		}
		content := files[name]
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		delim := heredocDelimiter(content)
		fmt.Fprintf(&b, "cat > \"$d/%s\" <<'%s' || exit %d\n%s%s\n", name, delim, exitNoInstanceDir, content, delim)
	}
	b.WriteString("printf 'dir=%s\\n' \"$d\"\n")
	return b.String(), nil
}

var fileNameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)

// heredocDelimiter 取一个不出现在内容里的随机定界符。
func heredocDelimiter(content string) string {
	for {
		var buf [8]byte
		_, _ = rand.Read(buf[:])
		delim := "LLMGATE_EOF_" + hex.EncodeToString(buf[:])
		if !strings.Contains(content, delim) {
			return delim
		}
	}
}

// teardownScript 是会话关闭时交给节点 /bin/sh -s 的收尾脚本：实例目录还在（CLI 没按 EOF 退出，
// 或被 KILL 而 trap 没跑）时，按目录里记的 pid 收集包装脚本的整棵后代进程树（pgrep -P，没有
// pgrep 的用 ps --ppid），逐个 KILL，最后删目录。目录已被 trap 删掉时什么也不做。
func teardownScript(dir string) string {
	return "d=" + shellQuote(dir) + "\n" + `[ -d "$d" ] || exit 0
kids() { pgrep -P "$1" 2>/dev/null || ps -o pid= --ppid "$1" 2>/dev/null; }
walk() { echo "$1"; for c in $(kids "$1"); do walk "$c"; done; }
p=$(cat "$d/pid" 2>/dev/null)
if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then
  for x in $(walk "$p"); do kill -KILL "$x" 2>/dev/null; done
fi
rm -rf "$d"
`
}

// wrapperScript 是流式命令本身：记 pid（准备脚本据此判断目录还在用）、退出时删实例目录、
// 进工作空间目录、拉起 CLI。CLI 的 stdin / stdout 就是会话通道。
func wrapperScript(dir, workdir, command string) string {
	return "d=" + shellQuote(dir) + "\n" +
		"echo $$ > \"$d/pid\"\n" +
		"trap 'rm -rf \"$d\"' EXIT\n" +
		"trap 'exit 129' HUP\n" +
		"trap 'exit 143' TERM\n" +
		"cd " + shellQuote(workdir) + " || exit " + strconv.Itoa(exitNoWorkdir) + "\n" +
		command + "\n"
}

// shellQuote 把任意字符串包成 POSIX sh 单引号字面量。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tailNote 取 stderr 末尾一小段附在错误后面（去掉终端控制序列）；空即空串。
func tailNote(stderr string) string {
	s := strings.TrimSpace(ansiRE.ReplaceAllString(stderr, ""))
	if s == "" {
		return ""
	}
	if r := []rune(s); len(r) > 600 {
		s = "…" + string(r[len(r)-600:])
	}
	return "\n" + s
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func notReady(msg string) error {
	return &hostagent.Error{Code: hostagent.CodeEngineNotReady, Msg: msg}
}
