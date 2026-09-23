package devhost

// 工作节点的「工具配置」：一台主机上 devd 之外的工具——git、tmux、创作工具（FFmpeg / CJK 字体 /
// ImageMagick，studiotools.go）与 gate（LLM Gate 开发工具引导器）——的探测、安装与卸载。全部经这台主机已有的免密 SSH 连接进行，读数不
// 落库：每次进页探一次，动作之后再探一次；devd 的读数仍在主机行里（devhost.go）。
//
//   - 探测以 SSH 用户身份跑一段 POSIX sh（经 stdin 交给 /bin/sh -s，不拼参数），只报
//     「装没装、什么版本、有没有包管理器 / curl」；读 gate 配置时只取 base_url 与
//     「有没有 Key」，Key 明文不读、不回。
//   - 安装 git / tmux 与创作工具要 root：与安装 devd 同一条 privileged 纪律（root 直接、免密 sudo -n、
//     否则 sudo -S 以那一次请求里的口令），按主机上的包管理器选一条非交互安装命令。git / tmux 在
//     请求里同步装；创作工具只经主机的动作队列（devtooljobs.go）装。tmux 是
//     工作空间终端持久会话的前提（没有它 devd 退化成不持久的登录 shell）；装完顺手刷新主机行里
//     devd 自述的 tmux 读数。
//   - 安装 / 升级 gate 以 SSH 用户身份跑设备自己的 /gate-helper/install.sh（主机要有
//     curl 并能按给定地址访问设备），首次安装须带一把 API Key：gate bootstrap 没有
//     Key 会去读控制终端，SSH 非交互会话没有它。Key 明文经 SSH 会话交给对端，
//     短暂出现在主机上 gate 的 argv 里（与管理台生成的安装命令同一风险，界面须提示）；
//     不入库、不进日志、不进审计 detail（§15.1）。页面上的「安装 gate」不在请求里同步等完，
//     而是排进这台主机的动作队列（devtooljobs.go，与开发工具的动作同一条队列），由设备
//     后台执行、页面陪等看进度。
//   - 升级 gate 跑 `gate update`：gate 从它已保存的设备地址匿名取本平台制品、校验后原地替换，
//     不动地址、Key 与工具关联，也不经手 Key。
//   - 设置 gate 跑 `gate bootstrap <地址> [Key]`：gate 先验这个地址的健康检查与 Key，都通过
//     才一起落盘，不重装、不动工具关联；不带 Key 即沿用已保存的那把。Key 明文的纪律与安装
//     相同（短暂在 argv 里、不入库、不进日志与审计 detail）。
//   - 卸载 gate 跑 `gate uninstall --yes`：删程序、接入与保存的地址 / Key，保留工具程序。
//   - gate 管的六个开发工具（codex / claude / grok / cursor / opencode / mcode，词汇表 devd.DevTools）
//     每个一行读数：探测脚本用 awk 从 gate 的 config.json 只抽 `tools` 段的关联表（路径、
//     自检版本、来源 gate / external），api_key 所在的顶层字段不经过输出；另看 PATH 上有没有
//     同名程序（未关联时界面据此把动作叫「关联」而不是「安装」）。动作是 gate 自己的生命周期
//     子命令 `gate <tool> install | update | disconnect | uninstall --yes`，以 SSH 用户身份跑：
//     install 幂等（已有可用安装只关联，缺失才经设备取官方安装物），要主机上 gate 已装且有
//     Key；不带 TTY、不需要交互。输出只取最后几行回给界面。
//
// 受控纳管的主机没有工具配置：全部入口先按类型拒绝（CodeKindNotAllowed）。

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// 错误码（补充 devhost.go 的那组）。
const (
	// CodeToolMissing：这个动作的前提在主机上不在（没有包管理器、没有 curl、没装 gate）。
	CodeToolMissing = "tool_missing"
	// CodeHostCannotReach：主机按给定地址访问不到本设备（取不到安装脚本）。
	CodeHostCannotReach = "host_cannot_reach_device"
)

// Tools 是一台工作节点上开发工具的一次探测读数。
type Tools struct {
	Host           *store.AgentHost
	Git            ToolPackage
	Tmux           ToolPackage
	Studio         StudioTools
	PackageManager string
	Curl           bool
	Gate           ToolGate
	// DevTools 是 gate 管的开发工具，按 devd.DevTools 的顺序恒六项。
	DevTools []ToolDev
	// Home 是 SSH 用户的家目录（gate 装在它的 ~/.local/bin）。
	Home string
}

// ToolDev 是主机上一个开发工具（codex / claude / grok / cursor / opencode / mcode）的读数。
type ToolDev struct {
	Name string
	// Linked 报告 gate 的配置里有它的关联；Path / Version / Origin 是关联表里的三项
	// （Origin 为 gate = gate 经设备装的受管安装，external = 使用者自己装的）。
	Linked  bool
	Path    string
	Version string
	Origin  string
	// Found 是 PATH 上找到的同名程序路径（cursor 找 cursor-agent）；未关联时有意义。
	Found string
}

// ToolPackage 是主机上一个经包管理器安装的程序（git / tmux / ffmpeg）的读数。
type ToolPackage struct {
	Installed bool
	// Version 是 `git --version` / `tmux -V` / `ffmpeg -version` 的第一行。
	Version string
}

// ToolGate 是主机上 gate 的读数。
type ToolGate struct {
	Installed bool
	Path      string
	// Version 是 `gate version` 的输出（与固件版本同一形状）。
	Version string
	// BaseURL 是 gate 配置里保存的设备地址；Configured 报告配置里有 API Key。
	BaseURL    string
	Configured bool
}

const (
	probeTimeout    = 60 * time.Second
	pkgInstallWait  = 10 * time.Minute
	gateInstallWait = 10 * time.Minute
	gateRemoveWait  = 2 * time.Minute
)

// probeMarker 是探测脚本的首行：假主机凭它认出这是探测。
const probeMarker = "# llmgate-tools-probe"

// probeScript 经 stdin 交给主机上的 /bin/sh -s。只输出 key=value 行；gate 配置只取
// base_url 与「api_key 非空」，不输出 Key。
const probeScript = probeMarker + `
if command -v git >/dev/null 2>&1; then
  printf 'git_version=%s\n' "$(git --version 2>/dev/null | head -n 1)"
fi
if command -v tmux >/dev/null 2>&1; then
  printf 'tmux_version=%s\n' "$(tmux -V 2>/dev/null | head -n 1)"
fi
if command -v ffmpeg >/dev/null 2>&1; then
  printf 'ffmpeg_version=%s\n' "$(ffmpeg -version 2>/dev/null | head -n 1)"
  ffmpeg -hide_banner -encoders 2>/dev/null | awk '
    $1 ~ /^V/ && ($2 == "libx264" || $2 ~ /^h264_/) { h264=1 }
    $1 ~ /^A/ && $2 == "aac" { aac=1 }
    END { if (h264) print "ffmpeg_h264=yes"; if (aac) print "ffmpeg_aac=yes" }
  '
  ffmpeg -hide_banner -filters 2>/dev/null | awk '$2 == "subtitles" { print "ffmpeg_subtitles=yes"; exit }'
fi
if command -v ffprobe >/dev/null 2>&1; then echo ffprobe=yes; fi
if command -v fc-list >/dev/null 2>&1; then
  fc-list ':lang=zh' -f '%{family[0]}\n' 2>/dev/null | LC_ALL=C sort -u | awk '
    NF { n++; if (n == 1) print "fonts_cjk_family=" $0 }
    END { print "fonts_cjk_count=" n+0 }
  '
fi
for im in magick convert; do
  if command -v "$im" >/dev/null 2>&1; then
    v=$("$im" -version 2>/dev/null | head -n 1)
    case "$v" in *ImageMagick*) printf 'imagemagick_version=%s\n' "$v"; break;; esac
  fi
done
for p in apt-get dnf yum apk pacman zypper; do
  if command -v "$p" >/dev/null 2>&1; then printf 'pkg=%s\n' "$p"; break; fi
done
if command -v curl >/dev/null 2>&1; then echo curl=yes; fi
gate_path=$(command -v gate 2>/dev/null) || gate_path=
if [ -z "$gate_path" ] && [ -x "$HOME/.local/bin/gate" ]; then gate_path=$HOME/.local/bin/gate; fi
if [ -n "$gate_path" ]; then
  printf 'gate_path=%s\n' "$gate_path"
  printf 'gate_version=%s\n' "$("$gate_path" version 2>/dev/null | head -n 1)"
fi
cfg=${XDG_CONFIG_HOME:-$HOME/.config}/gate/config.json
if [ -f "$cfg" ]; then
  printf 'gate_base=%s\n' "$(sed -n 's/^[[:space:]]*"base_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" | head -n 1)"
  if grep -Eq '"api_key"[[:space:]]*:[[:space:]]*"[^"]+"' "$cfg" 2>/dev/null; then echo gate_key=yes; fi
fi
printf 'home=%s\n' "$HOME"
if [ -f "$cfg" ]; then
  awk '
    /^  "tools": \{/ { intools=1; next }
    intools && /^  \}/ { intools=0 }
    intools && /^    "[a-z]+": \{/ { s=index($0, "\""); e=index(substr($0, s+1), "\""); tool=substr($0, s+1, e-1); next }
    intools && tool != "" && /^      "(binary_path|detected_version|origin)": "/ {
      key=$1; gsub(/[":]/, "", key)
      val=$0; sub(/^[^:]*: "/, "", val); sub(/",?$/, "", val)
      printf "tool_%s_%s=%s\n", tool, key, val
    }
  ' "$cfg" 2>/dev/null
fi
for t in codex claude grok opencode mcode; do
  p=$(command -v "$t" 2>/dev/null) && printf 'tool_%s_found=%s\n' "$t" "$p"
done
p=$(command -v cursor-agent 2>/dev/null) && printf 'tool_cursor_found=%s\n' "$p"
exit 0
`

// Tools 探一次这台工作节点上的开发工具。
func (m *Manager) Tools(ctx context.Context, id int64) (*Tools, error) {
	host, err := m.workerHost(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	return m.probe(ctx, c, host)
}

func (m *Manager) probe(ctx context.Context, c *agenthost.Conn, host *store.AgentHost) (*Tools, error) {
	out, stderr, code, err := m.run(ctx, c, "/bin/sh -s", probeScript, probeTimeout)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("探测主机上的开发工具失败（退出码 %d）%s", code, tail(stderr))}
	}
	return parseProbe(host, out), nil
}

func parseProbe(host *store.AgentHost, out string) *Tools {
	t := &Tools{Host: host, DevTools: make([]ToolDev, len(devd.DevTools))}
	index := map[string]int{}
	for i, name := range devd.DevTools {
		t.DevTools[i] = ToolDev{Name: name}
		index[name] = i
	}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if rest, isTool := strings.CutPrefix(key, "tool_"); isTool {
			name, field, _ := strings.Cut(rest, "_")
			if i, known := index[name]; known {
				dev := &t.DevTools[i]
				switch field {
				case "binary_path":
					dev.Linked, dev.Path = true, value
				case "detected_version":
					dev.Linked, dev.Version = true, value
				case "origin":
					dev.Linked, dev.Origin = true, value
				case "found":
					dev.Found = value
				}
			}
			continue
		}
		switch key {
		case "git_version":
			t.Git = ToolPackage{Installed: true, Version: value}
		case "tmux_version":
			t.Tmux = ToolPackage{Installed: true, Version: value}
		case "ffmpeg_version":
			t.Studio.FFmpeg.ToolPackage = ToolPackage{Installed: value != "", Version: value}
		case "ffprobe":
			t.Studio.FFmpeg.FFprobe = value == "yes"
		case "ffmpeg_h264":
			t.Studio.FFmpeg.H264 = value == "yes"
		case "ffmpeg_aac":
			t.Studio.FFmpeg.AAC = value == "yes"
		case "ffmpeg_subtitles":
			t.Studio.FFmpeg.Subtitles = value == "yes"
		case "fonts_cjk_count":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				t.Studio.FontsCJK.Count = n
			}
		case "fonts_cjk_family":
			t.Studio.FontsCJK.Family = value
		case "imagemagick_version":
			t.Studio.ImageMagick = ToolPackage{Installed: value != "", Version: value}
		case "pkg":
			t.PackageManager = value
		case "curl":
			t.Curl = value == "yes"
		case "gate_path":
			t.Gate.Installed = true
			t.Gate.Path = value
		case "gate_version":
			t.Gate.Version = value
		case "gate_base":
			t.Gate.BaseURL = value
		case "gate_key":
			t.Gate.Configured = value == "yes"
		case "home":
			t.Home = value
		}
	}
	t.Studio.Ready = t.Studio.FFmpeg.Ready() && t.Studio.FontsCJK.Count > 0
	return t
}

// CheckWorker 只看主机行：不存在答 host_not_found，受控纳管答 host_kind_not_allowed；不碰主机。
// 管理面在解封密钥之前先问一次，免得为一台根本不许装 gate 的主机泄一次明文。
func (m *Manager) CheckWorker(ctx context.Context, id int64) error {
	_, err := m.workerHost(ctx, id)
	return err
}

// workerHost 取主机行并要求它是工作节点。
func (m *Manager) workerHost(ctx context.Context, id int64) (*store.AgentHost, error) {
	host, err := m.hostRow(ctx, id)
	if err != nil {
		return nil, err
	}
	if !host.AllowsTools() {
		return nil, &Error{Code: CodeKindNotAllowed, Msg: "受控纳管的主机没有工具配置页，只能使用 Agent远控。"}
	}
	return host, nil
}

// ---- git / tmux 与创作工具的包安装 ----

// Packages 是可经 InstallPackage 同步安装的程序；创作工具（studioPackages 的行名）只经动作队列装。
// 实际包名由 packageCommand 按包管理器选择。
var Packages = []string{"git", "tmux"}

// pkgInstallCommands 是各包管理器的非交互安装命令模板（root 身份，%s 是包名）。脚本里不能有
// 单引号（privileged 把它整段放进 sh -c '…'）。
var pkgInstallCommands = map[string]string{
	"apt-get": "export DEBIAN_FRONTEND=noninteractive; apt-get update -q && apt-get install -y -q %s",
	"dnf":     "dnf install -y -q %s",
	"yum":     "yum install -y -q %s",
	"apk":     "apk add --no-progress %s",
	"pacman":  "pacman -Sy --noconfirm --noprogressbar %s",
	"zypper":  "zypper --non-interactive install %s",
}

// PackageInstallRequest 是包安装的入参。Name 取自 Packages，队列里的创作工具是 studioPackages 的行名
// （ffmpeg / fonts-cjk / imagemagick）；Password 同 InstallRequest。
type PackageInstallRequest struct {
	ID       int64
	Name     string
	Password string
}

// InstallPackage 用主机上的包管理器装 git 或 tmux，装完再探一次。装 tmux 之后若主机上有
// devd，顺手问一次它的自述写回主机行（tmux 读数）；问不到不算失败，留给「检查 devd」。
func (m *Manager) InstallPackage(ctx context.Context, req PackageInstallRequest) (*Tools, error) {
	if !slices.Contains(Packages, req.Name) {
		return nil, &Error{Code: CodeInvalidHost, Msg: fmt.Sprintf("不认识的程序 %q", req.Name)}
	}
	res, err := m.installPackage(ctx, req, func(string, string) {})
	if res != nil {
		return res.Tools, err
	}
	return nil, err
}

func (m *Manager) installPackage(ctx context.Context, req PackageInstallRequest, progress devToolProgress) (*GateInstallResult, error) {
	if !slices.Contains(Packages, req.Name) && !IsStudioTool("studio/"+req.Name) {
		return nil, &Error{Code: CodeInvalidHost, Msg: fmt.Sprintf("不认识的程序 %q", req.Name)}
	}
	host, err := m.workerHost(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if err := needRootFor(host, req.Password, "安装 "+req.Name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout+pkgInstallWait)
	defer cancel()
	progress("connecting", "")
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	progress("probing", "")
	tools, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	command, err := packageCommand(tools.PackageManager, req.Name)
	if err != nil {
		return nil, err
	}
	command, stdin, viaSudo := privileged(host.Username, command, req.Password)
	// A remote command must never echo this request's sudo password into a queue frame.
	redact := func(s string) string {
		if req.Password != "" {
			s = strings.ReplaceAll(s, req.Password, "[REDACTED]")
		}
		// The line observer trims progress lines before emitting them.
		if trimmed := strings.TrimSpace(req.Password); trimmed != "" {
			s = strings.ReplaceAll(s, trimmed, "[REDACTED]")
		}
		return s
	}
	progress("running", "")
	observe := &lineObserver{emit: func(line string) { progress("running", redact(line)) }}
	result, err := c.Run(ctx, agenthost.RunRequest{Command: command, Stdin: stdin, Timeout: pkgInstallWait, OutputLimit: outputLimit, Observe: observe})
	if err != nil {
		return nil, fromAgent(err)
	}
	out, stderr, code := redact(result.Stdout), redact(result.Stderr), result.ExitCode
	// Even a failed package command may have installed some dependencies. Keep its fresh readings.
	progress("verifying", "")
	if req.Name == "tmux" && code == 0 && host.Devd.Installed() {
		if info, err := newDaemonClient(c, specFor(host)).info(ctx); err == nil {
			if row, err := m.record(ctx, m.stateOf(host), info); err == nil {
				host = row
			}
		}
	}
	after, probeErr := m.probe(ctx, c, host)
	res := &GateInstallResult{Tools: after, Output: lastLines(stderr+"\n"+out, 6)}
	if code != 0 {
		if viaSudo {
			return res, sudoFailure(code, stderr, "在主机上安装 "+req.Name+" 失败")
		}
		return res, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("在主机上安装 %s 失败（退出码 %d）%s", req.Name, code, tail(stderr))}
	}
	if probeErr != nil {
		return res, probeErr
	}
	switch req.Name {
	case "ffmpeg":
		if !after.Studio.FFmpeg.Ready() {
			return res, &Error{Code: CodeToolMissing, Msg: "安装结束，但 FFmpeg、ffprobe、H.264 / AAC 编码器或 subtitles 滤镜仍有缺失。请检查这台主机的发行版软件仓库与 FFmpeg 构建。"}
		}
	case "fonts-cjk":
		if after.Studio.FontsCJK.Count == 0 {
			return res, &Error{Code: CodeToolMissing, Msg: "安装结束，但 fontconfig 仍未发现 CJK 字体。请检查字体包与字体缓存。"}
		}
	case "imagemagick":
		if !after.Studio.ImageMagick.Installed {
			return res, &Error{Code: CodeToolMissing, Msg: "安装结束，但主机上仍找不到 ImageMagick。"}
		}
	}
	return res, nil
}

// needRootFor 同 needRoot，动作名由调用方给。
func needRootFor(host *store.AgentHost, password, what string) error {
	if isRoot(host.Username) || host.SudoNoPasswd || password != "" {
		return nil
	}
	return &Error{Code: CodeSudoFailed, Msg: fmt.Sprintf(
		"%s需要 root 权限，而 %s 在这台主机上没有免密 sudo：请填写它的口令（只用这一次），或先在「重新纳管」里配置免密 sudo。", what, host.Username)}
}

// ---- gate ----

// GateInstallRequest 是「安装 / 升级 gate」的入参。
type GateInstallRequest struct {
	ID int64
	// BaseURL 是主机访问本设备的地址（站点根，http / https）。
	BaseURL string
	// Key 是要写进 gate 的 API Key 明文；首次安装必填，升级时留空沿用主机上已保存的。
	// 用完即弃：不入库、不进日志与审计。
	Key string
}

// GateInstallResult 是安装的结果：探测读数外加安装脚本的最后几行输出（gate 会说出地址
// 与 Key 验证的结果；不含 Key）。
type GateInstallResult struct {
	Tools  *Tools
	Output string
}

var (
	baseHostRE = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:.]+\](:[0-9]{1,5})?$`)
	keyRE      = regexp.MustCompile(`^[A-Za-z0-9._\-]{8,256}$`)
	pathRE     = regexp.MustCompile(`^/[A-Za-z0-9._/\-]+$`)
)

// normalizeBaseURL 把设备地址收成站点根：只认 http / https，不带 userinfo、路径、查询串
// 或 fragment；主机名只收字母、数字、点、连字符（或方括号 IPv6），这样拼进命令时
// 不需要额外转义。
func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || !baseHostRE.MatchString(u.Host) {
		return "", &Error{Code: CodeInvalidHost, Msg: "设备地址须是 http:// 或 https:// 开头的站点根，例如 http://192.168.1.10"}
	}
	return u.Scheme + "://" + u.Host, nil
}

// 安装命令的退出码：取脚本失败与建临时文件失败各占一个，不与安装脚本自己的退出码混。
const (
	gateExitFetchFailed = 98
	gateExitNoTempFile  = 97
)

// gateInstallCommand 拼安装命令：主机上的 curl 先把设备自己的安装脚本下到临时文件（取不到
// 就以 98 退出、把 curl 的错误留在 stderr——不用 `curl | sh`，那样管道只看 sh 的退出码，
// curl 失败时 sh 读到空输入照样退出 0），再由 sh 跑它、按位置参数收地址与（可选）Key。
// 地址与 Key 都已收窄到不含引号、空白与控制字符。
func gateInstallCommand(base, key string) string {
	args := "'" + base + "'"
	if key != "" {
		args += " '" + key + "'"
	}
	return fmt.Sprintf(`t=$(mktemp) || exit %d; if ! curl -fSsL --max-time 120 -o "$t" '%s/gate-helper/install.sh'; then rm -f "$t"; exit %d; fi; sh "$t" %s; rc=$?; rm -f "$t"; exit $rc`,
		gateExitNoTempFile, base, gateExitFetchFailed, args)
}

// normalizeGateInstall 收窄安装入参：地址只认站点根，Key 只认 keyRE 的形态。页面走的队列
// 在入队那一刻就做这一步（400 立刻回），执行时不再重验。
func normalizeGateInstall(req GateInstallRequest) (base, key string, err error) {
	base, err = normalizeBaseURL(req.BaseURL)
	if err != nil {
		return "", "", err
	}
	key = strings.TrimSpace(req.Key)
	if key != "" && !keyRE.MatchString(key) {
		return "", "", &Error{Code: CodeInvalidHost, Msg: "API Key 形态非法"}
	}
	return base, key, nil
}

// InstallGate 在主机上以 SSH 用户身份安装或升级 gate。页面走的是 EnqueueGateInstall 的队列
// （devtooljobs.go），这条同步形态留给测试与只做一件事的调用方。
func (m *Manager) InstallGate(ctx context.Context, req GateInstallRequest) (*GateInstallResult, error) {
	return m.installGate(ctx, req, nil)
}

func (m *Manager) installGate(ctx context.Context, req GateInstallRequest, progress devToolProgress) (*GateInstallResult, error) {
	if progress == nil {
		progress = func(string, string) {}
	}
	host, err := m.workerHost(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	base, key, err := normalizeGateInstall(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout+gateInstallWait)
	defer cancel()
	progress("connecting", "")
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	progress("probing", "")
	tools, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	if !tools.Curl {
		return nil, &Error{Code: CodeToolMissing, Msg: "主机上没有 curl：安装 gate 要用它从设备取安装脚本与压缩包，请先安装 curl。"}
	}
	if key == "" && !tools.Gate.Configured {
		return nil, &Error{Code: CodeInvalidHost, Msg: "这台主机上的 gate 还没有 API Key：首次安装请选一把密钥。"}
	}
	progress("running", "")
	// 安装脚本的输出里没有 Key（它只说地址与 Key 验证的结果、装到了哪里），逐行交给进度回调。
	observe := &lineObserver{emit: func(line string) { progress("running", line) }}
	res, err := c.Run(ctx, agenthost.RunRequest{Command: gateInstallCommand(base, key), Timeout: gateInstallWait, OutputLimit: outputLimit, Observe: observe})
	if err != nil {
		return nil, fromAgent(err)
	}
	out, stderr, code := res.Stdout, res.Stderr, res.ExitCode
	switch code {
	case 0:
	case gateExitFetchFailed:
		return nil, &Error{Code: CodeHostCannotReach, Msg: fmt.Sprintf(
			"主机从 %s 取不到安装脚本：请确认主机能解析并访问这个地址（HTTPS 须是主机信任的证书；经公网入口时该入口须开放 /gate-helper/）。主机上 curl 的报错", base) + tail(lastLines(stderr+"\n"+out, 4))}
	case gateExitNoTempFile:
		return nil, &Error{Code: CodeCommandFailed, Msg: "主机上建不了临时文件（mktemp 失败）" + tail(lastLines(stderr, 3))}
	default:
		return nil, &Error{Code: CodeCommandFailed, Msg: "在主机上安装 gate 失败" + tail(lastLines(stderr+"\n"+out, 6))}
	}
	progress("verifying", "")
	after, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	if !after.Gate.Installed {
		return nil, &Error{Code: CodeCommandFailed, Msg: "安装脚本已结束，但主机上仍找不到 gate" + tail(lastLines(out, 4))}
	}
	return &GateInstallResult{Tools: after, Output: lastLines(out, 6)}, nil
}

// installedGate 打开到主机的连接并确认 gate 已装、路径形态正常、配置里有设备地址。
// 调用方负责关闭返回的连接。
func (m *Manager) installedGate(ctx context.Context, host *store.AgentHost, needBase bool) (*agenthost.Conn, *Tools, error) {
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, nil, fromAgent(err)
	}
	tools, err := m.probe(ctx, c, host)
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	if !tools.Gate.Installed {
		c.Close()
		return nil, nil, &Error{Code: CodeToolMissing, Msg: "主机上没有装 gate：请先安装，安装时一并选择地址与密钥。"}
	}
	if needBase && tools.Gate.BaseURL == "" {
		c.Close()
		return nil, nil, &Error{Code: CodeToolMissing, Msg: "主机上 gate 的配置里没有设备地址：请先用「设置 gate」选择地址与密钥。"}
	}
	if !pathRE.MatchString(tools.Gate.Path) {
		c.Close()
		return nil, nil, &Error{Code: CodeCommandFailed, Msg: "主机上 gate 的路径形态异常：" + tools.Gate.Path}
	}
	return c, tools, nil
}

// UpdateGate 在主机上跑 `gate update`：gate 从已保存的设备地址匿名取本平台压缩包，校验版本、
// 大小与 SHA-256 后原地替换；已是最新也算成功。地址、Key 与工具关联都不动。
func (m *Manager) UpdateGate(ctx context.Context, id int64) (*GateInstallResult, error) {
	host, err := m.workerHost(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout+gateInstallWait)
	defer cancel()
	c, tools, err := m.installedGate(ctx, host, true)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	out, stderr, code, err := m.run(ctx, c, "'"+tools.Gate.Path+"' update", "", gateInstallWait)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: "在主机上升级 gate 失败" + tail(lastLines(stderr+"\n"+out, 6))}
	}
	after, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	return &GateInstallResult{Tools: after, Output: lastLines(out, 6)}, nil
}

// GateConfigRequest 是「设置 gate」的入参：设备地址必填；Key 为空即沿用主机上已保存的。
// Key 用完即弃：不入库、不进日志与审计。
type GateConfigRequest struct {
	ID      int64
	BaseURL string
	Key     string
}

// ConfigureGate 在主机上跑 `gate bootstrap <地址> [Key]`：gate 先访问这个地址的健康检查、
// 再用 Key 取一次运行配置，都通过才把地址与 Key 一起落盘，失败保留原配置。不重装、不动
// 工具关联。没带 Key 又没有已保存的 Key 时答 invalid_host：bootstrap 会去读控制终端。
func (m *Manager) ConfigureGate(ctx context.Context, req GateConfigRequest) (*GateInstallResult, error) {
	host, err := m.workerHost(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	base, err := normalizeBaseURL(req.BaseURL)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(req.Key)
	if key != "" && !keyRE.MatchString(key) {
		return nil, &Error{Code: CodeInvalidHost, Msg: "API Key 形态非法"}
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	c, tools, err := m.installedGate(ctx, host, false)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if key == "" && !tools.Gate.Configured {
		return nil, &Error{Code: CodeInvalidHost, Msg: "这台主机上的 gate 还没有 API Key：请选一把密钥。"}
	}
	command := "'" + tools.Gate.Path + "' bootstrap '" + base + "'"
	if key != "" {
		command += " '" + key + "'"
	}
	out, stderr, code, err := m.run(ctx, c, command, "", gateRemoveWait)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: "在主机上设置 gate 失败" + tail(lastLines(stderr+"\n"+out, 6))}
	}
	after, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	return &GateInstallResult{Tools: after, Output: lastLines(out, 6)}, nil
}

// UninstallGate 在主机上跑 gate uninstall --yes，然后再探一次。
func (m *Manager) UninstallGate(ctx context.Context, id int64) (*Tools, error) {
	host, err := m.workerHost(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout)
	defer cancel()
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	tools, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	if !tools.Gate.Installed {
		return nil, &Error{Code: CodeToolMissing, Msg: "主机上没有装 gate。"}
	}
	if !pathRE.MatchString(tools.Gate.Path) {
		return nil, &Error{Code: CodeCommandFailed, Msg: "主机上 gate 的路径形态异常，请手工卸载：" + tools.Gate.Path}
	}
	out, stderr, code, err := m.run(ctx, c, "'"+tools.Gate.Path+"' uninstall --yes", "", gateRemoveWait)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: "在主机上卸载 gate 失败" + tail(lastLines(stderr+"\n"+out, 6))}
	}
	return m.probe(ctx, c, host)
}

// ---- gate 管的开发工具 ----

// DevToolAction 是对一个开发工具能做的动作（gate 的生命周期子命令）。
type DevToolAction string

const (
	// DevToolInstall：`gate <tool> install`——已有可用安装只关联，缺失才安装（先直连官方源，不可达再经设备）。
	DevToolInstall DevToolAction = "install"
	// DevToolUpdate：`gate <tool> update`——只升级 gate 受管的安装；外部安装由 gate 拒绝。
	DevToolUpdate DevToolAction = "update"
	// DevToolDisconnect：`gate <tool> disconnect`——撤销接入配置，保留程序与会话。
	DevToolDisconnect DevToolAction = "disconnect"
	// DevToolUninstall：`gate <tool> uninstall --yes`——删 gate 装的程序并撤销接入，保留会话。
	DevToolUninstall DevToolAction = "uninstall"
)

// devToolCommands 是各动作拼进命令行的子命令（恒定字面量，不含用户输入）。
var devToolCommands = map[DevToolAction]string{
	DevToolInstall:    "install",
	DevToolUpdate:     "update",
	DevToolDisconnect: "disconnect",
	DevToolUninstall:  "uninstall --yes",
}

// DevToolRequest 是「对主机上一个开发工具做动作」的入参。
type DevToolRequest struct {
	ID     int64
	Tool   string
	Action DevToolAction
}

// devToolWait 是安装 / 升级的等待上限：安装物经设备下载，Cursor 的整树最大。
const devToolWait = 15 * time.Minute

// devToolProgress 是一次开发工具动作的进度回调：stage 是阶段（connecting / probing /
// running / verifying），line 是主机上 gate 刚吐出的一行（空串 = 只换阶段）。
type devToolProgress func(stage, line string)

// DevTool 在主机上以 SSH 用户身份跑 `gate <tool> <动作>`，跑完再探一次。工具名不在
// devd.DevTools、动作不认识答 invalid_host；主机没装 gate 或（安装 / 升级时）gate 没有
// Key 答 tool_missing；gate 自己拒绝（外部渠道管理、这把 Key 没有该工具可用的模型…）
// 原样带回 host_command_failed。页面走的是 EnqueueDevTools 的队列（devtooljobs.go），
// 这条同步形态留给测试与只做一件事的调用方。
func (m *Manager) DevTool(ctx context.Context, req DevToolRequest) (*GateInstallResult, error) {
	return m.devTool(ctx, req, nil)
}

func (m *Manager) devTool(ctx context.Context, req DevToolRequest, progress devToolProgress) (*GateInstallResult, error) {
	if err := devd.ValidTool(req.Tool); err != nil {
		return nil, &Error{Code: CodeInvalidHost, Msg: "不认识的开发工具：" + req.Tool}
	}
	sub, ok := devToolCommands[req.Action]
	if !ok {
		return nil, &Error{Code: CodeInvalidHost, Msg: "不认识的动作：" + string(req.Action)}
	}
	if progress == nil {
		progress = func(string, string) {}
	}
	host, err := m.workerHost(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	wait := gateRemoveWait
	if req.Action == DevToolInstall || req.Action == DevToolUpdate {
		wait = devToolWait
	}
	ctx, cancel := context.WithTimeout(ctx, hostTimeout+wait)
	defer cancel()
	progress("connecting", "")
	c, err := m.hosts.Open(ctx, host.ID)
	if err != nil {
		return nil, fromAgent(err)
	}
	defer c.Close()
	progress("probing", "")
	tools, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	if !tools.Gate.Installed {
		return nil, &Error{Code: CodeToolMissing, Msg: "主机上没有装 gate：开发工具由 gate 安装与关联，请先安装 gate。"}
	}
	if (req.Action == DevToolInstall || req.Action == DevToolUpdate) && (!tools.Gate.Configured || tools.Gate.BaseURL == "") {
		return nil, &Error{Code: CodeToolMissing, Msg: "主机上的 gate 还没有设备地址或 API 密钥：安装 / 升级开发工具要经它访问设备，请先配置密钥。"}
	}
	if !pathRE.MatchString(tools.Gate.Path) {
		return nil, &Error{Code: CodeCommandFailed, Msg: "主机上 gate 的路径形态异常：" + tools.Gate.Path}
	}
	progress("running", "")
	observe := &lineObserver{emit: func(line string) { progress("running", line) }}
	res, err := c.Run(ctx, agenthost.RunRequest{Command: "'" + tools.Gate.Path + "' " + req.Tool + " " + sub, Timeout: wait, OutputLimit: outputLimit, Observe: observe})
	if err != nil {
		return nil, fromAgent(err)
	}
	out, stderr, code := res.Stdout, res.Stderr, res.ExitCode
	if code != 0 {
		return nil, &Error{Code: CodeCommandFailed, Msg: fmt.Sprintf("在主机上执行 gate %s %s 失败", req.Tool, sub) + tail(lastLines(stderr+"\n"+out, 6))}
	}
	progress("verifying", "")
	after, err := m.probe(ctx, c, host)
	if err != nil {
		return nil, err
	}
	return &GateInstallResult{Tools: after, Output: lastLines(stderr+"\n"+out, 6)}, nil
}

// lineObserver 把远端输出按行（\n 或 \r——gate 在终端上用 \r 刷进度行，非终端时逐行）
// 切开交给 emit；写入来自 SSH 读取 goroutine，自己加锁。半截行留到下一段拼上。
type lineObserver struct {
	mu   sync.Mutex
	rest []byte
	emit func(line string)
}

func (o *lineObserver) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rest = append(o.rest, p...)
	for {
		i := bytes.IndexAny(o.rest, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(o.rest[:i]))
		o.rest = o.rest[i+1:]
		if line != "" {
			o.emit(line)
		}
	}
	if len(o.rest) > 4096 {
		// 没有换行的超长一段：不是进度行，丢掉免得无限积累。
		o.rest = o.rest[:0]
	}
	return len(p), nil
}

// lastLines 取输出的最后 n 个非空行。
func lastLines(s string, n int) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, "\n")
}
