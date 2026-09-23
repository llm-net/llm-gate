package hostagent

// 设备暴露给引擎的主机工具。引擎（模型）想动主机只能经这里：每次调用都在设备上凭
// 证书走 SSH 执行，命令、退出码、输出尾段、写过的文件与档案变更全部落时间线——这就是
// 「对该主机的操作日志」。引擎自己没有主机凭据，也没有别的通道。
//
// 四个工具：
//   exec           在主机上以登录用户身份执行一条 shell 命令（非交互，可给 stdin 与超时）
//   write_file     把内容写成主机上的文件（可 sudo、可追加、可 chmod）；内容经 stdin 传
//   read_profile   读主机档案（Markdown）
//   update_profile 整份替换主机档案
//
// 命令与 stdin 都不进日志（§15.1）；时间线里的输出只留尾段（storeOutputLimit）。

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/agenthost"
	"github.com/llm-net/llm-gate/firmware/internal/mcpserve"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

const (
	toolExec          = "exec"
	toolWriteFile     = "write_file"
	toolReadProfile   = "read_profile"
	toolUpdateProfile = "update_profile"

	// storeOutputLimit 是时间线里给命令输出留的尾段上限；storeFileLimit 是写文件事件
	// 里留的内容头部上限。给模型看的结果另按 agenthost.DefaultOutputLimit 截。
	storeOutputLimit = 16 << 10
	storeFileLimit   = 64 << 10
	// maxToolTimeout 是模型能要到的单条命令超时上限（秒）。
	maxToolTimeout = int(agenthost.MaxRunTimeout / time.Second)
	// maxWriteBytes 是单次写文件的内容上限。
	maxWriteBytes = 4 << 20
)

// 工具清单与说明文字在 tools.json（模型看的文本，不是界面文案，放 JSON 让 i18n 提取器
// 不把它当键）；形状与端点骨架在 internal/mcpserve。
//
//go:embed tools.json
var toolsJSON []byte

var toolCatalog = mcpserve.MustCatalog(toolsJSON)

// callTool 执行一次工具调用；返回给模型的文本与是否错误。
func (s *session) callTool(ctx context.Context, name string, args json.RawMessage) (string, bool) {
	switch name {
	case toolExec:
		var a struct {
			Command    string `json:"command"`
			Stdin      string `json:"stdin"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return err.Error(), true
		}
		if strings.TrimSpace(a.Command) == "" {
			return "command must not be empty", true
		}
		return s.execOnHost(ctx, a.Command, a.Stdin, time.Duration(a.TimeoutSec)*time.Second)
	case toolWriteFile:
		var a struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			Mode    string `json:"mode"`
			Sudo    bool   `json:"sudo"`
			Append  bool   `json:"append"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return err.Error(), true
		}
		return s.writeFileOnHost(ctx, a.Path, a.Content, a.Mode, a.Sudo, a.Append)
	case toolReadProfile:
		p, err := s.m.st.GetAgentHostProfile(ctx, s.hostID)
		if err != nil {
			return "failed to read the host profile", true
		}
		if strings.TrimSpace(p.Content) == "" {
			return "(the host profile is empty)", false
		}
		return p.Content, false
	case toolUpdateProfile:
		var a struct {
			Content string `json:"content"`
		}
		if err := decodeArgs(args, &a); err != nil {
			return err.Error(), true
		}
		if strings.TrimSpace(a.Content) == "" {
			return "content must not be empty", true
		}
		content := normalizeProfile(a.Content)
		if _, err := s.m.st.SetAgentHostProfile(ctx, s.hostID, content, "agent", s.m.now()); err != nil {
			return "failed to save the host profile", true
		}
		s.appendEvent(ctx, store.AgentHostEvent{RunID: s.currentRunID(), Kind: store.AgentEventProfile, Title: "agent", Body: content})
		return "host profile updated", false
	}
	return "unknown tool: " + name, true
}

func decodeArgs(raw json.RawMessage, dst any) error { return mcpserve.DecodeArgs(raw, dst) }

// hostConn 取（或建）到主机的 SSH 连接；调用方须持 s.connMu。
func (s *session) hostConn(ctx context.Context) (*agenthost.Conn, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	c, err := s.m.hosts.Open(ctx, s.hostID)
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

func (s *session) dropConn() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// execOnHost 跑一条命令并留 command 事件。
func (s *session) execOnHost(ctx context.Context, command, stdin string, timeout time.Duration) (string, bool) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.setActivity(nil, "执行命令："+firstLine(command))
	defer s.setActivity(nil, "")
	runID := s.currentRunID()
	conn, err := s.hostConn(ctx)
	if err != nil {
		s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventCommand, Title: command, Body: "failed to connect to the host: " + err.Error(),
			Meta: metaJSON(map[string]any{"error": true})})
		return "failed to connect to the host: " + err.Error(), true
	}
	res, err := conn.Run(ctx, agenthost.RunRequest{Command: command, Stdin: stdin, Timeout: timeout})
	if err != nil {
		// 连接层失败：丢掉这条连接，下次调用重连。
		s.dropConn()
		s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventCommand, Title: command, Body: err.Error(),
			DurationMs: res.Duration.Milliseconds(), Meta: metaJSON(map[string]any{"error": true, "stdin_bytes": len(stdin)})})
		return "the command could not be executed: " + err.Error(), true
	}
	code := res.ExitCode
	s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventCommand, Title: command,
		Body: tail(combinedOutput(res.Stdout, res.Stderr), storeOutputLimit), ExitCode: &code, DurationMs: res.Duration.Milliseconds(),
		Meta: metaJSON(map[string]any{"truncated": res.Truncated, "stdin_bytes": len(stdin)})})
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", code)
	if res.Truncated {
		b.WriteString("(output exceeded the limit and was truncated)\n")
	}
	if res.Stdout == "" && res.Stderr == "" {
		b.WriteString("(no output)\n")
	}
	if res.Stdout != "" {
		b.WriteString("stdout:\n" + res.Stdout)
		if !strings.HasSuffix(res.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if res.Stderr != "" {
		b.WriteString("stderr:\n" + res.Stderr)
		if !strings.HasSuffix(res.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String(), false
}

var modeRE = regexp.MustCompile(`^[0-7]{3,4}$`)

// writeFileOnHost 经 stdin 写文件并留 file 事件。
func (s *session) writeFileOnHost(ctx context.Context, path, content, mode string, sudo, appendTo bool) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || strings.ContainsAny(path, "\n\r\x00") {
		return "path is invalid", true
	}
	if mode != "" && !modeRE.MatchString(mode) {
		return "mode must be a 3- or 4-digit octal number", true
	}
	if len(content) > maxWriteBytes {
		return fmt.Sprintf("content exceeds the %d MiB limit", maxWriteBytes>>20), true
	}
	redirect := ">"
	if appendTo {
		redirect = ">>"
	}
	script := fmt.Sprintf(`f=%s; d=$(dirname -- "$f"); [ -d "$d" ] || mkdir -p -- "$d" || exit 1; cat %s "$f" || exit 1`, shellQuote(path), redirect)
	if mode != "" {
		script += fmt.Sprintf(`; chmod %s -- "$f"`, mode)
	}
	command := "sh -c " + shellQuote(script)
	if sudo {
		command = "sudo -n " + command
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.setActivity(nil, "写文件："+path)
	defer s.setActivity(nil, "")
	runID := s.currentRunID()
	meta := map[string]any{"bytes": len(content), "sudo": sudo, "append": appendTo, "mode": mode}
	conn, err := s.hostConn(ctx)
	if err != nil {
		meta["error"] = true
		s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventFile, Title: path, Body: "failed to connect to the host: " + err.Error(), Meta: metaJSON(meta)})
		return "failed to connect to the host: " + err.Error(), true
	}
	res, err := conn.Run(ctx, agenthost.RunRequest{Command: command, Stdin: content, Timeout: time.Minute})
	if err != nil {
		s.dropConn()
		meta["error"] = true
		s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventFile, Title: path, Body: err.Error(), Meta: metaJSON(meta)})
		return "the write could not be executed: " + err.Error(), true
	}
	code := res.ExitCode
	s.appendEvent(ctx, store.AgentHostEvent{RunID: runID, Kind: store.AgentEventFile, Title: path, Body: head(content, storeFileLimit),
		ExitCode: &code, DurationMs: res.Duration.Milliseconds(), Meta: metaJSON(meta)})
	if code != 0 {
		return fmt.Sprintf("write failed (exit_code %d): %s", code, strings.TrimSpace(combinedOutput(res.Stdout, res.Stderr))), true
	}
	return fmt.Sprintf("wrote %s (%d bytes)", path, len(content)), false
}

// shellQuote 把任意字符串包成 POSIX sh 单引号字面量。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func combinedOutput(stdout, stderr string) string {
	switch {
	case stderr == "":
		return stdout
	case stdout == "":
		return stderr
	}
	if !strings.HasSuffix(stdout, "\n") {
		stdout += "\n"
	}
	return stdout + "--- stderr ---\n" + stderr
}

// tail 保留末尾 n 字节（按 UTF-8 边界），截了就在前面标一句。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	for i := 0; i < len(cut) && i < 4; i++ {
		if cut[i]&0xC0 != 0x80 {
			cut = cut[i:]
			break
		}
	}
	return "…(earlier output truncated)\n" + cut
}

// head 保留开头 n 字节（按 UTF-8 边界）。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for i := len(cut) - 1; i >= 0 && i > len(cut)-4; i-- {
		if cut[i]&0xC0 != 0x80 {
			cut = cut[:i]
			break
		}
	}
	return cut + "\n…(rest truncated)"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}
