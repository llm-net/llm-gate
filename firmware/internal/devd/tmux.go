package devd

// 终端：每个标签页是一个 tmux 会话——tmux 在主机上常驻，浏览器断开、设备重启都
// 不丢会话，下次 attach 回到原处。守护进程为每次 attach 起一个 `tmux new-session -A`
// 客户端挂在伪终端上，把终端字节按 wire 帧搬运；浏览器那头关掉就杀这个客户端
// （会话本身留着）。主机上没有 tmux 时退化成直接起登录 shell（会话不持久）。
//
// 鼠标与剪贴板：浏览器里的 xterm.js 要靠 tmux 的 mouse 模式才能滚轮翻历史、拖选复制，
// 所以每次 attach 都把内嵌的 tmux.conf source 进主机的 tmux 服务器（写到 StateDir，
// 内容不同才重写），不依赖使用者自己的 ~/.tmux.conf。

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/devd/wire"
)

// TmuxSession 是一个 tmux 会话的读数。Tool 是会话里正在跑的开发工具（DevTools 之一，
// 见 devtool_detect.go），没有即缺省。
type TmuxSession struct {
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached int    `json:"attached"`
	Created  string `json:"created,omitempty"`
	Tool     string `json:"tool,omitempty"`
}

// TmuxList 是会话清单。
type TmuxList struct {
	Available bool          `json:"available"`
	Sessions  []TmuxSession `json:"sessions"`
}

//go:embed tmux.conf
var tmuxConf []byte

// tmuxConfName 是 StateDir 里 tmux.conf 的文件名。
const tmuxConfName = "tmux.conf"

var sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// DevTools 是「新终端直接启动开发工具」认得的工具名（封闭词汇表）：会话建好后往里敲
// `gate <tool>`，由主机上的 gate 用它保存的设备地址与 Key 拉起对应工具。设备不能往主机
// 塞任意命令，只能从这张表里选。
var DevTools = []string{"codex", "claude", "grok", "cursor", "opencode", "mcode"}

// 错误码（补充 devd.go 的那组）：主机上没有 gate / gate 没配 Key 时不建会话。
const (
	CodeGateMissing      = "gate_missing"
	CodeGateUnconfigured = "gate_unconfigured"
)

// ValidTool 报告 tool 在 DevTools 词汇表里；不在即答 bad_request。
func ValidTool(tool string) error {
	for _, t := range DevTools {
		if t == tool {
			return nil
		}
	}
	return &Error{Code: CodeBadRequest, Msg: "不认识的开发工具：" + tool}
}

// gateReady 看主机上 gate 在不在、配置里有没有 Key：PATH 找不到就看 ~/.local/bin/gate
// （安装脚本的缺省位置；登录 shell 的 PATH 由安装脚本补上），配置只读 api_key 非空，
// 明文不出这个函数。
func (s *Server) gateReady() error {
	if _, err := lookPath("gate"); err != nil {
		if info, err := os.Stat(filepath.Join(s.home, ".local", "bin", "gate")); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return &Error{Code: CodeGateMissing, Msg: "主机上没有 gate：请先在这台主机的「工具配置」页安装 gate。"}
		}
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(s.home, ".config")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gate", "config.json"))
	var cfg struct {
		APIKey string `json:"api_key"`
	}
	if err != nil || json.Unmarshal(raw, &cfg) != nil || strings.TrimSpace(cfg.APIKey) == "" {
		return &Error{Code: CodeGateUnconfigured, Msg: "主机上的 gate 还没有 API 密钥：请先在这台主机的「工具配置」页配置密钥。"}
	}
	return nil
}

func validSessionName(name string) error {
	if !sessionNameRE.MatchString(name) {
		return &Error{Code: CodeInvalidSession, Msg: "会话名须是 1–32 位字母、数字、点、下划线或连字符，且以字母或数字开头"}
	}
	return nil
}

func tmuxPath() string {
	p, err := exec.LookPath("tmux")
	if err != nil {
		return ""
	}
	return p
}

func (s *Server) tmuxList(ctx context.Context) (*TmuxList, error) {
	tmux := tmuxPath()
	out := &TmuxList{Available: tmux != "", Sessions: []TmuxSession{}}
	if tmux == "" {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmux, "list-sessions", "-F", "#{session_name}\t#{session_windows}\t#{session_attached}\t#{session_created}")
	cmd.Env = s.childEnv()
	raw, err := cmd.Output()
	if err != nil {
		// 没有任何会话时 tmux 以非零退出并报 "no server running"：这是空清单，不是错误。
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return out, nil
		}
		return nil, &Error{Code: CodeTmuxFailed, Msg: fmt.Sprintf("tmux 未能执行：%s", err)}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		sess := TmuxSession{Name: f[0]}
		sess.Windows, _ = strconv.Atoi(f[1])
		sess.Attached, _ = strconv.Atoi(f[2])
		if len(f) > 3 {
			if secs, err := strconv.ParseInt(f[3], 10, 64); err == nil {
				sess.Created = time.Unix(secs, 0).UTC().Format(time.RFC3339)
			}
		}
		out.Sessions = append(out.Sessions, sess)
	}
	s.fillTools(ctx, tmux, out.Sessions)
	return out, nil
}

// fillTools 给每个会话标上正在跑的开发工具：列出全部 pane 的 shell pid，按会话归组，
// 再在一次 /proc 快照里沿子进程树找。tmux 或 /proc 读不出来就都不标，清单照常返回。
func (s *Server) fillTools(ctx context.Context, tmux string, sessions []TmuxSession) {
	if len(sessions) == 0 {
		return
	}
	cmd := exec.CommandContext(ctx, tmux, "list-panes", "-a", "-F", "#{session_name}\t#{pane_pid}")
	cmd.Env = s.childEnv()
	raw, err := cmd.Output()
	if err != nil {
		return
	}
	panes := map[string][]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		name, pid, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(pid); err == nil && n > 0 {
			panes[name] = append(panes[name], n)
		}
	}
	procs := snapshotProcs()
	if procs == nil {
		return
	}
	for i := range sessions {
		for _, pid := range panes[sessions[i].Name] {
			if tool := procs.toolOf(pid); tool != "" {
				sessions[i].Tool = tool
				break
			}
		}
	}
}

// tmuxNew 建一个后台 tmux 会话（登录 shell、起始目录 dir）。tool 非空时先确认主机上的
// gate 可用，会话建好后把 `gate <tool>` 当作按键敲进去：使用者在终端里看得见实际执行的
// 命令，gate 报错也留在终端里，工具退出后回到 shell、会话仍在。
func (s *Server) tmuxNew(ctx context.Context, name, dir, tool string) error {
	if err := validSessionName(name); err != nil {
		return err
	}
	if tool != "" {
		if err := ValidTool(tool); err != nil {
			return err
		}
	}
	tmux := tmuxPath()
	if tmux == "" {
		return &Error{Code: CodeTmuxMissing, Msg: "主机上没有 tmux：请先安装（Debian/Ubuntu：sudo apt install tmux）"}
	}
	cwd, err := s.resolvePath(dir)
	if err != nil {
		return err
	}
	if tool != "" {
		if err := s.gateReady(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmux, "new-session", "-d", "-s", name, "-c", cwd)
	cmd.Env = s.childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "duplicate session") {
			return &Error{Code: CodeSessionExists, Msg: fmt.Sprintf("会话 %s 已存在", name)}
		}
		if msg == "" {
			msg = err.Error()
		}
		return &Error{Code: CodeTmuxFailed, Msg: "创建 tmux 会话失败：" + msg}
	}
	if tool == "" {
		return nil
	}
	// 命令文本按字面（-l）敲入，再单独送一个回车；tmux 把键排进 pane 的输入队列，shell
	// 起来后照常读到。目标写成 `=<会话>:`（精确匹配会话名、取它当前窗口的活动 pane），
	// 光 `=<会话>` 对 send-keys 解析不成 pane。发不进去就把刚建的会话收掉，不留半成品。
	for _, keys := range [][]string{{"-l", "gate " + tool}, {"Enter"}} {
		cmd := exec.CommandContext(ctx, tmux, append([]string{"send-keys", "-t", "=" + name + ":"}, keys...)...)
		cmd.Env = s.childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			exec.Command(tmux, "kill-session", "-t", "="+name).Run()
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return &Error{Code: CodeTmuxFailed, Msg: "向新会话发送启动命令失败：" + msg}
		}
	}
	return nil
}

func (s *Server) tmuxKill(ctx context.Context, name string) error {
	if err := validSessionName(name); err != nil {
		return err
	}
	tmux := tmuxPath()
	if tmux == "" {
		return &Error{Code: CodeTmuxMissing, Msg: "主机上没有 tmux"}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmux, "kill-session", "-t", "="+name)
	cmd.Env = s.childEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "can't find session") || strings.Contains(msg, "no server running") {
			return &Error{Code: CodeNotFound, Msg: fmt.Sprintf("会话 %s 不存在", name)}
		}
		if msg == "" {
			msg = err.Error()
		}
		return &Error{Code: CodeTmuxFailed, Msg: "结束 tmux 会话失败：" + msg}
	}
	return nil
}

// childEnv 是交给 tmux / shell 的环境：家目录、用户与 TERM 钉死，其余继承守护进程。
func (s *Server) childEnv() []string {
	env := os.Environ()
	set := func(k, v string) {
		prefix := k + "="
		for i, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				env[i] = prefix + v
				return
			}
		}
		env = append(env, prefix+v)
	}
	set("HOME", s.home)
	set("TERM", "xterm-256color")
	set("COLORTERM", "truecolor")
	set("LANG", langOrDefault())
	if s.user != "" {
		set("USER", s.user)
		set("LOGNAME", s.user)
	}
	if s.shell != "" {
		set("SHELL", s.shell)
	}
	return env
}

func langOrDefault() string {
	if v := os.Getenv("LANG"); v != "" {
		return v
	}
	return "C.UTF-8"
}

// termSession 是一次 attach：一个挂在伪终端上的 tmux 客户端（或 shell）。
type termSession struct {
	cmd    *exec.Cmd
	pty    *ptyPair
	done   chan struct{}
	once   sync.Once
	exit   error
	name   string
	closed bool
	mu     sync.Mutex
}

// ensureTmuxConf 把内嵌的 tmux.conf 放到 StateDir 里（已有且内容一致就不动），返回路径。
// 没给 StateDir 也没经 ListenAndServe 时，用进程私有的临时目录。
func (s *Server) ensureTmuxConf() (string, error) {
	s.confMu.Lock()
	defer s.confMu.Unlock()
	if s.stateDir == "" {
		dir, err := os.MkdirTemp("", "llmgate-devd-")
		if err != nil {
			return "", err
		}
		s.stateDir = dir
	}
	path := filepath.Join(s.stateDir, tmuxConfName)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, tmuxConf) {
		return path, nil
	}
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, tmuxConf, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// attach 起一个 tmux 客户端（会话不存在则一并创建）挂到伪终端上，并把内嵌的 tmux.conf
// source 进去（`new-session -A` 后串一条 `source-file`，新建与重新 attach 都走到）。
func (s *Server) attach(name, dir string, cols, rows uint16) (*termSession, error) {
	cwd, err := s.resolvePath(dir)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	if tmux := tmuxPath(); tmux != "" {
		if err := validSessionName(name); err != nil {
			return nil, err
		}
		args := []string{"-u", "new-session", "-A", "-s", name, "-c", cwd}
		if conf, err := s.ensureTmuxConf(); err == nil {
			args = append(args, ";", "source-file", "-q", conf)
		} else {
			// 配置落不下只是少了鼠标支持，终端照常开。
			s.log.Warn("写 tmux.conf 失败", "error", err.Error())
		}
		cmd = exec.Command(tmux, args...)
	} else {
		shell := s.shell
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.Command(shell, "-l")
		cmd.Dir = cwd
	}
	cmd.Env = s.childEnv()
	p, err := openPTY()
	if err != nil {
		return nil, &Error{Code: CodeTmuxFailed, Msg: "打开伪终端失败：" + err.Error()}
	}
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	setWinsize(p.master, cols, rows)
	if err := startWithPTY(cmd, p); err != nil {
		p.master.Close()
		p.slave.Close()
		return nil, &Error{Code: CodeTmuxFailed, Msg: "启动终端失败：" + err.Error()}
	}
	ts := &termSession{cmd: cmd, pty: p, done: make(chan struct{}), name: name}
	go func() {
		ts.exit = cmd.Wait()
		close(ts.done)
	}()
	return ts, nil
}

// serve 在 conn 上搬运帧直到任一侧结束。返回时终端客户端已被杀掉、伪终端已关闭。
func (ts *termSession) serve(conn net.Conn) {
	defer ts.close()
	go func() {
		// 浏览器 → 终端：Data 写进伪终端，Resize 调窗口尺寸；读到错误即结束。
		fr := wire.NewReader(conn)
		for {
			f, err := fr.Next()
			if err != nil {
				ts.close()
				return
			}
			switch f.Type {
			case wire.Data:
				if _, err := ts.pty.master.Write(f.Payload); err != nil {
					ts.close()
					return
				}
			case wire.Resize:
				if cols, rows, ok := wire.ParseResize(f.Payload); ok && cols > 0 && rows > 0 {
					setWinsize(ts.pty.master, cols, rows)
				}
			}
		}
	}()
	// 终端 → 浏览器：伪终端读到什么就打成 Data 帧。
	buf := make([]byte, 32<<10)
	for {
		n, err := ts.pty.master.Read(buf)
		if n > 0 {
			if werr := wire.Write(conn, wire.Data, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			break
		}
	}
	<-ts.done
	reason := ""
	if ts.exit != nil {
		var exit *exec.ExitError
		if errors.As(ts.exit, &exit) {
			reason = fmt.Sprintf("终端进程退出（%d）", exit.ExitCode())
		}
	}
	wire.Write(conn, wire.Exit, []byte(reason))
}

func (ts *termSession) close() {
	ts.once.Do(func() {
		ts.mu.Lock()
		ts.closed = true
		ts.mu.Unlock()
		if ts.cmd.Process != nil {
			ts.cmd.Process.Signal(os.Interrupt)
			select {
			case <-ts.done:
			case <-time.After(500 * time.Millisecond):
				ts.cmd.Process.Kill()
			}
		}
		ts.pty.master.Close()
	})
}
