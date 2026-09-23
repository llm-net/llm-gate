package modeld

// 本机 Claude Code：stream-json 模式（协议形态与 internal/nodeengine/claude.go 相同）。密钥不经
// 环境变量：--settings 的 apiKeyHelper 读实例目录里的 0600 文件；ANTHROPIC_BASE_URL 指向设备的
// /agents/claude。一律 --bare 且 --setting-sources 给空串：不读节点上用户或项目的 settings。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	claudeControlTimeout = 10 * time.Second
	claudeInterruptGrace = 10 * time.Second
	claudeMaxLine        = 64 << 20
	claudeTools          = "Bash,Read,Edit,Write,Glob,Grep"
)

type claudeDriver struct {
	w    io.Writer
	wMu  sync.Mutex
	done chan struct{}

	mu      sync.Mutex
	turn    *claudeTurn
	pending map[string]chan claudeControlResult
	nextID  atomic.Int64
}

type claudeTurn struct {
	msgs        chan json.RawMessage
	over        chan struct{}
	interrupted atomic.Bool
	tools       map[string]claudeToolCall
}

type claudeToolCall struct {
	name  string
	input map[string]any
}

type claudeControlResult struct {
	ok  bool
	msg string
	raw json.RawMessage
}

func (m *engineManager) startClaude(ctx context.Context, sess *session, in sessionInput) (engineDriver, error) {
	if err := writeInstanceFile(sess.dir, "key", in.APIKey); err != nil {
		return nil, err
	}
	if err := writeInstanceFile(sess.dir, "instructions.md", in.Instructions); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sess.dir+"/claude", 0o700); err != nil {
		return nil, err
	}
	env := append(m.baseEnv(),
		"CLAUDE_CONFIG_DIR="+sess.dir+"/claude",
		"ANTHROPIC_BASE_URL="+in.APIBase+"/agents/claude",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1",
		"BASH_MAX_TIMEOUT_MS=900000",
	)
	if in.Model != "" {
		for _, family := range []string{"FABLE", "OPUS", "SONNET", "HAIKU"} {
			env = append(env, "ANTHROPIC_DEFAULT_"+family+"_MODEL="+in.Model)
		}
		env = append(env, "ANTHROPIC_SMALL_FAST_MODEL="+in.Model, "CLAUDE_CODE_SUBAGENT_MODEL="+in.Model)
	}
	settings, _ := json.Marshal(map[string]string{"apiKeyHelper": "cat " + shellQuote(sess.dir+"/key")})
	args := []string{"-p", "--bare", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--no-session-persistence", "--setting-sources", "", "--append-system-prompt-file", sess.dir + "/instructions.md",
		"--settings", string(settings), "--tools", claudeTools, "--permission-prompt-tool", "stdio"}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.Effort != "" {
		args = append(args, "--effort", in.Effort)
	}
	stdin, stdout, err := m.spawn(sess, args, env)
	if err != nil {
		return nil, err
	}
	d := &claudeDriver{w: stdin, done: make(chan struct{}), pending: map[string]chan claudeControlResult{}}
	go d.readLoop(stdout)
	if _, err := d.control(ctx, map[string]any{"subtype": "initialize", "hooks": nil}); err != nil {
		stdin.Close()
		return nil, errors.New("Claude Code 握手失败（节点上的 claude 需支持 stream-json 与 --bare）：" + err.Error())
	}
	return d, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (d *claudeDriver) Done() <-chan struct{} { return d.done }

func (d *claudeDriver) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	d.wMu.Lock()
	defer d.wMu.Unlock()
	_, err = d.w.Write(b)
	return err
}

func (d *claudeDriver) readLoop(r io.Reader) {
	defer close(d.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), claudeMaxLine)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var env struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		switch env.Type {
		case "control_request":
			d.answer(env.RequestID, env.Request)
		case "control_response":
			d.mu.Lock()
			ch, ok := d.pending[env.Response.RequestID]
			delete(d.pending, env.Response.RequestID)
			d.mu.Unlock()
			if ok {
				ch <- claudeControlResult{ok: env.Response.Subtype == "success", msg: env.Response.Error, raw: env.Response.Response}
			}
		case "control_cancel_request", "keep_alive":
		default:
			d.mu.Lock()
			t := d.turn
			d.mu.Unlock()
			if t != nil {
				select {
				case t.msgs <- line:
				case <-t.over:
				}
			}
		}
	}
}

func (d *claudeDriver) answer(id string, raw json.RawMessage) {
	var req struct {
		Subtype string          `json:"subtype"`
		Input   json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(raw, &req)
	resp := map[string]any{"subtype": "success", "request_id": id}
	switch req.Subtype {
	case "can_use_tool":
		input := req.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		resp["response"] = map[string]any{"behavior": "allow", "updatedInput": input}
	default:
		resp = map[string]any{"subtype": "error", "request_id": id, "error": "unsupported control request: " + req.Subtype}
	}
	_ = d.send(map[string]any{"type": "control_response", "response": resp})
}

func (d *claudeDriver) control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	id := "llmgate-" + strconv.FormatInt(d.nextID.Add(1), 10)
	ch := make(chan claudeControlResult, 1)
	d.mu.Lock()
	d.pending[id] = ch
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
	}()
	if err := d.send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, errors.New("引擎连接已断开")
	}
	select {
	case res := <-ch:
		if !res.ok {
			return nil, errors.New(firstNonEmpty(res.msg, "引擎拒绝了请求"))
		}
		return res.raw, nil
	case <-d.done:
		return nil, errors.New("引擎进程已退出")
	case <-ctx.Done():
		return nil, errors.New("引擎未在期限内应答")
	}
}

func (d *claudeDriver) Run(ctx context.Context, text string, sink *sessionSink) (string, string) {
	t := &claudeTurn{msgs: make(chan json.RawMessage, 1024), over: make(chan struct{}), tools: map[string]claudeToolCall{}}
	d.mu.Lock()
	d.turn = t
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.turn = nil
		d.mu.Unlock()
		close(t.over)
	}()
	msg := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
	if err := d.send(msg); err != nil {
		return "failed", "引擎进程已退出"
	}
	for {
		select {
		case line := <-t.msgs:
			if out, errMsg, done := d.handle(t, line, sink); done {
				return out, errMsg
			}
		case <-d.done:
			return "failed", "引擎进程已退出"
		case <-ctx.Done():
			_ = d.Interrupt(context.Background())
			grace := time.NewTimer(claudeInterruptGrace)
			defer grace.Stop()
			for {
				select {
				case line := <-t.msgs:
					if out, errMsg, done := d.handle(t, line, sink); done {
						return out, errMsg
					}
				case <-grace.C:
					return "interrupted", ""
				case <-d.done:
					return "interrupted", ""
				}
			}
		}
	}
}

type claudeMessage struct {
	Type            string  `json:"type"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Event           struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Subtype string          `json:"subtype"`
	IsError bool            `json:"is_error"`
	Result  string          `json:"result"`
	Errors  []string        `json:"errors"`
	Usage   json.RawMessage `json:"usage"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (d *claudeDriver) handle(t *claudeTurn, line json.RawMessage, sink *sessionSink) (string, string, bool) {
	var m claudeMessage
	if json.Unmarshal(line, &m) != nil {
		return "", "", false
	}
	if m.ParentToolUseID != nil && *m.ParentToolUseID != "" {
		return "", "", false
	}
	switch m.Type {
	case "stream_event":
		if m.Event.Type == "content_block_delta" && m.Event.Delta.Type == "text_delta" && m.Event.Delta.Text != "" {
			sink.Delta(m.Event.Delta.Text)
		}
	case "assistant":
		var blocks []claudeBlock
		if json.Unmarshal(m.Message.Content, &blocks) != nil {
			break
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					sink.Message(b.Text)
				}
			case "thinking":
				if strings.TrimSpace(b.Thinking) != "" {
					sink.Reasoning(b.Thinking)
				}
			case "tool_use":
				t.tools[b.ID] = claudeToolCall{name: b.Name, input: b.Input}
				sink.Activity(toolActivity(b.Name, b.Input))
			}
		}
	case "user":
		var blocks []claudeBlock
		if json.Unmarshal(m.Message.Content, &blocks) != nil {
			break
		}
		for _, b := range blocks {
			if b.Type != "tool_result" {
				continue
			}
			call, ok := t.tools[b.ToolUseID]
			if !ok {
				continue
			}
			delete(t.tools, b.ToolUseID)
			reportTool(sink, call, resultText(b.Content), b.IsError)
		}
	case "result":
		var u struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		}
		if json.Unmarshal(m.Usage, &u) == nil {
			sink.Usage(u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens, u.OutputTokens)
		}
		switch {
		case t.interrupted.Load():
			return "interrupted", "", true
		case m.Subtype == "success" && !m.IsError:
			return "completed", "", true
		default:
			reason := strings.TrimSpace(m.Result)
			if reason == "" {
				reason = strings.Join(m.Errors, "；")
			}
			if reason == "" {
				reason = "引擎报告执行失败（" + m.Subtype + "）"
			}
			return "failed", reason, true
		}
	}
	return "", "", false
}

func toolActivity(name string, input map[string]any) string {
	switch name {
	case "Bash":
		return "执行命令：" + firstLine(str(input["command"]))
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "改文件：" + str(input["file_path"])
	case "Read":
		return "读文件：" + str(input["file_path"])
	}
	return "调用工具：" + name
}

func reportTool(sink *sessionSink, call claudeToolCall, output string, isError bool) {
	switch call.name {
	case "Bash":
		var code *int
		if !isError {
			zero := 0
			code = &zero
		} else if rest, ok := strings.CutPrefix(output, "Exit code "); ok {
			num, after, _ := strings.Cut(rest, "\n")
			if n, err := strconv.Atoi(strings.TrimSpace(num)); err == nil {
				code = &n
				output = after
			}
		}
		sink.Command(str(call.input["command"]), output, code)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if !isError {
			sink.File(str(call.input["file_path"]), "write")
		}
	case "Read":
		sink.Activity(fmt.Sprintf("已读文件：%s", str(call.input["file_path"])))
	}
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (d *claudeDriver) Interrupt(ctx context.Context) error {
	d.mu.Lock()
	t := d.turn
	d.mu.Unlock()
	if t == nil {
		return errors.New("没有正在执行的指令")
	}
	t.interrupted.Store(true)
	cctx, cancel := context.WithTimeout(ctx, claudeControlTimeout)
	defer cancel()
	_, err := d.control(cctx, map[string]any{"subtype": "interrupt"})
	return err
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
