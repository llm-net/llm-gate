// Package devhosttest 是离线的假开发主机：一台 agenthosttest 的假 SSH 主机（真跑 SSH 协议，
// 口令与公钥登录都认，解释设备下发的上传 / 安装 / 卸载守护进程的命令，并把到守护进程
// socket 的转发通道接到进程内的真 llmgate-devd（internal/devd）上）。它服务
// internal/devhost 的流程验收与 internal/admin 的端点验收，全程不出网、不起外部进程
// （守护进程的 tmux 部分除外，由用例自行 skip）。
//
// 本文件里的错误串一律用英文：测试支撑包里的中文会被 i18n 提取器收进译文目录。
package devhosttest

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost/agenthosttest"
	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/modeld"
)

// Options 是假主机的初始状态。
type Options struct {
	// Password 是接受的登录口令（纳管与 sudo -S 都用它；必填）。
	Password string
	// Username 是接受的用户名（空 = 不校验）。
	Username string
	// NoSudo 为真时主机上没有 sudo。
	NoSudo bool
	// SudoNoPasswd 是初始的免密 sudo 状态（纳管时勾「配置免密 sudo」会把它置真）。
	SudoNoPasswd bool
	// Arch 是 uname -m 的输出（空 = x86_64）。
	Arch string
	// Home 是守护进程的家目录（空 = t.TempDir()）。
	Home string
	// NoForward 为真时 sshd 禁止转发通道（模拟 AllowStreamLocalForwarding no）：像
	// OpenSSH 那样只回一句 open failed，与「守护进程没起来」在客户端看来分不开。
	NoForward bool
	// ForwardProhibited 让被禁的转发如实回 administratively prohibited（OpenSSH 不这么
	// 答，只有别的 SSH 实现会）。只在 NoForward 为真时有意义。
	ForwardProhibited bool

	// 工具配置（internal/devhost/tools.go）的初始状态：
	// Git / Tmux / FFmpeg 是 `git --version` / `tmux -V` / `ffmpeg -version` 的输出（空 = 没装）；Pkg 是包管理器名（空 =
	// 没有）；NoCurl 为真时主机上没有 curl；GateVersion 是已装 gate 的版本（空 = 没装），
	// GateBase / GateKey 是它配置里的设备地址与「有没有 Key」。
	Git             string
	Tmux            string
	FFmpeg          string
	FFprobe         bool
	FFmpegCodecs    []string
	FFmpegSubtitles bool
	FontsCJK        []string
	ImageMagick     string
	Pkg             string
	NoCurl          bool
	GateVersion     string
	GateBase        string
	GateKey         bool
	// GateUnreachable 为真时主机按任何地址都取不到安装脚本（像 DNS 解析不了那样）。
	GateUnreachable bool
	// DevToolsFound 是 PATH 上已有的开发工具程序（键是工具名，值是路径）：`gate <tool>
	// install` 遇到它只关联为 external，不装受管副本。
	DevToolsFound map[string]string
	// DevToolsLinked 是 gate 配置里初始就有的关联。
	DevToolsLinked map[string]DevToolState

	// Repos 是主机能连到的 git 仓库（键是地址）：工作空间脚本里的 ls-remote / clone 按它
	// 裁决。不在表里的地址按「找不到仓库」答；User / Token 非空即私有仓库，凭证不对按
	// 「认证失败」答。
	Repos map[string]FakeRepo

	// Command 接管设备脚本之外的任意命令（创作工作空间的 exec 工具用例）：先于内建识别
	// 查询，handled 为假时退回内建处理。
	Command func(command, stdin string) (stdout string, code int, handled bool)
}

// FakeRepo 是假主机眼里的一个远端仓库。
type FakeRepo struct {
	Branches []string
	// User / Token 非空即私有仓库：脚本经 stdin 交来的凭证要与之相同。
	User  string
	Token string
}

// FakeWorkspace 是假主机上建成的一个工作空间目录。
type FakeWorkspace struct {
	Name   string
	Path   string
	URL    string
	Branch string
	// User / Token 是克隆时脚本收到的凭证（公开仓库为空）。
	User  string
	Token string
}

// GateState 是假主机上 gate 的现状。
type GateState struct {
	Version string
	Base    string
	HasKey  bool
	// LastKey 是最近一次安装 / `bootstrap` 命令里带的 Key（空 = 没带）。
	LastKey string
}

// DevToolState 是 gate 配置里一个开发工具的关联（binary_path / detected_version / origin）。
type DevToolState struct {
	Path    string
	Version string
	Origin  string
}

// installedGateVersion 是经安装脚本装上的 gate 自述版本。
const installedGateVersion = "fake-gate"

// updatedGateVersion 是 `gate update` 之后的 gate 自述版本。
const updatedGateVersion = "fake-gate-updated"

// Exec 是假主机看见的一条命令。
type Exec = agenthosttest.Exec

// Host 是一台假开发主机。
type Host struct {
	t   *testing.T
	opt Options
	// SSH 是底下那台假 SSH 主机（纳管流程直接用它）。
	SSH *agenthosttest.Server

	daemon *devd.Server
	ln     net.Listener
	// modeld 是进程内的真 llmgate-modeld（模型服务节点的守护进程），mln 是它的回环监听。
	modeld *modeld.Server
	mln    net.Listener

	mu              sync.Mutex
	files           map[string][]byte
	installed       bool
	binary          []byte
	user            string
	sockets         []string
	git             string
	tmux            string
	ffmpeg          string
	ffprobe         bool
	ffmpegCodecs    []string
	ffmpegSubtitles bool
	fontsCJK        []string
	imageMagick     string
	gate            GateState
	devTools        map[string]DevToolState
	// workspaces 按名字记主机上建成的工作空间目录。
	workspaces map[string]FakeWorkspace
}

// New 起一台假主机，测试结束自动关闭。
func New(t *testing.T, opt Options) *Host {
	t.Helper()
	if opt.Arch == "" {
		opt.Arch = "x86_64"
	}
	if opt.Home == "" {
		opt.Home = t.TempDir()
	}
	h := &Host{t: t, opt: opt, files: map[string][]byte{}, git: opt.Git, tmux: opt.Tmux, ffmpeg: opt.FFmpeg, workspaces: map[string]FakeWorkspace{},
		ffprobe: opt.FFprobe, ffmpegCodecs: opt.FFmpegCodecs, ffmpegSubtitles: opt.FFmpegSubtitles, fontsCJK: opt.FontsCJK, imageMagick: opt.ImageMagick,
		gate: GateState{Version: opt.GateVersion, Base: opt.GateBase, HasKey: opt.GateKey}, devTools: map[string]DevToolState{}}
	for name, st := range opt.DevToolsLinked {
		h.devTools[name] = st
	}
	sshOpt := agenthosttest.Options{
		Password: opt.Password, Username: opt.Username, NoSudo: opt.NoSudo, SudoNoPasswd: opt.SudoNoPasswd,
		Command: h.command, CommandStderr: h.commandStderr,
	}
	if !opt.NoForward {
		sshOpt.Forward = h.forward
	}
	sshOpt.ForwardProhibited = opt.ForwardProhibited
	h.SSH = agenthosttest.New(t, sshOpt)
	// 进程内的真守护进程：跑在回环 TCP 上，转发通道把「主机上的 socket」接到它。
	h.daemon = devd.New(devd.Options{Home: opt.Home, User: "dev", Shell: "/bin/sh", Version: "fake", StateDir: t.TempDir()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.ln = ln
	srv := &http.Server{Handler: h.daemon.Handler(), ErrorLog: log.New(io.Discard, "", 0)}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	// 进程内的真 modeld：同样跑在回环 TCP 上，转发通道按 socket 路径分流。
	md, err := modeld.New(modeld.Options{StateDir: t.TempDir(), Home: opt.Home, User: "dev", Version: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	h.modeld = md
	md.Start()
	mln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.mln = mln
	msrv := &http.Server{Handler: md.Handler(), ErrorLog: log.New(io.Discard, "", 0)}
	go msrv.Serve(mln)
	t.Cleanup(func() { msrv.Close(); md.Stop() })
	return h
}

// Modeld 暴露进程内的 modeld。
func (h *Host) Modeld() *modeld.Server { return h.modeld }

// Addr 是假 SSH 主机的地址与端口。
func (h *Host) Addr() (string, int) { return h.SSH.Addr() }

// HostKeyLine 是假 SSH 主机的公钥行。
func (h *Host) HostKeyLine() string { return h.SSH.HostKeyLine() }

// Daemon 暴露进程内守护进程。
func (h *Host) Daemon() *devd.Server { return h.daemon }

// Installed 报告守护进程是否装着。
func (h *Host) Installed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.installed
}

// MarkInstalled / MarkUninstalled 直接摆弄守护进程的在场状态（模拟掉线 / 手动装好）。
func (h *Host) MarkInstalled() {
	h.mu.Lock()
	h.installed = true
	h.mu.Unlock()
}

// MarkUninstalled 让守护进程的 socket 拒绝连接（模拟掉线 / 卸载）。
func (h *Host) MarkUninstalled() {
	h.mu.Lock()
	h.installed = false
	h.mu.Unlock()
}

// Execs 是假主机看见的全部命令（含纳管那几条）。
func (h *Host) Execs() []Exec { return h.SSH.Execs() }

// Sockets 是设备经转发通道连过的 socket 路径。
func (h *Host) Sockets() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.sockets...)
}

// InstalledBinary 是安装时上传的二进制字节。
func (h *Host) InstalledBinary() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.binary
}

// InstalledAs 是安装时指定的运行用户。
func (h *Host) InstalledAs() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.user
}

// Git 是主机上 git 的自述版本（空 = 没装）。
func (h *Host) Git() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.git
}

// Tmux 是主机上 tmux 的自述版本（空 = 没装）。
func (h *Host) Tmux() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tmux
}

// FFmpeg 是主机上 ffmpeg 的自述版本（空 = 没装）。
func (h *Host) FFmpeg() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ffmpeg
}

// Gate 是主机上 gate 的现状。
func (h *Host) Gate() GateState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gate
}

// DevTools 是 gate 配置里现有的开发工具关联（按工具名）。
func (h *Host) DevTools() map[string]DevToolState {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]DevToolState, len(h.devTools))
	for k, v := range h.devTools {
		out[k] = v
	}
	return out
}

// Workspaces 是主机上现存的工作空间目录（按名字）。
func (h *Host) Workspaces() map[string]FakeWorkspace {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]FakeWorkspace, len(h.workspaces))
	for k, v := range h.workspaces {
		out[k] = v
	}
	return out
}

// forward 是 sshd 的转发通道：只认到守护进程 socket 的 Unix 转发，守护进程没装即拒绝。
func (h *Host) forward(network, address string) (net.Conn, error) {
	h.mu.Lock()
	h.sockets = append(h.sockets, address)
	installed := h.installed
	h.mu.Unlock()
	if network != "unix" || (address != devd.DefaultSocket && address != modeld.DefaultSocket) {
		return nil, fmt.Errorf("forwarding to %s %s refused", network, address)
	}
	if !installed {
		return nil, errors.New("connect failed: No such file or directory")
	}
	if address == modeld.DefaultSocket {
		return net.Dial("tcp", h.mln.Addr().String())
	}
	return net.Dial("tcp", h.ln.Addr().String())
}

var (
	userRE        = regexp.MustCompile(`--user ([a-z0-9_-]+)`)
	binRE         = regexp.MustCompile(`install -m 0755 (\S+) `)
	gateInstallRE = regexp.MustCompile(`sh "\$t" '([^']+)'(?: '([^']+)')?; rc=`)
	gateConfigRE  = regexp.MustCompile(`^'([^']+)' bootstrap '([^']+)'(?: '([^']+)')?$`)
	gateUpdateRE  = regexp.MustCompile(`^'([^']+)' update$`)
	devToolRE     = regexp.MustCompile(`^'([^']+)' (codex|claude|grok|cursor|opencode|mcode) (install|update|disconnect|uninstall --yes)$`)
	workspaceRE   = regexp.MustCompile(`^/bin/sh -c '# llmgate-workspace name=(\S+) url=(\S*) branch=(\S*)\n`)
	wsRemoveRE    = regexp.MustCompile(`^/bin/sh -c '# llmgate-workspace-remove path=(\S+)\n`)
)

// toolsProbeMarker 与 devhost 探测脚本的首行相同。
const toolsProbeMarker = "# llmgate-tools-probe"

// command 只接管设备为守护进程下发的那几条命令，纳管脚本交回 agenthosttest 内建处理。
func (h *Host) command(command, stdin string) (stdout string, code int, handled bool) {
	if h.opt.Command != nil {
		if out, code, ok := h.opt.Command(command, stdin); ok {
			return out, code, true
		}
	}
	switch {
	case command == "uname -m":
		return h.opt.Arch + "\n", 0, true
	case strings.HasPrefix(command, "umask 077 && cat > "):
		path := strings.TrimPrefix(command, "umask 077 && cat > ")
		h.mu.Lock()
		h.files[path] = []byte(stdin)
		h.mu.Unlock()
		return "", 0, true
	case command == "/bin/sh -s" && strings.HasPrefix(stdin, toolsProbeMarker):
		return h.probeTools(), 0, true
	case strings.Contains(command, "/gate-helper/install.sh"):
		return h.installGate(command)
	case gateConfigRE.MatchString(command):
		m := gateConfigRE.FindStringSubmatch(command)
		return h.configureGate(m[2], m[3])
	case gateUpdateRE.MatchString(command):
		return h.updateGate()
	case devToolRE.MatchString(command):
		m := devToolRE.FindStringSubmatch(command)
		return h.devTool(m[2], m[3])
	case strings.HasSuffix(command, " uninstall --yes"):
		h.mu.Lock()
		h.gate = GateState{}
		h.devTools = map[string]DevToolState{}
		h.mu.Unlock()
		return "gate removed\n", 0, true
	case wsRemoveRE.MatchString(command):
		m := wsRemoveRE.FindStringSubmatch(command)
		h.mu.Lock()
		defer h.mu.Unlock()
		for name, ws := range h.workspaces {
			if ws.Path == m[1] {
				delete(h.workspaces, name)
				if strings.HasPrefix(ws.Path, h.opt.Home+"/workspaces/") {
					_ = os.RemoveAll(ws.Path)
				}
			}
		}
		return "", 0, true
	case !strings.Contains(command, devd.DefaultBinPath) && !strings.Contains(command, modeld.DefaultBinPath) && !strings.Contains(command, " git") && !strings.Contains(command, " tmux") && !strings.Contains(command, " ffmpeg") && !strings.Contains(command, "fontconfig") && !strings.Contains(strings.ToLower(command), "imagemagick"):
		return "", 0, false
	case strings.HasPrefix(command, "sudo -S -p '' "):
		if h.opt.NoSudo {
			return "", 127, true
		}
		line, _, _ := strings.Cut(stdin, "\n")
		if line != h.opt.Password {
			return "", 1, true
		}
		return h.privileged(strings.TrimPrefix(command, "sudo -S -p '' "))
	case strings.HasPrefix(command, "sudo -n "):
		if h.opt.NoSudo {
			return "", 127, true
		}
		if !h.SSH.SudoNoPasswd() {
			return "", 1, true
		}
		return h.privileged(strings.TrimPrefix(command, "sudo -n "))
	case strings.HasPrefix(command, "/bin/sh -c '"):
		return h.privileged(command)
	}
	return "", 0, false
}

// privileged 解释 root 身份跑的内层脚本：安装或卸载。
func (h *Host) privileged(command string) (string, int, bool) {
	script := strings.TrimSuffix(strings.TrimPrefix(command, "/bin/sh -c '"), "'")
	switch {
	case strings.Contains(script, devd.DefaultBinPath+" install "), strings.Contains(script, modeld.DefaultBinPath+" install "):
		userM := userRE.FindStringSubmatch(script)
		binM := binRE.FindStringSubmatch(script)
		if userM == nil || binM == nil {
			return "", 2, true
		}
		h.mu.Lock()
		bin, okBin := h.files[binM[1]]
		delete(h.files, binM[1])
		h.mu.Unlock()
		if !okBin {
			return "", 1, true
		}
		h.mu.Lock()
		h.installed, h.binary, h.user = true, bin, userM[1]
		h.mu.Unlock()
		if strings.Contains(script, modeld.DefaultBinPath) {
			return fmt.Sprintf("socket=%s\nunit=%s\n", modeld.DefaultSocket, modeld.UnitPath), 0, true
		}
		return fmt.Sprintf("socket=%s\nunit=%s\n", devd.DefaultSocket, devd.UnitPath), 0, true
	case (strings.Contains(script, devd.DefaultBinPath) || strings.Contains(script, modeld.DefaultBinPath)) && strings.Contains(script, "uninstall"):
		h.mu.Lock()
		h.installed = false
		h.mu.Unlock()
		return "", 0, true
	case strings.HasSuffix(script, " git"), strings.HasSuffix(script, " tmux"), installsFFmpeg(script), strings.Contains(script, "fontconfig"), strings.HasSuffix(strings.ToLower(script), " imagemagick"):
		// 装 git / tmux / ffmpeg：脚本得用主机自报的那个包管理器。
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.opt.Pkg == "" || !strings.Contains(script, h.opt.Pkg) {
			return "", 127, true
		}
		if strings.HasSuffix(script, " git") {
			h.git = "git version 2.43.0"
		} else if installsFFmpeg(script) {
			h.ffmpeg = "ffmpeg version 6.1.1"
			h.ffprobe, h.ffmpegSubtitles = true, true
			h.ffmpegCodecs = []string{"h264", "aac"}
		} else if strings.Contains(script, "fontconfig") {
			h.fontsCJK = []string{"Noto Sans CJK SC"}
		} else if strings.HasSuffix(strings.ToLower(script), " imagemagick") {
			h.imageMagick = "Version: ImageMagick 7.1.1"
		} else {
			h.tmux = "tmux 3.4"
		}
		return "", 0, true
	}
	h.t.Errorf("fake host got an unrecognized privileged script: %q", script)
	return "", 127, true
}

// installsFFmpeg 认出装 FFmpeg 的包命令：单个包名结尾，或 RPM 系的候选链（ffmpeg || ffmpeg-free）。
func installsFFmpeg(script string) bool {
	return strings.HasSuffix(script, " ffmpeg") || strings.Contains(script, " ffmpeg || ")
}

// FakeBinaries 是测试用的二进制提供者：每个架构一小段可辨认的字节。
type FakeBinaries struct{}

// Binary 实现 devhost.BinaryProvider。
func (FakeBinaries) Binary(goarch string) ([]byte, string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return nil, "", errors.New("unsupported arch " + goarch)
	}
	return []byte("#!/bin/sh\n# fake llmgate-devd for " + goarch + "\n"), "test", nil
}

// commandStderr 只接管要往 stderr 写原因的工作空间创建脚本。
func (h *Host) commandStderr(command, stdin string) (string, string, int, bool) {
	if !workspaceRE.MatchString(command) {
		return "", "", 0, false
	}
	out, stderr, code := h.createWorkspace(command, stdin)
	return out, stderr, code, true
}

// createWorkspace 解释工作空间创建脚本：退出码与真脚本相同（90 没 git、91 目录已在、
// 92 远端出错、93 没这个分支），成功回 path=… 行。凭证从 stdin 的前两行读。
func (h *Host) createWorkspace(command, stdin string) (stdout, stderr string, code int) {
	m := workspaceRE.FindStringSubmatch(command)
	name, url, branch := m[1], m[2], m[3]
	if strings.Contains(command, "secret") && !strings.Contains(command, "credential.helper") {
		h.t.Errorf("workspace script leaks a secret into argv: %q", command)
	}
	user, token := "", ""
	if strings.Contains(command, "read -r GIT_USER") {
		lines := strings.SplitN(stdin, "\n", 3)
		if len(lines) < 2 {
			return "", "", 2
		}
		user, token = lines[0], lines[1]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.git == "" {
		return "", "", 90
	}
	if _, exists := h.workspaces[name]; exists {
		return "", "", 91
	}
	if url != "" {
		repo, ok := h.opt.Repos[url]
		switch {
		case !ok:
			return "", "fatal: repository '" + url + "' not found\n", 92
		case repo.User != "" && (repo.User != user || repo.Token != token):
			return "", "fatal: Authentication failed for '" + url + "'\n", 92
		case branch != "":
			found := false
			for _, b := range repo.Branches {
				found = found || b == branch
			}
			if !found {
				return "", "", 93
			}
		case len(repo.Branches) == 0:
			return "", "", 93
		}
	}
	path := h.opt.Home + "/workspaces/" + name
	// 目录真的建在守护进程的家目录下：设备经守护进程读写工作空间里的文件（创作工作空间）。
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err.Error() + "\n", 94
	}
	h.workspaces[name] = FakeWorkspace{Name: name, Path: path, URL: url, Branch: branch, User: user, Token: token}
	return "path=" + path + "\n", "", 0
}

// probeTools 答探测脚本：与主机上的 sh 输出同形（key=value 行）。
func (h *Host) probeTools() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	if h.git != "" {
		fmt.Fprintf(&b, "git_version=%s\n", h.git)
	}
	if h.tmux != "" {
		fmt.Fprintf(&b, "tmux_version=%s\n", h.tmux)
	}
	if h.ffmpeg != "" {
		fmt.Fprintf(&b, "ffmpeg_version=%s\n", h.ffmpeg)
	}
	if h.ffprobe {
		b.WriteString("ffprobe=yes\n")
	}
	for _, codec := range h.ffmpegCodecs {
		fmt.Fprintf(&b, "ffmpeg_%s=yes\n", codec)
	}
	if h.ffmpegSubtitles {
		b.WriteString("ffmpeg_subtitles=yes\n")
	}
	if len(h.fontsCJK) > 0 {
		fmt.Fprintf(&b, "fonts_cjk_count=%d\nfonts_cjk_family=%s\n", len(h.fontsCJK), h.fontsCJK[0])
	}
	if h.imageMagick != "" {
		fmt.Fprintf(&b, "imagemagick_version=%s\n", h.imageMagick)
	}
	if h.opt.Pkg != "" {
		fmt.Fprintf(&b, "pkg=%s\n", h.opt.Pkg)
	}
	if !h.opt.NoCurl {
		b.WriteString("curl=yes\n")
	}
	if h.gate.Version != "" {
		fmt.Fprintf(&b, "gate_path=%s/.local/bin/gate\ngate_version=%s\n", h.opt.Home, h.gate.Version)
	}
	if h.gate.Base != "" || h.gate.HasKey {
		fmt.Fprintf(&b, "gate_base=%s\n", h.gate.Base)
		if h.gate.HasKey {
			b.WriteString("gate_key=yes\n")
		}
	}
	fmt.Fprintf(&b, "home=%s\n", h.opt.Home)
	if h.gate.Version != "" {
		for name, st := range h.devTools {
			fmt.Fprintf(&b, "tool_%s_binary_path=%s\ntool_%s_detected_version=%s\ntool_%s_origin=%s\n", name, st.Path, name, st.Version, name, st.Origin)
		}
	}
	for name, path := range h.opt.DevToolsFound {
		fmt.Fprintf(&b, "tool_%s_found=%s\n", name, path)
	}
	return b.String()
}

// devTool 解释 `gate <tool> install | update | disconnect | uninstall --yes`，退出码与消息
// 按真 gate：没装 gate 即 127；install / update 没有地址或 Key 报错；install 幂等——PATH 上
// 有同名程序只关联为 external，否则装受管副本；update 只升级受管安装；disconnect 只删关联；
// uninstall 删受管程序并撤关联，外部安装只撤关联。
func (h *Host) devTool(name, action string) (string, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gate.Version == "" {
		return "", 127, true
	}
	cur, linked := h.devTools[name]
	switch action {
	case "install":
		if h.gate.Base == "" || !h.gate.HasKey {
			return "gate: device address or API Key not set\n", 1, true
		}
		if linked {
			return name + " already linked: " + cur.Path + "\n", 0, true
		}
		if found, ok := h.opt.DevToolsFound[name]; ok {
			h.devTools[name] = DevToolState{Path: found, Version: "fake-" + name + " 1.0.0", Origin: "external"}
			return "linked " + name + " from PATH: " + found + "\n", 0, true
		}
		h.devTools[name] = DevToolState{Path: h.opt.Home + "/.local/share/gate/tools/" + name + "/bin/" + name, Version: "fake-" + name + " 1.0.0", Origin: "gate"}
		return "installing " + name + " via device...\n" + name + " installed and linked\n", 0, true
	case "update":
		if h.gate.Base == "" || !h.gate.HasKey {
			return "gate: device address or API Key not set\n", 1, true
		}
		if !linked {
			return "gate: " + name + " is not installed; run gate " + name + " install first\n", 1, true
		}
		if cur.Origin == "external" {
			return "gate: " + name + " is managed externally; upgrade through that channel or use update --adopt\n", 1, true
		}
		cur.Version = "fake-" + name + " 1.1.0"
		h.devTools[name] = cur
		return name + " updated\n", 0, true
	case "disconnect":
		if !linked {
			return "gate: " + name + " is not linked\n", 1, true
		}
		delete(h.devTools, name)
		return name + " disconnected\n", 0, true
	case "uninstall --yes":
		if !linked {
			return "gate: " + name + " is not linked\n", 1, true
		}
		delete(h.devTools, name)
		return name + " uninstalled\n", 0, true
	}
	return "", 2, true
}

// configureGate 解释「gate bootstrap 地址 [Key]」：像真 gate 一样，没装即 127；没带 Key 又
// 没有已保存的 Key 时因为读不到控制终端而失败；否则地址与 Key 一起换，版本不动。
func (h *Host) configureGate(base, key string) (string, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gate.Version == "" {
		return "", 127, true
	}
	if key == "" && !h.gate.HasKey {
		return "gate: read API Key: no controlling terminal\n", 1, true
	}
	h.gate.Base = base
	h.gate.HasKey = true
	h.gate.LastKey = key
	return "device address and API Key verified and saved.\n", 0, true
}

// updateGate 解释「gate update」：没装即 127，没有设备地址即报错；否则只换版本。
func (h *Host) updateGate() (string, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gate.Version == "" {
		return "", 127, true
	}
	if h.gate.Base == "" {
		return "gate: run gate -url <device address> first\n", 1, true
	}
	h.gate.Version = updatedGateVersion
	return "gate updated to " + updatedGateVersion + ".\n", 0, true
}

// installGate 解释「curl 取安装脚本 | sh -s -- 地址 [Key]」：没有 curl 即 127；没带 Key
// 又没有已保存的 Key 时像真 gate bootstrap 一样因为读不到控制终端而失败。
func (h *Host) installGate(command string) (string, int, bool) {
	if h.opt.NoCurl {
		return "", 127, true
	}
	if h.opt.GateUnreachable {
		// 真主机上 curl 的报错在 stderr；假主机只有 stdout，一并放这里。
		return "curl: (6) Could not resolve host\n", 98, true
	}
	m := gateInstallRE.FindStringSubmatch(command)
	if m == nil {
		h.t.Errorf("fake host got an unrecognized gate install command: %q", command)
		return "", 2, true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if m[2] == "" && !h.gate.HasKey {
		return "", 1, true
	}
	h.gate = GateState{Version: installedGateVersion, Base: m[1], HasKey: true, LastKey: m[2]}
	return "device address and API Key verified and saved.\ngate installed to " + h.opt.Home + "/.local/bin/gate\n", 0, true
}
