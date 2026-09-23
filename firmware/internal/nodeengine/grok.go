package nodeengine

// Grok Build 在工作节点以 ACP（stdio JSON-RPC）运行。模型接入与 Studio MCP 均经 SSH
// 转发回设备。Grok 会自动保存会话，故整个 GROK_HOME 必须位于 tmpfs，关闭时一并清理。
// 协议正文只在内存流转；引擎错误只报告错误码，不把上游正文或凭据带进错误/日志。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
	"github.com/llm-net/llm-gate/firmware/internal/hostagent"
)

var GrokEfforts = []string{"low", "medium", "high", "xhigh"}

const grokHandshakeTimeout = 90 * time.Second
const grokInterruptGrace = 10 * time.Second

type grokEngine struct{ o Options }

func NewGrok(o Options) Engine          { return &grokEngine{o: o} }
func (e *grokEngine) ID() string        { return GrokID }
func (e *grokEngine) Label() string     { return "Grok" }
func (e *grokEngine) Tool() string      { return "grok" }
func (e *grokEngine) Efforts() []string { return GrokEfforts }
func (e *grokEngine) Validate(ctx context.Context, cfg hostagent.ChatConfig) (hostagent.ChatConfig, error) {
	return validate(ctx, &e.o, e.Tool(), e.Efforts(), cfg)
}

func (e *grokEngine) Start(ctx context.Context, req Request) (hostagent.EngineSession, error) {
	cfg, err := resolve(ctx, &e.o, e.Tool(), req.Chat)
	if err != nil {
		return nil, err
	}
	r, err := e.o.start(ctx, req, launchSpec{
		volatile: true,
		files: func(base string) map[string]string {
			return map[string]string{"config.toml": grokConfig(base, req.Tools, cfg)}
		},
		command: func(string) string { return grokCommand(req.Binary) },
	})
	if err != nil {
		return nil, err
	}
	s := &grokSession{r: r, client: codexappserver.NewClient(r.stream.Stdout, r.stream.Stdin, 1024)}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, grokHandshakeTimeout)
	defer cancel()
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
		AuthMethods     []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
		AgentCapabilities struct {
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	// 不声明客户端文件/终端能力：Grok 使用节点上自己的工具，执行事实从 ACP 通知接收。
	err = s.call(hctx, "initialize", map[string]any{
		"protocolVersion": 1, "clientCapabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "llmgate", "version": e.o.Version},
	}, &init, nil)
	if err != nil {
		return nil, notReady("Grok ACP 握手失败，请检查节点上的 Grok 版本：" + grokError(err))
	}
	if init.ProtocolVersion != 1 {
		return nil, notReady("Grok ACP 协议版本不受支持")
	}
	hasKey := false
	for _, method := range init.AuthMethods {
		if method.ID == "xai.api_key" {
			hasKey = true
		}
	}
	if !hasKey {
		return nil, notReady("节点上的 Grok 不支持独立模型凭据，请升级 Grok")
	}
	if err = s.call(hctx, "authenticate", map[string]any{"methodId": "xai.api_key", "_meta": map[string]any{"headless": true}}, nil, nil); err != nil {
		return nil, notReady("Grok 认证失败：" + grokError(err))
	}
	meta := map[string]any{
		"modelId": "llmgate", "rules": req.Instructions,
		// ACP 不采用顶层 --tools 参数。使用内部工具 ID；未知 ID 会令 Grok 放弃整个白名单。
		"agentProfile": map[string]any{
			"name": "llmgate-studio", "description": "LLM Gate Studio",
			"discoverSkills": false, "agentsMd": false,
			"tools": []string{"run_terminal_cmd", "read_file", "search_replace", "list_dir", "grep", "write", "search_tool", "use_tool", "get_task_output", "kill_task", "wait_tasks"},
		},
	}
	if cfg.Effort != "" {
		meta["reasoningEffort"] = cfg.Effort
	}
	var session struct {
		ID     string `json:"sessionId"`
		Models struct {
			Current string `json:"currentModelId"`
		} `json:"models"`
	}
	if err = s.call(hctx, "session/new", map[string]any{"cwd": req.Workdir, "mcpServers": []any{}, "_meta": meta}, &session, nil); err != nil {
		return nil, notReady("Grok 创建会话失败：" + grokError(err))
	}
	if session.ID == "" || session.Models.Current != "llmgate" {
		return nil, notReady("Grok 未采用对话指定的模型配置")
	}
	s.id, s.images = session.ID, init.AgentCapabilities.PromptCapabilities.Image
	e.o.logger().Info("节点端引擎会话已启动", "srv", "nodeengine", "engine", GrokID, "host_id", req.HostID)
	ok = true
	return s, nil
}

func grokConfig(base string, tools hostagent.ToolEndpoint, cfg hostagent.KeyConfig) string {
	var b strings.Builder
	b.WriteString(`[cli]
auto_update = false
[models]
default = "llmgate"
session_summary = "llmgate"
image_description = "llmgate"
prompt_suggestion = "llmgate"
[model.llmgate]
name = "LLM Gate"
api_backend = "responses"
supports_reasoning_effort = true
`)
	fmt.Fprintf(&b, "model = %s\nbase_url = %s\napi_key = %s\n", tomlQuote(cfg.Model), tomlQuote(base+"/agents/grok/v1"), tomlQuote(cfg.KeyPlaintext))
	b.WriteString(`[features]
telemetry = false
feedback = false
managed_config = false
codebase_indexing = false
image_gen = false
video_gen = false
title_refresh = false
[session]
load_envrc = false
[subagents]
enabled = false
[memory]
enabled = false
[managed_mcps]
enabled = false
`)
	fmt.Fprintf(&b, "[mcp_servers.%s]\nurl = %s\nheaders = { Authorization = %s }\nstartup_timeout_sec = 30\ntool_timeout_sec = 960\n", tools.ServerName(), tomlQuote(base+tools.URL), tomlQuote("Bearer "+tools.Token))
	return b.String()
}

func grokCommand(bin string) string {
	// 清掉继承的 Grok 配置/凭据/日志目的地；不把 API Key 或 MCP token 放进环境变量。
	return `for n in $(env | sed -n 's/^\(GROK_[A-Za-z0-9_]*\|XAI_[A-Za-z0-9_]*\|OTEL_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$n"; done
GROK_HOME="$d" GROK_MEMORY=0 GROK_SUBAGENTS=0 GROK_TELEMETRY_ENABLED=0 GROK_FEEDBACK_ENABLED=0 GROK_TELEMETRY_TRACE_UPLOAD=0 GROK_STORAGE_MODE=local GROK_IMAGE_EDIT=0 GROK_LOG_FILE=/dev/null RUST_LOG=off ` + shellQuote(bin) + ` --disable-web-search agent stdio`
}

type grokSession struct {
	r         *remote
	client    *codexappserver.Client
	id        string
	images    bool
	mu        sync.Mutex
	turn      *grokTurn
	closed    bool
	closeOnce sync.Once
}

type grokTurn struct {
	sink          hostagent.Sink
	text, thought strings.Builder
	tools         map[string]*grokTool
	interrupt     chan struct{}
	once          sync.Once
}

type grokTool struct {
	ID      string          `json:"toolCallId"`
	Title   string          `json:"title"`
	Kind    string          `json:"kind"`
	Status  string          `json:"status"`
	Input   map[string]any  `json:"rawInput"`
	Output  json.RawMessage `json:"rawOutput"`
	Content []struct {
		Type    string `json:"type"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
		Path string `json:"path"`
	} `json:"content"`
	Locations []struct {
		Path string `json:"path"`
	} `json:"locations"`
	started  time.Time
	reported bool
}

// call 在等 RPC 应答时消费通知和反向请求。返回前排空应答之前已入队的通知，保证末尾正文和
// 工具结果先于本轮完成落到 Sink。中止超时则关闭进程，下一轮不会混入旧通知。
func (s *grokSession) call(ctx context.Context, method string, params, result any, turn *grokTurn) error {
	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type response struct {
		raw json.RawMessage
		err error
	}
	completed := make(chan response, 1)
	go func() {
		var raw json.RawMessage
		err := s.client.Call(callCtx, method, params, &raw)
		completed <- response{raw: raw, err: err}
	}()
	var interrupted <-chan struct{}
	if turn != nil {
		interrupted = turn.interrupt
	}
	var grace <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	ctxDone := ctx.Done()
	for {
		select {
		case msg := <-s.client.Incoming():
			s.handle(msg, turn)
		case res := <-completed:
			for {
				select {
				case msg := <-s.client.Incoming():
					s.handle(msg, turn)
				default:
					if res.err != nil {
						return res.err
					}
					if result != nil {
						return json.Unmarshal(res.raw, result)
					}
					return nil
				}
			}
		case <-ctxDone:
			if turn == nil {
				return ctx.Err()
			}
			_ = s.Interrupt(context.Background())
			ctxDone = nil
		case <-interrupted:
			interrupted = nil
			timer = time.NewTimer(grokInterruptGrace)
			grace = timer.C
		case <-grace:
			s.Close()
			return context.Canceled
		}
	}
}

func (s *grokSession) Run(ctx context.Context, in hostagent.Input, sink hostagent.Sink) (hostagent.Outcome, error) {
	s.mu.Lock()
	if s.closed || s.turn != nil {
		s.mu.Unlock()
		return hostagent.Outcome{}, &hostagent.Error{Code: hostagent.CodeUnavailable, Msg: "引擎会话已关闭或正在执行指令"}
	}
	if len(in.Images) > 0 && !s.images {
		s.mu.Unlock()
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "节点上的 Grok 未声明支持图片输入，请把图片上传到素材库后引用文件路径"}, nil
	}
	turn := &grokTurn{sink: sink, tools: map[string]*grokTool{}, interrupt: make(chan struct{})}
	s.turn = turn
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.turn = nil; s.mu.Unlock() }()
	prompt := []map[string]any{{"type": "text", "text": in.Text}}
	for _, img := range in.Images {
		head, data, ok := strings.Cut(img, ";base64,")
		mime, isData := strings.CutPrefix(head, "data:")
		if !ok || !isData {
			return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "图片输入格式不受支持"}, nil
		}
		prompt = append(prompt, map[string]any{"type": "image", "mimeType": mime, "data": data})
	}
	var result struct {
		Stop string `json:"stopReason"`
		Meta struct {
			Input  int64 `json:"inputTokens"`
			Output int64 `json:"outputTokens"`
			Usage  *struct {
				ModelUsage map[string]struct {
					Input  int64 `json:"inputTokens"`
					Output int64 `json:"outputTokens"`
				} `json:"modelUsage"`
			} `json:"usage"`
		} `json:"_meta"`
	}
	err := s.call(ctx, "session/prompt", map[string]any{"sessionId": s.id, "prompt": prompt}, &result, turn)
	turn.flush()
	sink.Activity("")
	if result.Meta.Usage != nil {
		result.Meta.Input, result.Meta.Output = 0, 0
		for _, u := range result.Meta.Usage.ModelUsage {
			result.Meta.Input += u.Input
			result.Meta.Output += u.Output
		}
	}
	sink.Usage(result.Meta.Input, result.Meta.Output)
	select {
	case <-turn.interrupt:
		return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
	default:
	}
	if err != nil {
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: grokError(err)}, nil
	}
	switch result.Stop {
	case "end_turn":
		return hostagent.Outcome{Status: hostagent.OutcomeCompleted}, nil
	case "cancelled":
		return hostagent.Outcome{Status: hostagent.OutcomeInterrupted}, nil
	default:
		return hostagent.Outcome{Status: hostagent.OutcomeFailed, Error: "Grok 未完成本轮指令"}, nil
	}
}

func (t *grokTurn) flush() {
	if t.thought.Len() > 0 {
		t.sink.Reasoning(t.thought.String())
		t.thought.Reset()
	}
	if t.text.Len() > 0 {
		t.sink.Message(t.text.String())
		t.text.Reset()
	}
}

func (s *grokSession) handle(msg codexappserver.Message, turn *grokTurn) {
	if len(msg.ID) > 0 {
		if msg.Method != "session/request_permission" {
			_ = s.client.ReplyError(msg.ID, -32601, "Unsupported client method")
			return
		}
		var req struct {
			SessionID string `json:"sessionId"`
			Options   []struct {
				ID   string `json:"optionId"`
				Kind string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(msg.Params, &req)
		outcome := map[string]string{"outcome": "cancelled"}
		if turn != nil && req.SessionID == s.id {
			select {
			case <-turn.interrupt:
			default:
				for _, opt := range req.Options {
					if opt.Kind == "allow_once" {
						outcome = map[string]string{"outcome": "selected", "optionId": opt.ID}
						break
					}
				}
			}
		}
		_ = s.client.Reply(msg.ID, map[string]any{"outcome": outcome})
		return
	}
	if turn == nil || msg.Method != "session/update" {
		return
	}
	var msgUpdate struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Type    string `json:"sessionUpdate"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	// tool_call 的 content 是数组，需单独解包，不能与正文块共用结构。
	var env struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(msg.Params, &env) != nil || env.SessionID != s.id {
		return
	}
	var kind struct {
		Type string `json:"sessionUpdate"`
	}
	if json.Unmarshal(env.Update, &kind) != nil {
		return
	}
	switch kind.Type {
	case "agent_message_chunk", "agent_thought_chunk":
		if json.Unmarshal(msg.Params, &msgUpdate) != nil || msgUpdate.Update.Content.Type != "text" {
			return
		}
		text := msgUpdate.Update.Content.Text
		if kind.Type == "agent_message_chunk" {
			turn.text.WriteString(text)
			turn.sink.Delta(text)
		} else {
			turn.thought.WriteString(text)
		}
	case "tool_call", "tool_call_update":
		var id struct {
			ID string `json:"toolCallId"`
		}
		if json.Unmarshal(env.Update, &id) != nil || id.ID == "" {
			return
		}
		call := turn.tools[id.ID]
		if call == nil {
			call = &grokTool{started: time.Now()}
			turn.tools[id.ID] = call
		}
		if json.Unmarshal(env.Update, call) != nil {
			return
		}
		if call.reported {
			return
		}
		turn.flush()
		if call.Status == "completed" || call.Status == "failed" {
			call.reported = true
			if es, ok := turn.sink.(hostagent.ExecSink); ok {
				call.report(es)
			}
			turn.sink.Activity("")
		} else {
			turn.sink.Activity("正在调用工具：" + call.Title)
		}
	}
}

func (c *grokTool) report(sink hostagent.ExecSink) {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := c.Input[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch c.Kind {
	case "execute":
		var raw struct {
			Output   json.RawMessage `json:"output"`
			Stdout   string          `json:"stdout"`
			Stderr   string          `json:"stderr"`
			ExitCode *int            `json:"exit_code"`
		}
		_ = json.Unmarshal(c.Output, &raw)
		var output string
		if json.Unmarshal(raw.Output, &output) != nil {
			// Grok 1.0 的 Bash 输出是字节数组，文本仍以 UTF-8 交给时间线。
			var bytes []byte
			if json.Unmarshal(raw.Output, &bytes) == nil {
				output = string(bytes)
			}
		}
		output = firstNonEmpty(output, raw.Stdout)
		if raw.Stderr != "" {
			output += raw.Stderr
		}
		if output == "" {
			for _, part := range c.Content {
				if part.Type == "content" {
					output += part.Content.Text
				}
			}
		}
		sink.Command(firstNonEmpty(get("command", "cmd"), c.Title), output, raw.ExitCode, time.Since(c.started))
	case "edit", "delete", "move":
		if c.Status == "failed" {
			sink.ToolUse(c.Title, get("path", "file_path"))
			return
		}
		action := "write"
		if c.Kind == "delete" {
			action = "delete"
		}
		paths := map[string]bool{}
		for _, loc := range c.Locations {
			if loc.Path != "" {
				paths[loc.Path] = true
			}
		}
		for _, part := range c.Content {
			if part.Type == "diff" && part.Path != "" {
				paths[part.Path] = true
			}
		}
		if len(paths) == 0 {
			if p := get("path", "file_path"); p != "" {
				paths[p] = true
			}
		}
		for p := range paths {
			sink.FileChange(p, action)
		}
	default:
		sink.ToolUse(c.Title, get("path", "file_path", "pattern", "query"))
	}
}

func (s *grokSession) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return nil
	}
	var err error
	s.turn.once.Do(func() {
		err = s.client.Notify("session/cancel", map[string]any{"sessionId": s.id})
		close(s.turn.interrupt)
	})
	return err
}
func (s *grokSession) Done() <-chan struct{} { return s.client.Done() }
func (s *grokSession) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.client.Close()
		_ = s.r.Close()
	})
	return nil
}

func grokError(err error) string {
	var rpc *codexappserver.RPCError
	if errors.As(err, &rpc) {
		return fmt.Sprintf("Grok 请求失败（错误码 %d）", rpc.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Grok 未在期限内应答"
	}
	return "Grok 连接已断开"
}
