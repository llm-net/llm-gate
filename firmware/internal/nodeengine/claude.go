package nodeengine

// 节点端 Claude Code：工作节点上 gate 关联的 claude 以 stream-json 模式运行（与 Claude Agent
// SDK 驱动 CLI 的方式相同）。stdin / stdout 上每行一条 JSON：
//
//   - 设备 → CLI：`{"type":"user","message":{...}}` 是一条指令；`{"type":"control_request",
//     "request_id","request":{"subtype":"initialize"|"interrupt"}}` 是握手与中止；
//     `{"type":"control_response","response":{"subtype":"success","request_id","response":{...}}}`
//     应答 CLI 的反向请求。
//   - CLI → 设备：system / assistant / user / stream_event / result 消息；`control_request`
//     （subtype can_use_tool：`--permission-prompt-tool stdio` 下每次要用工具都先问一声）；
//     `control_response`（应答设备的请求）。
//
// 已按真机验证的协议事实（2.1.241 / 2.1.278，假上游抓取）：assistant 消息逐块给出 text /
// thinking / tool_use；工具结果在随后的 user 消息里（tool_result.content 是字符串或文本块数组，
// Bash 非零退出时 is_error 为真、正文以「Exit code N」起头）；--include-partial-messages 下
// 正文增量是 stream_event 的 content_block_delta / text_delta；一轮以 result 收尾（subtype
// success / error_*，usage 是这一轮的合计）；interrupt 后这一轮以 result（error_during_execution，
// terminal_reason aborted_tools 之类）收尾，进程照常等下一条指令。can_use_tool 答
// {behavior:"allow", updatedInput:<原入参>} 即放行。--tools 里不认识的名字被静默忽略（新版的
// --bare 只有 Bash / Read / Edit），所以把想要的都列上。
//
// 密钥不经环境变量：--settings 的 apiKeyHelper 读实例目录里的 0600 文件（CLI 的 Bash 子进程会
// 继承环境，CLAUDE_CODE_SUBPROCESS_ENV_SCRUB 又要求节点装 bubblewrap）；MCP 令牌同理直接写进
// mcp.json 的请求头。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

const (
	// claudeHandshakeTimeout 覆盖 CLI 冷启动与连上 MCP 服务器。
	claudeHandshakeTimeout = 90 * time.Second
	claudeControlTimeout   = 10 * time.Second
	claudeInterruptGrace   = 10 * time.Second
	// claudeMaxLine 是单条消息的上限（读大文件的工具结果也在一行里）。
	claudeMaxLine = 64 << 20
	// claudeToolTimeoutMs 是 MCP 工具调用的上限（生成视频要陪等到终态，与 Codex 同值）；
	// claudeBashMaxMs 是 Bash 单条命令可要到的上限（与 agenthost.MaxRunTimeout 的 15 分钟一致）。
	claudeToolTimeoutMs = 960_000
	claudeBashMaxMs     = 900_000
)

// ClaudeEfforts 是 Claude Code 的推理档位（--effort）。
var ClaudeEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// claudeTools 是开放给引擎的内置工具（不认识的名字 CLI 会忽略）。不含 WebFetch / WebSearch /
// Task：节点只处理工作空间里的素材，也不开子智能体。
const claudeTools = "Bash,Read,Edit,Write,Glob,Grep"

type claudeEngine struct{ o Options }

// NewClaude 装配节点端 Claude Code 引擎。
func NewClaude(o Options) Engine { return &claudeEngine{o: o} }

func (e *claudeEngine) ID() string        { return ClaudeID }
func (e *claudeEngine) Label() string     { return "Claude Code" }
func (e *claudeEngine) Tool() string      { return "claude" }
func (e *claudeEngine) Efforts() []string { return ClaudeEfforts }

func (e *claudeEngine) Validate(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	return validate(ctx, &e.o, e.Tool(), e.Efforts(), cfg)
}

// Start 在节点上拉起 claude、做 initialize 握手。
func (e *claudeEngine) Start(ctx context.Context, req Request) (hostagent.EngineSession, error) {
	cfg, err := resolve(ctx, &e.o, e.Tool(), req.Chat)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	r, err := e.o.start(ctx, req, launchSpec{
		files: func(base string) map[string]string {
			return map[string]string{
				"key":             cfg.KeyPlaintext,
				"mcp.json":        claudeMCPConfig(base, req.Tools),
				"instructions.md": req.Instructions,
			}
		},
		command: func(base string) string { return claudeCommand(req.Binary, base, cfg) },
	})
	if err != nil {
		return nil, err
	}
	log := e.o.logger().With("srv", "nodeengine", "engine", ClaudeID, "host_id", req.HostID)
	s := &claudeSession{r: r, w: r.stream.Stdin, log: log, pending: map[string]chan claudeControlResult{}, done: make(chan struct{})}
	go s.readLoop(r.stream.Stdout)
	hctx, cancel := context.WithTimeout(ctx, claudeHandshakeTimeout)
	defer cancel()
	if _, err := s.control(hctx, map[string]any{"subtype": "initialize", "hooks": nil}); err != nil {
		derr := r.diagnose("Claude Code 握手失败（节点上的 claude 需支持 stream-json 与 --bare）", err)
		s.Close()
		return nil, derr
	}
	log.Info("节点端引擎会话已启动", "duration_ms", time.Since(started).Milliseconds())
	return s, nil
}

// claudeMCPConfig 渲染 mcp.json：设备的 MCP 端点经远程转发可达，令牌直接写在请求头里。
func claudeMCPConfig(base string, tools hostagent.ToolEndpoint) string {
	cfg := map[string]any{"mcpServers": map[string]any{
		tools.ServerName(): map[string]any{
			"type":    "http",
			"url":     base + tools.URL,
			"headers": map[string]string{"Authorization": "Bearer " + tools.Token},
		},
	}}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// claudeCommand 是包装脚本里拉起 claude 的那一段（实例目录在 $d）。先清掉登录环境里可能把
// 请求改道或换凭据的变量；模型的各档别名都钉成对话选定的模型，后台的小模型请求也记在它名下。
func claudeCommand(bin, base string, cfg hostagent.KeyConfig) string {
	var b strings.Builder
	b.WriteString("unset ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_MODEL ANTHROPIC_CUSTOM_HEADERS " +
		"ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY\n")
	b.WriteString("CLAUDE_CONFIG_DIR=\"$d/claude\" ANTHROPIC_BASE_URL=" + shellQuote(base+"/agents/claude") + " ")
	b.WriteString("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 ")
	b.WriteString("CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 ")
	if cfg.Model != "" {
		m := shellQuote(cfg.Model)
		for _, family := range []string{"FABLE", "OPUS", "SONNET", "HAIKU"} {
			b.WriteString("ANTHROPIC_DEFAULT_" + family + "_MODEL=" + m + " ")
		}
		b.WriteString("ANTHROPIC_SMALL_FAST_MODEL=" + m + " CLAUDE_CODE_SUBAGENT_MODEL=" + m + " ")
	}
	fmt.Fprintf(&b, "MCP_TIMEOUT=30000 MCP_TOOL_TIMEOUT=%d BASH_MAX_TIMEOUT_MS=%d ", claudeToolTimeoutMs, claudeBashMaxMs)
	b.WriteString(shellQuote(bin) + " -p --bare --input-format stream-json --output-format stream-json --verbose --include-partial-messages")
	b.WriteString(" --no-session-persistence --setting-sources '' --strict-mcp-config --mcp-config \"$d/mcp.json\"")
	b.WriteString(" --append-system-prompt-file \"$d/instructions.md\"")
	b.WriteString(" --settings \"{\\\"apiKeyHelper\\\":\\\"cat '$d/key'\\\"}\"")
	b.WriteString(" --tools " + claudeTools + " --permission-prompt-tool stdio")
	if cfg.Model != "" {
		b.WriteString(" --model " + shellQuote(cfg.Model))
	}
	if cfg.Effort != "" {
		b.WriteString(" --effort " + shellQuote(cfg.Effort))
	}
	return b.String()
}

// claudeControlResult 是设备发出的控制请求的应答。
type claudeControlResult struct {
	ok  bool
	msg string
	raw json.RawMessage
}

// claudeSession 是一段 stream-json 会话。
type claudeSession struct {
	r   *remote
	log *slog.Logger

	wMu sync.Mutex
	w   io.Writer

	mu      sync.Mutex
	turn    *claudeTurn
	pending map[string]chan claudeControlResult
	closed  bool
	nextID  atomic.Int64
	done    chan struct{}
}

// claudeTurn 是正在执行的一条指令。
type claudeTurn struct {
	sink        hostagent.Sink
	msgs        chan json.RawMessage
	over        chan struct{}
	interrupted atomic.Bool
	// tools 是已发出、还没收到结果的工具调用（tool_use id → 调用）。
	tools map[string]claudeToolCall
}

type claudeToolCall struct {
	name    string
	input   map[string]any
	started time.Time
}

// send 写一行 JSON。
func (s *claudeSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.wMu.Lock()
	defer s.wMu.Unlock()
	_, err = s.w.Write(b)
	return err
}

// readLoop 读 CLI 的输出：反向请求当场作答，控制应答交给等它的人，其余交给当前指令。
func (s *claudeSession) readLoop(r io.Reader) {
	defer close(s.done)
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
			s.answer(env.RequestID, env.Request)
		case "control_response":
			s.mu.Lock()
			ch, ok := s.pending[env.Response.RequestID]
			delete(s.pending, env.Response.RequestID)
			s.mu.Unlock()
			if ok {
				ch <- claudeControlResult{ok: env.Response.Subtype == "success", msg: env.Response.Error, raw: env.Response.Response}
			}
		case "control_cancel_request", "keep_alive":
		default:
			s.mu.Lock()
			t := s.turn
			s.mu.Unlock()
			if t != nil {
				select {
				case t.msgs <- line:
				case <-t.over:
				}
			}
		}
	}
}

// answer 应答 CLI 的反向请求：工具调用一律放行（权限就是节点登录用户的，每次调用都落时间线），
// 其它请求答不支持。
func (s *claudeSession) answer(id string, raw json.RawMessage) {
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
	if err := s.send(map[string]any{"type": "control_response", "response": resp}); err != nil {
		s.log.Warn("应答引擎反向请求失败", "subtype", req.Subtype, "error", err.Error())
	}
}

// control 发一个控制请求并等应答。
func (s *claudeSession) control(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	id := "llmgate-" + strconv.FormatInt(s.nextID.Add(1), 10)
	ch := make(chan claudeControlResult, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.send(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		return nil, errors.New("引擎连接已断开")
	}
	select {
	case res := <-ch:
		if !res.ok {
			return nil, errors.New(firstNonEmpty(res.msg, "引擎拒绝了请求"))
		}
		return res.raw, nil
	case <-s.done:
		return nil, errors.New("引擎进程已退出")
	case <-ctx.Done():
		return nil, errors.New("引擎未在期限内应答")
	}
}

func (s *claudeSession) Done() <-chan struct{} { return s.done }

// Run 执行一条指令：写一条 user 消息，处理流式事实直到 result。
func (s *claudeSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return hostagent.Outcome{}, &hostagent.Error{Code: hostagent.CodeUnavailable, Msg: "引擎会话已关闭"}
	}
	if s.turn != nil {
		s.mu.Unlock()
		return hostagent.Outcome{}, &hostagent.Error{Code: hostagent.CodeUnavailable, Msg: "引擎会话上已有指令在执行"}
	}
	t := &claudeTurn{sink: sink, msgs: make(chan json.RawMessage, 1024), over: make(chan struct{}), tools: map[string]claudeToolCall{}}
	s.turn = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
		close(t.over)
	}()
	if err := s.send(userMessage(in)); err != nil {
		return hostagent.Outcome{}, &hostagent.Error{Code: hostagent.CodeUnavailable, Msg: "引擎进程已退出"}
	}
	for {
		select {
		case line := <-t.msgs:
			if out, done := s.handle(t, line); done {
				return out, nil
			}
		case <-s.done:
			return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "引擎进程已退出" + tailNote(s.r.stream.StderrTail())}, nil
		case <-ctx.Done():
			_ = s.Interrupt(context.Background())
			grace := time.NewTimer(claudeInterruptGrace)
			defer grace.Stop()
			for {
				select {
				case line := <-t.msgs:
					if out, done := s.handle(t, line); done {
						return out, nil
					}
				case <-grace.C:
					return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
				case <-s.done:
					return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
				}
			}
		}
	}
}

// userMessage 把一条指令折成 stream-json 的 user 消息：文本块 + 图片块（data URI 拆成 base64）。
func userMessage(in hostagent.Input) map[string]any {
	content := make([]map[string]any, 0, 1+len(in.Images))
	if in.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": in.Text})
	}
	for _, img := range in.Images {
		head, data, ok := strings.Cut(img, ";base64,")
		mediaType, isData := strings.CutPrefix(head, "data:")
		if !ok || !isData {
			continue
		}
		content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
	}
	return map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": content},
		"parent_tool_use_id": nil,
		"session_id":         "",
	}
}

// claudeMessage 是 CLI 输出里本包关心的字段。
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
	// result 消息。
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

// handle 处理一条消息；done 为真表示这一轮结束。
func (s *claudeSession) handle(t *claudeTurn, line json.RawMessage) (hostagent.Outcome, bool) {
	var m claudeMessage
	if json.Unmarshal(line, &m) != nil {
		return hostagent.Outcome{}, false
	}
	// 子智能体（Task）的消息带 parent_tool_use_id；本引擎不开子智能体，稳妥起见一律略过。
	if m.ParentToolUseID != nil && *m.ParentToolUseID != "" {
		return hostagent.Outcome{}, false
	}
	switch m.Type {
	case "stream_event":
		if m.Event.Type == "content_block_delta" && m.Event.Delta.Type == "text_delta" && m.Event.Delta.Text != "" {
			t.sink.Delta(m.Event.Delta.Text)
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
					t.sink.Message(b.Text)
				}
			case "thinking":
				if strings.TrimSpace(b.Thinking) != "" {
					t.sink.Reasoning(b.Thinking)
				}
			case "tool_use":
				t.tools[b.ID] = claudeToolCall{name: b.Name, input: b.Input, started: time.Now()}
				t.sink.Activity(toolActivity(b.Name, b.Input))
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
			t.sink.Activity("")
			if es, isExec := t.sink.(hostagent.ExecSink); isExec {
				reportTool(es, call, resultText(b.Content), b.IsError)
			}
		}
	case "result":
		var u struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		}
		if json.Unmarshal(m.Usage, &u) == nil {
			t.sink.Usage(u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens, u.OutputTokens)
		}
		switch {
		case t.interrupted.Load():
			return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, true
		case m.Subtype == "success" && !m.IsError:
			return hostagent.Outcome{Status: hostagent.OutcomeCompleted}, true
		default:
			reason := strings.TrimSpace(m.Result)
			if reason == "" {
				reason = strings.Join(m.Errors, "；")
			}
			if reason == "" {
				reason = "引擎报告执行失败（" + m.Subtype + "）"
			}
			return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: reason}, true
		}
	}
	return hostagent.Outcome{}, false
}

// toolActivity 是工具调用中的一句话读数。
func toolActivity(name string, input map[string]any) string {
	switch name {
	case "Bash":
		return "执行命令：" + firstLine(str(input["command"]))
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "改文件：" + str(input["file_path"])
	case "Read":
		return "读文件：" + str(input["file_path"])
	}
	if tool, ok := strings.CutPrefix(name, "mcp__"); ok {
		if _, inner, found := strings.Cut(tool, "__"); found {
			return "调用工具：" + inner
		}
	}
	return "调用工具：" + name
}

// reportTool 把一次内置工具调用落成时间线事实（MCP 工具由设备自己留痕，这里不报）。
func reportTool(es hostagent.ExecSink, call claudeToolCall, output string, isError bool) {
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
		es.Command(str(call.input["command"]), output, code, time.Since(call.started))
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		if !isError {
			es.FileChange(str(call.input["file_path"]), "write")
		}
	case "Read":
		es.ToolUse(call.name, str(call.input["file_path"]))
	case "Glob", "Grep":
		es.ToolUse(call.name, str(call.input["pattern"]))
	}
}

// resultText 取 tool_result.content 的文本：字符串，或文本块数组拼起来。
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

// Interrupt 请求中止当前这一轮。
func (s *claudeSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	t := s.turn
	s.mu.Unlock()
	if t == nil {
		return errors.New("没有正在执行的指令")
	}
	t.interrupted.Store(true)
	cctx, cancel := context.WithTimeout(ctx, claudeControlTimeout)
	defer cancel()
	_, err := s.control(cctx, map[string]any{"subtype": "interrupt"})
	return err
}

// Close 结束会话：关 stdin 让 CLI 退出、收掉节点上的资源。可重复调用。
func (s *claudeSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	err := s.r.Close()
	s.log.Info("节点端引擎会话已关闭")
	return err
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
