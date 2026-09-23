package hostagent

// Codex App Server 引擎（internal/codexappserver 的运行期）。一段会话 = 一个 app-server
// 实例 + 一条 ephemeral 线程；每条指令是一个 turn。
//
// 模型调用**不直连 OpenAI**：实例的 provider 指向设备自己的 Codex 协议面
// （<回环地址>/agents/codex/v1），凭管理员新建对话时选定的 API 密钥鉴权——订阅授权、
// 计量、预算与其它客户端同一道闸，Key 明文只经环境变量交给实例（与 gate 派生配置同一
// 做法），不落 config.toml。模型与推理档位也是对话钉死的。主机工具经 [mcp_servers.host]
// 指回设备的 /agent-mcp（mcp.go），承载令牌同样只在环境变量里。
//
// 已按真机验证的协议事实（v0.154）：thread/start 的 sandbox 是字符串、developerInstructions
// 直接生效；turn/start 的 input 收 {type:text} 与 {type:image,url:<data URI>}；MCP 工具调用
// 在 approvalPolicy=on-request 下以 `mcpServer/elicitation/request` 反向请求征求许可
// （_meta.codex_approval_kind = mcp_tool_call），应答 {action:"accept"} 后才执行；新模型把
// MCP 工具放在 tool_search 之后（模型自行检索），旧模型直接以 namespace 形式暴露——两者
// 对本包透明。本地 shell 审批一律拒绝（沙箱 read-only，目标主机上的动作只走 MCP）。
//
// §15.1：本文件的日志只有槽位、pid、耗时与结局；通知正文、指令、回复不进日志。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/llm-net/llm-gate/firmware/internal/codexappserver"
)

// CodexEngineID 是引擎标识。
const CodexEngineID = "codex_app_server"

// KeyConfig 是一次会话要用的凭据与模型（由管理面按对话钉死的密钥 / 模型、在引擎对应的
// 开发工具接入面上解析，internal/admin/hostagentoptions.go）。
type KeyConfig struct {
	// Ready 为假时 Reason 说明缺什么（密钥不存在、停用、未授权订阅、模型不可见…）。
	Ready  bool
	Reason string
	// KeyPlaintext 是选定 API 密钥的明文（只经环境变量交给实例）；KeyDisplay 是它的展示串。
	KeyPlaintext string
	KeyDisplay   string
	Model        string
	Effort       string
}

// CodexOptions 装配 Codex 引擎。
type CodexOptions struct {
	Manager *codexappserver.Manager
	// Config 按对话钉死的密钥 / 模型 / 档位解析出可用的凭据；新建对话、每次提交与起会话
	// 都调一次（明文只在起会话那次真正用到）。
	Config func(ctx context.Context, cfg ChatConfig) (KeyConfig, error)
	// BaseURL 是设备自己的回环基址（如 http://127.0.0.1:80），provider 指向它的 /agents/codex/v1。
	BaseURL string
	Logger  *slog.Logger
	Now     func() time.Time
}

// CodexEngine 实现 Engine。
type CodexEngine struct {
	o CodexOptions
}

// NewCodexEngine 装配。
func NewCodexEngine(o CodexOptions) *CodexEngine {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &CodexEngine{o: o}
}

func (e *CodexEngine) ID() string { return CodexEngineID }

// Status 组件装好即就绪；密钥 / 模型按对话各自校验（Validate）。
func (e *CodexEngine) Status(context.Context) EngineStatus {
	st := EngineStatus{ID: CodexEngineID, Label: "Codex App Server"}
	if e.o.Manager == nil {
		st.Reason = "本进程未接入 Codex App Server 组件管理器"
		return st
	}
	if _, err := e.o.Manager.Installed(); err != nil {
		st.Reason = "Codex App Server 尚未安装：请到「组件管理」页安装"
		return st
	}
	st.Ready = true
	return st
}

// CodexEfforts 是可选的推理档位（空 = 引擎缺省）。
var CodexEfforts = []string{"low", "medium", "high", "xhigh"}

func validEffort(effort string) bool {
	if effort == "" {
		return true
	}
	for _, e := range CodexEfforts {
		if e == effort {
			return true
		}
	}
	return false
}

// Validate 校验对话钉死的密钥 / 模型 / 档位此刻能不能起会话，并把留空的模型落成缺省。
func (e *CodexEngine) Validate(ctx context.Context, cfg ChatConfig) (ChatConfig, error) {
	if !validEffort(cfg.Effort) {
		return cfg, &Error{Code: CodeInvalidInput, Msg: "推理档位须是 low、medium、high、xhigh 之一或留空"}
	}
	if cfg.KeyID <= 0 {
		return cfg, &Error{Code: CodeKeyInvalid, Msg: "请为对话选择一把 API 密钥"}
	}
	if e.o.Config == nil {
		return cfg, &Error{Code: CodeEngineNotReady, Msg: "本进程未接入密钥解析"}
	}
	resolved, err := e.o.Config(ctx, cfg)
	if err != nil {
		return cfg, err
	}
	cfg.KeyDisplay = resolved.KeyDisplay
	if !resolved.Ready {
		return cfg, &Error{Code: CodeKeyInvalid, Msg: resolved.Reason}
	}
	cfg.Model = resolved.Model
	return cfg, nil
}

// LoopbackBaseURL 由监听地址算出设备自己的回环基址：未指定主机（空、0.0.0.0、::）取
// 127.0.0.1，指定了就用那个地址。
func LoopbackBaseURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://127.0.0.1"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

const (
	codexHandshakeTimeout = 20 * time.Second
	codexStartTimeout     = 60 * time.Second
	codexInterruptGrace   = 10 * time.Second
	// codexToolTimeoutSec 必须大于 agenthost.MaxRunTimeout，否则引擎先于设备放弃一条长命令。
	codexToolTimeoutSec = 960
)

// Start 拉起实例、握手、开线程。
func (e *CodexEngine) Start(ctx context.Context, req StartRequest) (EngineSession, error) {
	if e.o.Manager == nil {
		return nil, &Error{Code: CodeEngineNotReady, Msg: "本进程未接入 Codex App Server 组件管理器"}
	}
	if _, err := e.o.Manager.Installed(); err != nil {
		return nil, &Error{Code: CodeEngineNotReady, Msg: "Codex App Server 尚未安装：请到「组件管理」页安装"}
	}
	cfg, err := e.o.Config(ctx, ChatConfig{KeyID: req.Chat.KeyID, KeyDisplay: req.Chat.KeyDisplay, Model: req.Chat.Model, Effort: req.Chat.Effort})
	if err != nil {
		return nil, err
	}
	if !cfg.Ready {
		return nil, &Error{Code: CodeKeyInvalid, Msg: cfg.Reason}
	}
	p, err := e.o.Manager.Launch(codexappserver.LaunchOptions{
		ConfigTOML: []byte(CodexConfigTOML(e.o.BaseURL, req.Tools, cfg.Model, cfg.Effort)),
		Env:        []string{"LLMGATE_API_KEY=" + cfg.KeyPlaintext, "LLMGATE_MCP_TOKEN=" + req.Tools.Token},
	})
	if err != nil {
		return nil, &Error{Code: CodeEngineNotReady, Msg: "启动 Codex App Server 失败：" + err.Error()}
	}
	hctx, cancel := context.WithTimeout(ctx, codexHandshakeTimeout)
	defer cancel()
	if _, err := p.Initialize(hctx); err != nil {
		p.Close()
		return nil, &Error{Code: CodeEngineNotReady, Msg: "Codex App Server 握手失败：" + err.Error()}
	}
	return StartCodexThread(ctx, p.Client, p.Close, CodexThreadOptions{
		Cwd: filepath.Join(p.Home, "work"), Sandbox: "read-only", Instructions: req.Instructions,
		Model: cfg.Model, Effort: cfg.Effort, Log: e.o.Logger.With("srv", "hostagent", "pid", p.PID()),
	})
}

// CodexThreadOptions 是在一条已握手的 app-server 连接上开线程的参数（板端与节点端共用）。
type CodexThreadOptions struct {
	// Cwd 是线程的工作目录；Sandbox 是 SandboxMode 字符串（read-only / danger-full-access）。
	Cwd, Sandbox string
	// Instructions 是开发者指令；Model 可空，Effort 是每条指令的推理档位（可空）。
	Instructions, Model, Effort string
	// LocalExec 为真时引擎自己的命令 / 改文件审批一律放行（节点端：引擎就跑在目标机器上，
	// 权限就是节点登录用户的）；为假一律拒绝（板端：目标主机上的动作只走 MCP）。
	LocalExec bool
	Log       *slog.Logger
}

// StartCodexThread 在 client 上开一条 ephemeral 线程并返回会话；closeFn 负责结束引擎进程（会话
// Close 时调用，开线程失败时也会调用）。
func StartCodexThread(ctx context.Context, client *codexappserver.Client, closeFn func() error, o CodexThreadOptions) (EngineSession, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	sctx, cancel := context.WithTimeout(ctx, codexStartTimeout)
	defer cancel()
	params := map[string]any{
		"ephemeral":             true,
		"cwd":                   o.Cwd,
		"approvalPolicy":        "on-request",
		"sandbox":               o.Sandbox,
		"developerInstructions": o.Instructions,
	}
	if o.Model != "" {
		params["model"] = o.Model
	}
	if err := client.Call(sctx, "thread/start", params, &started); err != nil {
		closeFn()
		return nil, &Error{Code: CodeEngineNotReady, Msg: "Codex App Server 开线程失败：" + err.Error()}
	}
	s := &codexSession{client: client, closeFn: closeFn, log: o.Log, threadID: started.Thread.ID, effort: o.Effort, localExec: o.LocalExec}
	go s.readLoop()
	return s, nil
}

// CodexConfigTOML 渲染实例的 config.toml：provider 指向 baseURL 的 /agents/codex/v1，MCP 服务器
// 指向 tools.URL。Key 与令牌不在其中（env_key / bearer_token_env_var）；MCP 服务器名取
// tools.ServerName()（Agent远控是 host，创作工作空间是 studio）。history 与 analytics 关掉：
// 会话记录只在设备上（线程本就 ephemeral，history.jsonl 也不写），不向 OpenAI 报遥测。
func CodexConfigTOML(baseURL string, tools ToolEndpoint, model, effort string) string {
	var b strings.Builder
	b.WriteString("model_provider = \"llmgate\"\n")
	if model != "" {
		fmt.Fprintf(&b, "model = %s\n", tomlQuote(model))
	}
	if effort != "" {
		fmt.Fprintf(&b, "model_reasoning_effort = %s\n", tomlQuote(effort))
	}
	b.WriteString("web_search = \"disabled\"\n\n")
	b.WriteString("[model_providers.llmgate]\nname = \"LLM Gate\"\n")
	fmt.Fprintf(&b, "base_url = %s\n", tomlQuote(strings.TrimRight(baseURL, "/")+"/agents/codex/v1"))
	b.WriteString("wire_api = \"responses\"\nenv_key = \"LLMGATE_API_KEY\"\n\n")
	b.WriteString("[features]\nmulti_agent = false\nmulti_agent_v2 = false\n\n")
	b.WriteString("[history]\npersistence = \"none\"\n\n[analytics]\nenabled = false\n\n")
	fmt.Fprintf(&b, "[mcp_servers.%s]\n", tools.ServerName())
	fmt.Fprintf(&b, "url = %s\n", tomlQuote(tools.URL))
	b.WriteString("bearer_token_env_var = \"LLMGATE_MCP_TOKEN\"\nstartup_timeout_sec = 30\n")
	fmt.Fprintf(&b, "tool_timeout_sec = %d\n", codexToolTimeoutSec)
	return b.String()
}

func tomlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codexSession 是一段 app-server 会话。
type codexSession struct {
	client    *codexappserver.Client
	closeFn   func() error
	log       *slog.Logger
	threadID  string
	effort    string
	localExec bool

	mu     sync.Mutex
	turn   *codexTurn
	closed bool
	// usageIn / usageOut 是线程累计用量（thread/tokenUsage/updated 的 total），每条指令按差值记。
	usageIn, usageOut int64
}

// codexTurn 是正在执行的一轮。
type codexTurn struct {
	id     string
	sink   Sink
	msgs   chan codexappserver.Message
	baseIn int64
	baseOu int64
}

// readLoop 持续消费实例通知：反向请求当场作答，通知转给当前 turn（没有 turn 时丢弃）。
func (s *codexSession) readLoop() {
	for {
		select {
		case msg := <-s.client.Incoming():
			if len(msg.ID) > 0 {
				s.answer(msg)
				continue
			}
			s.mu.Lock()
			t := s.turn
			s.mu.Unlock()
			if t == nil {
				continue
			}
			select {
			case t.msgs <- msg:
			case <-s.client.Done():
				return
			}
		case <-s.client.Done():
			return
		}
	}
}

// answer 应答服务端反向请求：MCP 工具调用一律放行（每次调用都在设备侧留痕）；本地命令 /
// 补丁审批按 localExec 放行（节点端）或拒绝（板端）；权限升级一律拒绝；模型向用户提问
// （request_user_input）答空的 answers 映射——没有人在场作答，空答案让模型自己往下走；其它未知
// 请求答空对象让引擎继续。审批类请求没有服务端超时，不答这一轮就一直挂着，所以每种都要答。
func (s *codexSession) answer(msg codexappserver.Message) {
	var result any
	switch msg.Method {
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": "accept", "content": map[string]any{}}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		if s.localExec {
			result = map[string]any{"decision": "accept"}
		} else {
			result = map[string]any{"decision": "decline"}
		}
	case "item/permissions/requestApproval":
		result = map[string]any{"decision": "decline"}
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
	default:
		result = map[string]any{}
	}
	if err := s.client.Reply(msg.ID, result); err != nil {
		s.log.Warn("应答引擎反向请求失败", "method", msg.Method, "error", err.Error())
	}
}

func (s *codexSession) Done() <-chan struct{} { return s.client.Done() }

// Run 执行一条指令：turn/start 后收通知直到 turn/completed。
func (s *codexSession) Run(ctx context.Context, in Input, sink Sink) (Outcome, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Outcome{}, &Error{Code: CodeUnavailable, Msg: "引擎会话已关闭"}
	}
	if s.turn != nil {
		s.mu.Unlock()
		return Outcome{}, &Error{Code: CodeUnavailable, Msg: "引擎会话上已有指令在执行"}
	}
	t := &codexTurn{sink: sink, msgs: make(chan codexappserver.Message, 1024), baseIn: s.usageIn, baseOu: s.usageOut}
	s.turn = t
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()

	items := make([]map[string]any, 0, 1+len(in.Images))
	if in.Text != "" {
		items = append(items, map[string]any{"type": "text", "text": in.Text})
	}
	for _, img := range in.Images {
		items = append(items, map[string]any{"type": "image", "url": img})
	}
	params := map[string]any{"threadId": s.threadID, "input": items}
	if s.effort != "" {
		params["effort"] = s.effort
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	callCtx, cancel := context.WithTimeout(ctx, codexStartTimeout)
	err := s.client.Call(callCtx, "turn/start", params, &started)
	cancel()
	if err != nil {
		if errors.Is(err, codexappserver.ErrClientClosed) {
			return Outcome{}, &Error{Code: CodeUnavailable, Msg: "引擎进程已退出"}
		}
		return Outcome{}, &Error{Code: CodeUnavailable, Msg: "引擎未能开始执行：" + err.Error()}
	}
	s.mu.Lock()
	t.id = started.Turn.ID
	s.mu.Unlock()

	lastError := ""
	for {
		select {
		case msg := <-t.msgs:
			if out, done := s.handle(t, msg, &lastError); done {
				return out, nil
			}
		case <-s.client.Done():
			return Outcome{Status: OutcomeFailed, Error: firstNonEmpty(lastError, "引擎进程已退出")}, nil
		case <-ctx.Done():
			// 指令被取消或超时：请引擎中止这一轮，再等一小会儿收尾。
			_ = s.Interrupt(context.Background())
			grace := time.NewTimer(codexInterruptGrace)
			for {
				select {
				case msg := <-t.msgs:
					if out, done := s.handle(t, msg, &lastError); done {
						grace.Stop()
						return out, nil
					}
				case <-grace.C:
					return Outcome{Status: OutcomeInterrupted}, nil
				case <-s.client.Done():
					grace.Stop()
					return Outcome{Status: OutcomeInterrupted}, nil
				}
			}
		}
	}
}

// handle 处理一条通知；done 为真表示这一轮结束。
func (s *codexSession) handle(t *codexTurn, msg codexappserver.Message, lastError *string) (Outcome, bool) {
	switch msg.Method {
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.Delta != "" {
			t.sink.Delta(p.Delta)
		}
	case "item/started", "item/completed":
		var p struct {
			Item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Server  string `json:"server"`
				Tool    string `json:"tool"`
				Command string `json:"command"`
				Summary []struct {
					Text string `json:"text"`
				} `json:"summary"`
				Status string `json:"status"`
				// 以下是节点端引擎本地执行的读数（commandExecution / fileChange）。
				AggregatedOutput string `json:"aggregatedOutput"`
				ExitCode         *int   `json:"exitCode"`
				DurationMs       int64  `json:"durationMs"`
				Changes          []struct {
					Path string          `json:"path"`
					Kind json.RawMessage `json:"kind"`
				} `json:"changes"`
			} `json:"item"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return Outcome{}, false
		}
		completed := msg.Method == "item/completed"
		switch p.Item.Type {
		case "agentMessage":
			if completed {
				t.sink.Message(p.Item.Text)
			}
		case "reasoning":
			if completed {
				parts := make([]string, 0, len(p.Item.Summary))
				for _, sm := range p.Item.Summary {
					if strings.TrimSpace(sm.Text) != "" {
						parts = append(parts, sm.Text)
					}
				}
				t.sink.Reasoning(strings.Join(parts, "\n"))
			} else {
				t.sink.Activity("思考中…")
			}
		case "mcpToolCall":
			if !completed {
				t.sink.Activity("调用工具：" + p.Item.Tool)
			}
		case "commandExecution":
			if !completed {
				t.sink.Activity("本地命令：" + firstLine(UnwrapShellCommand(p.Item.Command)))
				break
			}
			t.sink.Activity("")
			if es, ok := t.sink.(ExecSink); ok && s.localExec {
				es.Command(UnwrapShellCommand(p.Item.Command), p.Item.AggregatedOutput, p.Item.ExitCode, time.Duration(p.Item.DurationMs)*time.Millisecond)
			}
		case "fileChange":
			if es, ok := t.sink.(ExecSink); ok && completed && s.localExec {
				for _, ch := range p.Item.Changes {
					es.FileChange(ch.Path, fileChangeAction(ch.Kind))
				}
			}
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Total struct {
					InputTokens  int64 `json:"inputTokens"`
					OutputTokens int64 `json:"outputTokens"`
				} `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			s.mu.Lock()
			s.usageIn, s.usageOut = p.TokenUsage.Total.InputTokens, p.TokenUsage.Total.OutputTokens
			in, out := s.usageIn-t.baseIn, s.usageOut-t.baseOu
			s.mu.Unlock()
			if in < 0 {
				in = 0
			}
			if out < 0 {
				out = 0
			}
			t.sink.Usage(in, out)
		}
	case "error":
		var p struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && !p.WillRetry && p.Error.Message != "" {
			*lastError = p.Error.Message
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return Outcome{Status: OutcomeFailed, Error: "引擎回报了无法解析的结局"}, true
		}
		switch p.Turn.Status {
		case "completed":
			return Outcome{Status: OutcomeCompleted}, true
		case "interrupted":
			return Outcome{Status: OutcomeInterrupted}, true
		default:
			reason := *lastError
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				reason = p.Turn.Error.Message
			}
			return Outcome{Status: OutcomeFailed, Error: firstNonEmpty(reason, "引擎报告执行失败")}, true
		}
	}
	return Outcome{}, false
}

// Interrupt 请求中止当前这一轮。
func (s *codexSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	t := s.turn
	s.mu.Unlock()
	if t == nil || t.id == "" {
		return errors.New("没有正在执行的指令")
	}
	cctx, cancel := context.WithTimeout(ctx, codexInterruptGrace)
	defer cancel()
	return s.client.Call(cctx, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": t.id}, nil)
}

// Close 结束实例。
func (s *codexSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.closeFn()
}

// shellWrapRE 认出 codex 包在命令外面的那层登录 shell（`/bin/bash -lc '…'`）。
var shellWrapRE = regexp.MustCompile(`^\S*/?(?:ba|z|da)?sh -l?c '(.*)'$`)

// UnwrapShellCommand 剥掉 `<shell> -lc '…'` 这层包装，还原模型写的那条命令；认不出原样返回。
func UnwrapShellCommand(cmd string) string {
	m := shellWrapRE.FindStringSubmatch(strings.TrimSpace(cmd))
	if m == nil {
		return cmd
	}
	return strings.ReplaceAll(m[1], `'\''`, `'`)
}

// fileChangeAction 把 fileChange 的 kind（{"type":"add|update|delete"} 或同名字符串）折成
// ExecSink 的动作：delete 或 write。
func fileChangeAction(kind json.RawMessage) string {
	var k struct {
		Type string `json:"type"`
	}
	var name string
	if json.Unmarshal(kind, &k) == nil && k.Type != "" {
		name = k.Type
	} else {
		_ = json.Unmarshal(kind, &name)
	}
	if strings.EqualFold(name, "delete") {
		return "delete"
	}
	return "write"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
